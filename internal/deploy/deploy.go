// Package deploy implements the per-stack deployment pipeline:
//
//	lock → git sync → SHA diff → checkout → pre-hook gate → sops → pull →
//	up → verify → post-hook → state save
//
// Every failure maps to one notification event (see the table in the design
// doc). The pipeline is idempotent and self-healing: the SHA diff compares
// against the persisted state, so an aborted run is retried on the next tick.
package deploy

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"plico/internal/compose"
	"plico/internal/config"
	"plico/internal/execx"
	"plico/internal/gitrepo"
	"plico/internal/hooks"
	"plico/internal/notify"
	"plico/internal/sopsx"
	"plico/internal/state"
)

// Outcome of one RunStack call.
type Outcome int

const (
	OutcomeSkipped  Outcome = iota // previous run still in progress (F37)
	OutcomeUpToDate                // no git delta, nothing done
	OutcomeDeployed
	OutcomeFailed
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSkipped:
		return "skipped"
	case OutcomeUpToDate:
		return "up_to_date"
	case OutcomeDeployed:
		return "deployed"
	default:
		return "failed"
	}
}

// Pipeline stages, used in logs and notifications.
const (
	StageGitSync   = "git_sync"
	StageQueue     = "queue_wait"
	StageCheckout  = "checkout"
	StagePreHook   = "pre_hook"
	StageSops      = "sops"
	StagePull      = "pull"
	StageUp        = "up"
	StageVerify    = "verify"
	StagePostHook  = "post_hook"
	StageStateSave = "state_save"
)

const (
	verifyPollInterval = 5 * time.Second
	// notifyTimeout bounds each notification send. Notifications run on a
	// context detached from the run: a deploy that died on run_timeout must
	// still be able to deliver its failure alert.
	notifyTimeout = 30 * time.Second
)

type Deployer struct {
	cfg      *config.Config
	git      *gitrepo.Client
	runtime  compose.Runtime
	hooks    *hooks.Runner
	notifier notify.Notifier
	store    *state.Store
	log      *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	sem   chan struct{} // bounds concurrent deployments across stacks

	// TmpfsRoot is overridable in tests; defaults to /dev/shm on Linux.
	TmpfsRoot string

	runner execx.Runner // for sops tmpfs decryption
}

func New(cfg *config.Config, git *gitrepo.Client, rt compose.Runtime, hk *hooks.Runner,
	n notify.Notifier, st *state.Store, r execx.Runner, log *slog.Logger) *Deployer {
	return &Deployer{
		cfg:       cfg,
		git:       git,
		runtime:   rt,
		hooks:     hk,
		notifier:  n,
		store:     st,
		runner:    r,
		log:       log,
		locks:     map[string]*sync.Mutex{},
		sem:       make(chan struct{}, cfg.MaxConcurrentDeploys),
		TmpfsRoot: sopsx.DefaultTmpfsRoot,
	}
}

