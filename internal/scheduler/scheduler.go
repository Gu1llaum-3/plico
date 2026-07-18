// Package scheduler drives the polling loop: every tick, each due stack gets
// a goroutine; the per-stack lock inside the deployer guarantees skip-running
// semantics (F37).
//
// Scheduled stacks (F5/F7/F8) follow a strict window policy: a cron firing
// opens a deployment window [firing, firing+window]; deployments only ever
// happen inside a window, with a tolerance of one poll interval on the tick
// that discovers the firing (ticker jitter must not turn a healthy firing
// into a missed one). The schedule anchor — last accounted firing plus the
// cron expression it was computed under — is persisted in the state store:
// a restart during a still-open window re-opens it; a schedule edit resets
// the anchor instead of synthesizing phantom past firings; a firing whose
// window fully elapsed (daemon down, host paused, in-flight run covering
// the whole window) is loudly logged as missed, never replayed late.
//
// DST (robfig/cron on wall-clock times): a firing scheduled inside the
// skipped hour does not run; inside the repeated hour it runs once.
package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/deploy"
	"github.com/Gu1llaum-3/plico/internal/state"
)

// StackRunner is what the scheduler needs from the deployer.
type StackRunner interface {
	RunStack(ctx context.Context, st config.StackConfig) deploy.Outcome
	// CheckStack fetches and diffs without deploying (F6).
	CheckStack(ctx context.Context, st config.StackConfig) deploy.Outcome
}

// catchUpLimit bounds the firing catch-up loop: robfig/cron's Next() is
// guarded against zero times, but a pathological spec plus a huge clock jump
// must degrade to a log line, not wedge the scheduler.
const catchUpLimit = 100_000

type Scheduler struct {
	cfg          *config.Config
	deployer     StackRunner
	store        *state.Store
	log          *slog.Logger
	loc          *time.Location
	catchUpLimit int // per-instance so tests can exercise the abort path

	mu       sync.Mutex
	lastTick time.Time // carries a monotonic reading: /healthz liveness must be immune to wall-clock steps
	running  map[string]time.Time
	outcomes map[string]deploy.Outcome
	scheds   map[string]*stackSched // only stacks with a cron schedule
}

// stackSched tracks one stack's deployment window state.
type stackSched struct {
	sched         cron.Schedule
	spec          string // cron expression, persisted with the anchor
	window        time.Duration
	next          time.Time // earliest firing not yet accounted for
	windowFiring  time.Time // firing of the currently open window (zero = none)
	windowUntil   time.Time
	attempted     bool      // a run of this window completed (set on completion, attributed by dispatch firing)
	dispatched    time.Time // firing the currently in-flight run was dispatched for (zero = none)
	dormantLogged bool      // the "no future firing" error was already emitted
}

// StackStatus is one stack's live view for /healthz.
type StackStatus struct {
	RunningSince *time.Time `json:"running_since,omitempty"`
	LastOutcome  string     `json:"last_outcome,omitempty"`
	NextRun      *time.Time `json:"next_run,omitempty"` // next window opening (scheduled stacks only)
}

// Snapshot feeds the semantic healthcheck (F35).
type Snapshot struct {
	LastTick time.Time
	Stacks   map[string]StackStatus
}

// New builds the scheduler anchored at the current time. It fails closed: an
// unparsable or never-firing schedule is an error, never a silent fall-back
// to every-tick.
func New(cfg *config.Config, d StackRunner, store *state.Store, log *slog.Logger) (*Scheduler, error) {
	return NewAt(cfg, d, store, log, time.Now())
}

