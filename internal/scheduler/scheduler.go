// Package scheduler drives the polling loop: every tick, each stack gets a
// goroutine; the per-stack lock inside the deployer guarantees skip-running
// semantics (F37). Designed so per-stack cron schedules (v1) only have to
// change due().
package scheduler

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/deploy"
)

// StackRunner is what the scheduler needs from the deployer.
type StackRunner interface {
	RunStack(ctx context.Context, st config.StackConfig) deploy.Outcome
}

type Scheduler struct {
	cfg      *config.Config
	deployer StackRunner
	log      *slog.Logger
	loc      *time.Location

	mu       sync.Mutex
	lastTick time.Time
	running  map[string]time.Time // stack -> start of in-flight run
	outcomes map[string]deploy.Outcome
	scheds   map[string]*stackSched // only stacks with a cron schedule
}

// stackSched tracks one stack's deployment window (F5/F7): the cron firing
// opens a window of `window` duration; during it, every poll tick processes
// the stack. Cron times are evaluated in the configured timezone (F8).
//
// DST behavior (robfig/cron on wall-clock times): a firing scheduled inside
// the skipped hour does not run; a firing inside the repeated hour runs once
// (first occurrence).
type stackSched struct {
	sched       cron.Schedule
	window      time.Duration
	next        time.Time // next window opening
	windowUntil time.Time // end of the currently/last opened window
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

func New(cfg *config.Config, d StackRunner, log *slog.Logger) *Scheduler {
	s := &Scheduler{
		cfg:      cfg,
		deployer: d,
		log:      log,
		loc:      cfg.Location(),
		running:  map[string]time.Time{},
		outcomes: map[string]deploy.Outcome{},
		scheds:   map[string]*stackSched{},
	}
	now := time.Now().In(s.loc)
	for _, st := range cfg.Stacks {
		if st.Schedule == "" {
			continue // no schedule: due at every poll tick
		}
		sched, err := cron.ParseStandard(st.Schedule)
		if err != nil {
			// Validated at config load; defensive fallback to every-tick.
			log.Error("invalid schedule, stack will run every tick", "stack", st.Name, "error", err)
			continue
		}
		ss := &stackSched{sched: sched, window: st.Window.Duration, next: sched.Next(now)}
		s.scheds[st.Name] = ss
		log.Info("stack scheduled", "stack", st.Name, "schedule", st.Schedule,
			"window", st.Window.String(), "next_run", ss.next.Format(time.RFC3339))
	}
	return s
}

// Run blocks until ctx is cancelled, then waits for in-flight runs to end.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval.Duration)
	defer ticker.Stop()

	var wg sync.WaitGroup
	tick := func() {
		now := time.Now().In(s.loc)
		s.mu.Lock()
		s.lastTick = now
		s.mu.Unlock()
		for _, st := range s.due(now) {
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

// due returns the stacks to process this tick. A stack without a schedule
// is always due; a scheduled stack is due while its window is open. The
// tick that opens a window always counts, even when window < poll_interval,
// so every firing yields at least one run. (F5/F7)
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
		opened := false
		if !now.Before(ss.next) {
			ss.windowUntil = ss.next.Add(ss.window)
			ss.next = ss.sched.Next(now)
			opened = true
			s.log.Info("deployment window opened", "stack", st.Name,
				"until", ss.windowUntil.Format(time.RFC3339),
				"next_run", ss.next.Format(time.RFC3339))
		}
		if opened || now.Before(ss.windowUntil) {
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
	}
	s.mu.Unlock()
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
			next := ss.next
			status.NextRun = &next
		}
		snap.Stacks[st.Name] = status
	}
	return snap
}