// RunStack executes one full cycle for a stack. It never panics; every error
// is logged and notified before returning.
func (d *Deployer) RunStack(ctx context.Context, st config.StackConfig) Outcome {
	lock := d.stackLock(st.Name)
	if !lock.TryLock() {
		d.log.Warn("skip_running: previous run still in progress", "stack", st.Name)
		return OutcomeSkipped
	}
	defer lock.Unlock()

	runID := newRunID()
	log := d.log.With("run_id", runID, "stack", st.Name)

	ctx, cancel := context.WithTimeout(ctx, d.cfg.RunTimeout.Duration)
	defer cancel()

	dir := filepath.Join(d.cfg.BaseDir, st.Name)

	// 1. git sync — a fetch failure is not a deploy failure: nothing was
	// going to be deployed. Logged, visible via the scheduler snapshot.
	newSHA, err := d.git.SyncAndResolve(ctx, st.Repo, st.Ref, dir)
	if err != nil {
		log.Error("git sync failed", "stage", StageGitSync, "error", err)
		return OutcomeFailed
	}

	// 2. SHA diff against the persisted state (self-healing, see package doc).
	prev, _ := d.store.Get(st.Name)
	oldSHA := prev.LastDeployedSHA
	if newSHA == oldSHA {
		log.Debug("up to date", "sha", newSHA)
		return OutcomeUpToDate
	}
	log.Info("git delta detected", "old_sha", oldSHA, "new_sha", newSHA)

	// A failed revision is retried every tick (self-healing), but the same
	// (revision, stage) failure is only notified once — not every minute.
	repeat := prev.LastFailedSHA == newSHA

	// Notifications run on a detached context (see notifyTimeout).
	ev := func(t notify.EventType, stage, detail string) {
		nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
		defer cancel()
		_ = d.notifier.Notify(nctx, notify.Event{
			Type: t, Stack: st.Name, RunID: runID, Ref: st.Ref,
			OldSHA: oldSHA, NewSHA: newSHA, Stage: stage, Detail: detail,
			Time: time.Now(),
		})
	}
	// fail records the failure (keeping deployedSHA as the deployed revision
	// and remembering what failed for dedup) and notifies unless this exact
	// failure was already notified on a previous tick.
	fail := func(t notify.EventType, stage, detail, deployedSHA, status string) {
		if repeat && prev.LastFailedStage == stage {
			log.Warn("repeated failure on same revision, notification suppressed",
				"stage", stage, "sha", newSHA)
		} else {
			ev(t, stage, detail)
		}
		d.saveState(log, st.Name, state.StackState{
			LastDeployedSHA: deployedSHA, LastStatus: status, LastRunID: runID,
			UpdatedAt: time.Now(), LastFailedSHA: newSHA, LastFailedStage: stage,
		})
	}

	// Concurrency budget: only taken once real work is due.
	select {
	case d.sem <- struct{}{}:
		defer func() { <-d.sem }()
	case <-ctx.Done():
		log.Error("timed out waiting for a deployment slot", "stage", StageQueue)
		fail(notify.DeployFailed, StageQueue,
			"timed out waiting for a free deployment slot (max_concurrent_deploys)",
			oldSHA, state.StatusFailed)
		return OutcomeFailed
	}

	if !repeat {
		ev(notify.DeployQueued, "", "")
	}

	// 4. checkout the exact revision being deployed.
	if err := d.git.CheckoutDetached(ctx, st.Repo, dir, newSHA); err != nil {
		log.Error("checkout failed", "stage", StageCheckout, "error", err)
		fail(notify.DeployFailed, StageCheckout, err.Error(), oldSHA, state.StatusFailed)
		return OutcomeFailed
	}

	if !repeat {
		ev(notify.DeployStart, "", "")
	}

	// 6. pre-deploy hook — the backup gate (F9–F14).
	hctx := hooks.Context{Stack: st.Name, Dir: dir, GitRef: st.Ref, OldSHA: oldSHA, NewSHA: newSHA}
	res, err := hooks.Resolve(dir, hooks.RepoPreDeploy, d.cfg.Hooks.PreDeployPath)
	if err != nil {
		log.Error("pre-deploy hook unusable", "stage", StagePreHook, "error", err)
		fail(notify.PreHookFailed, StagePreHook, err.Error(), oldSHA, state.StatusPreHookFailed)
		return OutcomeFailed
	}
	if res.Path == "" {
		log.Info("no pre-deploy hook found, continuing", "stage", StagePreHook)
		ev(notify.PreHookSkipped, StagePreHook, "no pre-deploy hook in repo or config")
	} else {
		log.Info("running pre-deploy hook", "hook", res.Path, "source", res.Source)
		hres, err := d.hooks.Run(ctx, res.Path, hctx, st.HookTimeout.Duration)
		if err != nil {
			log.Error("pre-deploy hook failed, deployment aborted", "stage", StagePreHook, "error", err)
			fail(notify.PreHookFailed, StagePreHook,
				fmt.Sprintf("%v\n%s", err, execx.Tail(hres.Stderr, 1024)),
				oldSHA, state.StatusPreHookFailed)
			return OutcomeFailed
		}
	}

	// 7. sops plumbing (F16).
	opts := compose.Options{Dir: dir, ComposeFile: st.ComposeFile, Project: st.Name}
	cleanup, err := d.setupSops(ctx, st, dir, runID, &opts)
	if err != nil {
		log.Error("sops setup failed", "stage", StageSops, "error", err)
		fail(notify.DeployFailed, StageSops, err.Error(), oldSHA, state.StatusFailed)
		return OutcomeFailed
	}
	defer cleanup()

	// 8. pull — a failure leaves the running stack untouched (F18).
	if st.ForcePullEnabled() {
		if err := d.runtime.Pull(ctx, opts); err != nil {
			log.Error("pull failed, running stack untouched", "stage", StagePull, "error", err)
			fail(notify.DeployFailed, StagePull, err.Error(), oldSHA, state.StatusFailed)
			return OutcomeFailed
		}
	}

	// 9. up.
	if err := d.runtime.Up(ctx, opts); err != nil {
		log.Error("up failed", "stage", StageUp, "error", err)
		fail(notify.DeployFailed, StageUp, err.Error(), oldSHA, state.StatusFailed)
		return OutcomeFailed
	}

	// 10. verify (F19). On failure the new SHA is recorded anyway so the
	// same broken revision is not redeployed in a loop; recovery is a git
	// revert (or a forced deploy once the v1 CLI lands).
	if err := d.verify(ctx, opts, st.VerifyTimeout.Duration); err != nil {
		log.Error("post-up verification failed", "stage", StageVerify, "error", err)
		fail(notify.DeployFailed, StageVerify, err.Error(), newSHA, state.StatusFailed)
		return OutcomeFailed
	}

	// 11. post-deploy hook, non-blocking (F15).
	if res, err := hooks.Resolve(dir, hooks.RepoPostDeploy, d.cfg.Hooks.PostDeployPath); err != nil {
		log.Warn("post-deploy hook unusable", "stage", StagePostHook, "error", err)
	} else if res.Path != "" {
		if _, err := d.hooks.Run(ctx, res.Path, hctx, st.HookTimeout.Duration); err != nil {
			log.Warn("post-deploy hook failed (non-blocking)", "stage", StagePostHook, "error", err)
		}
	}

	// 12. state save — success clears the failure-dedup fields.
	if err := d.store.Put(st.Name, state.StackState{
		LastDeployedSHA: newSHA, LastStatus: state.StatusSuccess,
		LastRunID: runID, UpdatedAt: time.Now(),
	}); err != nil {
		log.Error("state save failed", "stage", StageStateSave, "error", err)
		ev(notify.DeployFailed, StageStateSave, err.Error())
		return OutcomeFailed
	}

	ev(notify.DeploySuccess, "", "")
	log.Info("deployed", "sha", newSHA)
	return OutcomeDeployed
}

