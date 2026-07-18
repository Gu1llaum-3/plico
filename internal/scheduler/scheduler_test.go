package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/deploy"
	"github.com/Gu1llaum-3/plico/internal/state"
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

func testStore(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.Open(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
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

func mustNewAt(t *testing.T, cfg *config.Config, r StackRunner, store *state.Store, now time.Time) *Scheduler {
	t.Helper()
	s, err := NewAt(cfg, r, store, discard(), now)
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

func paris(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func nightlyConfig(window time.Duration) *config.Config {
	return &config.Config{
		Timezone:     "Europe/Paris",
		PollInterval: config.Duration{Duration: time.Minute},
		Stacks: []config.StackConfig{
			{Name: "nightly", Schedule: "0 4 * * *", Window: config.Duration{Duration: window}},
			{Name: "always"}, // no schedule: due every tick
		},
	}
}

func TestFirstTickRunsAllStacks(t *testing.T) {
	t.Parallel()
	runner := &countingRunner{outcome: deploy.OutcomeDeployed}
	s := mustNewAt(t, testConfig(), runner, testStore(t), time.Now())

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
	s := mustNewAt(t, testConfig(), runner, testStore(t), time.Now())

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
	s := mustNewAt(t, cfg, runner, testStore(t), time.Now())

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

func TestDueRespectsScheduleWindow(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	boot := time.Date(2026, 7, 19, 3, 0, 0, 0, loc)
	runner := &countingRunner{outcome: deploy.OutcomeDeployed}
	s := mustNewAt(t, nightlyConfig(time.Hour), runner, store, boot)

	fire := time.Date(2026, 7, 19, 4, 0, 0, 0, loc)

	// Before the firing: only the unscheduled stack is due.
	if got := dueNames(s, fire.Add(-time.Minute)); len(got) != 1 || got[0] != "always" {
		t.Errorf("before window: due = %v, want [always]", got)
	}
	// Tick that opens the window (30s late, as a real poll tick would be).
	if got := dueNames(s, fire.Add(30*time.Second)); len(got) != 2 {
		t.Errorf("window opening: due = %v, want both stacks", got)
	}
	// A run happens: the firing is accounted and persisted.
	s.runOne(context.Background(), s.cfg.Stacks[0])
	if st, _ := store.Get("nightly"); !st.LastFiring.Equal(fire) {
		t.Errorf("anchor = %v, want %v", st.LastFiring, fire)
	}
	// Still inside the window: retries are possible.
	if got := dueNames(s, fire.Add(30*time.Minute)); len(got) != 2 {
		t.Errorf("inside window: due = %v, want both stacks", got)
	}
	// Window closed.
	if got := dueNames(s, fire.Add(61*time.Minute)); len(got) != 1 || got[0] != "always" {
		t.Errorf("after window: due = %v, want [always]", got)
	}
	// Snapshot exposes the next firing (tomorrow 04:00).
	next := s.Snapshot().Stacks["nightly"].NextRun
	want := time.Date(2026, 7, 20, 4, 0, 0, 0, loc)
	if next == nil || !next.Equal(want) {
		t.Errorf("next_run = %v, want %v", next, want)
	}
}

func TestMissedWindowIsNeverDeployedLate(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	boot := time.Date(2026, 7, 19, 3, 0, 0, 0, loc)
	s := mustNewAt(t, nightlyConfig(time.Hour), &countingRunner{}, store, boot)

	// Host paused from 03:30 to 09:00: the first tick after resume sees the
	// 04:00 firing but its window (04:00-05:00) is long gone.
	if got := dueNames(s, time.Date(2026, 7, 19, 9, 0, 0, 0, loc)); len(got) != 1 || got[0] != "always" {
		t.Errorf("missed window must not deploy late, due = %v", got)
	}
	// The missed firing is accounted (persisted anchor) so it is not
	// rediscovered after a restart.
	fire := time.Date(2026, 7, 19, 4, 0, 0, 0, loc)
	if st, _ := store.Get("nightly"); !st.LastFiring.Equal(fire) {
		t.Errorf("missed firing not persisted: anchor = %v, want %v", st.LastFiring, fire)
	}
}

func TestRestartInsideOpenWindowReopensIt(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	// The daemon deployed yesterday (anchor = yesterday 04:00) and restarts
	// today at 04:10, inside today's still-open window.
	yesterday := time.Date(2026, 7, 18, 4, 0, 0, 0, loc)
	if err := store.Update("nightly", func(st *state.StackState) { st.LastFiring = yesterday }); err != nil {
		t.Fatal(err)
	}
	restart := time.Date(2026, 7, 19, 4, 10, 0, 0, loc)
	s := mustNewAt(t, nightlyConfig(time.Hour), &countingRunner{}, store, restart)

	if got := dueNames(s, restart.Add(30*time.Second)); len(got) != 2 {
		t.Errorf("restart inside window must re-open it, due = %v", got)
	}
}

func TestRestartAfterAttemptDoesNotReplayWindow(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	// Today's firing was already attempted (anchor = today 04:00); the
	// daemon restarts at 04:10. The window must NOT re-open.
	today := time.Date(2026, 7, 19, 4, 0, 0, 0, loc)
	if err := store.Update("nightly", func(st *state.StackState) { st.LastFiring = today }); err != nil {
		t.Fatal(err)
	}
	restart := time.Date(2026, 7, 19, 4, 10, 0, 0, loc)
	s := mustNewAt(t, nightlyConfig(time.Hour), &countingRunner{}, store, restart)

	if got := dueNames(s, restart.Add(30*time.Second)); len(got) != 1 || got[0] != "always" {
		t.Errorf("already-attempted window must not replay, due = %v", got)
	}
	next := s.Snapshot().Stacks["nightly"].NextRun
	want := time.Date(2026, 7, 20, 4, 0, 0, 0, loc)
	if next == nil || !next.Equal(want) {
		t.Errorf("next_run = %v, want %v", next, want)
	}
}

func TestSkippedRunDoesNotConsumeWindow(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	boot := time.Date(2026, 7, 19, 3, 0, 0, 0, loc)
	runner := &countingRunner{outcome: deploy.OutcomeSkipped}
	s := mustNewAt(t, nightlyConfig(time.Hour), runner, store, boot)

	fire := time.Date(2026, 7, 19, 4, 0, 0, 0, loc)
	if got := dueNames(s, fire.Add(30*time.Second)); len(got) != 2 {
		t.Fatalf("window opening: due = %v", got)
	}
	// The run is skipped (previous deploy still in flight): the firing must
	// NOT be accounted, so the next tick inside the window retries.
	s.runOne(context.Background(), s.cfg.Stacks[0])
	if st, _ := store.Get("nightly"); !st.LastFiring.IsZero() {
		t.Errorf("skipped run must not persist the anchor, got %v", st.LastFiring)
	}
	if got := dueNames(s, fire.Add(10*time.Minute)); len(got) != 2 {
		t.Errorf("window must stay open after a skipped run, due = %v", got)
	}
	// Whole window elapses with only skips: accounted + warned, not replayed.
	if got := dueNames(s, fire.Add(2*time.Hour)); len(got) != 1 {
		t.Errorf("elapsed window must close, due = %v", got)
	}
	if st, _ := store.Get("nightly"); !st.LastFiring.Equal(fire) {
		t.Errorf("unattempted elapsed window must be accounted, anchor = %v", st.LastFiring)
	}
}

func TestCollapsedFiringsOpenLatestWindowOnly(t *testing.T) {
	t.Parallel()
	store := testStore(t)
	cfg := &config.Config{
		PollInterval: config.Duration{Duration: time.Minute},
		Stacks: []config.StackConfig{
			{Name: "fast", Schedule: "*/2 * * * *", Window: config.Duration{Duration: 2 * time.Minute}},
		},
	}
	boot := time.Date(2026, 7, 19, 12, 0, 30, 0, time.UTC)
	s := mustNewAt(t, cfg, &countingRunner{}, store, boot)

	// Ticks stall until 12:05: firings at 12:02 and 12:04 both passed; only
	// the latest (12:04, window until 12:06) opens.
	now := time.Date(2026, 7, 19, 12, 5, 0, 0, time.UTC)
	if got := dueNames(s, now); len(got) != 1 {
		t.Fatalf("latest collapsed firing should open, due = %v", got)
	}
	s.mu.Lock()
	firing := s.scheds["fast"].windowFiring
	s.mu.Unlock()
	want := time.Date(2026, 7, 19, 12, 4, 0, 0, time.UTC)
	if !firing.Equal(want) {
		t.Errorf("window firing = %v, want %v (the latest)", firing, want)
	}
}

func TestNewFailsClosedOnBadSchedule(t *testing.T) {
	t.Parallel()
	cfg := &config.Config{
		PollInterval: config.Duration{Duration: time.Minute},
		Stacks:       []config.StackConfig{{Name: "web", Schedule: "not a cron"}},
	}
	if _, err := NewAt(cfg, &countingRunner{}, testStore(t), discard(), time.Now()); err == nil {
		t.Fatal("an unparsable schedule must be a construction error, not a silent every-tick fallback")
	}
}

func TestLastTickKeepsMonotonicReading(t *testing.T) {
	t.Parallel()
	s := mustNewAt(t, testConfig(), &countingRunner{}, testStore(t), time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.After(5 * time.Second)
	for s.Snapshot().LastTick.IsZero() {
		select {
		case <-deadline:
			t.Fatal("no tick recorded")
		case <-time.After(10 * time.Millisecond):
		}
	}
	cancel()
	<-done
	// time.Time.Round(0) strips the monotonic reading: if LastTick still
	// equals itself after stripping under ==, it carried no monotonic part
	// and /healthz liveness would be at the mercy of wall-clock steps.
	// (== is intentional here: time.Equal ignores the monotonic part, which
	// is exactly what this test needs to observe.)
	last := s.Snapshot().LastTick
	if last == last.Round(0) { //nolint:staticcheck // deliberate struct comparison, see above
		t.Error("lastTick lost its monotonic clock reading (In()/Round() applied before storing?)")
	}
}

func TestSkippedOutcomeDoesNotOverwriteLast(t *testing.T) {
	t.Parallel()
	runner := &countingRunner{outcome: deploy.OutcomeDeployed}
	s := mustNewAt(t, testConfig(), runner, testStore(t), time.Now())
	s.runOne(context.Background(), s.cfg.Stacks[0])
	runner.outcome = deploy.OutcomeSkipped
	s.runOne(context.Background(), s.cfg.Stacks[0])
	if got := s.Snapshot().Stacks["web"].LastOutcome; got != "deployed" {
		t.Errorf("outcome = %q, want deployed (skip must not overwrite)", got)
	}
}
