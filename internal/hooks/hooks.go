// Package hooks resolves and runs the pre/post-deploy hooks (F9–F15).
// Resolution order: hook versioned in the repo first, then the global path
// from the config, then none.
package hooks

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"plico/internal/execx"
)

// Repo-side hook locations (F10).
const (
	RepoPreDeploy  = ".deploy/pre-deploy.sh"
	RepoPostDeploy = ".deploy/post-deploy.sh"
)

// Context is passed to hooks as environment variables (F11).
type Context struct {
	Stack  string
	Dir    string // stack worktree
	GitRef string
	OldSHA string
	NewSHA string
}

func (c Context) env() []string {
	return []string{
		"DEPLOY_STACK=" + c.Stack,
		"DEPLOY_DIR=" + c.Dir,
		"DEPLOY_GIT_REF=" + c.GitRef,
		"DEPLOY_OLD_SHA=" + c.OldSHA,
		"DEPLOY_NEW_SHA=" + c.NewSHA,
	}
}

type Runner struct {
	runner execx.Runner
	log    *slog.Logger
}

func New(r execx.Runner, log *slog.Logger) *Runner {
	return &Runner{runner: r, log: log}
}

// Resolution outcome for one hook kind.
type Resolution struct {
	Path   string // "" when no hook anywhere
	Source string // "repo" | "global" | ""
}

// Resolve picks the hook to run. A global path that is configured but
// missing or not executable is an error: a declared backup gate that
// silently disappears must block the deployment, not skip it.
func Resolve(repoDir, repoRelPath, globalPath string) (Resolution, error) {
	repoHook := filepath.Join(repoDir, repoRelPath)
	if isExecutable(repoHook) {
		return Resolution{Path: repoHook, Source: "repo"}, nil
	}
	if globalPath == "" {
		return Resolution{}, nil
	}
	if !isExecutable(globalPath) {
		return Resolution{}, fmt.Errorf("configured hook %s is missing or not executable", globalPath)
	}
	return Resolution{Path: globalPath, Source: "global"}, nil
}

// Run executes the hook with the deploy context and a hard timeout (F13).
// stdout/stderr are captured for the log and the failure notification (F14).
func (r *Runner) Run(ctx context.Context, path string, hctx Context, timeout time.Duration) (execx.Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	res, err := r.runner.Run(runCtx, execx.Cmd{
		Name: path,
		Dir:  hctx.Dir,
		Env:  hctx.env(),
	})

	logger := r.log.With("hook", path, "stack", hctx.Stack)
	if len(res.Stdout) > 0 {
		logger.Info("hook stdout", "output", string(res.Stdout))
	}
	if len(res.Stderr) > 0 {
		logger.Info("hook stderr", "output", string(res.Stderr))
	}
	if runCtx.Err() == context.DeadlineExceeded {
		return res, fmt.Errorf("hook %s timed out after %s", path, timeout)
	}
	return res, err
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}
