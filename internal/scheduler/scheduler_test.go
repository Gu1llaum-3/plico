package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/deploy"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type countingRunner struct {
	mu      sync.Mutex
	calls   map[string]int
	outcome deploy.Outcome
	block   chan struct{} // when set, RunStack waits on it
}

func (c *countingRunner) RunStack(_ context.Context, st config.StackConfig) deploy.Outcome {
	c.mu.Lock()
	if c.calls == nil {
		c.calls = map[string]int{}
	}
	c.calls[st.Name]++
	c.mu.Unlock()
	if c.block != nil {
		<-c.block
	}
	return c.outcome
}

func testConfig() *config.Config {
	return &config.Config{
		PollInterval: config.Duration{Duration: time.Hour}, // only the immediate first tick fires
		Stacks: []config.StackConfig{
			{Name: "web"},
			{Name: "db"},
		},
	}
}

func TestFirstTickRunsAllStacks(t *testing.T) {
	t.Parallel()
	runner := &countingRunner{outcome: deploy.OutcomeDeployed}
	s := New(testConfig(), runner, discard())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	deadline := time.After(5 * time.Second)
	for {
		runner.mu.Lock()
		n := runner.calls["web"] + runner.calls["db"]
		runner.mu.Unlock()
		if n == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("stacks not run on first tick")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done

	snap := s.Snapshot()
	if snap.LastTick.IsZero() {
		t.Error("lastTick not recorded")
	}
	if snap.Stacks["web"].LastOutcome != "deployed" {
		t.Errorf("web outcome = %q", snap.Stacks["web"].LastOutcome)
	}
}

func TestSnapshotShowsRunningStack(t *testing.T) {
	t.Parallel()
	runner := &countingRunner{outcome: deploy.OutcomeDeployed, block: make(chan struct{})}
	s := New(testConfig(), runner, discard())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	deadline := time.After(5 * time.Second)
	for {
		snap := s.Snapshot()
		if snap.Stacks["web"].RunningSince != nil && snap.Stacks["db"].RunningSince != nil {
			break
		}
		select {
		case <-deadline:
			t.Fatal("running stacks never visible in snapshot")
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(runner.block)
	cancel()
	<-done

	snap := s.Snapshot()
	if snap.Stacks["web"].RunningSince != nil {
		t.Error("run should be cleared after completion")
	}
}

// ctxCheckRunner blocks until released, then records whether its context
// survived a scheduler shutdown (a graceful drain must not cancel it).
type ctxCheckRunner struct {
	started  chan struct{}
	release  chan struct{}
	mu       sync.Mutex
	ctxErrs  []error
	startOne sync.Once
}

func (c *ctxCheckRunner) RunStack(ctx context.Context, _ config.StackConfig) deploy.Outcome {
	c.startOne.Do(func() { close(c.started) })
	<-c.release
	c.mu.Lock()
	c.ctxErrs = append(c.ctxErrs, ctx.Err())
	c.mu.Unlock()
	return deploy.OutcomeDeployed
}

func TestShutdownDrainsWithoutCancellingInFlightRuns(t *testing.T) {
	t.Parallel()
	runner := &ctxCheckRunner{started: make(chan struct{}), release: make(chan struct{})}
	cfg := &config.Config{
		PollInterval: config.Duration{Duration: time.Hour},
		Stacks:       []config.StackConfig{{Name: "web"}},
	}
	s := New(cfg, runner, discard())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	<-runner.started // a run is in flight
	cancel()         // SIGTERM arrives

	select {
	case <-done:
		t.Fatal("Run returned before the in-flight run finished: no drain")
	case <-time.After(200 * time.Millisecond):
	}

	close(runner.release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after drain")
	}

	runner.mu.Lock()
	defer runner.mu.Unlock()
	for _, err := range runner.ctxErrs {
		if err != nil {
			t.Errorf("in-flight run saw a cancelled context (%v): docker compose up would be SIGKILLed mid-deploy", err)
		}
	}
}

func mustSchedule(t *testing.T, expr string) cron.Schedule {
	t.Helper()
	s, err := cron.ParseStandard(expr)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dueNames(s *Scheduler, now time.Time) []string {
	var names []string
	for _, st := range s.due(now) {
		names = append(names, st.Name)
	}
	return names
}

func TestDueRespectsScheduleWindow(t *testing.T) {
	t.Parallel()
	paris, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		PollInterval: config.Duration{Duration: time.Minute},
		Stacks: []config.StackConfig{
			{Name: "nightly", Schedule: "0 4 * * *", Window: config.Duration{Duration: time.Hour}},
			{Name: "always"}, // no schedule: due every tick
		},
	}
	s := New(cfg, &countingRunner{}, discard())
	fire := time.Date(2026, 7, 19, 4, 0, 0, 0, paris)
	s.mu.Lock()
	s.scheds["nightly"] = &stackSched{
		sched: mustSchedule(t, "0 4 * * *"), window: time.Hour, next: fire,
	}
	s.mu.Unlock()

	// Before the firing: only the unscheduled stack is due.
	if got := dueNames(s, fire.Add(-time.Minute)); len(got) != 1 || got[0] != "always" {
		t.Errorf("before window: due = %v, want [always]", got)
	}
	// Tick that opens the window (30s late, as a real poll tick would be).
	if got := dueNames(s, fire.Add(30*time.Second)); len(got) != 2 {
		t.Errorf("window opening: due = %v, want both stacks", got)
	}
	// Still inside the window.
	if got := dueNames(s, fire.Add(30*time.Minute)); len(got) != 2 {
		t.Errorf("inside window: due = %v, want both stacks", got)
	}
	// Window closed, next firing is tomorrow.
	if got := dueNames(s, fire.Add(61*time.Minute)); len(got) != 1 || got[0] != "always" {
		t.Errorf("after window: due = %v, want [always]", got)
	}
	// Snapshot exposes the recomputed next run (tomorrow 04:00).
	next := s.Snapshot().Stacks["nightly"].NextRun
	want := time.Date(2026, 7, 20, 4, 0, 0, 0, paris)
	if next == nil || !next.Equal(want) {
		t.Errorf("next_run = %v, want %v", next, want)
	}
}

func TestDueOpeningTickCountsEvenWithTinyWindow(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		PollInterval: config.Duration{Duration: time.Minute},
		Stacks:       []config.StackConfig{{Name: "web", Schedule: "0 4 * * *"}},
	}
	s := New(cfg, &countingRunner{}, discard())
	fire := time.Date(2026, 7, 19, 4, 0, 0, 0, time.UTC)
	s.mu.Lock()
	s.scheds["web"] = &stackSched{
		sched: mustSchedule(t, "0 4 * * *"), window: time.Second, next: fire,
	}
	s.mu.Unlock()

	// The poll tick arrives 45s after the firing, past the 1s window: the
	// opening tick must still count — every firing yields at least one run.
	if got := dueNames(s, fire.Add(45*time.Second)); len(got) != 1 {
		t.Errorf("opening tick skipped: due = %v", got)
	}
	// But the following tick is out of the window.
	if got := dueNames(s, fire.Add(105*time.Second)); len(got) != 0 {
		t.Errorf("tiny window should be closed: due = %v", got)
	}
}

func TestSkippedOutcomeDoesNotOverwriteLast(t *testing.T) {
	t.Parallel()
	runner := &countingRunner{outcome: deploy.OutcomeDeployed}
	s := New(testConfig(), runner, discard())
	s.runOne(context.Background(), s.cfg.Stacks[0])
	runner.outcome = deploy.OutcomeSkipped
	s.runOne(context.Background(), s.cfg.Stacks[0])
	if got := s.Snapshot().Stacks["web"].LastOutcome; got != "deployed" {
		t.Errorf("outcome = %q, want deployed (skip must not overwrite)", got)
	}
}
