package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/deploy"
	"github.com/Gu1llaum-3/plico/internal/scheduler"
	"github.com/Gu1llaum-3/plico/internal/state"
)

type nopRunner struct{}

func (nopRunner) RunStack(context.Context, config.StackConfig) deploy.Outcome {
	return deploy.OutcomeUpToDate
}

func setup(t *testing.T) (*scheduler.Scheduler, *state.Store, *http.Server) {
	t.Helper()
	cfg := &config.Config{
		PollInterval: config.Duration{Duration: time.Hour},
		Stacks:       []config.StackConfig{{Name: "web"}},
	}
	store, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	sched, err := scheduler.New(cfg, nopRunner{}, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv := New("127.0.0.1:0", sched, store, time.Minute, time.Minute)
	return sched, store, srv
}

func get(t *testing.T, srv *http.Server) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON body: %v\n%s", err, rec.Body.String())
	}
	return rec, body
}

func TestHealthzDegradedBeforeFirstTick(t *testing.T) {
	t.Parallel()
	_, _, srv := setup(t)
	rec, body := get(t, srv)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 before first tick", rec.Code)
	}
	if body["status"] != "degraded" {
		t.Errorf("body status = %v", body["status"])
	}
}

func TestHealthzOKAfterTick(t *testing.T) {
	t.Parallel()
	sched, store, srv := setup(t)

	// Run one scheduler pass (immediate first tick), then stop.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sched.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for sched.Snapshot().LastTick.IsZero() {
		select {
		case <-deadline:
			t.Fatal("scheduler never ticked")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	if err := store.Put("web", state.StackState{
		LastDeployedSHA: "abc", LastStatus: state.StatusSuccess, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	rec, body := get(t, srv)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	stacks := body["stacks"].(map[string]any)
	web := stacks["web"].(map[string]any)
	if web["last_deployed_sha"] != "abc" {
		t.Errorf("web = %v", web)
	}
}