// NewAt is New with an explicit construction time, for deterministic tests.
func NewAt(cfg *config.Config, d StackRunner, store *state.Store, log *slog.Logger, now time.Time) (*Scheduler, error) {
	s := &Scheduler{
		cfg:          cfg,
		deployer:     d,
		store:        store,
		log:          log,
		loc:          cfg.Location(),
		catchUpLimit: catchUpLimit,
		running:      map[string]time.Time{},
		outcomes:     map[string]deploy.Outcome{},
		scheds:       map[string]*stackSched{},
	}
	nowLoc := now.In(s.loc)
	for _, st := range cfg.Stacks {
		if st.Schedule == "" {
			continue // no schedule: due at every poll tick
		}
		sched, err := cron.ParseStandard(st.Schedule)
		if err != nil {
			return nil, fmt.Errorf("stack %q: schedule %q: %w", st.Name, st.Schedule, err)
		}
		ss := &stackSched{sched: sched, spec: st.Schedule, window: st.Window.Duration}

		// Resume from the persisted anchor so a firing that happened while
		// the daemon was down is seen (re-opened or declared missed) instead
		// of silently dropped. The anchor is only valid under the expression
		// it was computed with: a schedule edit re-anchors at now, otherwise
		// old anchors would synthesize phantom firings under the new spec.
		anchor := time.Time{}
		prev, _ := store.Get(st.Name)
		switch {
		case prev.LastFiring.IsZero():
			// fresh install
		case prev.LastFiring.After(nowLoc):
			// Checked before legacy adoption: a future anchor freezes the
			// schedule until the wall clock catches up, whatever wrote it.
			log.Warn("persisted schedule anchor is in the future (wall clock stepped back?), re-anchoring at now",
				"stack", st.Name, "anchor", prev.LastFiring.Format(time.RFC3339))
			// Deliberate backward re-anchor: bypass the monotonic guard.
			s.resetAnchor(st.Name, nowLoc, st.Schedule)
			anchor = nowLoc
		case prev.ScheduleSpec == "":
			// State written before schedule_spec existed: adopt the anchor
			// (resetting would silently drop a window across the upgrade).
			log.Info("adopting legacy schedule anchor", "stack", st.Name,
				"anchor", prev.LastFiring.Format(time.RFC3339))
			anchor = prev.LastFiring.In(s.loc)
			s.persistAnchor(st.Name, anchor, st.Schedule)
		case prev.ScheduleSpec != st.Schedule:
			log.Info("schedule changed, resetting anchor", "stack", st.Name,
				"old", prev.ScheduleSpec, "new", st.Schedule)
		default:
			anchor = prev.LastFiring.In(s.loc)
		}
		if anchor.IsZero() {
			anchor = nowLoc
			// Persist immediately: a crash during the very first window must
			// re-open it on restart, which needs a pre-window anchor on disk.
			// (persistAnchor skips unchanged writes, so a normal restart
			// performs zero state writes here.)
			s.persistAnchor(st.Name, anchor, st.Schedule)
		}

		ss.next = ss.sched.Next(anchor)
		if ss.next.IsZero() {
			// Rejected by config validation; double-checked here because a
			// zero time would wedge the catch-up loop below.
			return nil, fmt.Errorf("stack %q: schedule %q never fires", st.Name, st.Schedule)
		}
		s.scheds[st.Name] = ss
		log.Info("stack scheduled", "stack", st.Name, "schedule", st.Schedule,
			"window", st.Window.String(), "next_run", ss.next.Format(time.RFC3339))
	}
	return s, nil
}

// Run blocks until ctx is cancelled, then waits for in-flight runs to end.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval.Duration)
	defer ticker.Stop()

	var wg sync.WaitGroup
	tick := func() {
		now := time.Now() // keeps its monotonic reading for lastTick
		s.mu.Lock()
		s.lastTick = now
		s.mu.Unlock()
		apply, check := s.due(now.In(s.loc))
		for _, st := range apply {
			wg.Add(1)
			go func(st config.StackConfig) {
				defer wg.Done()
				s.runOne(ctx, st, false)
			}(st)
		}
		for _, st := range check {
			wg.Add(1)
			go func(st config.StackConfig) {
				defer wg.Done()
				s.runOne(ctx, st, true)
			}(st)
		}
	}

	tick() // first pass immediately, then on every tick
	for {
		select {
		case <-ctx.Done():
			s.log.Info("scheduler stopping, draining in-flight runs")
			wg.Wait()
			return
		case <-ticker.C:
			tick()
		}
	}
}

