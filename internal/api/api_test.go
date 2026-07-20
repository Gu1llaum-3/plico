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
	"github.com/Gu1llaum-3/plico/internal/notify"
	"github.com/Gu1llaum-3/plico/internal/scheduler"
	"github.com/Gu1llaum-3/plico/internal/state"
)

type nopRunner struct{}

func (nopRunner) RunStack(context.Context, config.StackConfig) deploy.Outcome {
	return deploy.OutcomeUpToDate
}

func (nopRunner) CheckStack(context.Context, config.StackConfig) deploy.Outcome {
	return deploy.OutcomeUpToDate
}

func (nopRunner) CheckHealth(context.Context, config.StackConfig) deploy.Outcome {
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
	sched, err := scheduler.New(cfg, nopRunner{}, store, notify.Nop{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
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

func TestHealthy(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	poll := time.Minute
	runTimeout := 30 * time.Minute

	tick := func(ago time.Duration) time.Time { return now.Add(-ago) }
	running := func(ago time.Duration) *time.Time { t := now.Add(-ago); return &t }

	tests := []struct {
		name string
		snap scheduler.Snapshot
		want bool
	}{
		{"never ticked", scheduler.Snapshot{}, false},
		{"recent tick, idle", scheduler.Snapshot{LastTick: tick(10 * time.Second)}, true},
		{"tick older than 2x poll", scheduler.Snapshot{LastTick: tick(3 * time.Minute)}, false},
		{
			"recent tick, run within run_timeout",
			scheduler.Snapshot{LastTick: tick(5 * time.Second), Stacks: map[string]scheduler.StackStatus{
				"web": {RunningSince: running(10 * time.Minute)},
			}},
			true,
		},
		{
			"recent tick, run stuck past run_timeout",
			scheduler.Snapshot{LastTick: tick(5 * time.Second), Stacks: map[string]scheduler.StackStatus{
				"web": {RunningSince: running(31 * time.Minute)},
			}},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Healthy(tt.snap, poll, runTimeout, now); got != tt.want {
				t.Errorf("Healthy = %v, want %v", got, tt.want)
			}
		})
	}
}
