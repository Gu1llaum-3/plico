// Package api serves plico's local interfaces: the semantic /healthz
// endpoint (F35) over local HTTP, and the client CLI API over a unix socket
// (F24, see socket.go). Both share the same status builder.
package api

import (
	"net/http"
	"time"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/scheduler"
	"github.com/Gu1llaum-3/plico/internal/state"
)

type healthResponse struct {
	Status   string                 `json:"status"`
	LastTick time.Time              `json:"last_tick"`
	Stacks   map[string]stackHealth `json:"stacks"`
}

type stackHealth struct {
	LastDeployedSHA string     `json:"last_deployed_sha,omitempty"`
	LastStatus      string     `json:"last_status,omitempty"`
	LastRunID       string     `json:"last_run_id,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	RunningSince    *time.Time `json:"running_since,omitempty"`
	LastOutcome     string     `json:"last_outcome,omitempty"`
	NextRun         *time.Time `json:"next_run,omitempty"`
	QueuedSHA       string     `json:"queued_sha,omitempty"` // pending revision announced by a check (F6)
}

// buildStatus assembles the shared health/status view. Healthy means: the
// scheduler ticked recently (< 2× poll interval) and no run has been in
// flight longer than runTimeout.
func buildStatus(sched *scheduler.Scheduler, store *state.Store,
	pollInterval, runTimeout time.Duration) healthResponse {

	snap := sched.Snapshot()
	now := time.Now()

	healthy := !snap.LastTick.IsZero() && now.Sub(snap.LastTick) < 2*pollInterval
	for _, st := range snap.Stacks {
		if st.RunningSince != nil && now.Sub(*st.RunningSince) > runTimeout {
			healthy = false // stuck run
		}
	}

	resp := healthResponse{Status: "ok", LastTick: snap.LastTick, Stacks: map[string]stackHealth{}}
	if !healthy {
		resp.Status = "degraded"
	}
	persisted := store.All()
	for name, live := range snap.Stacks {
		h := stackHealth{RunningSince: live.RunningSince, LastOutcome: live.LastOutcome, NextRun: live.NextRun}
		if p, ok := persisted[name]; ok {
			h.LastDeployedSHA = p.LastDeployedSHA
			h.LastStatus = p.LastStatus
			h.LastRunID = p.LastRunID
			h.QueuedSHA = p.LastQueuedSHA
			t := p.UpdatedAt
			h.UpdatedAt = &t
		}
		resp.Stacks[name] = h
	}
	return resp
}

// New builds the /healthz HTTP server (local listen address, F35).
func New(listen string, sched *scheduler.Scheduler, store *state.Store,
	pollInterval, runTimeout time.Duration) *http.Server {

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		resp := buildStatus(sched, store, pollInterval, runTimeout)
		code := http.StatusOK
		if resp.Status != "ok" {
			code = http.StatusServiceUnavailable
		}
		writeJSON(w, code, resp)
	})

	return &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
}

// statusFromConfig adapts buildStatus for the socket API.
func statusFromConfig(sched *scheduler.Scheduler, store *state.Store, cfg *config.Config) healthResponse {
	return buildStatus(sched, store, cfg.PollInterval.Duration, cfg.RunTimeout.Duration)
}