// due returns the stacks to process this tick: apply gets the full pipeline,
// check only fetch+diff+queued-notification (F6). A stack without a schedule
// is always apply-due; a scheduled stack is apply-due while its window is
// open, and check-due outside it when checks are enabled.
func (s *Scheduler) due(now time.Time) (apply, check []config.StackConfig) {
	grace := s.cfg.PollInterval.Duration // tolerated discovery lateness (ticker jitter)
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.cfg.Stacks {
		ss, scheduled := s.scheds[st.Name]
		if !scheduled {
			apply = append(apply, st)
			continue
		}
		// A run in flight only defers accounting for the window it was
		// dispatched for; other windows are judged on their own.
		inFlightForWindow := !ss.dispatched.IsZero() && ss.dispatched.Equal(ss.windowFiring)

		// Catch up on firings that occurred up to now. Only the latest can
		// open a window; earlier ones (daemon down across several firings,
		// or cron period shorter than the poll interval) are reported in
		// one aggregate warning.
		var fired, firstSkipped, lastSkipped, gapStart time.Time
		skipped := 0
		aborted := false
		for i := 0; !ss.next.IsZero() && !now.Before(ss.next); i++ {
			if i == 0 {
				gapStart = ss.next // true start of the gap, for diagnostics
			}
			if i >= s.catchUpLimit {
				aborted = true
				break
			}
			if !fired.IsZero() {
				if skipped == 0 {
					firstSkipped = fired
				}
				lastSkipped = fired
				skipped++
			}
			fired = ss.next
			s.advanceNext(ss, st.Name, ss.next)
		}
		if aborted {
			// Pathological gap (huge clock jump): report once, re-anchor at
			// now AND persist it — a stale on-disk anchor would repeat this
			// whole walk on every restart.
			s.log.Error("schedule catch-up aborted after too many firings, re-anchoring at now",
				"stack", st.Name, "skipped_at_least", s.catchUpLimit,
				"first", gapStart.Format(time.RFC3339),
				"last_walked", fired.Format(time.RFC3339))
			s.advanceNext(ss, st.Name, now)
			s.resetAnchor(st.Name, now, ss.spec)
			fired = time.Time{}
			skipped = 0
		}

		justOpened := false
		if !fired.IsZero() {
			if skipped > 0 {
				s.log.Warn("scheduled firings skipped without a run (daemon unavailable, or cron period < poll_interval)",
					"stack", st.Name, "count", skipped,
					"first", firstSkipped.Format(time.RFC3339),
					"last", lastSkipped.Format(time.RFC3339))
			}
			// A still-open window superseded by a new firing before any of
			// its runs happened is a missed deployment. Deferred only when
			// the in-flight run belongs to THAT window (its completion will
			// account for it); a run from an older window must not shield
			// the superseded one from being reported.
			if !ss.windowFiring.IsZero() && !ss.attempted && !inFlightForWindow {
				s.log.Warn("deployment window superseded before any run",
					"stack", st.Name, "firing", ss.windowFiring.Format(time.RFC3339))
				s.persistAnchor(st.Name, ss.windowFiring, ss.spec)
			}
			if now.After(fired.Add(ss.window).Add(grace)) {
				// Window fully elapsed before we could discover the firing
				// (daemon down, host paused): report, account, never deploy
				// outside the window.
				s.log.Warn("deployment window missed, not deploying outside window",
					"stack", st.Name,
					"firing", fired.Format(time.RFC3339),
					"window_end", fired.Add(ss.window).Format(time.RFC3339),
					"next_run", ss.next.Format(time.RFC3339))
				s.persistAnchor(st.Name, fired, ss.spec)
				ss.windowFiring = time.Time{}
			} else {
				ss.windowFiring = fired
				ss.windowUntil = fired.Add(ss.window)
				ss.attempted = false
				justOpened = true
				s.log.Info("deployment window opened", "stack", st.Name,
					"until", ss.windowUntil.Format(time.RFC3339),
					"next_run", ss.next.Format(time.RFC3339))
			}
		}

		// Close an expired window. Deferred while ITS run is in flight: a
		// run dispatched at the end of the window may legitimately finish
		// after it, and must not be reported as "window without any run".
		if !justOpened && !ss.windowFiring.IsZero() && !now.Before(ss.windowUntil) && !inFlightForWindow {
			if !ss.attempted {
				s.log.Warn("deployment window elapsed without any run (previous deploy overlapped the whole window?)",
					"stack", st.Name, "firing", ss.windowFiring.Format(time.RFC3339))
				s.persistAnchor(st.Name, ss.windowFiring, ss.spec)
			}
			ss.windowFiring = time.Time{}
		}

		switch {
		case justOpened || (!ss.windowFiring.IsZero() && now.Before(ss.windowUntil)):
			apply = append(apply, st)
		case st.CheckEnabled():
			check = append(check, st)
		}
	}
	return apply, check
}

