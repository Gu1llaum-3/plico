package main

import (
	"testing"
	"time"

	"github.com/Gu1llaum-3/plico/internal/api"
)

func TestDisplayStatus(t *testing.T) {
	t.Parallel()
	deployed := time.Date(2026, 7, 19, 4, 0, 0, 0, time.UTC)
	later := deployed.Add(5 * time.Minute)

	tests := []struct {
		name string
		st   api.StackHealth
		want string
	}{
		{"fresh install", api.StackHealth{}, "-"},
		{"persisted success", api.StackHealth{LastStatus: "success"}, "success"},
		{
			// The CI flake: a routine up_to_date poll tick right after a
			// deploy must not mask the persisted success.
			"up_to_date does not mask success",
			api.StackHealth{LastStatus: "success", UpdatedAt: &deployed,
				LastOutcome: "up_to_date", LastOutcomeAt: &later},
			"success",
		},
		{
			"deployed defers to persisted status",
			api.StackHealth{LastStatus: "success", UpdatedAt: &deployed,
				LastOutcome: "deployed", LastOutcomeAt: &later},
			"success",
		},
		{
			// A live failure (e.g. git sync) IS newer information.
			"recent failed outcome overrides",
			api.StackHealth{LastStatus: "success", UpdatedAt: &deployed,
				LastOutcome: "failed", LastOutcomeAt: &later},
			"failed",
		},
		{
			// A stale outcome older than the persisted state does not.
			"stale outcome ignored",
			api.StackHealth{LastStatus: "success", UpdatedAt: &later,
				LastOutcome: "failed", LastOutcomeAt: &deployed},
			"success",
		},
		{
			"queued outcome shows pending work",
			api.StackHealth{LastStatus: "success", UpdatedAt: &deployed,
				LastOutcome: "queued", LastOutcomeAt: &later},
			"queued",
		},
		{
			"running wins over everything",
			api.StackHealth{LastStatus: "success", RunningSince: &later},
			"running",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := displayStatus(tt.st); got != tt.want {
				t.Errorf("displayStatus() = %q, want %q", got, tt.want)
			}
		})
	}
}
