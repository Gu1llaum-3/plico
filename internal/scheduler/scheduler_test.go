package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"plico/internal/config"
	"plico/internal/deploy"
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
