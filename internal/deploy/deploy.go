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
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Gu1llaum-3/plico/internal/compose"
	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/execx"
	"github.com/Gu1llaum-3/plico/internal/gitrepo"
	"github.com/Gu1llaum-3/plico/internal/hooks"
	"github.com/Gu1llaum-3/plico/internal/notify"
	"github.com/Gu1llaum-3/plico/internal/sopsx"
	"github.com/Gu1llaum-3/plico/internal/state"
)

// Outcome of one RunStack call.
type Outcome int

const (
	OutcomeSkipped  Outcome = iota // previous run still in progress (F37)
	OutcomeUpToDate                // no git delta, nothing done
	OutcomeDeployed
	OutcomeFailed
	OutcomeQueued  // check only: a delta is pending until the next window (F6)
	OutcomeDrifted // health check only: a deployed stack has degraded
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSkipped:
		return "skipped"
	case OutcomeUpToDate:
		return "up_to_date"
	case OutcomeDeployed:
		return "deployed"
	case OutcomeQueued:
		return "queued"
	case OutcomeDrifted:
		return "drifted"
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

	mu       sync.Mutex
	locks    map[string]*sync.Mutex
	gitFails map[string]int  // consecutive git sync failures per stack
	driftBad map[string]bool // stacks currently in a drift episode (dedup)
	sem      chan struct{}   // bounds concurrent deployments across stacks

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
		gitFails:  map[string]int{},
		driftBad:  map[string]bool{},
		sem:       make(chan struct{}, cfg.MaxConcurrentDeploys),
		TmpfsRoot: sopsx.DefaultTmpfsRoot,
	}
}

// RunOptions alter one manual run (F26/F30). The zero value is the normal
// scheduled behavior.
type RunOptions struct {
	// Force deploys even without a git delta (recovery path after a failed
	// verify, or redeploy of the current revision).
	Force bool
	// SkipPre bypasses the backup gate. The API layer refuses it without an
	// explicit force acknowledgement; it is always loud (F30).
	SkipPre bool
	// SkipPost skips the non-blocking post-deploy hook (low risk).
	SkipPost bool
}

// DryRunReport is what a deployment WOULD do (F28), without acting.
type DryRunReport struct {
	Stack    string   `json:"stack"`
	Ref      string   `json:"ref"`
	OldSHA   string   `json:"old_sha"`
	NewSHA   string   `json:"new_sha"`
	UpToDate bool     `json:"up_to_date"`
	Commits  []string `json:"commits,omitempty"` // git log --oneline old..new
}

// stackRoot is the stack's content root: the git checkout dir, or a
// subdirectory of it when st.Path is set (monorepo). Compose, hooks and sops
// resolve here; git operations stay at the checkout dir. filepath.Join cleans
// the result, and st.Path is validated repo-relative at config load.
func stackRoot(dir string, st config.StackConfig) string {
	if st.Path == "" {
		return dir
	}
	return filepath.Join(dir, st.Path)
}

// RunStack executes one full cycle for a stack. It never panics; every error
// is logged and notified before returning.
func (d *Deployer) RunStack(ctx context.Context, st config.StackConfig) Outcome {
	return d.RunStackWith(ctx, st, RunOptions{})
}