// setupSops fills opts with either the exec-env prefix or tmpfs env-files.
// The returned cleanup must always run.
func (d *Deployer) setupSops(ctx context.Context, st config.StackConfig, dir, runID string, opts *compose.Options) (func(), error) {
	if len(st.SopsFiles) == 0 {
		d.log.Debug("sops: no files configured, skipping", "stack", st.Name)
		return func() {}, nil
	}
	for _, f := range st.SopsFiles {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			return func() {}, fmt.Errorf("sops file %q not found in worktree at deployed revision: %w", f, err)
		}
	}
	if st.SopsMode == "tmpfs" {
		abs := make([]string, len(st.SopsFiles))
		for i, f := range st.SopsFiles {
			abs[i] = filepath.Join(dir, f)
		}
		env, err := sopsx.DecryptToTmpfs(ctx, d.runner, abs, d.TmpfsRoot, st.Name, runID)
		if err != nil {
			return func() {}, err
		}
		opts.ExtraArgs = env.Args
		return env.Cleanup, nil
	}
	// exec-env: paths stay repo-relative, the command runs with Dir = worktree.
	files := st.SopsFiles
	opts.Wrap = func(argv []string) []string { return sopsx.ExecEnvArgv(files, argv) }
	return func() {}, nil
}

// verify polls `compose ps` until every service is healthy or the timeout
// expires. Fails immediately on an unhealthy or crashed service.
func (d *Deployer) verify(ctx context.Context, opts compose.Options, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var last string
	for {
		services, err := d.runtime.PS(ctx, opts)
		switch {
		case err != nil:
			last = err.Error()
		case len(services) == 0:
			last = "no services found for project"
		default:
			bad, pending := assess(services)
			if len(bad) > 0 {
				return fmt.Errorf("failed services: %s", strings.Join(bad, ", "))
			}
			if len(pending) == 0 {
				return nil
			}
			last = "still starting: " + strings.Join(pending, ", ")
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("verification timed out after %s (%s)", timeout, last)
		case <-time.After(verifyPollInterval):
		}
	}
}

// assess splits services into definitely-failed and still-pending. A service
// that exited with code 0 is tolerated (one-shot init containers).
func assess(services []compose.Service) (bad, pending []string) {
	for _, s := range services {
		switch {
		case s.Health == "unhealthy",
			s.State == "dead",
			s.State == "exited" && s.ExitCode != 0:
			bad = append(bad, fmt.Sprintf("%s (%s/%s exit=%d)", s.Name, s.State, s.Health, s.ExitCode))
		case s.State == "exited" && s.ExitCode == 0:
			// one-shot service, fine
		case s.State == "running" && (s.Health == "healthy" || s.Health == ""):
			// ready
		default:
			pending = append(pending, fmt.Sprintf("%s (%s/%s)", s.Name, s.State, s.Health))
		}
	}
	return bad, pending
}

func (d *Deployer) saveState(log *slog.Logger, stack string, st state.StackState) {
	if err := d.store.Put(stack, st); err != nil {
		log.Error("state save failed", "error", err)
	}
}

func (d *Deployer) stackLock(name string) *sync.Mutex {
	d.mu.Lock()
	defer d.mu.Unlock()
	l, ok := d.locks[name]
	if !ok {
		l = &sync.Mutex{}
		d.locks[name] = l
	}
	return l
}

func newRunID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}
