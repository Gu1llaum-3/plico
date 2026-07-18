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
}

// catchUpLimit bounds the firing catch-up loop: robfig/cron's Next() is
// guarded against zero times, but a pathological spec plus a huge clock jump
// must degrade to a log line, not wedge the scheduler.
const catchUpLimit = 100_000

type Scheduler struct {
	cfg      *config.Config
	deployer StackRunner
	store    *state.Store
	log      *slog.Logger
	loc      *time.Location

	mu       sync.Mutex
	lastTick time.Time // carries a monotonic reading: /healthz liveness must be immune to wall-clock steps
	running  map[string]time.Time
	outcomes map[string]deploy.Outcome
	scheds   map[string]*stackSched // only stacks with a cron schedule
}

// stackSched tracks one stack's deployment window state.
type stackSched struct {
	sched        cron.Schedule
	spec         string // cron expression, persisted with the anchor
	window       time.Duration
	next         time.Time // earliest firing not yet accounted for
	windowFiring time.Time // firing of the currently open window (zero = none)
	windowUntil  time.Time
	attempted    bool // a run of this window completed (set on completion, attributed by dispatch firing)
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
		cfg:      cfg,
		deployer: d,
		store:    store,
		log:      log,
		loc:      cfg.Location(),
		running:  map[string]time.Time{},
		outcomes: map[string]deploy.Outcome{},
		scheds:   map[string]*stackSched{},
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
		case prev.ScheduleSpec != st.Schedule:
			log.Info("schedule changed, resetting anchor", "stack", st.Name,
				"old", prev.ScheduleSpec, "new", st.Schedule)
		case prev.LastFiring.After(nowLoc):
			log.Warn("persisted schedule anchor is in the future (wall clock stepped back?), re-anchoring at now",
				"stack", st.Name, "anchor", prev.LastFiring.Format(time.RFC3339))
		default:
			anchor = prev.LastFiring.In(s.loc)
		}
		if anchor.IsZero() {
			anchor = nowLoc
			// Persist immediately: a crash during the very first window must
			// re-open it on restart, which needs a pre-window anchor on disk.
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
		for _, st := range s.due(now.In(s.loc)) {
			wg.Add(1)
			go func(st config.StackConfig) {
				defer wg.Done()
				s.runOne(ctx, st)
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

// due returns the stacks to process this tick. A stack without a schedule is
// always due; a scheduled stack is due while its window is open.
func (s *Scheduler) due(now time.Time) []config.StackConfig {
	grace := s.cfg.PollInterval.Duration // tolerated discovery lateness (ticker jitter)
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []config.StackConfig
	for _, st := range s.cfg.Stacks {
		ss, scheduled := s.scheds[st.Name]
		if !scheduled {
			out = append(out, st)
			continue
		}
		_, inFlight := s.running[st.Name]

		// Catch up on firings that occurred up to now. Only the latest can
		// open a window; earlier ones (daemon down across several firings,
		// or cron period shorter than the poll interval) are reported in
		// one aggregate warning.
		var fired, firstSkipped, lastSkipped time.Time
		skipped := 0
		for i := 0; !ss.next.IsZero() && !now.Before(ss.next); i++ {
			if i >= catchUpLimit {
				s.log.Error("schedule catch-up aborted after too many firings, re-anchoring at now",
					"stack", st.Name, "limit", catchUpLimit)
				ss.next = ss.sched.Next(now)
				fired = time.Time{}
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
			ss.next = ss.sched.Next(ss.next)
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
			// its runs happened is a missed deployment — unless a run is in
			// flight right now, in which case its completion will account
			// for the old firing (attribution is captured at dispatch).
			if !ss.windowFiring.IsZero() && !ss.attempted && !inFlight {
				s.log.Warn("deployment window superseded before any run",
					"stack", st.Name, "firing", ss.windowFiring.Format(time.RFC3339))
				s.persistAnchorLocked(st.Name, ss.windowFiring, ss.spec)
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
				s.persistAnchorLocked(st.Name, fired, ss.spec)
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

		// Close an expired window. Deferred while a run is in flight: a run
		// dispatched at the end of the window may legitimately finish after
		// it, and must not be reported as "window without any run".
		if !justOpened && !ss.windowFiring.IsZero() && !now.Before(ss.windowUntil) && !inFlight {
			if !ss.attempted {
				s.log.Warn("deployment window elapsed without any run (previous deploy overlapped the whole window?)",
					"stack", st.Name, "firing", ss.windowFiring.Format(time.RFC3339))
				s.persistAnchorLocked(st.Name, ss.windowFiring, ss.spec)
			}
			ss.windowFiring = time.Time{}
		}

		if justOpened || (!ss.windowFiring.IsZero() && now.Before(ss.windowUntil)) {
			out = append(out, st)
		}
	}
	return out
}

func (s *Scheduler) runOne(ctx context.Context, st config.StackConfig) {
	s.mu.Lock()
	if _, inFlight := s.running[st.Name]; inFlight {
		// The deployer's TryLock would skip anyway; avoid goroutine churn.
		s.mu.Unlock()
		return
	}
	s.running[st.Name] = time.Now()
	// Capture which firing this run belongs to NOW: a long run may outlive
	// its window, and crediting whatever window is open at completion time
	// would consume a later firing that this run never observed.
	var dispatchFiring time.Time
	if ss, ok := s.scheds[st.Name]; ok {
		dispatchFiring = ss.windowFiring
	}
	s.mu.Unlock()

	// Detached context: shutdown must stop NEW ticks but let an in-flight
	// deployment finish (Run waits on the WaitGroup) — cancelling here would
	// SIGKILL a docker compose up mid-flight and leave the stack half
	// updated. Each run stays bounded by its own run_timeout.
	outcome := s.deployer.RunStack(context.WithoutCancel(ctx), st)

	s.mu.Lock()
	delete(s.running, st.Name)
	if outcome != deploy.OutcomeSkipped {
		s.outcomes[st.Name] = outcome
		// A run actually happened: account the firing it was dispatched
		// for. A skipped run (F37) must not consume the firing — the window
		// keeps retrying on later ticks.
		if ss, ok := s.scheds[st.Name]; ok && !dispatchFiring.IsZero() {
			if ss.windowFiring.Equal(dispatchFiring) {
				ss.attempted = true
			}
			s.persistAnchorLocked(st.Name, dispatchFiring, ss.spec)
		}
	}
	s.mu.Unlock()
}

// persistAnchorLocked records the last accounted firing and the expression
// it was computed under. Callers must hold s.mu (writes are rare: at most
// one per firing).
func (s *Scheduler) persistAnchorLocked(stack string, firing time.Time, spec string) {
	s.persistAnchor(stack, firing, spec)
}

func (s *Scheduler) persistAnchor(stack string, firing time.Time, spec string) {
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
		if ss, ok := s.scheds[st.Name]; ok {
			// While a window is open, the "next run" is effectively now.
			next := ss.next
			if !ss.windowFiring.IsZero() {
				next = ss.windowFiring
			}
			status.NextRun = &next
		}
		snap.Stacks[st.Name] = status
	}
	return snap
}
