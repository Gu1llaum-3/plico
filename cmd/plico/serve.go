package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
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

	log, closeLog, err := newLogger(cfg.Log)
	if err != nil {
		return err
	}
	defer closeLog()

	// tmpfs mode is validated once at startup, not at deploy time.
	for _, st := range cfg.Stacks {
		if st.SopsMode == "tmpfs" && len(st.SopsFiles) > 0 {
			if err := sopsx.CheckTmpfs(sopsx.DefaultTmpfsRoot); err != nil {
				return fmt.Errorf("stack %q: %w", st.Name, err)
			}
		}
	}

	if err := os.MkdirAll(cfg.BaseDir, 0o755); err != nil {
		return err
	}
	store, err := state.Open(filepath.Join(cfg.BaseDir, "state.json"))
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

	sockSrv := api.NewSocket(cfg, sched, store, deployer, log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		log.Info("healthz listening", "addr", cfg.Health.Listen)
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("healthz server failed", "error", err)
		}
	}()
	go func() {
		if err := sockSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("client API server failed", "error", err)
		}
	}()

	log.Info("plico starting",
		"version", version,
		"stacks", len(cfg.Stacks),
		"poll_interval", cfg.PollInterval.String(),
		"base_dir", cfg.BaseDir,
	)
	sched.Run(ctx) // blocks until SIGINT/SIGTERM, drains in-flight runs

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	_ = sockSrv.Shutdown(shutdownCtx)
	log.Info("plico stopped")
	return nil
}

func newLogger(lc config.LogConfig) (*slog.Logger, func(), error) {
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
	var level slog.Level
	if err := level.UnmarshalText([]byte(lc.Level)); err != nil {
		return nil, nil, fmt.Errorf("log.level: %w", err)
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})), closeFn, nil
}
