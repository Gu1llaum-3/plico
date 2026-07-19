package deploy

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Gu1llaum-3/plico/internal/compose"
	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/execx"
	"github.com/Gu1llaum-3/plico/internal/gitrepo"
	"github.com/Gu1llaum-3/plico/internal/hooks"
	"github.com/Gu1llaum-3/plico/internal/notify"
	"github.com/Gu1llaum-3/plico/internal/state"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// fakeRuntime implements compose.Runtime and records every call.
type fakeRuntime struct {
	mu       sync.Mutex
	pullErr  error
	upErr    error
	services []compose.Service
	psErr    error
	pulls    []compose.Options
	ups      []compose.Options
}

func (f *fakeRuntime) Pull(_ context.Context, o compose.Options) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulls = append(f.pulls, o)
	return f.pullErr
}

func (f *fakeRuntime) Up(_ context.Context, o compose.Options) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ups = append(f.ups, o)
	return f.upErr
}

func (f *fakeRuntime) PS(context.Context, compose.Options) ([]compose.Service, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.services, f.psErr
}

// eventRecorder implements notify.Notifier and records whether the context
// it was called with was still alive (a dead ctx = undeliverable in real life).
type eventRecorder struct {
	mu      sync.Mutex
	events  []notify.Event
	ctxErrs []error
}

func (r *eventRecorder) Notify(ctx context.Context, ev notify.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, ev)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	return nil
}

func (r *eventRecorder) types() []notify.EventType {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]notify.EventType, len(r.events))
	for i, e := range r.events {
		out[i] = e.Type
	}
	return out
}

func (r *eventRecorder) last() notify.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.events[len(r.events)-1]
}

type harness struct {
	deployer *Deployer
	cfg      *config.Config
	stack    config.StackConfig
	runtime  *fakeRuntime
	events   *eventRecorder
	store    *state.Store
	worktree string
	gitFake  *execx.FakeRunner
}

const (
	oldSHA = "1111111111111111111111111111111111111111"
	newSHA = "2222222222222222222222222222222222222222"
)

// newHarness wires a Deployer with a fake git CLI (fetch/rev-parse/checkout
// scripted), a fake compose runtime, real hook execution, a real state store
// and a recording notifier. The worktree pre-exists (clone already done).
func newHarness(t *testing.T) *harness {
	t.Helper()
	base := t.TempDir()
	worktree := filepath.Join(base, "web")
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		BaseDir:              base,
		PollInterval:         config.Duration{Duration: time.Minute},
		RunTimeout:           config.Duration{Duration: time.Minute},
		MaxConcurrentDeploys: 2,
	}
	cfg.Stacks = []config.StackConfig{{
		Name: "web", Repo: "https://example.com/r.git", Ref: "main",
		ComposeFile: "docker-compose.yml", SopsMode: "exec-env",
		HookTimeout:   config.Duration{Duration: 30 * time.Second},
		VerifyTimeout: config.Duration{Duration: 30 * time.Second},
	}}

	gitFake := &execx.FakeRunner{Match: func(c execx.Cmd) (execx.Result, error) {
		if c.Name != "git" {
			return execx.Result{}, errors.New("harness: only git goes through the fake runner")
		}
		switch c.Args[0] {
		case "fetch", "checkout":
			return execx.Result{}, nil
		case "rev-parse":
			return execx.Result{Stdout: []byte(newSHA + "\n")}, nil
		case "log":
			return execx.Result{Stdout: []byte("2222222 add feature\n")}, nil
		}
		return execx.Result{}, errors.New("harness: unexpected git subcommand " + c.Args[0])
	}}

	store, err := state.Open(filepath.Join(base, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("web", state.StackState{LastDeployedSHA: oldSHA, LastStatus: state.StatusSuccess}); err != nil {
		t.Fatal(err)
	}

	rt := &fakeRuntime{services: []compose.Service{{Name: "nginx", State: "running", Health: "healthy"}}}
	events := &eventRecorder{}
	realRunner := execx.NewRunner(discard())

	d := New(cfg,
		gitrepo.New(gitFake, nil, discard()),
		rt,
		hooks.New(realRunner, discard()),
		events,
		store,
		gitFake, // sops tmpfs path unused in these tests
		discard(),
	)
	return &harness{deployer: d, cfg: cfg, stack: cfg.Stacks[0], runtime: rt, events: events, store: store, worktree: worktree, gitFake: gitFake}
}

func (h *harness) writePreHook(t *testing.T, body string) {
	t.Helper()
	path := filepath.Join(h.worktree, hooks.RepoPreDeploy)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func wantEvents(t *testing.T, got []notify.EventType, want ...notify.EventType) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events = %v\nwant     %v", got, want)
	}
}

