// Package notify defines the notification abstraction and its backends.
// Failure to notify must never fail a deployment (F33): wrap every backend
// with WithLogFallback.
package notify

import (
	"context"
	"log/slog"
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
)

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

// Multi fans out to several notifiers; it returns nil (each backend is
// expected to be wrapped with WithLogFallback already).
func Multi(ns ...Notifier) Notifier {
	return multi(ns)
}

type multi []Notifier

func (m multi) Notify(ctx context.Context, ev Event) error {
	for _, n := range m {
		_ = n.Notify(ctx, ev)
	}
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

// Nop is used when no notifier is configured.
type Nop struct{}

func (Nop) Notify(context.Context, Event) error { return nil }
