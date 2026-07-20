// Package heartbeat pushes a periodic liveness beat to an external monitor
// (Uptime Kuma push, Healthchecks.io, …) as a dead-man's switch: it pings
// while the daemon is healthy and stays SILENT otherwise. The absence of a
// beat is what the monitor alerts on — which also covers the daemon being
// dead (a dead process cannot send a "down"). It is the outbound mirror of
// /healthz, so it works even when the monitor cannot reach the host.
package heartbeat

import (
	"context"
	"log/slog"
	"net/http"
	"time"
)

// Beater pings url every interval while healthy() reports true.
type Beater struct {
	url      string
	interval time.Duration
	healthy  func() bool
	client   *http.Client
	log      *slog.Logger
}

// New builds a Beater. healthy is injected (the daemon supplies a closure
// over its health state), so this package depends on nothing plico-specific.
func New(url string, interval time.Duration, healthy func() bool, log *slog.Logger) *Beater {
	// A beat must never outlast its own cadence, or a slow monitor would
	// make ticks coalesce and stretch the effective interval past the
	// monitor's grace. Cap the per-beat timeout at the interval (≤ 10s).
	timeout := 10 * time.Second
	if interval < timeout {
		timeout = interval
	}
	return &Beater{
		url:      url,
		interval: interval,
		healthy:  healthy,
		client:   &http.Client{Timeout: timeout},
		log:      log,
	}
}

// Run beats immediately, then on a ticker until ctx is cancelled. The
// immediate first beat matters on restart (config changes apply via
// systemctl restart): waiting a full interval would leave a beat gap that
// can trip a tight monitor grace into a spurious "down". The interval is
// decoupled from the poll loop so a death is detected quickly even with a
// long poll_interval.
func (b *Beater) Run(ctx context.Context) {
	b.log.Info("heartbeat enabled", "interval", b.interval.String())
	b.beatOnce(ctx)
	ticker := time.NewTicker(b.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.beatOnce(ctx)
		}
	}
}

// beatOnce pings the monitor iff the daemon is currently healthy. A send
// failure (monitor unreachable, 5xx) is logged and swallowed — never fatal,
// and NOT treated as unhealthy: a healthy daemon that cannot reach its
// monitor must keep trying (the monitor's own absence-detection handles the
// network fault). Isolated for deterministic tests.
func (b *Beater) beatOnce(ctx context.Context) {
	if !b.healthy() {
		return // dead-man's switch: stay silent while degraded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.url, nil)
	if err != nil {
		b.log.Error("heartbeat request build failed", "error", err)
		return
	}
	resp, err := b.client.Do(req)
	if err != nil {
		// A cancelled context is a clean shutdown racing the ticker, not a
		// network fault — don't cry "monitor unreachable" on a tidy stop.
		if ctx.Err() == nil {
			b.log.Warn("heartbeat send failed (monitor unreachable?)", "error", err)
		}
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		b.log.Warn("heartbeat unexpected status", "status", resp.Status)
	}
}
