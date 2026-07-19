package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/deploy"
	"github.com/Gu1llaum-3/plico/internal/notify"
	"github.com/Gu1llaum-3/plico/internal/scheduler"
	"github.com/Gu1llaum-3/plico/internal/state"
)

// fakeTrigger records every action requested through the socket API.
type fakeTrigger struct {
	mu      sync.Mutex
	deploys []deploy.RunOptions
	ctxErrs []error
	checks  int
}

func (f *fakeTrigger) RunStackWith(ctx context.Context, _ config.StackConfig, opts deploy.RunOptions) deploy.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deploys = append(f.deploys, opts)
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	return deploy.OutcomeDeployed
}

func (f *fakeTrigger) CheckStack(context.Context, config.StackConfig) deploy.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return deploy.OutcomeQueued
}

func (f *fakeTrigger) DryRun(_ context.Context, st config.StackConfig) (deploy.DryRunReport, error) {
	return deploy.DryRunReport{
		Stack: st.Name, Ref: st.Ref, OldSHA: "old", NewSHA: "new",
		Commits: []string{"abc123 fix things"},
	}, nil
}

func socketSetup(t *testing.T) (*SocketServer, *fakeTrigger) {
	t.Helper()
	cfg := &config.Config{
		PollInterval: config.Duration{Duration: time.Minute},
		RunTimeout:   config.Duration{Duration: time.Minute},
		Stacks:       []config.StackConfig{{Name: "web"}, {Name: "db"}},
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	trigger := &fakeTrigger{}
	sched, err := scheduler.New(cfg, nopRunner{}, store, notify.Nop{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return NewSocket(cfg, sched, store, trigger, slog.New(slog.NewTextHandler(io.Discard, nil))), trigger
}

func do(t *testing.T, s *SocketServer, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestSkipPreRequiresForce(t *testing.T) {
	t.Parallel()
	s, trigger := socketSetup(t)
	rec := do(t, s, http.MethodPost, "/v1/deploy", `{"stack":"web","skip_pre":true}`)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (F30: --skip-pre without --force)", rec.Code)
	}
	if len(trigger.deploys) != 0 {
		t.Fatal("the deploy must not have been triggered")
	}
	// With force: accepted, options propagated.
	rec = do(t, s, http.MethodPost, "/v1/deploy", `{"stack":"web","skip_pre":true,"force":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(trigger.deploys) != 1 || !trigger.deploys[0].SkipPre || !trigger.deploys[0].Force {
		t.Errorf("options not propagated: %+v", trigger.deploys)
	}
}

func TestDeployAllFansOut(t *testing.T) {
	t.Parallel()
	s, trigger := socketSetup(t)
	rec := do(t, s, http.MethodPost, "/v1/deploy", `{"stack":"*"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(trigger.deploys) != 2 {
		t.Errorf("deploys = %d, want 2 (all stacks)", len(trigger.deploys))
	}
	if !strings.Contains(rec.Body.String(), `"web"`) || !strings.Contains(rec.Body.String(), `"db"`) {
		t.Errorf("results missing stacks: %s", rec.Body.String())
	}
}

func TestUnknownStackIs404(t *testing.T) {
	t.Parallel()
	s, _ := socketSetup(t)
	rec := do(t, s, http.MethodPost, "/v1/check", `{"stack":"nope"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestActionRequestRequiresExplicitKnownFields(t *testing.T) {
	t.Parallel()
	s, trigger := socketSetup(t)
	for _, body := range []string{`{}`, `{"stak":"web"}`, `{"stack":"web"} {"stack":"db"}`} {
		rec := do(t, s, http.MethodPost, "/v1/deploy", body)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("body %s: status = %d, want 400", body, rec.Code)
		}
	}
	if len(trigger.deploys) != 0 {
		t.Fatalf("invalid requests triggered %d deployments", len(trigger.deploys))
	}
}

func TestCheckAndDryRun(t *testing.T) {
	t.Parallel()
	s, trigger := socketSetup(t)
	if rec := do(t, s, http.MethodPost, "/v1/check", `{"stack":"web"}`); rec.Code != http.StatusOK {
		t.Fatalf("check status = %d", rec.Code)
	}
	if trigger.checks != 1 {
		t.Errorf("checks = %d", trigger.checks)
	}
	rec := do(t, s, http.MethodPost, "/v1/dry-run", `{"stack":"web"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("dry-run status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "abc123 fix things") {
		t.Errorf("dry-run body missing commits: %s", rec.Body.String())
	}
	// dry-run refuses fan-out.
	if rec := do(t, s, http.MethodPost, "/v1/dry-run", `{"stack":"*"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("dry-run all: status = %d, want 400", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, "/v1/check", `{"stack":"web","force":true}`); rec.Code != http.StatusBadRequest {
		t.Errorf("check with deploy option: status = %d, want 400", rec.Code)
	}
	if rec := do(t, s, http.MethodPost, "/v1/dry-run", `{"stack":"web","skip_post":true}`); rec.Code != http.StatusBadRequest {
		t.Errorf("dry-run with deploy option: status = %d, want 400", rec.Code)
	}
}

func TestDryRunUnknownStackIs404(t *testing.T) {
	t.Parallel()
	s, _ := socketSetup(t)
	rec := do(t, s, http.MethodPost, "/v1/dry-run", `{"stack":"wbe"}`)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (unknown stack, not a flags error)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown stack") {
		t.Errorf("body = %s", rec.Body.String())
	}
}

func TestDeployDetachedFromClientContext(t *testing.T) {
	t.Parallel()
	s, trigger := socketSetup(t)
	// The client disconnects (Ctrl-C): the request context is cancelled,
	// but the deploy must run on a live context anyway.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/deploy",
		strings.NewReader(`{"stack":"web"}`)).WithContext(ctx)
	s.Handler().ServeHTTP(rec, req)

	trigger.mu.Lock()
	defer trigger.mu.Unlock()
	if len(trigger.ctxErrs) != 1 {
		t.Fatalf("deploys = %d, want 1", len(trigger.ctxErrs))
	}
	if trigger.ctxErrs[0] != nil {
		t.Errorf("deploy ran on a cancelled context (%v): a client Ctrl-C would kill compose mid-flight", trigger.ctxErrs[0])
	}
}

func shortSocketDir(t *testing.T) string {
	t.Helper()
	// unix socket paths are length-limited (~104 bytes): avoid t.TempDir's
	// deep hierarchy.
	dir, err := os.MkdirTemp("", "plico")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func socketServerAt(t *testing.T, path string) *SocketServer {
	t.Helper()
	cfg := &config.Config{
		PollInterval: config.Duration{Duration: time.Minute},
		RunTimeout:   config.Duration{Duration: time.Minute},
		Api:          config.ApiConfig{Socket: path},
		Stacks:       []config.StackConfig{{Name: "web"}},
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.New(cfg, nopRunner{}, store, notify.Nop{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return NewSocket(cfg, sched, store, &fakeTrigger{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestListenRefusesLiveDaemonSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortSocketDir(t), "plico.sock")
	s1 := socketServerAt(t, path)
	if err := s1.Listen(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s1.Shutdown(context.Background()) }()

	// An accidental second daemon must refuse to start, not steal the socket.
	s2 := socketServerAt(t, path)
	if err := s2.Listen(); err == nil {
		t.Fatal("second Listen on a live socket must fail")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("live socket was removed by the refused second daemon: %v", err)
	}
}

func TestConcurrentListenHasSingleWinner(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortSocketDir(t), "plico.sock")
	servers := []*SocketServer{socketServerAt(t, path), socketServerAt(t, path)}
	errs := make(chan error, len(servers))
	for _, s := range servers {
		go func() { errs <- s.Listen() }()
	}
	winners := 0
	for range servers {
		if err := <-errs; err == nil {
			winners++
		}
	}
	for _, s := range servers {
		s.Close()
	}
	if winners != 1 {
		t.Fatalf("successful concurrent listeners = %d, want 1", winners)
	}
}

func TestShutdownDeadlineBoundsPartialRequest(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortSocketDir(t), "plico.sock")
	s := socketServerAt(t, path)
	defer s.Close()
	if err := s.Listen(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve() }()

	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "POST /v1/deploy HTTP/1.1\r\nHost: plico\r\nContent-Length: 100\r\n\r\n{")
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := s.Shutdown(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("Shutdown ignored its deadline and took %s", elapsed)
	}
}

func TestListenReplacesStaleSocket(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortSocketDir(t), "plico.sock")
	// Simulate a crash leftover: a bound-then-dead socket file.
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	_ = ln.Close()

	s := socketServerAt(t, path)
	if err := s.Listen(); err != nil {
		t.Fatalf("stale socket must be replaced: %v", err)
	}
	_ = s.Shutdown(context.Background())
}

func TestShutdownKeepsStartLockUntilDrainCompletes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(shortSocketDir(t), "plico.sock")
	s1 := socketServerAt(t, path)
	if err := s1.Listen(); err != nil {
		t.Fatal(err)
	}
	// Removing the socket path must not let a successor bypass the
	// process-lifetime lock while the first daemon is still draining.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s2 := socketServerAt(t, path)
	if err := s2.Listen(); err == nil {
		t.Fatal("successor started before the first daemon completed shutdown")
	}

	if err := s1.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s2.Listen(); err != nil {
		t.Fatalf("successor could not start after shutdown: %v", err)
	}
	defer func() { _ = s2.Shutdown(context.Background()) }()
}

func TestSocketStatusIncludesStacks(t *testing.T) {
	t.Parallel()
	s, _ := socketSetup(t)
	rec := do(t, s, http.MethodGet, "/v1/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	for _, want := range []string{`"web"`, `"db"`, `"status"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("status body missing %s: %s", want, rec.Body.String())
		}
	}
}
