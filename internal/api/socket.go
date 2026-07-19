package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/deploy"
	"github.com/Gu1llaum-3/plico/internal/scheduler"
	"github.com/Gu1llaum-3/plico/internal/state"
)

// Trigger is what the client API needs from the deployer. Every action goes
// through the deployer's per-stack lock, so manual runs serialize with the
// scheduler instead of racing it (F24).
type Trigger interface {
	RunStackWith(ctx context.Context, st config.StackConfig, opts deploy.RunOptions) deploy.Outcome
	CheckStack(ctx context.Context, st config.StackConfig) deploy.Outcome
	DryRun(ctx context.Context, st config.StackConfig) (deploy.DryRunReport, error)
}

// SocketServer serves the client CLI over a unix socket (F24–F28, F30).
type SocketServer struct {
	cfg     *config.Config
	sched   *scheduler.Scheduler
	store   *state.Store
	trigger Trigger
	log     *slog.Logger
	server  *http.Server
	ln      net.Listener
	actions sync.WaitGroup // in-flight manual runs, drained at shutdown
}

func NewSocket(cfg *config.Config, sched *scheduler.Scheduler, store *state.Store,
	trigger Trigger, log *slog.Logger) *SocketServer {

	s := &SocketServer{cfg: cfg, sched: sched, store: store, trigger: trigger, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("POST /v1/check", s.handleCheck)
	mux.HandleFunc("POST /v1/deploy", s.handleDeploy)
	mux.HandleFunc("POST /v1/dry-run", s.handleDryRun)
	s.server = &http.Server{
		Handler: mux,
		// No Read/WriteTimeout: a synchronous deploy-now legitimately takes
		// minutes (the run itself is bounded by run_timeout). Header reads
		// are bounded so an idle connection cannot hold Shutdown hostage.
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Listen binds the unix socket. Call it synchronously at startup, before
// other goroutines create files: the 0660 permissions are applied through
// the umask AT bind time — a chmod-after-listen would leave a window where
// the socket is world-connectable (unix sockets check permissions at
// connect time, so an early connection would survive the chmod).
func (s *SocketServer) Listen() error {
	path := s.cfg.Api.Socket

	// A leftover socket from a crash must be replaced, but never steal the
	// socket of a live daemon (accidental double start): probe it first.
	if _, err := os.Stat(path); err == nil {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			_ = conn.Close()
			return fmt.Errorf("another plico daemon is already listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("removing stale socket: %w", err)
		}
		s.log.Info("removed stale socket", "socket", path)
	}

	old := syscall.Umask(0o117) // 0777 &^ 0117 = 0660
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	s.ln = ln
	s.log.Info("client API listening", "socket", path)
	return nil
}

// Serve blocks until Shutdown. Listen must have been called first.
func (s *SocketServer) Serve() error {
	return s.server.Serve(s.ln)
}

// Shutdown stops accepting requests, then waits for in-flight manual runs
// to finish — a socket-triggered deploy gets the same drain guarantee as a
// scheduled one (the runs themselves are bounded by run_timeout).
func (s *SocketServer) Shutdown(ctx context.Context) error {
	err := s.server.Shutdown(ctx)
	s.actions.Wait()
	_ = os.Remove(s.cfg.Api.Socket)
	return err
}

// Handler exposes the mux for tests.
func (s *SocketServer) Handler() http.Handler { return s.server.Handler }

// ── requests / responses ────────────────────────────────────────────────

// ActionRequest is the body of /v1/check, /v1/deploy and /v1/dry-run.
type ActionRequest struct {
	Stack    string `json:"stack"` // "" or "*" = all stacks (not for dry-run)
	Force    bool   `json:"force,omitempty"`
	SkipPre  bool   `json:"skip_pre,omitempty"`
	SkipPost bool   `json:"skip_post,omitempty"`
}

// ActionResult is one stack's result for check/deploy actions.
type ActionResult struct {
	Stack   string `json:"stack"`
	Outcome string `json:"outcome"`
}

func (s *SocketServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, statusFromConfig(s.sched, s.store, s.cfg))
}

func (s *SocketServer) handleCheck(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, "check", func(ctx context.Context, st config.StackConfig, _ ActionRequest) string {
		return s.trigger.CheckStack(ctx, st).String()
	})
}

func (s *SocketServer) handleDeploy(w http.ResponseWriter, r *http.Request) {
	s.handleAction(w, r, "deploy", func(ctx context.Context, st config.StackConfig, req ActionRequest) string {
		return s.trigger.RunStackWith(ctx, st, deploy.RunOptions{
			Force:    req.Force,
			SkipPre:  req.SkipPre,
			SkipPost: req.SkipPost,
		}).String()
	})
}

func (s *SocketServer) handleDryRun(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodeAction(w, r)
	if !ok {
		return
	}
	stacks, err := s.selectStacks(req.Stack)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	if len(stacks) != 1 {
		httpError(w, http.StatusBadRequest, "dry-run requires exactly one --stack")
		return
	}
	report, err := s.trigger.DryRun(r.Context(), stacks[0])
	if err != nil {
		httpError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, report)
}

func (s *SocketServer) handleAction(w http.ResponseWriter, r *http.Request, name string,
	run func(context.Context, config.StackConfig, ActionRequest) string) {

	req, ok := s.decodeAction(w, r)
	if !ok {
		return
	}
	// F30: skipping the backup gate demands an explicit acknowledgement,
	// enforced server-side so no client can bypass it.
	if req.SkipPre && !req.Force {
		httpError(w, http.StatusForbidden, "--skip-pre bypasses the backup gate and requires --force")
		return
	}
	stacks, err := s.selectStacks(req.Stack)
	if err != nil {
		httpError(w, http.StatusNotFound, err.Error())
		return
	}
	s.log.Info("manual action requested via client API", "action", name,
		"stack", req.Stack, "force", req.Force, "skip_pre", req.SkipPre, "skip_post", req.SkipPost)

	// Detached from the request context: an operator's Ctrl-C (client
	// disconnect) must not SIGKILL a docker compose up mid-flight. Tracked
	// in the actions WaitGroup so shutdown drains these runs like the
	// scheduler drains its own.
	s.actions.Add(1)
	defer s.actions.Done()
	ctx := context.WithoutCancel(r.Context())

	results := make([]ActionResult, 0, len(stacks))
	for _, st := range stacks {
		results = append(results, ActionResult{
			Stack:   st.Name,
			Outcome: run(ctx, st, req),
		})
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *SocketServer) decodeAction(w http.ResponseWriter, r *http.Request) (ActionRequest, bool) {
	var req ActionRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return req, false
	}
	return req, true
}

func (s *SocketServer) selectStacks(name string) ([]config.StackConfig, error) {
	if name == "" || name == "*" {
		return s.cfg.Stacks, nil
	}
	for _, st := range s.cfg.Stacks {
		if st.Name == name {
			return []config.StackConfig{st}, nil
		}
	}
	return nil, fmt.Errorf("unknown stack %q", name)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}
