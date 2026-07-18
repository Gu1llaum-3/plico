// Package state persists the minimal per-stack deployment state as a flat
// JSON file. Git remains the source of truth for the deployed content; this
// file only feeds `status`, /healthz and the SHA diff after a restart. (F38)
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Statuses recorded per stack.
const (
	StatusSuccess       = "success"
	StatusFailed        = "failed"
	StatusPreHookFailed = "pre_hook_failed"
)

type StackState struct {
	LastDeployedSHA string    `json:"last_deployed_sha"`
	LastStatus      string    `json:"last_status"`
	LastRunID       string    `json:"last_run_id"`
	UpdatedAt       time.Time `json:"updated_at"`
	// LastFailedSHA/Stage dedupe notifications: retrying the same revision
	// that already failed at the same stage is logged but not re-notified.
	LastFailedSHA   string `json:"last_failed_sha,omitempty"`
	LastFailedStage string `json:"last_failed_stage,omitempty"`
	// LastFiring is the schedule anchor: the last cron firing fully
	// accounted for (attempted or declared missed), or the startup instant
	// on first install. It survives restarts so a still-open window is
	// re-opened instead of silently dropped, and a fully missed one is
	// reported instead of replayed. ScheduleSpec records the cron
	// expression the anchor was computed under: when the schedule changes,
	// the anchor is reset instead of synthesizing phantom past firings.
	LastFiring   time.Time `json:"last_firing,omitzero"`
	ScheduleSpec string    `json:"schedule_spec,omitempty"`
	// LastQueuedSHA dedupes the out-of-window "deployment queued"
	// notification (F6): one announcement per pending revision.
	LastQueuedSHA string `json:"last_queued_sha,omitempty"`
}

// Store is a concurrency-safe view of the state file.
type Store struct {
	path string
	mu   sync.Mutex
	data map[string]StackState
}

// Open loads the state file at path, tolerating its absence (fresh install).
func Open(path string) (*Store, error) {
	s := &Store{path: path, data: map[string]StackState{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, fmt.Errorf("%s: corrupted state file: %w", path, err)
	}
	return s, nil
}

func (s *Store) Get(stack string) (StackState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.data[stack]
	return st, ok
}

// All returns a copy of the whole state map (for /healthz and status).
func (s *Store) All() map[string]StackState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]StackState, len(s.data))
	for k, v := range s.data {
		out[k] = v
	}
	return out
}

// Put replaces one stack's state and rewrites the file atomically.
func (s *Store) Put(stack string, st StackState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[stack] = st
	return s.writeLocked()
}

// Update mutates one stack's state under the store lock. Writers owning
// different fields (the deployer and the scheduler's schedule anchor) must
// use it so they never clobber each other's fields with a stale full Put.
func (s *Store) Update(stack string, mutate func(*StackState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.data[stack]
	mutate(&st)
	s.data[stack] = st
	return s.writeLocked()
}

// writeLocked rewrites the whole file atomically (temp file in the same
// directory + fsync + rename). Callers must hold s.mu.
func (s *Store) writeLocked() error {
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".state-*.tmp")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // no-op after successful rename
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.path)
}