func TestUpToDateIsSilentNoOp(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.store.Put("web", state.StackState{LastDeployedSHA: newSHA}); err != nil {
		t.Fatal(err)
	}
	outcome := h.deployer.RunStack(context.Background(), h.stack)
	if outcome != OutcomeUpToDate {
		t.Fatalf("outcome = %s, want up_to_date", outcome)
	}
	if len(h.events.types()) != 0 {
		t.Errorf("no-op must not notify, got %v", h.events.types())
	}
	if len(h.runtime.pulls)+len(h.runtime.ups) != 0 {
		t.Error("no-op must not touch compose")
	}
}

func TestSuccessfulDeployWithRepoHook(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 0")

	outcome := h.deployer.RunStack(context.Background(), h.stack)
	if outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s, want deployed", outcome)
	}
	wantEvents(t, h.events.types(),
		notify.DeployQueued, notify.DeployStart, notify.DeploySuccess)
	if len(h.runtime.pulls) != 1 || len(h.runtime.ups) != 1 {
		t.Errorf("pull/up calls = %d/%d, want 1/1", len(h.runtime.pulls), len(h.runtime.ups))
	}
	st, _ := h.store.Get("web")
	if st.LastDeployedSHA != newSHA || st.LastStatus != state.StatusSuccess {
		t.Errorf("state = %+v", st)
	}
}

func TestNoHookAnywhereEmitsSkippedAndContinues(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	outcome := h.deployer.RunStack(context.Background(), h.stack)
	if outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s, want deployed", outcome)
	}
	wantEvents(t, h.events.types(),
		notify.DeployQueued, notify.DeployStart, notify.PreHookSkipped, notify.DeploySuccess)
}

func TestPreHookFailureBlocksDeployment(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "echo 'pg_dump: connection refused' >&2\nexit 1")

	outcome := h.deployer.RunStack(context.Background(), h.stack)
	if outcome != OutcomeFailed {
		t.Fatalf("outcome = %s, want failed", outcome)
	}
	wantEvents(t, h.events.types(),
		notify.DeployQueued, notify.DeployStart, notify.PreHookFailed)
	if len(h.runtime.pulls)+len(h.runtime.ups) != 0 {
		t.Fatal("F12 violated: compose was touched after a failed backup gate")
	}
	ev := h.events.last()
	if ev.Stage != StagePreHook {
		t.Errorf("stage = %q", ev.Stage)
	}
	if want := "pg_dump: connection refused"; !contains(ev.Detail, want) {
		t.Errorf("F14: hook stderr missing from notification detail: %q", ev.Detail)
	}
	st, _ := h.store.Get("web")
	if st.LastDeployedSHA != oldSHA || st.LastStatus != state.StatusPreHookFailed {
		t.Errorf("state must keep old SHA for natural retry, got %+v", st)
	}
}

func TestPullFailureLeavesStackUntouched(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 0")
	h.runtime.pullErr = errors.New("manifest unknown: tag v9.9.9 not found")

	outcome := h.deployer.RunStack(context.Background(), h.stack)
	if outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", outcome)
	}
	if len(h.runtime.ups) != 0 {
		t.Fatal("F18 violated: up was called after a failed pull")
	}
	ev := h.events.last()
	if ev.Type != notify.DeployFailed || ev.Stage != StagePull {
		t.Errorf("last event = %s/%s", ev.Type, ev.Stage)
	}
	st, _ := h.store.Get("web")
	if st.LastDeployedSHA != oldSHA {
		t.Errorf("SHA must stay old after pull failure, got %q", st.LastDeployedSHA)
	}
}

func TestUpFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 0")
	h.runtime.upErr = errors.New("network plico_default not found")

	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", outcome)
	}
	ev := h.events.last()
	if ev.Type != notify.DeployFailed || ev.Stage != StageUp {
		t.Errorf("last event = %s/%s", ev.Type, ev.Stage)
	}
}

