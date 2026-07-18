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

	mu       sync.Mutex
	lastTick time.Time
	running  map[string]time.Time // stack -> start of in-flight run
	outcomes map[string]deploy.Outcome
}

// StackStatus is one stack's live view for /healthz.
type StackStatus struct {
	RunningSince *time.Time `json:"running_since,omitempty"`
	LastOutcome  string     `json:"last_outcome,omitempty"`
}

// Snapshot feeds the semantic healthcheck (F35).
type Snapshot struct {
	LastTick time.Time
	Stacks   map[string]StackStatus
}

func New(cfg *config.Config, d StackRunner, log *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:      cfg,
		deployer: d,
		log:      log,
		running:  map[string]time.Time{},
		outcomes: map[string]deploy.Outcome{},
	}
}

// Run blocks until ctx is cancelled, then waits for in-flight runs to end.
func (s *Scheduler) Run(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.PollInterval.Duration)
	defer ticker.Stop()

	var wg sync.WaitGroup
	tick := func() {
		s.mu.Lock()
		s.lastTick = time.Now()
		s.mu.Unlock()
		for _, st := range s.due() {
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

// due returns the stacks to check this tick. MVP: all of them; v1 will
// consult a per-stack cron schedule here.
func (s *Scheduler) due() []config.StackConfig {
	return s.cfg.Stacks
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
		snap.Stacks[st.Name] = status
	}
	return snap
}
