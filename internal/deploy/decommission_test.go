package deploy

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Gu1llaum-3/plico/internal/notify"
	"github.com/Gu1llaum-3/plico/internal/state"
)

func gitCalls(h *harness) int {
	n := 0
	for _, c := range h.gitFake.Calls {
		if c.Name == "git" {
			n++
		}
	}
	return n
}

func TestDecommissionStopsStackAndKeepsVolumes(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.deployer.Decommission(context.Background(), h.stack); err != nil {
		t.Fatalf("Decommission: %v", err)
	}
	if len(h.runtime.downs) != 1 {
		t.Fatalf("downs = %d, want 1", len(h.runtime.downs))
	}
	if d := h.runtime.downs[0]; d.Project != "web" || d.ComposeFile != "docker-compose.yml" {
		t.Errorf("down options = %+v", d)
	}
	if len(h.runtime.ups)+len(h.runtime.pulls) != 0 {
		t.Error("decommission must never pull or up")
	}
	st, ok := h.store.Get("web")
	if !ok || !st.Decommissioned {
		t.Fatalf("state = %+v, want tombstone", st)
	}
	if st.LastDeployedSHA != "" {
		t.Errorf("deployed SHA must be forgotten, got %q", st.LastDeployedSHA)
	}
}

func TestDecommissionedStackIsNeverDeployed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.deployer.Decommission(context.Background(), h.stack); err != nil {
		t.Fatal(err)
	}
	before := gitCalls(h)
	// A new revision upstream must not bring the stack back.
	if got := h.deployer.RunStack(context.Background(), h.stack); got != OutcomeSkipped {
		t.Fatalf("RunStack outcome = %s, want skipped", got)
	}
	if len(h.runtime.ups)+len(h.runtime.pulls) != 0 {
		t.Error("a decommissioned stack must not be deployed")
	}
	if gitCalls(h) != before {
		t.Error("a decommissioned stack must not even be fetched")
	}
}

func TestDecommissionedStackIsNeverChecked(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.deployer.Decommission(context.Background(), h.stack); err != nil {
		t.Fatal(err)
	}
	notified := len(h.events.types()) // the decommission's own event is expected
	before := gitCalls(h)
	if got := h.deployer.CheckStack(context.Background(), h.stack); got != OutcomeSkipped {
		t.Errorf("CheckStack outcome = %s, want skipped", got)
	}
	if gitCalls(h) != before {
		t.Error("check must not fetch a decommissioned stack")
	}
	if got := h.deployer.CheckHealth(context.Background(), h.stack); got != OutcomeSkipped {
		t.Errorf("CheckHealth outcome = %s, want skipped", got)
	}
	if got := h.events.types(); len(got) != notified {
		t.Errorf("a decommissioned stack must not notify again, got %v", got[notified:])
	}
	if _, err := h.deployer.DryRun(context.Background(), h.stack); err == nil {
		t.Error("DryRun must refuse a decommissioned stack")
	}
}

func TestDecommissionFailureKeepsTombstone(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.runtime.downErr = os.ErrPermission
	if err := h.deployer.Decommission(context.Background(), h.stack); err == nil {
		t.Fatal("a failed down must be reported")
	}
	st, _ := h.store.Get("web")
	if !st.Decommissioned {
		t.Fatal("tombstone must survive a failed down: containers may still run, a redeploy would be wrong")
	}
	if got := h.deployer.RunStack(context.Background(), h.stack); got != OutcomeSkipped {
		t.Errorf("RunStack after failed decommission = %s, want skipped", got)
	}
}

func TestDecommissionRefusedWhileRunning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	lock := h.deployer.stackLock("web")
	lock.Lock()
	defer lock.Unlock()
	if err := h.deployer.Decommission(context.Background(), h.stack); err == nil {
		t.Fatal("decommission must refuse while a run holds the stack")
	}
	if len(h.runtime.downs) != 0 {
		t.Error("nothing must be stopped while a run is in progress")
	}
	if st, _ := h.store.Get("web"); st.Decommissioned {
		t.Error("state must be untouched when refused")
	}
}

