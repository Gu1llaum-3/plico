package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
	lock    *os.File // process-lifetime lock preventing concurrent daemons
	sockID  fileID   // identity of the socket file WE bound (see Shutdown)
	stop    sync.Once
}

// fileID identifies a file on disk (device + inode).
type fileID struct {
	dev uint64
	ino uint64
}

func statID(path string) (fileID, bool) {
	info, err := os.Stat(path)
	if err != nil {
		return fileID{}, false
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{dev: uint64(st.Dev), ino: st.Ino}, true //nolint:unconvert // Dev is int32 on darwin, uint64 on linux
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
		// Responses may legitimately take minutes, but request bodies are tiny.
		// Bound reads so a partial request cannot hold shutdown hostage.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
	}
	return s
}

// Listen binds the unix socket with 0660 permissions, with no window where
// it is world-connectable (see the staging-dir + rename below).
func (s *SocketServer) Listen() error {
	path := s.cfg.Api.Socket
	lock, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening socket lock: %w", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = lock.Close()
		return fmt.Errorf("another plico daemon is starting or running for %s", path)
	}
	s.lock = lock
	success := false
	defer func() {
		if !success {
			s.releaseLock()
		}
	}()

	// A leftover socket from a crash must be replaced, but never steal the
	// socket of a live daemon (accidental double start): probe it first.
	// Lstat, not Stat: a dangling symlink is a removable leftover too.
	if _, err := os.Lstat(path); err == nil {
		conn, dialErr := net.DialTimeout("unix", path, time.Second)
		switch {
		case dialErr == nil:
			_ = conn.Close()
			return fmt.Errorf("another plico daemon is already listening on %s", path)
		case errors.Is(dialErr, syscall.ECONNREFUSED) || errors.Is(dialErr, fs.ErrNotExist):
			// Nobody accepts (crash leftover) or the target is gone
			// (dangling symlink): safe to replace.
			if err := os.Remove(path); err != nil {
				return fmt.Errorf("removing stale socket: %w", err)
			}
			s.log.Info("removed stale socket", "socket", path)
		default:
			// Timeout or anything ambiguous: a wedged-but-alive daemon may
			// own it. Never steal a socket we cannot prove dead.
			return fmt.Errorf("socket %s exists and its owner may still be alive (probe: %v); refusing to replace it", path, dialErr)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("inspecting socket: %w", err)
	}

	// Bind in a private 0700 staging dir, chmod, then atomically rename to
	// the final path: at no point is a world-connectable socket reachable
	// (unix sockets check permissions at connect time), and no process-wide
	// umask fiddling is needed.
	tmpDir, err := os.MkdirTemp(filepath.Dir(path), ".plico-socket-")
	if err != nil {
		return err
	}
	if err := os.Chmod(tmpDir, 0o700); err != nil {
		_ = os.RemoveAll(tmpDir)
		return err
	}
	defer func() { _ = os.RemoveAll(tmpDir) }()
	tmp := filepath.Join(tmpDir, "s")
	ln, err := net.Listen("unix", tmp)
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp, 0o660); err != nil {
		_ = ln.Close()
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = ln.Close()
		return err
	}
	s.ln = ln
	// The unix listener would unlink the path on Close even if a successor
	// daemon already re-bound it; Shutdown does an identity-checked removal
	// instead.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	s.sockID, _ = statID(path)
	s.log.Info("client API listening", "socket", path)
	success = true
	return nil
}

// Serve blocks until Shutdown. Listen must have been called first.
func (s *SocketServer) Serve() error {
	return s.server.Serve(s.ln)
}

// StopAccepting closes the listener and removes its path while preserving
// active requests. The process lock remains held until Shutdown completes.
func (s *SocketServer) StopAccepting() error {
	var err error
	s.stop.Do(func() {
		if s.ln != nil {
			err = s.ln.Close()
		}
		if id, ok := statID(s.cfg.Api.Socket); ok && id == s.sockID {
			if removeErr := os.Remove(s.cfg.Api.Socket); err == nil {
				err = removeErr
			}
		}
	})
	return err
}

// Shutdown stops accepting requests and waits for active handlers. The
// process lock is released only after a successful drain.
func (s *SocketServer) Shutdown(ctx context.Context) error {
	stopErr := s.StopAccepting()
	shutdownErr := s.server.Shutdown(ctx)
	if shutdownErr != nil {
		return shutdownErr
	}
	s.releaseLock()
	return stopErr
}

// Close releases resources if startup or serving exits before Shutdown.
func (s *SocketServer) Close() {
	_ = s.StopAccepting()
	s.releaseLock()
}

func (s *SocketServer) releaseLock() {
	if s.lock != nil {
		_ = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_UN)
		_ = s.lock.Close()
		s.lock = nil
	}
}

// Handler exposes the mux for tests.
func (s *SocketServer) Handler() http.Handler { return s.server.Handler }

// ── requests / responses ────────────────────────────────────────────────

// ActionRequest is the body of /v1/check, /v1/deploy and /v1/dry-run.
type ActionRequest struct {
	Stack    string `json:"stack"` // "*" explicitly targets all stacks (not for dry-run)
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
	if req.Force || req.SkipPre || req.SkipPost {
		httpError(w, http.StatusBadRequest, "dry-run does not accept deployment options")
		return
	}
	if req.Stack == "" {
		httpError(w, http.StatusBadRequest, "stack is required")
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
	if name == "check" && (req.Force || req.SkipPre || req.SkipPost) {
		httpError(w, http.StatusBadRequest, "check does not accept deployment options")
		return
	}
	if req.Stack == "" {
		httpError(w, http.StatusBadRequest, "stack is required; use \"*\" to target every stack")
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
	// disconnect) must not SIGKILL a docker compose up mid-flight.
	ctx := context.WithoutCancel(r.Context())

	results := make([]ActionResult, len(stacks))
	var wg sync.WaitGroup
	for i, st := range stacks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = ActionResult{Stack: st.Name, Outcome: run(ctx, st, req)}
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, results)
}

func (s *SocketServer) decodeAction(w http.ResponseWriter, r *http.Request) (ActionRequest, bool) {
	var req ActionRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return req, false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		httpError(w, http.StatusBadRequest, "request body must contain exactly one JSON object")
		return req, false
	}
	return req, true
}

func (s *SocketServer) selectStacks(name string) ([]config.StackConfig, error) {
	if name == "" {
		return nil, fmt.Errorf("stack is required; use \"*\" to target every stack")
	}
	if name == "*" {
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
