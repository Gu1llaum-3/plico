// Package notify defines the notification abstraction and its backends.
// Failure to notify must never fail a deployment (F33): wrap every backend
// with WithLogFallback.
package notify

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

type EventType string

const (
	DeployQueued   EventType = "deploy_queued"
	DeployStart    EventType = "deploy_start"
	PreHookFailed  EventType = "pre_hook_failed"
	PreHookSkipped EventType = "pre_hook_skipped"
	DeployFailed   EventType = "deploy_failed"
	DeploySuccess  EventType = "deploy_success"
	// WindowMissed: a scheduled deployment window produced no run (daemon
	// down across the window, previous run covering it, or a firing
	// superseded before any run).
	WindowMissed EventType = "window_missed"
	// GitSyncFailed: git fetch has been failing for N consecutive runs
	// (revoked token, moved repo) — the stack is effectively unmanaged.
	GitSyncFailed EventType = "git_sync_failed"
	// DriftDetected: a periodic health re-check found a previously-deployed
	// stack degraded (a service unhealthy, dead, or crashed) between
	// deployments. Detection only — plico never auto-remediates.
	DriftDetected EventType = "drift_detected"
	// DriftResolved: a drifted stack returned to health. A recovery/positive
	// signal, opt-in per channel like deploy_success.
	DriftResolved EventType = "drift_resolved"
)

// AllEvents lists every event type, for config validation.
var AllEvents = []EventType{
	DeployQueued, DeployStart, PreHookFailed, PreHookSkipped,
	DeployFailed, DeploySuccess, WindowMissed, GitSyncFailed,
	DriftDetected, DriftResolved,
}

// DefaultEvents is what a channel receives when it does not configure an
// `events` list: failure-oriented (F32). deploy_success, deploy_queued and
// deploy_start are opt-in per channel.
var DefaultEvents = []EventType{
	PreHookFailed, PreHookSkipped, DeployFailed, WindowMissed, GitSyncFailed,
	DriftDetected,
}

// ParseEvents validates a config-provided list. Empty means DefaultEvents;
// "all" anywhere in the list means every event. Every OTHER name is still
// validated even when "all" is present, so a typo cannot ride along
// undetected (the operator may later drop "all" and silently lose it).
func ParseEvents(names []string) ([]EventType, error) {
	if len(names) == 0 {
		return DefaultEvents, nil
	}
	hasAll := false
	var out []EventType
	for _, n := range names {
		if n == "all" {
			hasAll = true
			continue
		}
		found := false
		for _, e := range AllEvents {
			if string(e) == n {
				out = append(out, e)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("unknown event %q (valid: all, %s)", n, joinEvents(AllEvents))
		}
	}
	if hasAll {
		return AllEvents, nil
	}
	return out, nil
}

func joinEvents(evs []EventType) string {
	names := make([]string, len(evs))
	for i, e := range evs {
		names[i] = string(e)
	}
	return strings.Join(names, ", ")
}

type Event struct {
	Type   EventType
	Stack  string
	RunID  string
	Ref    string
	OldSHA string
	NewSHA string
	Stage  string // pipeline stage that failed ("pull", "up", "verify", ...)
	Detail string // human message, e.g. tail of hook stderr (truncated)
	Time   time.Time
}

type Notifier interface {
	Notify(ctx context.Context, ev Event) error
}

// Multi fans out to several notifiers CONCURRENTLY and waits for all of
// them; it returns nil (each backend is expected to be wrapped with
// WithLogFallback already). Concurrency matters: channels share the
// caller's per-event deadline, and a hung ntfy endpoint must not consume
// the whole budget before a healthy SMTP channel even starts.
func Multi(ns ...Notifier) Notifier {
	return multi(ns)
}

type multi []Notifier

func (m multi) Notify(ctx context.Context, ev Event) error {
	var wg sync.WaitGroup
	for _, n := range m {
		wg.Add(1)
		go func(n Notifier) {
			defer wg.Done()
			_ = n.Notify(ctx, ev)
		}(n)
	}
	wg.Wait()
	return nil
}

// WithLogFallback decorates n so a send failure is logged locally and never
// propagated to the deployment pipeline (F33).
func WithLogFallback(n Notifier, log *slog.Logger) Notifier {
	return &logged{n: n, log: log}
}

type logged struct {
	n   Notifier
	log *slog.Logger
}

func (l *logged) Notify(ctx context.Context, ev Event) error {
	if err := l.n.Notify(ctx, ev); err != nil {
		l.log.Error("notification failed",
			"event", string(ev.Type), "stack", ev.Stack, "run_id", ev.RunID, "error", err)
	}
	return nil
}

// FilterEvents decorates n so it only receives the listed event types
// (per-channel filtering, F32).
func FilterEvents(n Notifier, events []EventType) Notifier {
	set := make(map[EventType]bool, len(events))
	for _, e := range events {
		set[e] = true
	}
	return &filtered{n: n, set: set}
}

type filtered struct {
	n   Notifier
	set map[EventType]bool
}

func (f *filtered) Notify(ctx context.Context, ev Event) error {
	if !f.set[ev.Type] {
		return nil
	}
	return f.n.Notify(ctx, ev)
}

// Nop is used when no notifier is configured.
type Nop struct{}

func (Nop) Notify(context.Context, Event) error { return nil }
