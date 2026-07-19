package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/deploy"
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
	sched, err := scheduler.New(cfg, nopRunner{}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