func TestVerifyFailureRecordsNewSHA(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 0")
	h.runtime.services = []compose.Service{{Name: "api", State: "running", Health: "unhealthy"}}

	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", outcome)
	}
	ev := h.events.last()
	if ev.Type != notify.DeployFailed || ev.Stage != StageVerify {
		t.Errorf("last event = %s/%s", ev.Type, ev.Stage)
	}
	if !contains(ev.Detail, "api") {
		t.Errorf("failing service not named in detail: %q", ev.Detail)
	}
	// Deliberate: record the new SHA so the same broken revision is not
	// redeployed in a loop.
	st, _ := h.store.Get("web")
	if st.LastDeployedSHA != newSHA || st.LastStatus != state.StatusFailed {
		t.Errorf("state = %+v", st)
	}
}

func TestSopsFileMissingFailsBeforePull(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 0")
	h.stack.SopsFiles = []string{".deploy/secrets.enc.env"} // not created in worktree

	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeFailed {
		t.Fatal("want failure on missing sops file")
	}
	ev := h.events.last()
	if ev.Type != notify.DeployFailed || ev.Stage != StageSops {
		t.Errorf("last event = %s/%s", ev.Type, ev.Stage)
	}
	if len(h.runtime.pulls) != 0 {
		t.Error("pull must not run when sops setup failed")
	}
}

func TestSopsExecEnvPrefixReachesCompose(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 0")
	secret := filepath.Join(h.worktree, ".deploy", "secrets.enc.env")
	if err := os.WriteFile(secret, []byte("ENC"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.stack.SopsFiles = []string{".deploy/secrets.enc.env"}

	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s", outcome)
	}
	for _, opts := range [][]compose.Options{h.runtime.pulls, h.runtime.ups} {
		if len(opts) != 1 || opts[0].Wrap == nil {
			t.Fatal("pull and up must receive the sops wrap")
		}
		got := opts[0].Wrap([]string{"docker", "compose", "up", "-d"})
		want := []string{"sops", "exec-env", ".deploy/secrets.enc.env", "'docker' 'compose' 'up' '-d'"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("wrapped argv = %q, want %q", got, want)
		}
	}
}

func TestForcePullDisabledSkipsPull(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 0")
	f := false
	h.stack.ForcePull = &f

	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s", outcome)
	}
	if len(h.runtime.pulls) != 0 {
		t.Error("pull should be skipped when force_pull = false")
	}
	if len(h.runtime.ups) != 1 {
		t.Error("up should still run")
	}
}

func TestConcurrentRunIsSkipped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "sleep 1")

	done := make(chan Outcome, 1)
	go func() { done <- h.deployer.RunStack(context.Background(), h.stack) }()
	time.Sleep(300 * time.Millisecond) // first run is inside its pre-hook

	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeSkipped {
		t.Errorf("second run outcome = %s, want skipped (F37)", outcome)
	}
	if first := <-done; first != OutcomeDeployed {
		t.Errorf("first run outcome = %s, want deployed", first)
	}
}

func TestRepeatedPreHookFailureNotifiesOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 1")

	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeFailed {
		t.Fatalf("first run outcome = %s", outcome)
	}
	first := len(h.events.types())
	if first == 0 {
		t.Fatal("first failure must notify")
	}
	st, _ := h.store.Get("web")
	if st.LastFailedSHA != newSHA || st.LastFailedStage != StagePreHook {
		t.Fatalf("failure not recorded for dedup: %+v", st)
	}

	// Next poll ticks retry the same revision: no new notifications.
	for i := 0; i < 3; i++ {
		if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeFailed {
			t.Fatalf("retry outcome = %s", outcome)
		}
	}
	if got := len(h.events.types()); got != first {
		t.Errorf("retries re-notified: %d events after retries, %d after first failure\n%v",
			got, first, h.events.types())
	}
}

func TestFailureAtDifferentStageNotifiesAgain(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 1")
	if h.deployer.RunStack(context.Background(), h.stack) != OutcomeFailed {
		t.Fatal("first run should fail at pre_hook")
	}
	// The hook is fixed but now the pull fails: this is new information.
	h.writePreHook(t, "exit 0")
	h.runtime.pullErr = errors.New("registry down")
	if h.deployer.RunStack(context.Background(), h.stack) != OutcomeFailed {
		t.Fatal("second run should fail at pull")
	}
	ev := h.events.last()
	if ev.Type != notify.DeployFailed || ev.Stage != StagePull {
		t.Errorf("stage change must re-notify, last = %s/%s", ev.Type, ev.Stage)
	}
}

