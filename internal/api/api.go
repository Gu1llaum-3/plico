// Package api serves the local observability endpoint. /healthz is semantic
// (F35): it reflects the scheduler's real liveness, not just an open port.
// The v1 client CLI will reuse this mux over a unix socket.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"plico/internal/scheduler"
	"plico/internal/state"
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
}

// New builds the HTTP server. Healthy means: the scheduler ticked recently
// (< 2× poll interval) and no run has been in flight longer than runTimeout.
func New(listen string, sched *scheduler.Scheduler, store *state.Store,
	pollInterval, runTimeout time.Duration) *http.Server {

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		snap := sched.Snapshot()
		now := time.Now()

		healthy := !snap.LastTick.IsZero() && now.Sub(snap.LastTick) < 2*pollInterval
		for _, st := range snap.Stacks {
			if st.RunningSince != nil && now.Sub(*st.RunningSince) > runTimeout {
				healthy = false // stuck run
			}
		}

		resp := healthResponse{Status: "ok", LastTick: snap.LastTick, Stacks: map[string]stackHealth{}}
		persisted := store.All()
		for name, live := range snap.Stacks {
			h := stackHealth{RunningSince: live.RunningSince, LastOutcome: live.LastOutcome}
			if p, ok := persisted[name]; ok {
				h.LastDeployedSHA = p.LastDeployedSHA
				h.LastStatus = p.LastStatus
				h.LastRunID = p.LastRunID
				t := p.UpdatedAt
				h.UpdatedAt = &t
			}
			resp.Stacks[name] = h
		}

		w.Header().Set("Content-Type", "application/json")
		if !healthy {
			resp.Status = "degraded"
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	return &http.Server{
		Addr:              listen,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}
}
