package heartbeat

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBeatOncePingsOnlyWhenHealthy(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	healthy := true
	b := New(srv.URL, time.Second, func() bool { return healthy }, discard())

	// Healthy → one ping.
	b.beatOnce(context.Background())
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Fatalf("healthy beat: hits = %d, want 1", got)
	}
	// Unhealthy → dead-man's switch: no ping.
	healthy = false
	b.beatOnce(context.Background())
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("unhealthy beat pinged: hits = %d, want still 1", got)
	}
}

func TestBeatOnceSurvivesServerError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	b := New(srv.URL, time.Second, func() bool { return true }, discard())
	// Must not panic; a 5xx is logged and swallowed.
	b.beatOnce(context.Background())
}

func TestBeatOnceSurvivesUnreachable(t *testing.T) {
	t.Parallel()
	// A healthy daemon whose monitor is unreachable must not panic — the
	// absence of the ping is what the monitor will (correctly) alert on.
	b := New("http://127.0.0.1:1/nope", time.Second, func() bool { return true }, discard())
	b.beatOnce(context.Background())
}

func TestNewCapsTimeoutAtInterval(t *testing.T) {
	t.Parallel()
	// A beat must not outlast its cadence: timeout capped at the interval.
	if b := New("http://x", 5*time.Second, func() bool { return true }, discard()); b.client.Timeout != 5*time.Second {
		t.Errorf("timeout = %s, want 5s (capped at interval)", b.client.Timeout)
	}
	// …but never longer than the 10s ceiling for a long interval.
	if b := New("http://x", time.Minute, func() bool { return true }, discard()); b.client.Timeout != 10*time.Second {
		t.Errorf("timeout = %s, want 10s ceiling", b.client.Timeout)
	}
}

func TestRunBeatsImmediately(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	// A long interval: if the first beat waited for the ticker, none would
	// arrive within a second. It must beat immediately (restart gap fix).
	b := New(srv.URL, time.Hour, func() bool { return true }, discard())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.Run(ctx)

	deadline := time.After(2 * time.Second)
	for atomic.LoadInt32(&hits) == 0 {
		select {
		case <-deadline:
			t.Fatal("no immediate beat — Run waited a full interval")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestBeatOnceCancelledContextIsQuiet(t *testing.T) {
	t.Parallel()
	// A cancelled context (shutdown racing the ticker) must not log a
	// spurious "monitor unreachable" warning.
	var buf strings.Builder
	log := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	b := New("http://127.0.0.1:1/nope", time.Second, func() bool { return true }, log)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.beatOnce(ctx)
	if strings.Contains(buf.String(), "unreachable") {
		t.Errorf("spurious warn on cancelled context: %q", buf.String())
	}
}

func TestRunPingsThenStopsOnCancel(t *testing.T) {
	t.Parallel()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	b := New(srv.URL, 15*time.Millisecond, func() bool { return true }, discard())
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); b.Run(ctx) }()

	deadline := time.After(5 * time.Second)
	for atomic.LoadInt32(&hits) == 0 {
		select {
		case <-deadline:
			t.Fatal("Run never pinged")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after ctx cancel (goroutine leak)")
	}
}