func TestDecommissionNeedsWorktree(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := os.RemoveAll(h.worktree); err != nil {
		t.Fatal(err)
	}
	if err := h.deployer.Decommission(context.Background(), h.stack); err == nil {
		t.Fatal("without a worktree the compose file is unknown: must refuse")
	}
	if len(h.runtime.downs) != 0 {
		t.Error("nothing must be stopped without the worktree")
	}
	if st, ok := h.store.Get("web"); !ok || st.Decommissioned || st.LastDeployedSHA != oldSHA {
		t.Errorf("state must be untouched when refused, got %+v", st)
	}
}

// The tombstone must not erase the schedule anchor: a restart right after a
// decommission must resume the cron state, not behave like a first install.
func TestDecommissionPreservesScheduleAnchor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	anchor := time.Date(2026, 11, 20, 2, 0, 0, 0, time.UTC)
	if err := h.store.Put("web", state.StackState{
		LastDeployedSHA: oldSHA, LastFiring: anchor, ScheduleSpec: "0 2 * * *",
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.deployer.Decommission(context.Background(), h.stack); err != nil {
		t.Fatal(err)
	}
	st, _ := h.store.Get("web")
	if !st.Decommissioned {
		t.Fatal("tombstone missing")
	}
	if !st.LastFiring.Equal(anchor) || st.ScheduleSpec != "0 2 * * *" {
		t.Errorf("schedule anchor erased: LastFiring=%v ScheduleSpec=%q", st.LastFiring, st.ScheduleSpec)
	}
	if st.LastDeployedSHA != "" {
		t.Errorf("deployed SHA must be forgotten, got %q", st.LastDeployedSHA)
	}
}

func TestDecommissionBusyIsSentinel(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	lock := h.deployer.stackLock("web")
	lock.Lock()
	defer lock.Unlock()
	if err := h.deployer.Decommission(context.Background(), h.stack); !errors.Is(err, ErrStackBusy) {
		t.Fatalf("err = %v, want ErrStackBusy", err)
	}
}

func TestDecommissionNoWorktreeIsPrecondition(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := os.RemoveAll(h.worktree); err != nil {
		t.Fatal(err)
	}
	if err := h.deployer.Decommission(context.Background(), h.stack); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("err = %v, want ErrPrecondition", err)
	}
}

// In a monorepo the compose file lives under the stack's path, so the
// precondition must check that directory, not the checkout root: a missing
// subdirectory must refuse before any state is written.
func TestDecommissionChecksStackPathNotCheckoutRoot(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.stack.Path = "services/web" // worktree exists, the subdirectory does not
	if err := h.deployer.Decommission(context.Background(), h.stack); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("err = %v, want ErrPrecondition", err)
	}
	if len(h.runtime.downs) != 0 {
		t.Error("nothing must be stopped")
	}
	if st, _ := h.store.Get("web"); st.Decommissioned {
		t.Error("no tombstone may be written when the compose directory is missing")
	}
}

// Stopping a stack is a destructive operator action: it must notify, like a
// skipped pre-hook does, so the team learns about it without reading logs.
func TestDecommissionNotifies(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.deployer.Decommission(context.Background(), h.stack); err != nil {
		t.Fatal(err)
	}
	ev := h.events.last()
	if ev.Type != notify.StackDecommissioned || ev.Stack != "web" {
		t.Fatalf("event = %+v, want stack_decommissioned for web", ev)
	}
}

// A failed down leaves containers that may still run: that is a failure to
// notify as deploy_failed at stage "decommission", not a silent error.
func TestDecommissionFailureNotifies(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.runtime.downErr = os.ErrPermission
	_ = h.deployer.Decommission(context.Background(), h.stack)
	ev := h.events.last()
	if ev.Type != notify.DeployFailed || ev.Stage != StageDecommission {
		t.Fatalf("event = %+v, want deploy_failed at stage decommission", ev)
	}
}
