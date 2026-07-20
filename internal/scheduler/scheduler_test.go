package scheduler

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/deploy"
	"github.com/Gu1llaum-3/plico/internal/notify"
	"github.com/Gu1llaum-3/plico/internal/state"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

type countingRunner struct {
	mu      sync.Mutex
	calls   map[string]int
	checks  map[string]int
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

func (c *countingRunner) CheckStack(_ context.Context, st config.StackConfig) deploy.Outcome {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.checks == nil {
		c.checks = map[string]int{}
	}
	c.checks[st.Name]++
	return deploy.OutcomeQueued
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
	s, err := NewAt(cfg, r, store, notify.Nop{}, discard(), now)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dueNames(s *Scheduler, now time.Time) []string {
	var names []string
	apply, _, _ := s.due(now)
	for _, st := range apply {
		names = append(names, st.Name)
	}
	return names
}

// dueEvents returns the notifications due() would emit this tick, for
// deterministic assertions (no async goroutine, no sleeps).
func dueEvents(s *Scheduler, now time.Time) []notify.Event {
	_, _, events := s.due(now)
	return events
}

func checkNames(s *Scheduler, now time.Time) []string {
	var names []string
	_, check, _ := s.due(now)
	for _, st := range check {
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

func (c *ctxCheckRunner) CheckStack(context.Context, config.StackConfig) deploy.Outcome {
	return deploy.OutcomeUpToDate
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
	s.runOne(context.Background(), s.cfg.Stacks[0], false)
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
	// 04:00 firing but its window (04:00-05:00) is long gone. That same tick
	// both selects the due stacks AND surfaces the missed-window event.
	apply, _, events := s.due(time.Date(2026, 7, 19, 9, 0, 0, 0, loc))
	if len(apply) != 1 || apply[0].Name != "always" {
		t.Errorf("missed window must not deploy late, apply = %v", apply)
	}
	// The missed firing is accounted (persisted anchor) so it is not
	// rediscovered after a restart.
	fire := time.Date(2026, 7, 19, 4, 0, 0, 0, loc)
	if st, _ := store.Get("nightly"); !st.LastFiring.Equal(fire) {
		t.Errorf("missed firing not persisted: anchor = %v, want %v", st.LastFiring, fire)
	}
	// A missed nightly deployment is exactly what the operator sleeps
	// through: it must NOTIFY, not just log. Assert EXACTLY one, so a
	// duplicate regression (both the elapsed and superseded branches
	// firing) is caught.
	if len(events) != 1 {
		t.Fatalf("want exactly 1 window_missed event, got %d: %+v", len(events), events)
	}
	if events[0].Type != notify.WindowMissed || events[0].Stack != "nightly" {
		t.Errorf("window_missed notification wrong: %+v", events[0])
	}
}

func TestRestartInsideOpenWindowReopensIt(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	// The daemon deployed yesterday (anchor = yesterday 04:00) and restarts
	// today at 04:10, inside today's still-open window.
	yesterday := time.Date(2026, 7, 18, 4, 0, 0, 0, loc)
	if err := store.Update("nightly", func(st *state.StackState) {
		st.LastFiring = yesterday
		st.ScheduleSpec = "0 4 * * *"
	}); err != nil {
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
	if err := store.Update("nightly", func(st *state.StackState) {
		st.LastFiring = today
		st.ScheduleSpec = "0 4 * * *"
	}); err != nil {
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
	// NOT be accounted — the anchor stays at the boot value written by NewAt.
	s.runOne(context.Background(), s.cfg.Stacks[0], false)
	if st, _ := store.Get("nightly"); !st.LastFiring.Equal(boot) {
		t.Errorf("skipped run must not advance the anchor, got %v, want boot %v", st.LastFiring, boot)
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

func countSkipEvents(events []notify.Event) int {
	n := 0
	for _, ev := range events {
		if strings.Contains(ev.Detail, "skipped") {
			n++
		}
	}
	return n
}

func TestSkippedFiringsNotifyOncePerStreak(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	cfg := &config.Config{
		Timezone:     "Europe/Paris",
		PollInterval: config.Duration{Duration: 5 * time.Minute},
		Stacks: []config.StackConfig{
			// Cron period (1m) < poll interval (5m): every tick skips firings.
			{Name: "fast", Schedule: "* * * * *", Window: config.Duration{Duration: time.Minute}},
		},
	}
	boot := time.Date(2026, 7, 19, 12, 0, 30, 0, loc)
	s := mustNewAt(t, cfg, &countingRunner{}, store, boot)

	// due() returns its events: count deterministically across three
	// consecutive skipping ticks — exactly one skip notification for the
	// whole continuous streak, no per-tick spam, no sleep.
	total := 0
	for i := 1; i <= 3; i++ {
		total += countSkipEvents(dueEvents(s, boot.Add(time.Duration(i)*5*time.Minute)))
	}
	if total != 1 {
		t.Errorf("per-tick spam: %d skipped-firings events for one continuous streak, want 1", total)
	}
}

// A second distinct skip streak, separated from the first only by
// firing-less ticks, must notify again — skipNotified must reset when a
// tick has no skips, not only when a skip-free FIRING is caught up.
func TestSecondSkipStreakNotifiesAgain(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	cfg := &config.Config{
		Timezone:     "Europe/Paris",
		PollInterval: config.Duration{Duration: time.Minute},
		Stacks: []config.StackConfig{
			{Name: "nightly", Schedule: "0 4 * * *", Window: config.Duration{Duration: 2 * time.Hour}},
		},
	}
	boot := time.Date(2026, 7, 19, 3, 0, 0, 0, loc)
	s := mustNewAt(t, cfg, &countingRunner{}, store, boot)

	// First streak: suspended across the 19th and 20th 04:00 firings,
	// resume the 20th at 09:00 → one catch-up tick skips one firing.
	first := countSkipEvents(dueEvents(s, time.Date(2026, 7, 20, 9, 0, 0, 0, loc)))
	if first != 1 {
		t.Fatalf("first streak: %d skip events, want 1", first)
	}
	// Firing-less ticks in between (nothing due) must clear the flag.
	_ = dueEvents(s, time.Date(2026, 7, 20, 12, 0, 0, 0, loc))
	// Second streak: suspended across the 21st and 22nd firings, resume the
	// 22nd at 09:00 → must notify again.
	second := countSkipEvents(dueEvents(s, time.Date(2026, 7, 22, 9, 0, 0, 0, loc)))
	if second != 1 {
		t.Errorf("second distinct streak was not notified (skipNotified stuck): %d skip events, want 1", second)
	}
}

func TestNewFailsClosedOnBadSchedule(t *testing.T) {
	t.Parallel()
	for _, spec := range []string{"not a cron", "0 0 30 2 *" /* Feb 30: parses but never fires */} {
		cfg := &config.Config{
			PollInterval: config.Duration{Duration: time.Minute},
			Stacks:       []config.StackConfig{{Name: "web", Schedule: spec}},
		}
		if _, err := NewAt(cfg, &countingRunner{}, testStore(t), notify.Nop{}, discard(), time.Now()); err == nil {
			t.Errorf("schedule %q must be a construction error, not a silent fallback or a wedge", spec)
		}
	}
}

func TestDiscoveryToleratesOneTickOfJitter(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	boot := time.Date(2026, 7, 19, 3, 0, 0, 0, loc)
	// Tiny window (one-shot per firing): the discovering tick must still
	// count even when it lands after windowUntil, within one poll interval.
	s := mustNewAt(t, nightlyConfig(time.Second), &countingRunner{}, store, boot)

	fire := time.Date(2026, 7, 19, 4, 0, 0, 0, loc)
	if got := dueNames(s, fire.Add(45*time.Second)); len(got) != 2 {
		t.Errorf("discovery within window+poll_interval must open, due = %v", got)
	}
	// Beyond window + one poll interval: genuinely missed.
	s2 := mustNewAt(t, nightlyConfig(time.Second), &countingRunner{}, testStore(t), boot)
	if got := dueNames(s2, fire.Add(2*time.Minute)); len(got) != 1 {
		t.Errorf("discovery beyond the jitter grace must be missed, due = %v", got)
	}
}

func TestFreshInstallAnchorIsPersisted(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	boot := time.Date(2026, 7, 19, 3, 0, 0, 0, loc)
	mustNewAt(t, nightlyConfig(time.Hour), &countingRunner{}, store, boot)

	// A crash during the first window must find a pre-window anchor on disk
	// so the window is re-opened, not silently dropped.
	st, ok := store.Get("nightly")
	if !ok || st.LastFiring.IsZero() {
		t.Fatal("fresh-install anchor not persisted")
	}
	if st.ScheduleSpec != "0 4 * * *" {
		t.Errorf("schedule spec not persisted with the anchor: %q", st.ScheduleSpec)
	}
	// Simulated restart at 04:10: the window opened at 04:00 must re-open.
	s2 := mustNewAt(t, nightlyConfig(time.Hour), &countingRunner{}, store,
		time.Date(2026, 7, 19, 4, 10, 0, 0, loc))
	if got := dueNames(s2, time.Date(2026, 7, 19, 4, 11, 0, 0, loc)); len(got) != 2 {
		t.Errorf("first window after fresh install must survive a crash, due = %v", got)
	}
}

func TestScheduleChangeResetsAnchor(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	// Anchor persisted under the old daily-04:00 schedule.
	if err := store.Update("nightly", func(st *state.StackState) {
		st.LastFiring = time.Date(2026, 7, 19, 4, 0, 0, 0, loc)
		st.ScheduleSpec = "0 4 * * *"
	}); err != nil {
		t.Fatal(err)
	}
	// The admin switches to every-2-hours and restarts at 10:30: no phantom
	// firings (06:00, 08:00, 10:00) may be synthesized under the new spec.
	cfg := nightlyConfig(time.Hour)
	cfg.Stacks[0].Schedule = "0 */2 * * *"
	restart := time.Date(2026, 7, 19, 10, 30, 0, 0, loc)
	s := mustNewAt(t, cfg, &countingRunner{}, store, restart)

	if got := dueNames(s, restart.Add(time.Minute)); len(got) != 1 || got[0] != "always" {
		t.Errorf("schedule change must not replay phantom firings, due = %v", got)
	}
	next := s.Snapshot().Stacks["nightly"].NextRun
	want := time.Date(2026, 7, 19, 12, 0, 0, 0, loc)
	if next == nil || !next.Equal(want) {
		t.Errorf("next_run = %v, want %v (first firing after the restart)", next, want)
	}
}

func TestFutureAnchorIsReanchored(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	// Wall clock stepped back: the persisted anchor is 2h in the future.
	if err := store.Update("nightly", func(st *state.StackState) {
		st.LastFiring = time.Date(2026, 7, 19, 5, 0, 0, 0, loc)
		st.ScheduleSpec = "0 4 * * *"
	}); err != nil {
		t.Fatal(err)
	}
	boot := time.Date(2026, 7, 19, 3, 0, 0, 0, loc)
	s := mustNewAt(t, nightlyConfig(time.Hour), &countingRunner{}, store, boot)

	// The schedule must not freeze: today's 04:00 firing still opens.
	if got := dueNames(s, time.Date(2026, 7, 19, 4, 0, 30, 0, loc)); len(got) != 2 {
		t.Errorf("future anchor froze the schedule, due = %v", got)
	}
}

func TestSupersededUnattemptedWindowIsReportedAndAccounted(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	cfg := &config.Config{
		Timezone:     "Europe/Paris",
		PollInterval: config.Duration{Duration: time.Minute},
		Stacks: []config.StackConfig{
			// Window (1h) intentionally longer than the cron period (30m).
			{Name: "fast", Schedule: "*/30 * * * *", Window: config.Duration{Duration: time.Hour}},
		},
	}
	boot := time.Date(2026, 7, 19, 9, 50, 0, 0, loc)
	s := mustNewAt(t, cfg, &countingRunner{}, store, boot)

	first := time.Date(2026, 7, 19, 10, 0, 0, 0, loc)
	if got := dueNames(s, first.Add(30*time.Second)); len(got) != 1 {
		t.Fatalf("first window should open, due = %v", got)
	}
	// No run happens (ticks never dispatched here); the 10:30 firing
	// supersedes the still-open 10:00 window: the old firing must be
	// accounted so a restart does not rediscover it.
	if got := dueNames(s, first.Add(31*time.Minute)); len(got) != 1 {
		t.Fatalf("second window should open, due = %v", got)
	}
	st, _ := store.Get("fast")
	if !st.LastFiring.Equal(first) {
		t.Errorf("superseded firing not accounted: anchor = %v, want %v", st.LastFiring, first)
	}
}

func TestLongRunIsAttributedToItsDispatchWindow(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	cfg := &config.Config{
		Timezone:     "Europe/Paris",
		PollInterval: config.Duration{Duration: time.Minute},
		Stacks: []config.StackConfig{
			{Name: "fast", Schedule: "*/30 * * * *", Window: config.Duration{Duration: time.Hour}},
		},
	}
	boot := time.Date(2026, 7, 19, 9, 50, 0, 0, loc)
	runner := &countingRunner{outcome: deploy.OutcomeDeployed, block: make(chan struct{})}
	s := mustNewAt(t, cfg, runner, store, boot)

	firstFire := time.Date(2026, 7, 19, 10, 0, 0, 0, loc)
	if got := dueNames(s, firstFire.Add(30*time.Second)); len(got) != 1 {
		t.Fatalf("first window should open, due = %v", got)
	}
	// A run dispatched in window 10:00 blocks past the 10:30 firing.
	done := make(chan struct{})
	go func() { s.runOne(context.Background(), cfg.Stacks[0], false); close(done) }()
	deadline := time.After(5 * time.Second)
	for s.Snapshot().Stacks["fast"].RunningSince == nil {
		select {
		case <-deadline:
			t.Fatal("run never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// The 10:30 firing opens a new window while the old run is in flight;
	// no "superseded" warning should fire (the old firing will be accounted
	// at completion) and the new window must not be consumed.
	if got := dueNames(s, firstFire.Add(31*time.Minute)); len(got) != 1 {
		t.Fatalf("second window should open, due = %v", got)
	}
	close(runner.block)
	<-done

	// Completion accounts the 10:00 firing (dispatch attribution), NOT the
	// 10:30 one whose git state this run never observed.
	st, _ := store.Get("fast")
	if !st.LastFiring.Equal(firstFire) {
		t.Errorf("anchor = %v, want the dispatch firing %v", st.LastFiring, firstFire)
	}
	// The 10:30 window is still retryable.
	if got := dueNames(s, firstFire.Add(35*time.Minute)); len(got) != 1 {
		t.Errorf("second window must stay open after the old run completed, due = %v", got)
	}
}

func TestSupersedeReportedDespiteUnrelatedInFlightRun(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	cfg := &config.Config{
		Timezone:     "Europe/Paris",
		PollInterval: config.Duration{Duration: time.Minute},
		Stacks: []config.StackConfig{
			{Name: "fast", Schedule: "*/30 * * * *", Window: config.Duration{Duration: time.Hour}},
		},
	}
	boot := time.Date(2026, 7, 19, 9, 50, 0, 0, loc)
	runner := &countingRunner{outcome: deploy.OutcomeDeployed, block: make(chan struct{})}
	s := mustNewAt(t, cfg, runner, store, boot)

	fire1 := time.Date(2026, 7, 19, 10, 0, 0, 0, loc)
	fire2 := time.Date(2026, 7, 19, 10, 30, 0, 0, loc)
	if got := dueNames(s, fire1.Add(30*time.Second)); len(got) != 1 {
		t.Fatalf("first window should open, due = %v", got)
	}
	// The 10:00 run blocks for hours.
	done := make(chan struct{})
	go func() { s.runOne(context.Background(), cfg.Stacks[0], false); close(done) }()
	deadline := time.After(5 * time.Second)
	for s.Snapshot().Stacks["fast"].RunningSince == nil {
		select {
		case <-deadline:
			t.Fatal("run never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// 10:30 supersedes 10:00 silently (the in-flight run belongs to 10:00).
	if got := dueNames(s, fire2.Add(30*time.Second)); len(got) != 1 {
		t.Fatalf("second window should open, due = %v", got)
	}
	// 11:00 supersedes 10:30 — the in-flight run does NOT belong to 10:30,
	// so the superseded window must be accounted, not silently dropped.
	if got := dueNames(s, fire2.Add(31*time.Minute)); len(got) != 1 {
		t.Fatalf("third window should open, due = %v", got)
	}
	if st, _ := store.Get("fast"); !st.LastFiring.Equal(fire2) {
		t.Errorf("superseded 10:30 window not accounted during unrelated run: anchor = %v", st.LastFiring)
	}
	// Completion of the old 10:00 run must NOT move the anchor backward.
	close(runner.block)
	<-done
	if st, _ := store.Get("fast"); !st.LastFiring.Equal(fire2) {
		t.Errorf("anchor regressed on completion: %v, want %v", st.LastFiring, fire2)
	}
}

func TestLegacyStateWithoutSpecAdoptsAnchor(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	// State written by a version that predates schedule_spec.
	yesterday := time.Date(2026, 7, 18, 4, 0, 0, 0, loc)
	if err := store.Update("nightly", func(st *state.StackState) {
		st.LastFiring = yesterday // no ScheduleSpec
	}); err != nil {
		t.Fatal(err)
	}
	restart := time.Date(2026, 7, 19, 4, 10, 0, 0, loc)
	s := mustNewAt(t, nightlyConfig(time.Hour), &countingRunner{}, store, restart)

	// The anchor must be adopted, not reset: today's still-open window
	// re-opens across the upgrade.
	if got := dueNames(s, restart.Add(30*time.Second)); len(got) != 2 {
		t.Errorf("legacy anchor must be adopted (window re-opened), due = %v", got)
	}
	if st, _ := store.Get("nightly"); st.ScheduleSpec != "0 4 * * *" {
		t.Errorf("adopted anchor must persist the spec, got %q", st.ScheduleSpec)
	}
}

func TestLegacyFutureAnchorIsReanchored(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	// Legacy state (no schedule_spec) whose anchor is in the future: the
	// future-anchor guard must win over legacy adoption, or the schedule
	// would freeze until the wall clock catches up.
	if err := store.Update("nightly", func(st *state.StackState) {
		st.LastFiring = time.Date(2026, 7, 19, 6, 0, 0, 0, loc) // 3h ahead
	}); err != nil {
		t.Fatal(err)
	}
	boot := time.Date(2026, 7, 19, 3, 0, 0, 0, loc)
	s := mustNewAt(t, nightlyConfig(time.Hour), &countingRunner{}, store, boot)

	if got := dueNames(s, time.Date(2026, 7, 19, 4, 0, 30, 0, loc)); len(got) != 2 {
		t.Errorf("legacy future anchor froze the schedule, due = %v", got)
	}
}

func TestCatchUpAbortReanchorsAndPersists(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	// Anchor far in the past relative to a 1-minute cron.
	if err := store.Update("fast", func(st *state.StackState) {
		st.LastFiring = time.Date(2026, 7, 19, 9, 0, 0, 0, loc)
		st.ScheduleSpec = "* * * * *"
	}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Timezone:     "Europe/Paris",
		PollInterval: config.Duration{Duration: time.Minute},
		Stacks: []config.StackConfig{
			{Name: "fast", Schedule: "* * * * *", Window: config.Duration{Duration: time.Minute}},
		},
	}
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, loc) // 180 firings behind
	s := mustNewAt(t, cfg, &countingRunner{}, store, now)
	s.catchUpLimit = 10

	if got := dueNames(s, now); len(got) != 0 {
		t.Errorf("aborted catch-up must not open a window, due = %v", got)
	}
	// The re-anchor must be persisted so a restart does not repeat the walk.
	st, _ := store.Get("fast")
	if !st.LastFiring.Equal(now) {
		t.Errorf("abort re-anchor not persisted: %v, want %v", st.LastFiring, now)
	}
	// And the schedule keeps living afterwards.
	if got := dueNames(s, now.Add(90*time.Second)); len(got) != 1 {
		t.Errorf("schedule must resume after abort, due = %v", got)
	}
}

func TestExpiredWindowCloseIsDeferredWhileRunInFlight(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	store := testStore(t)
	runner := &countingRunner{outcome: deploy.OutcomeDeployed, block: make(chan struct{})}
	s := mustNewAt(t, nightlyConfig(time.Hour), runner, store, time.Date(2026, 7, 19, 3, 0, 0, 0, loc))

	fire := time.Date(2026, 7, 19, 4, 0, 0, 0, loc)
	if got := dueNames(s, fire.Add(59*time.Minute)); len(got) != 2 {
		t.Fatalf("window should be open, due = %v", got)
	}
	done := make(chan struct{})
	go func() { s.runOne(context.Background(), s.cfg.Stacks[0], false); close(done) }()
	deadline := time.After(5 * time.Second)
	for s.Snapshot().Stacks["nightly"].RunningSince == nil {
		select {
		case <-deadline:
			t.Fatal("run never started")
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Window expires while the run (started at 04:59) is still in flight:
	// the close must be deferred, without a false "no run" warning, and the
	// anchor must not be written yet.
	if got := dueNames(s, fire.Add(65*time.Minute)); len(got) != 1 {
		t.Errorf("expired window must not be due, due = %v", got)
	}
	// /healthz must keep reporting a FUTURE next_run during the deferral,
	// not the stale past firing of the still-open window.
	if next := s.Snapshot().Stacks["nightly"].NextRun; next == nil || !next.After(fire) {
		t.Errorf("next_run during deferred close = %v, want a future firing", next)
	}
	if st, _ := store.Get("nightly"); !st.LastFiring.Equal(time.Date(2026, 7, 19, 3, 0, 0, 0, loc)) {
		t.Errorf("anchor must still be the boot anchor while the run is in flight, got %v", st.LastFiring)
	}
	close(runner.block)
	<-done
	// Completion accounts the firing.
	if st, _ := store.Get("nightly"); !st.LastFiring.Equal(fire) {
		t.Errorf("anchor after completion = %v, want %v", st.LastFiring, fire)
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

func TestCheckDispatchedOutsideWindowOnly(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	yes := true
	cfg := nightlyConfig(time.Hour)
	cfg.Stacks[0].Check = &yes
	boot := time.Date(2026, 7, 19, 3, 0, 0, 0, loc)
	s := mustNewAt(t, cfg, &countingRunner{}, testStore(t), boot)

	fire := time.Date(2026, 7, 19, 4, 0, 0, 0, loc)

	// Outside the window: the scheduled stack is check-due, not apply-due.
	if got := checkNames(s, fire.Add(-time.Minute)); len(got) != 1 || got[0] != "nightly" {
		t.Errorf("outside window: check = %v, want [nightly]", got)
	}
	if got := dueNames(s, fire.Add(-30*time.Second)); len(got) != 1 || got[0] != "always" {
		t.Errorf("outside window: apply = %v, want [always]", got)
	}
	// Inside the window: apply, never check.
	if got := checkNames(s, fire.Add(30*time.Second)); len(got) != 0 {
		t.Errorf("inside window: check = %v, want none", got)
	}
	if got := dueNames(s, fire.Add(time.Minute)); len(got) != 2 {
		t.Errorf("inside window: apply = %v, want both", got)
	}
	// The unscheduled stack is never check-due.
	for _, names := range [][]string{checkNames(s, fire.Add(2*time.Hour))} {
		for _, n := range names {
			if n == "always" {
				t.Error("unscheduled stack must never be check-due")
			}
		}
	}
}

func TestCheckDisabledByDefault(t *testing.T) {
	t.Parallel()
	loc := paris(t)
	s := mustNewAt(t, nightlyConfig(time.Hour), &countingRunner{}, testStore(t),
		time.Date(2026, 7, 19, 3, 0, 0, 0, loc))
	if got := checkNames(s, time.Date(2026, 7, 19, 3, 30, 0, 0, loc)); len(got) != 0 {
		t.Errorf("check must be opt-in, got %v", got)
	}
}

func TestSkippedOutcomeDoesNotOverwriteLast(t *testing.T) {
	t.Parallel()
	runner := &countingRunner{outcome: deploy.OutcomeDeployed}
	s := mustNewAt(t, testConfig(), runner, testStore(t), time.Now())
	s.runOne(context.Background(), s.cfg.Stacks[0], false)
	runner.outcome = deploy.OutcomeSkipped
	s.runOne(context.Background(), s.cfg.Stacks[0], false)
	if got := s.Snapshot().Stacks["web"].LastOutcome; got != "deployed" {
		t.Errorf("outcome = %q, want deployed (skip must not overwrite)", got)
	}
}