func (s *Scheduler) runOne(ctx context.Context, st config.StackConfig, checkOnly bool) {
	s.mu.Lock()
	if _, inFlight := s.running[st.Name]; inFlight {
		// The deployer's TryLock would skip anyway; avoid goroutine churn.
		s.mu.Unlock()
		return
	}
	s.running[st.Name] = time.Now()
	// Capture which firing this run belongs to NOW: a long run may outlive
	// its window, and crediting whatever window is open at completion time
	// would consume a later firing that this run never observed. Checks
	// never belong to a window and never consume a firing.
	var dispatchFiring time.Time
	if ss, ok := s.scheds[st.Name]; ok && !checkOnly {
		dispatchFiring = ss.windowFiring
		ss.dispatched = dispatchFiring
	}
	s.mu.Unlock()

	// Detached context: shutdown must stop NEW ticks but let an in-flight
	// deployment finish (Run waits on the WaitGroup) — cancelling here would
	// SIGKILL a docker compose up mid-flight and leave the stack half
	// updated. Each run stays bounded by its own run_timeout.
	var outcome deploy.Outcome
	if checkOnly {
		outcome = s.deployer.CheckStack(context.WithoutCancel(ctx), st)
	} else {
		outcome = s.deployer.RunStack(context.WithoutCancel(ctx), st)
	}

	s.mu.Lock()
	delete(s.running, st.Name)
	if ss, ok := s.scheds[st.Name]; ok && !checkOnly {
		ss.dispatched = time.Time{}
	}
	if outcome != deploy.OutcomeSkipped {
		s.outcomes[st.Name] = outcome
		// A run actually happened: account the firing it was dispatched
		// for. A skipped run (F37) must not consume the firing — the window
		// keeps retrying on later ticks.
		if ss, ok := s.scheds[st.Name]; ok && !dispatchFiring.IsZero() {
			if ss.windowFiring.Equal(dispatchFiring) {
				ss.attempted = true
			}
			s.persistAnchor(st.Name, dispatchFiring, ss.spec)
		}
	}
	s.mu.Unlock()
}

// advanceNext moves ss.next to the firing after `from`, loudly flagging a
// schedule whose next occurrence falls beyond the cron library's lookahead
// (zero time): that stack will not be scheduled again until restart, and
// that must never happen silently.
func (s *Scheduler) advanceNext(ss *stackSched, stack string, from time.Time) {
	ss.next = ss.sched.Next(from)
	if ss.next.IsZero() && !ss.dormantLogged {
		ss.dormantLogged = true
		s.log.Error("schedule has no future firing within the cron lookahead; stack will not be scheduled again",
			"stack", stack, "schedule", ss.spec)
	}
}

// persistAnchor records the last accounted firing and the expression it was
// computed under. The anchor is monotonic: writes that would move it
// backward (a long run completing after newer firings were accounted) or
// that change nothing (retries within one window) are skipped — the state
// file is rewritten at most once per firing.
func (s *Scheduler) persistAnchor(stack string, firing time.Time, spec string) {
	if prev, ok := s.store.Get(stack); ok && prev.ScheduleSpec == spec && !firing.After(prev.LastFiring) {
		return
	}
	if err := s.store.Update(stack, func(st *state.StackState) {
		if st.ScheduleSpec != spec {
			st.ScheduleSpec = spec
			st.LastFiring = firing
		} else if firing.After(st.LastFiring) {
			st.LastFiring = firing
		}
	}); err != nil {
		s.log.Error("persisting schedule anchor failed", "stack", stack, "error", err)
	}
}

// resetAnchor deliberately bypasses the monotonic guard (future anchor after
// a wall-clock step back, catch-up abort re-anchoring at now).
func (s *Scheduler) resetAnchor(stack string, firing time.Time, spec string) {
	if err := s.store.Update(stack, func(st *state.StackState) {
		st.LastFiring = firing
		st.ScheduleSpec = spec
	}); err != nil {
		s.log.Error("persisting schedule anchor failed", "stack", stack, "error", err)
	}
}

func (s *Scheduler) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := Snapshot{LastTick: s.lastTick, Stacks: map[string]StackStatus{}}
	for _, st := range s.cfg.Stacks {
		var status StackStatus
		if t, ok := s.running[st.Name]; ok {
			tt := t
			status.RunningSince = &tt
		}
		if o, ok := s.outcomes[st.Name]; ok {
			status.LastOutcome = o.String()
		}
		if ss, ok := s.scheds[st.Name]; ok && !ss.next.IsZero() {
			// Always the next FUTURE firing: reporting the current window's
			// firing would show a next_run in the past for as long as a
			// deferred close keeps the window around.
			next := ss.next
			status.NextRun = &next
		}
		snap.Stacks[st.Name] = status
	}
	return snap
}
