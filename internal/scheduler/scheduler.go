// Package scheduler drives the polling loop: every tick, each due stack gets
// a goroutine; the per-stack lock inside the deployer guarantees skip-running
// semantics (F37).
//
// Scheduled stacks (F5/F7/F8) follow a strict window policy: a cron firing
// opens a deployment window [firing, firing+window]; deployments only ever
// happen inside a window. The schedule anchor (last accounted firing) is
// persisted in the state store, so a restart during a still-open window
// re-opens it, and a firing whose window fully elapsed — daemon down, VM
// paused, or an in-flight run overlapping the whole window — is loudly
// logged as missed, never replayed late.
//
// DST (robfig/cron on wall-clock times): a firing scheduled inside the
// skipped hour does not run; inside the repeated hour it runs once.
package scheduler

import (
	"context"
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
	window       time.Duration
	next         time.Time // earliest firing not yet accounted for
	windowFiring time.Time // firing of the currently open window (zero = none)
	windowUntil  time.Time
	attempted    bool // a run actually started during the current window
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
// unparsable schedule is an error, never a silent fall-back to every-tick.
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
	for _, st := range cfg.Stacks {
		if st.Schedule == "" {
			continue // no schedule: due at every poll tick
		}
		sched, err := cron.ParseStandard(st.Schedule)
		if err != nil {
			return nil, err // validated at config load; fail closed on any skew
		}
		ss := &stackSched{sched: sched, window: st.Window.Duration}
		// Resume from the persisted anchor: the next unaccounted firing is
		// derived from the last accounted one, so a firing that happened
		// while the daemon was down is seen (re-opened or declared missed)
		// instead of silently dropped. First install: anchor at startup.
		anchor := now.In(s.loc)
		if prev, ok := store.Get(st.Name); ok && !prev.LastFiring.IsZero() {
			anchor = prev.LastFiring.In(s.loc)
		}
		ss.next = sched.Next(anchor)
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
// always due; a scheduled stack is due while its window is open. The window
// is authoritative: a firing whose window already fully elapsed at the tick
// that discovers it is declared missed (WARN), never deployed late.
func (s *Scheduler) due(now time.Time) []config.StackConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []config.StackConfig
	for _, st := range s.cfg.Stacks {
		ss, scheduled := s.scheds[st.Name]
		if !scheduled {
			out = append(out, st)
			continue
		}

		// Collect every firing that occurred up to now; only the latest can
		// still have an open window, earlier ones are collapsed (a cron
		// period shorter than poll_interval cannot yield one run each).
		var fired time.Time
		collapsed := 0
		for !now.Before(ss.next) {
			if !fired.IsZero() {
				collapsed++
			}
			fired = ss.next
			ss.next = ss.sched.Next(ss.next)
		}

		justOpened := false
		if !fired.IsZero() {
			if collapsed > 0 {
				s.log.Warn("cron firings collapsed (period shorter than poll_interval)",
					"stack", st.Name, "collapsed", collapsed)
			}
			if now.After(fired.Add(ss.window)) {
				// Window fully elapsed before we could open it (daemon down,
				// host paused): report, account, never deploy outside it.
				s.log.Warn("deployment window missed, not deploying outside window",
					"stack", st.Name,
					"firing", fired.Format(time.RFC3339),
					"window_end", fired.Add(ss.window).Format(time.RFC3339),
					"next_run", ss.next.Format(time.RFC3339))
				s.persistAnchorLocked(st.Name, fired)
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

		// Close an expired window; a window that saw no actual run (every
		// tick skipped over an in-flight deploy) is reported as missed.
		if !justOpened && !ss.windowFiring.IsZero() && !now.Before(ss.windowUntil) {
			if !ss.attempted {
				s.log.Warn("deployment window elapsed without any run (in-flight run overlapped?)",
					"stack", st.Name, "firing", ss.windowFiring.Format(time.RFC3339))
				s.persistAnchorLocked(st.Name, ss.windowFiring)
			}
			ss.windowFiring = time.Time{}
		}

		if !ss.windowFiring.IsZero() {
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
		// A run actually happened: the current window's firing is accounted
		// for. Persisting it here (not at window close) means a restart
		// after the attempt will not re-open the window nor warn "missed".
		if ss, ok := s.scheds[st.Name]; ok && !ss.windowFiring.IsZero() && !ss.attempted {
			ss.attempted = true
			s.persistAnchorLocked(st.Name, ss.windowFiring)
		}
	}
	s.mu.Unlock()
}

// persistAnchorLocked records the last accounted firing in the state store.
// Callers must hold s.mu (writes are rare: at most one per firing).
func (s *Scheduler) persistAnchorLocked(stack string, firing time.Time) {
	if err := s.store.Update(stack, func(st *state.StackState) {
		st.LastFiring = firing
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