func TestRecoveryAfterFailureNotifiesSuccess(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 1")
	if h.deployer.RunStack(context.Background(), h.stack) != OutcomeFailed {
		t.Fatal("first run should fail")
	}
	h.writePreHook(t, "exit 0")
	if h.deployer.RunStack(context.Background(), h.stack) != OutcomeDeployed {
		t.Fatal("second run should deploy")
	}
	ev := h.events.last()
	if ev.Type != notify.DeploySuccess {
		t.Errorf("last event = %s, want deploy_success", ev.Type)
	}
	st, _ := h.store.Get("web")
	if st.LastFailedSHA != "" || st.LastFailedStage != "" {
		t.Errorf("success must clear failure-dedup fields: %+v", st)
	}
}

func TestQueueSlotTimeoutNotifiesAndPersists(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 0")
	h.cfg.RunTimeout = config.Duration{Duration: 300 * time.Millisecond}
	// Saturate the deployment semaphore (cap = MaxConcurrentDeploys = 2).
	h.deployer.sem <- struct{}{}
	h.deployer.sem <- struct{}{}

	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", outcome)
	}
	ev := h.events.last()
	if ev.Type != notify.DeployFailed || ev.Stage != StageQueue {
		t.Errorf("last event = %s/%s, want deploy_failed/queue_wait", ev.Type, ev.Stage)
	}
	st, _ := h.store.Get("web")
	if st.LastDeployedSHA != oldSHA || st.LastStatus != state.StatusFailed || st.LastFailedStage != StageQueue {
		t.Errorf("state = %+v", st)
	}
}

func TestFailureNotificationSurvivesExpiredRunContext(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "sleep 30")
	h.cfg.RunTimeout = config.Duration{Duration: 300 * time.Millisecond}

	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeFailed {
		t.Fatalf("outcome = %s", outcome)
	}
	ev := h.events.last()
	if ev.Type != notify.PreHookFailed {
		t.Fatalf("last event = %s", ev.Type)
	}
	h.events.mu.Lock()
	lastCtxErr := h.events.ctxErrs[len(h.events.ctxErrs)-1]
	h.events.mu.Unlock()
	if lastCtxErr != nil {
		t.Errorf("failure notification was sent on a dead context (%v): it would never be delivered", lastCtxErr)
	}
}

func TestCheckStackQueuesOncePerRevision(t *testing.T) {
	t.Parallel()
	h := newHarness(t)

	// First check: announce the pending revision, touch nothing.
	if outcome := h.deployer.CheckStack(context.Background(), h.stack); outcome != OutcomeQueued {
		t.Fatalf("outcome = %s, want queued", outcome)
	}
	wantEvents(t, h.events.types(), notify.DeployQueued)
	if len(h.runtime.pulls)+len(h.runtime.ups) != 0 {
		t.Fatal("a check must never touch compose")
	}
	st, _ := h.store.Get("web")
	if st.LastQueuedSHA != newSHA {
		t.Errorf("LastQueuedSHA = %q, want %q", st.LastQueuedSHA, newSHA)
	}
	if st.LastDeployedSHA != oldSHA {
		t.Errorf("a check must not move the deployed SHA, got %q", st.LastDeployedSHA)
	}

	// Subsequent checks on the same revision: silent.
	for i := 0; i < 3; i++ {
		if outcome := h.deployer.CheckStack(context.Background(), h.stack); outcome != OutcomeQueued {
			t.Fatalf("repeat check outcome = %s", outcome)
		}
	}
	wantEvents(t, h.events.types(), notify.DeployQueued)
}

func TestApplyAfterCheckDoesNotReannounceQueued(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 0")

	if h.deployer.CheckStack(context.Background(), h.stack) != OutcomeQueued {
		t.Fatal("check should queue")
	}
	// The window opens and applies: no second deploy_queued.
	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s", outcome)
	}
	wantEvents(t, h.events.types(),
		notify.DeployQueued, notify.DeployStart, notify.DeploySuccess)
	// Success clears the queued marker for the next cycle.
	st, _ := h.store.Get("web")
	if st.LastQueuedSHA != "" {
		t.Errorf("LastQueuedSHA not cleared on success: %q", st.LastQueuedSHA)
	}
}