// RunStackWith is RunStack with manual-run options (F26/F30).
func (d *Deployer) RunStackWith(ctx context.Context, st config.StackConfig, opts RunOptions) Outcome {
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

	dir := filepath.Join(d.cfg.BaseDir, st.Name) // git checkout (whole repo)
	root := stackRoot(dir, st)                   // stack content root (== dir unless st.Path is set)

	// 1. git sync — a fetch failure is not a deploy failure: nothing was
	// going to be deployed. Logged and counted: a PERSISTENT failure
	// (revoked token, moved repo) fires git_sync_failed once per outage.
	newSHA, err := d.git.SyncAndResolve(ctx, st.Repo, st.Ref, dir)
	d.noteGitSync(ctx, st, err)
	if err != nil {
		log.Error("git sync failed", "stage", StageGitSync, "error", err)
		return OutcomeFailed
	}

	// 2. SHA diff against the persisted state (self-healing, see package doc).
	prev, _ := d.store.Get(st.Name)
	oldSHA := prev.LastDeployedSHA
	if newSHA == oldSHA && !opts.Force {
		log.Debug("up to date", "sha", newSHA)
		return OutcomeUpToDate
	}
	// Monorepo scoping: a stack rooted at a subdirectory only redeploys when
	// the delta actually touches that subtree. A commit to a sibling stack
	// must not re-run this stack's hook/pull. The deployed SHA is left
	// untouched on a skip (the state never claims a deployment that did not
	// happen). Bypassed by --force and by the first deploy (no oldSHA to diff
	// against); fail-open if the diff errors (oldSHA gone after a force-push).
	if st.Path != "" && oldSHA != "" && !opts.Force {
		changed, derr := d.git.PathChanged(ctx, st.Repo, dir, oldSHA, newSHA, st.Path)
		switch {
		case derr != nil:
			log.Warn("path-scoped diff failed, deploying to be safe",
				"path", st.Path, "old_sha", oldSHA, "new_sha", newSHA, "error", derr)
		case !changed:
			log.Info("git delta is outside this stack's path, skipping",
				"path", st.Path, "old_sha", oldSHA, "new_sha", newSHA)
			return OutcomeUpToDate
		}
	}
	log.Info("git delta detected", "old_sha", oldSHA, "new_sha", newSHA, "forced", opts.Force)

	// A failed revision is retried every tick (self-healing), but the same
	// (revision, stage) failure is only notified once — not every minute.
	// A manual forced run always notifies: the operator is acting NOW.
	repeat := prev.LastFailedSHA == newSHA && !opts.Force

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
		d.saveState(log, st.Name, func(s2 *state.StackState) {
			s2.LastDeployedSHA = deployedSHA
			s2.LastStatus = status
			s2.LastRunID = runID
			s2.UpdatedAt = time.Now()
			s2.LastFailedSHA = newSHA
			s2.LastFailedStage = stage
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

	if !repeat && prev.LastQueuedSHA != newSHA && newSHA != oldSHA {
		// A pending revision already announced by an out-of-window check
		// (F6) is not re-announced when its window finally applies it, and
		// a forced redeploy of the CURRENT revision has nothing queued.
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

	// 6. pre-deploy hook — the backup gate (F9–F14). Manually skippable
	// only through the API's force acknowledgement, and always loud (F30).
	// The hook runs with a scoped environment (baseline + DEPLOY_* + the
	// global∪stack passthrough), never the daemon's secrets.
	hctx := hooks.Context{Stack: st.Name, Dir: root, GitRef: st.Ref, OldSHA: oldSHA, NewSHA: newSHA}
	hookEnv := st.HookEnvPassthrough(d.cfg.Hooks.EnvPassthrough)
	if opts.SkipPre {
		log.Warn("PRE-DEPLOY HOOK MANUALLY SKIPPED: deploying without the backup gate",
			"stage", StagePreHook)
		ev(notify.PreHookSkipped, StagePreHook, "pre-deploy hook manually skipped (--skip-pre --force)")
	} else {
		res, err := hooks.Resolve(root, hooks.RepoPreDeploy, d.cfg.Hooks.PreDeployPath)
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
			hres, err := d.hooks.Run(ctx, res.Path, hctx, st.HookTimeout.Duration, hookEnv)
			if err != nil {
				log.Error("pre-deploy hook failed, deployment aborted", "stage", StagePreHook, "error", err)
				fail(notify.PreHookFailed, StagePreHook,
					fmt.Sprintf("%v\n%s", err, execx.Tail(hres.Stderr, 1024)),
					oldSHA, state.StatusPreHookFailed)
				return OutcomeFailed
			}
		}
	}

	// 7. sops plumbing (F16).
	copts := compose.Options{Dir: root, ComposeFile: st.ComposeFile, Project: st.Name}
	cleanup, err := d.setupSops(ctx, st, root, runID, &copts)
	if err != nil {
		log.Error("sops setup failed", "stage", StageSops, "error", err)
		fail(notify.DeployFailed, StageSops, err.Error(), oldSHA, state.StatusFailed)
		return OutcomeFailed
	}
	defer cleanup()

	// 8. pull — a failure leaves the running stack untouched (F18).
	if st.ForcePullEnabled() {
		if err := d.runtime.Pull(ctx, copts); err != nil {
			log.Error("pull failed, running stack untouched", "stage", StagePull, "error", err)
			fail(notify.DeployFailed, StagePull, err.Error(), oldSHA, state.StatusFailed)
			return OutcomeFailed
		}
	}

	// 9. up.
	if err := d.runtime.Up(ctx, copts); err != nil {
		log.Error("up failed", "stage", StageUp, "error", err)
		fail(notify.DeployFailed, StageUp, err.Error(), oldSHA, state.StatusFailed)
		return OutcomeFailed
	}

	// 10. verify (F19). On failure the new SHA is recorded anyway so the
	// same broken revision is not redeployed in a loop; recovery is a git
	// revert (or a forced deploy: deploy-now --force).
	if err := d.verify(ctx, copts, st.VerifyTimeout.Duration); err != nil {
		log.Error("post-up verification failed", "stage", StageVerify, "error", err)
		fail(notify.DeployFailed, StageVerify, err.Error(), newSHA, state.StatusFailed)
		return OutcomeFailed
	}

	// 11. post-deploy hook, non-blocking (F15); manually skippable (F30,
	// low risk).
	if opts.SkipPost {
		log.Info("post-deploy hook manually skipped", "stage", StagePostHook)
	} else if res, err := hooks.Resolve(root, hooks.RepoPostDeploy, d.cfg.Hooks.PostDeployPath); err != nil {
		log.Warn("post-deploy hook unusable", "stage", StagePostHook, "error", err)
	} else if res.Path != "" {
		if _, err := d.hooks.Run(ctx, res.Path, hctx, st.HookTimeout.Duration, hookEnv); err != nil {
			log.Warn("post-deploy hook failed (non-blocking)", "stage", StagePostHook, "error", err)
		}
	}

	// 12. state save — success clears the failure- and queued-dedup fields.
	if err := d.store.Update(st.Name, func(s2 *state.StackState) {
		s2.LastDeployedSHA = newSHA
		s2.LastStatus = state.StatusSuccess
		s2.LastRunID = runID
		s2.UpdatedAt = time.Now()
		s2.LastFailedSHA = ""
		s2.LastFailedStage = ""
		s2.LastQueuedSHA = ""
	}); err != nil {
		log.Error("state save failed", "stage", StageStateSave, "error", err)
		ev(notify.DeployFailed, StageStateSave, err.Error())
		return OutcomeFailed
	}

	// A successful deploy re-establishes a healthy baseline: close any open
	// drift episode so a later health check does not fire a phantom
	// drift_resolved for a state the deploy already fixed.
	d.mu.Lock()
	delete(d.driftBad, st.Name)
	d.mu.Unlock()

	ev(notify.DeploySuccess, "", "")
	log.Info("deployed", "sha", newSHA)
	return OutcomeDeployed
}

// CheckStack is the out-of-window half of F6: fetch + SHA diff only, never
// a deployment. A pending revision is announced with deploy_queued exactly
// once (deduped via state.LastQueuedSHA); the window's apply run will not
// re-announce it.
func (d *Deployer) CheckStack(ctx context.Context, st config.StackConfig) Outcome {
	lock := d.stackLock(st.Name)
	if !lock.TryLock() {
		return OutcomeSkipped // a deploy is running; it owns the stack
	}
	defer lock.Unlock()

	runID := newRunID()
	log := d.log.With("run_id", runID, "stack", st.Name, "mode", "check")

	ctx, cancel := context.WithTimeout(ctx, d.cfg.RunTimeout.Duration)
	defer cancel()

	dir := filepath.Join(d.cfg.BaseDir, st.Name)
	newSHA, err := d.git.SyncAndResolve(ctx, st.Repo, st.Ref, dir)
	d.noteGitSync(ctx, st, err)
	if err != nil {
		log.Error("git sync failed", "stage", StageGitSync, "error", err)
		return OutcomeFailed
	}

	prev, _ := d.store.Get(st.Name)
	switch newSHA {
	case prev.LastDeployedSHA:
		if prev.LastQueuedSHA != "" {
			d.saveState(log, st.Name, func(s2 *state.StackState) {
				s2.LastQueuedSHA = ""
			})
		}
		return OutcomeUpToDate
	case prev.LastQueuedSHA:
		return OutcomeQueued // already announced, stays pending
	case prev.LastFailedSHA:
		return OutcomeQueued // already reported as failing; no new announcement
	}

	// Monorepo scoping (mirrors RunStackWith): don't announce a stack as
	// pending when the delta is entirely in a sibling stack's subtree. Skip
	// the first check (no prior SHA) and fail open on a diff error.
	if st.Path != "" && prev.LastDeployedSHA != "" {
		changed, derr := d.git.PathChanged(ctx, st.Repo, dir, prev.LastDeployedSHA, newSHA, st.Path)
		switch {
		case derr != nil:
			log.Warn("path-scoped diff failed, announcing to be safe",
				"path", st.Path, "old_sha", prev.LastDeployedSHA, "new_sha", newSHA, "error", derr)
		case !changed:
			log.Info("git delta is outside this stack's path, not queuing",
				"path", st.Path, "old_sha", prev.LastDeployedSHA, "new_sha", newSHA)
			// If a revision was previously queued but the current HEAD has
			// since restored this stack's subtree to the deployed content,
			// the announced revision is no longer pending: clear the stale
			// dedup marker so state.json does not advertise a phantom queue.
			if prev.LastQueuedSHA != "" {
				d.saveState(log, st.Name, func(s2 *state.StackState) {
					s2.LastQueuedSHA = ""
				})
			}
			return OutcomeUpToDate
		}
	}

	log.Info("git delta detected, queued until the next deployment window",
		"old_sha", prev.LastDeployedSHA, "new_sha", newSHA)
	nctx, ncancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer ncancel()
	_ = d.notifier.Notify(nctx, notify.Event{
		Type: notify.DeployQueued, Stack: st.Name, RunID: runID, Ref: st.Ref,
		OldSHA: prev.LastDeployedSHA, NewSHA: newSHA,
		Detail: "pending until the next deployment window",
		Time:   time.Now(),
	})
	d.saveState(log, st.Name, func(s2 *state.StackState) {
		s2.LastQueuedSHA = newSHA
	})
	return OutcomeQueued
}

// DryRun reports what a deployment would do (F28): fetch + diff + the list
// of pending commits, without touching hooks, sops or compose.
func (d *Deployer) DryRun(ctx context.Context, st config.StackConfig) (DryRunReport, error) {
	lock := d.stackLock(st.Name)
	if !lock.TryLock() {
		return DryRunReport{}, fmt.Errorf("a run is in progress for stack %q, try again later", st.Name)
	}
	defer lock.Unlock()

	ctx, cancel := context.WithTimeout(ctx, d.cfg.RunTimeout.Duration)
	defer cancel()

	dir := filepath.Join(d.cfg.BaseDir, st.Name)
	newSHA, err := d.git.SyncAndResolve(ctx, st.Repo, st.Ref, dir)
	d.noteGitSync(ctx, st, err) // a successful manual sync resets the outage counter
	if err != nil {
		return DryRunReport{}, err
	}
	prev, _ := d.store.Get(st.Name)
	report := DryRunReport{
		Stack: st.Name, Ref: st.Ref,
		OldSHA: prev.LastDeployedSHA, NewSHA: newSHA,
		UpToDate: newSHA == prev.LastDeployedSHA,
	}
	// Monorepo scoping (mirrors RunStackWith): a delta outside this stack's
	// subtree would be a no-op deploy, so report it as up-to-date. Fail open
	// on a diff error (report the delta rather than hide it).
	if !report.UpToDate && st.Path != "" && prev.LastDeployedSHA != "" {
		if changed, derr := d.git.PathChanged(ctx, st.Repo, dir, prev.LastDeployedSHA, newSHA, st.Path); derr == nil && !changed {
			report.UpToDate = true
		}
	}
	if !report.UpToDate && prev.LastDeployedSHA != "" {
		commits, err := d.git.LogRange(ctx, st.Repo, dir, prev.LastDeployedSHA, newSHA)
		if err != nil {
			return DryRunReport{}, fmt.Errorf("listing pending commits: %w", err)
		}
		report.Commits = commits
	}
	return report, nil
}

// CheckHealth is the drift-detection probe (reconciliation-lite): a single
// `compose ps` snapshot of an already-deployed stack, notifying on a
// regression (a service unhealthy/dead/crashed) and once again on recovery.
// It NEVER remediates — detection only; the operator decides (backup + human).
//
// It is deliberately quiet in every ambiguous case: it runs only for a stack
// whose last deploy succeeded (a baseline exists), skips when a deploy owns
// the stack (a `compose up` in flight would show a transient state), and
// swallows a `ps` error (a docker hiccup is not a drift). A manually stopped
// service (exited 0) or a fully downed stack (no services) is out of scope,
// same family as a one-shot init container — see assess.
func (d *Deployer) CheckHealth(ctx context.Context, st config.StackConfig) Outcome {
	lock := d.stackLock(st.Name)
	if !lock.TryLock() {
		return OutcomeSkipped // a deploy owns the stack; its own verify covers health
	}
	defer lock.Unlock()

	// Bound the probe by the health-check budget (verify_timeout), NOT the
	// whole-deploy run_timeout: this lock is held across the ps, and a wedged
	// docker under a 30m run_timeout would TryLock-starve real deploys for the
	// stack. The caller passes a cancellable ctx so shutdown interrupts a hung
	// probe at once.
	timeout := st.VerifyTimeout.Duration
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	log := d.log.With("stack", st.Name, "mode", "drift")

	prev, _ := d.store.Get(st.Name)
	if prev.LastStatus != state.StatusSuccess {
		return OutcomeUpToDate // no healthy baseline to drift from
	}

	dir := filepath.Join(d.cfg.BaseDir, st.Name)
	root := stackRoot(dir, st)
	// PS addresses the project by name only (-p), so no compose file or sops
	// setup is needed (it never re-triggers secret decryption).
	services, err := d.runtime.PS(ctx, compose.Options{Dir: root, Project: st.Name})
	if err != nil {
		// A docker hiccup is not a drift signal: log and leave the episode
		// state untouched rather than raise (or clear) a false alert.
		if ctx.Err() == nil {
			log.Warn("drift check: compose ps failed, skipping this round", "error", err)
		}
		return OutcomeUpToDate
	}
	bad := driftBad(services) // pending (still starting) is not drift

	d.mu.Lock()
	wasBad := d.driftBad[st.Name]
	switch {
	case len(bad) > 0 && !wasBad:
		d.driftBad[st.Name] = true
		d.mu.Unlock()
		log.Warn("drift detected", "services", bad)
		d.notifyDrift(ctx, st, notify.DriftDetected,
			fmt.Sprintf("degraded service(s): %s", strings.Join(bad, ", ")))
		return OutcomeDrifted
	case len(bad) > 0:
		d.mu.Unlock()
		return OutcomeDrifted // still bad, already notified — suppress
	case wasBad && len(services) > 0:
		// Genuine recovery: services are present and none are bad.
		delete(d.driftBad, st.Name)
		d.mu.Unlock()
		log.Info("drift resolved", "stack", st.Name)
		d.notifyDrift(ctx, st, notify.DriftResolved, "all services healthy again")
		return OutcomeUpToDate
	case wasBad:
		// Zero services while an episode is open (e.g. the stack was torn down
		// during incident response): NOT a recovery — nothing is healthy, it
		// is gone. Keep the episode open and stay quiet rather than fire a
		// false "all services healthy again".
		d.mu.Unlock()
		return OutcomeDrifted
	default:
		d.mu.Unlock()
		return OutcomeUpToDate // healthy (or out of scope: 0 services, never drifted)
	}
}

// notifyDrift sends a drift event on a detached, bounded context (a drift
// notification must not be tied to the poll tick's lifetime), mirroring
// noteGitSync.
func (d *Deployer) notifyDrift(ctx context.Context, st config.StackConfig, t notify.EventType, detail string) {
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()
	prev, _ := d.store.Get(st.Name)
	_ = d.notifier.Notify(nctx, notify.Event{
		Type: t, Stack: st.Name, Ref: st.Ref,
		OldSHA: prev.LastDeployedSHA, Stage: "drift",
		Detail: detail, Time: time.Now(),
	})
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

// driftBad is assess's "bad" set widened for the periodic drift lens: a
// crash-looping container reports State=="restarting", which assess buckets as
// "pending". verify() tolerates that (it polls to a deadline, then fails), but
// the drift check has no deadline — so without this a crash loop (the classic
// OOM-with-restart-policy regression) would be ignored on every round forever.
func driftBad(services []compose.Service) []string {
	bad, _ := assess(services)
	for _, s := range services {
		// s.Health=="unhealthy" is already counted by assess; guard against
		// listing a restarting-and-unhealthy container twice.
		if s.State == "restarting" && s.Health != "unhealthy" {
			bad = append(bad, fmt.Sprintf("%s (restarting)", s.Name))
		}
	}
	return bad
}

// noteGitSync tracks consecutive git sync failures per stack and fires
// git_sync_failed exactly once per outage when the configured threshold is
// crossed. A success resets the counter.
func (d *Deployer) noteGitSync(ctx context.Context, st config.StackConfig, syncErr error) {
	threshold := d.cfg.GitSyncAlertThreshold()
	if threshold == 0 {
		return
	}
	// A cancellation or timeout is not a git outage: a user Ctrl-C on a
	// dry-run, or a run_timeout, must neither count toward the alert nor
	// reset the counter — it carries no information about git's health.
	if syncErr != nil && (errors.Is(syncErr, context.Canceled) || errors.Is(syncErr, context.DeadlineExceeded)) {
		return
	}
	d.mu.Lock()
	if syncErr == nil {
		if d.gitFails[st.Name] >= threshold {
			d.log.Info("git sync recovered", "stack", st.Name)
		}
		delete(d.gitFails, st.Name)
		d.mu.Unlock()
		return
	}
	d.gitFails[st.Name]++
	count := d.gitFails[st.Name]
	d.mu.Unlock()
	if count != threshold {
		return
	}
	nctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer cancel()
	_ = d.notifier.Notify(nctx, notify.Event{
		Type: notify.GitSyncFailed, Stack: st.Name, Ref: st.Ref,
		Stage:  StageGitSync,
		Detail: fmt.Sprintf("git sync has failed %d consecutive times, the stack is effectively unmanaged: %v", count, syncErr),
		Time:   time.Now(),
	})
}

func (d *Deployer) saveState(log *slog.Logger, stack string, mutate func(*state.StackState)) {
	if err := d.store.Update(stack, mutate); err != nil {
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
