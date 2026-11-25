package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Gu1llaum-3/plico/internal/compose"
	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/notify"
	"github.com/Gu1llaum-3/plico/internal/state"
)

// ErrStackBusy is returned when a run already holds the stack's lock.
var ErrStackBusy = errors.New("stack is busy: a run is in progress, retry when it finishes")

// ErrPrecondition marks a refusal caused by the stack's state on disk (no
// worktree, unusable recipe), not by a daemon fault. The API maps it to 422.
var ErrPrecondition = errors.New("precondition failed")

// Decommission stops a stack for good on this host: `docker compose down`
// without volumes, then a tombstone in state so no tick deploys, fetches or
// probes it again. The worktree, the volumes and the secrets are left alone.
//
// The stack must still be in config.toml, because the compose file, project
// and sops wiring come from it. The tombstone is written before `down`, so a
// crash or a failed down never lets the scheduler bring the stack back.
func (d *Deployer) Decommission(ctx context.Context, st config.StackConfig) error {
	lock := d.stackLock(st.Name)
	if !lock.TryLock() {
		return fmt.Errorf("stack %q: %w", st.Name, ErrStackBusy)
	}
	defer lock.Unlock()

	// One deadline for the whole operation, sops decryption included: it runs
	// under the stack lock, so a wedged KMS must not hold the lock forever.
	ctx, cancel := context.WithTimeout(ctx, d.cfg.RunTimeout.Duration)
	defer cancel()

	dir := filepath.Join(d.cfg.BaseDir, st.Name)
	root := stackRoot(dir, st)
	if _, err := os.Stat(root); err != nil {
		return fmt.Errorf("stack %q has no compose directory at %s, so its compose file is unknown: %w", st.Name, root, errors.Join(ErrPrecondition, err))
	}
	rec, err := d.resolveRecipe(st, root)
	if err != nil {
		return fmt.Errorf("stack %q: resolving recipe: %w", st.Name, errors.Join(ErrPrecondition, err))
	}

	runID := newRunID()
	copts := compose.Options{
		Dir: root, ComposeFile: rec.ComposeFile, Project: st.Name,
		ExtraArgs: rec.TopLevelArgs(),
	}
	cleanup, err := d.setupSops(ctx, rec.SopsFiles, rec.SopsMode, st.Name, root, runID, &copts)
	if err != nil {
		return fmt.Errorf("stack %q: sops setup: %w", st.Name, err)
	}
	defer cleanup()

	// Update, not Put: the schedule anchor (LastFiring, ScheduleSpec) must
	// survive, or a restart would treat the stack as a first install.
	if err := d.store.Update(st.Name, func(s *state.StackState) {
		s.Decommissioned = true
		s.LastDeployedSHA = ""
		s.LastStatus = ""
		s.LastRunID = ""
		s.LastFailedSHA = ""
		s.LastFailedStage = ""
		s.LastQueuedSHA = ""
		s.UpdatedAt = time.Now()
	}); err != nil {
		return fmt.Errorf("stack %q: writing tombstone: %w", st.Name, err)
	}

	if err := d.runtime.Down(ctx, copts); err != nil {
		d.log.Error("decommission: compose down failed", "stack", st.Name, "error", err)
		d.notifyDecommission(ctx, st, runID, notify.DeployFailed, StageDecommission, err.Error())
		return fmt.Errorf("stack %q: docker compose down failed (still marked decommissioned, nothing will redeploy it; fix the cause and run decommission again): %w", st.Name, err)
	}
	d.log.Warn("stack decommissioned: containers stopped, volumes and worktree kept", "stack", st.Name)
	d.notifyDecommission(ctx, st, runID, notify.StackDecommissioned, "", "containers stopped, volumes and worktree kept")
	return nil
}

// notifyDecommission emits one event on a detached, bounded context, like the
// deployer's own notifications: a slow channel must not hold the stack lock.
func (d *Deployer) notifyDecommission(ctx context.Context, st config.StackConfig, runID string, t notify.EventType, stage, detail string) {
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()
	_ = d.notifier.Notify(nctx, notify.Event{
		Type: t, Stack: st.Name, RunID: runID, Ref: st.Ref,
		Stage: stage, Detail: detail, Time: time.Now(),
	})
}