func TestCheckStackUpToDateIsSilent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.store.Put("web", state.StackState{LastDeployedSHA: newSHA}); err != nil {
		t.Fatal(err)
	}
	if outcome := h.deployer.CheckStack(context.Background(), h.stack); outcome != OutcomeUpToDate {
		t.Fatalf("outcome = %s", outcome)
	}
	if len(h.events.types()) != 0 {
		t.Errorf("up-to-date check must not notify, got %v", h.events.types())
	}
}

func TestCheckStackClearsObsoleteQueuedRevision(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	if err := h.store.Put("web", state.StackState{
		LastDeployedSHA: newSHA,
		LastQueuedSHA:   oldSHA,
	}); err != nil {
		t.Fatal(err)
	}
	if outcome := h.deployer.CheckStack(context.Background(), h.stack); outcome != OutcomeUpToDate {
		t.Fatalf("outcome = %s", outcome)
	}
	st, _ := h.store.Get("web")
	if st.LastQueuedSHA != "" {
		t.Errorf("obsolete queued SHA was not cleared: %q", st.LastQueuedSHA)
	}
}

func TestForceRedeploysSameRevision(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 0")
	if err := h.store.Put("web", state.StackState{LastDeployedSHA: newSHA}); err != nil {
		t.Fatal(err)
	}
	// Without force: no-op. With force: full pipeline on the same SHA.
	if outcome := h.deployer.RunStack(context.Background(), h.stack); outcome != OutcomeUpToDate {
		t.Fatalf("outcome = %s, want up_to_date", outcome)
	}
	if outcome := h.deployer.RunStackWith(context.Background(), h.stack, RunOptions{Force: true}); outcome != OutcomeDeployed {
		t.Fatalf("forced outcome = %s, want deployed", outcome)
	}
	if len(h.runtime.ups) != 1 {
		t.Errorf("forced run must deploy, ups = %d", len(h.runtime.ups))
	}
	// Redeploying the CURRENT revision has nothing "queued": no misleading
	// deploy_queued announcement.
	for _, tpe := range h.events.types() {
		if tpe == notify.DeployQueued {
			t.Error("forced same-revision redeploy must not announce deploy_queued")
		}
	}
}

func TestSkipPreBypassesGateLoudly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.writePreHook(t, "exit 1") // the gate would normally block

	outcome := h.deployer.RunStackWith(context.Background(), h.stack, RunOptions{SkipPre: true})
	if outcome != OutcomeDeployed {
		t.Fatalf("outcome = %s, want deployed (gate manually skipped)", outcome)
	}
	// F30: the skip must be announced.
	found := false
	h.events.mu.Lock()
	for _, ev := range h.events.events {
		if ev.Type == notify.PreHookSkipped && strings.Contains(ev.Detail, "manually skipped") {
			found = true
		}
	}
	h.events.mu.Unlock()
	if !found {
		t.Error("pre_hook_skipped notification missing for a manual skip")
	}
}

func TestDryRunReportsDeltaWithoutActing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	report, err := h.deployer.DryRun(context.Background(), h.stack)
	if err != nil {
		t.Fatal(err)
	}
	if report.UpToDate || report.OldSHA != oldSHA || report.NewSHA != newSHA {
		t.Errorf("report = %+v", report)
	}
	if len(h.runtime.pulls)+len(h.runtime.ups) != 0 {
		t.Fatal("dry-run must not touch compose")
	}
	if len(h.events.types()) != 0 {
		t.Errorf("dry-run must not notify, got %v", h.events.types())
	}
	st, _ := h.store.Get("web")
	if st.LastDeployedSHA != oldSHA {
		t.Errorf("dry-run must not move state, got %q", st.LastDeployedSHA)
	}
}

func TestDryRunFailsWhenPendingCommitsCannotBeListed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	original := h.gitFake.Match
	h.gitFake.Match = func(c execx.Cmd) (execx.Result, error) {
		if c.Name == "git" && c.Args[0] == "log" {
			return execx.Result{}, errors.New("old revision is unavailable")
		}
		return original(c)
	}
	if _, err := h.deployer.DryRun(context.Background(), h.stack); err == nil || !strings.Contains(err.Error(), "listing pending commits") {
		t.Fatalf("DryRun error = %v", err)
	}
}

