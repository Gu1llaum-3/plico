package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Gu1llaum-3/plico/internal/api"
	"github.com/Gu1llaum-3/plico/internal/compose"
	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/deploy"
	"github.com/Gu1llaum-3/plico/internal/execx"
	"github.com/Gu1llaum-3/plico/internal/gitrepo"
	"github.com/Gu1llaum-3/plico/internal/hooks"
	"github.com/Gu1llaum-3/plico/internal/notify"
	"github.com/Gu1llaum-3/plico/internal/scheduler"
	"github.com/Gu1llaum-3/plico/internal/sopsx"
	"github.com/Gu1llaum-3/plico/internal/state"
)

var serveConfigPath string

var serveCmd = &cobra.Command{
	Use:   "serve",
	Short: "Run the plico daemon (polling loop + /healthz)",
	Args:  cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		return serve(serveConfigPath)
	},
}

func init() {
	serveCmd.Flags().StringVarP(&serveConfigPath, "config", "c", "/etc/plico/config.toml", "path to config.toml")
	rootCmd.AddCommand(serveCmd)
}

func serve(configPath string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	level, err := runtimeChecks(cfg, true)
	if err != nil {
		return err
	}
	log, closeLog, err := newLogger(cfg.Log, level)
	if err != nil {
		return err
	}
	defer closeLog()

	if err := os.MkdirAll(cfg.BaseDir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfg.StateFile), 0o750); err != nil {
		return err
	}
	store, err := state.Open(cfg.StateFile)
	if err != nil {
		return err
	}

	var notifier notify.Notifier = notify.Nop{}
	if cfg.Ntfy.URL != "" {
		notifier = notify.WithLogFallback(notify.NewNtfy(cfg.Ntfy.URL, cfg.Ntfy.Token), log)
	}

	runner := execx.NewRunner(log)
	git := gitrepo.New(runner, cfg.Git.Auths, log)
	runtime := compose.NewDocker(runner)
	hookRunner := hooks.New(runner, log)
	deployer := deploy.New(cfg, git, runtime, hookRunner, notifier, store, runner, log)
	sched, err := scheduler.New(cfg, deployer, store, log)
	if err != nil {
		return fmt.Errorf("scheduler: %w", err)
	}
	server := api.New(cfg.Health.Listen, sched, store,
		cfg.PollInterval.Duration, cfg.RunTimeout.Duration)

	// Bind the socket synchronously, before other goroutines run: the bind
	// must happen under the restrictive umask (see SocketServer.Listen),
	// and a double daemon start must fail here, loudly.
	sockSrv := api.NewSocket(cfg, sched, store, deployer, log)
	if err := sockSrv.Listen(); err != nil {
		return err
	}
	defer sockSrv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 2)
	go func() {
		log.Info("healthz listening", "addr", cfg.Health.Listen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("healthz server failed", "error", err)
			serveErr <- fmt.Errorf("healthz server: %w", err)
		}
	}()
	go func() {
		if err := sockSrv.Serve(); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, net.ErrClosed) {
			log.Error("client API server failed", "error", err)
			serveErr <- fmt.Errorf("client API server: %w", err)
		}
	}()

	log.Info("plico starting",
		"version", version,
		"stacks", len(cfg.Stacks),
		"poll_interval", cfg.PollInterval.String(),
		"base_dir", cfg.BaseDir,
	)
	schedDone := make(chan struct{})
	go func() {
		sched.Run(ctx)
		close(schedDone)
	}()
	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-serveErr:
		stop()
	}
	// Refuse new manual work immediately instead of leaving the client API
	// open while scheduler-triggered runs drain.
	if err := sockSrv.StopAccepting(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Warn("stopping client API listener", "error", err)
	}
	<-schedDone

	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.RunTimeout.Duration+5*time.Second)
	defer cancel()
	healthErr := server.Shutdown(shutdownCtx)
	socketErr := sockSrv.Shutdown(shutdownCtx)
	if runErr != nil {
		return runErr
	}
	if healthErr != nil {
		return fmt.Errorf("shutting down healthz server: %w", healthErr)
	}
	if socketErr != nil {
		return fmt.Errorf("shutting down client API server: %w", socketErr)
	}
	log.Info("plico stopped")
	return nil
}

// runtimeChecks are the startup validations beyond config.Load. `plico
// validate` (F29) runs the host-independent set (hostChecks=false): checks
// that depend on the machine the daemon runs on — tmpfs availability, log
// file writability — can only be honest at `serve` time on the target host.
func runtimeChecks(cfg *config.Config, hostChecks bool) (slog.Level, error) {
	var level slog.Level
	if err := level.UnmarshalText([]byte(cfg.Log.Level)); err != nil {
		return level, fmt.Errorf("log.level: %w", err)
	}
	if hostChecks {
		// tmpfs mode is validated once at startup, not at deploy time.
		for _, st := range cfg.Stacks {
			if st.SopsMode == "tmpfs" && len(st.SopsFiles) > 0 {
				if err := sopsx.CheckTmpfs(sopsx.DefaultTmpfsRoot); err != nil {
					return level, fmt.Errorf("stack %q: %w", st.Name, err)
				}
			}
		}
	}
	return level, nil
}

func newLogger(lc config.LogConfig, level slog.Level) (*slog.Logger, func(), error) {
	var w io.Writer = os.Stderr
	closeFn := func() {}
	if lc.Path != "" {
		f, err := os.OpenFile(lc.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, nil, fmt.Errorf("opening log file: %w", err)
		}
		w = f
		closeFn = func() { _ = f.Close() }
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})), closeFn, nil
}