func TestGitSyncFailedAlertsOncePerOutage(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	worktree := filepath.Join(base, "web")
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		BaseDir:              base,
		PollInterval:         config.Duration{Duration: time.Minute},
		RunTimeout:           config.Duration{Duration: time.Minute},
		MaxConcurrentDeploys: 2, // GitSyncAlertAfter unset -> threshold 5
	}
	cfg.Stacks = []config.StackConfig{{Name: "web", Repo: "https://example.com/r.git", Ref: "main"}}

	gitDown := true
	gitFake := &execx.FakeRunner{Match: func(c execx.Cmd) (execx.Result, error) {
		if gitDown {
			return execx.Result{ExitCode: 128}, errors.New("fatal: could not resolve host")
		}
		switch c.Args[0] {
		case "fetch", "checkout":
			return execx.Result{}, nil
		case "rev-parse":
			return execx.Result{Stdout: []byte(oldSHA + "\n")}, nil
		}
		return execx.Result{}, errors.New("unexpected " + c.Args[0])
	}}
	store, err := state.Open(filepath.Join(base, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Put("web", state.StackState{LastDeployedSHA: oldSHA}); err != nil {
		t.Fatal(err)
	}
	events := &eventRecorder{}
	d := New(cfg, gitrepo.New(gitFake, nil, discard()), &fakeRuntime{},
		hooks.New(execx.NewRunner(discard()), discard()), events, store, gitFake, discard())

	// 7 consecutive failures: exactly one alert, at the 5th.
	for i := 0; i < 7; i++ {
		if outcome := d.RunStack(context.Background(), cfg.Stacks[0]); outcome != OutcomeFailed {
			t.Fatalf("run %d outcome = %s", i, outcome)
		}
	}
	types := events.types()
	count := 0
	for _, tpe := range types {
		if tpe == notify.GitSyncFailed {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("git_sync_failed sent %d times, want exactly 1: %v", count, types)
	}
	// Recovery resets the counter: a new outage alerts again.
	gitDown = false
	if outcome := d.RunStack(context.Background(), cfg.Stacks[0]); outcome != OutcomeUpToDate {
		t.Fatalf("recovery outcome = %s", outcome)
	}
	gitDown = true
	for i := 0; i < 5; i++ {
		_ = d.RunStack(context.Background(), cfg.Stacks[0])
	}
	count = 0
	for _, tpe := range events.types() {
		if tpe == notify.GitSyncFailed {
			count++
		}
	}
	if count != 2 {
		t.Errorf("a new outage after recovery must alert again, got %d alerts", count)
	}

	// A successful manual dry-run sync also resets the counter: without it,
	// 4 old failures + 1 new one would fire a premature, wrong alert.
	gitDown = true
	for i := 0; i < 4; i++ {
		_ = d.RunStack(context.Background(), cfg.Stacks[0])
	}
	gitDown = false
	if _, err := d.DryRun(context.Background(), cfg.Stacks[0]); err != nil {
		t.Fatalf("dry-run should succeed: %v", err)
	}
	gitDown = true
	for i := 0; i < 4; i++ {
		_ = d.RunStack(context.Background(), cfg.Stacks[0])
	}
	count = 0
	for _, tpe := range events.types() {
		if tpe == notify.GitSyncFailed {
			count++
		}
	}
	if count != 2 {
		t.Errorf("dry-run success must reset the counter (no alert at 4 post-reset failures), got %d alerts", count)
	}
}

func TestAssess(t *testing.T) {
	t.Parallel()
	services := []compose.Service{
		{Name: "ok", State: "running", Health: "healthy"},
		{Name: "nohc", State: "running", Health: ""},
		{Name: "oneshot", State: "exited", ExitCode: 0},
		{Name: "crash", State: "exited", ExitCode: 137},
		{Name: "sick", State: "running", Health: "unhealthy"},
		{Name: "boot", State: "running", Health: "starting"},
		{Name: "creating", State: "created", Health: ""},
	}
	bad, pending := assess(services)
	if len(bad) != 2 {
		t.Errorf("bad = %v, want crash+sick", bad)
	}
	if len(pending) != 2 {
		t.Errorf("pending = %v, want boot+creating", pending)
	}
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }
