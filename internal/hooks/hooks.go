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
	"strings"
	"time"

	"github.com/Gu1llaum-3/plico/internal/execx"
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

// baseline is the set of environment variables a hook needs to be a usable
// shell + Docker client, and nothing sensitive: PATH (find docker, pg_dump,
// sleep…), HOME (~/.docker/config.json, ~/.pgpass), locale, and the Docker
// client's non-secret pointers. Everything else — SOPS_AGE_KEY_FILE, ntfy /
// SMTP / git tokens from the daemon environment — is withheld by default.
// SSH_AUTH_SOCK is deliberately NOT here: a backup-over-ssh hook opts into it
// via env_passthrough (a repo-controlled hook must not get the operator's ssh
// agent for free).
var baseline = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "LC_MESSAGES",
	"TZ", "TERM", "TMPDIR",
	// XDG_RUNTIME_DIR: how the docker/podman CLI finds the ROOTLESS socket
	// and config when DOCKER_HOST is unset — omitting it breaks every hook
	// that shells out to docker on a rootless host.
	"XDG_RUNTIME_DIR",
	// Docker/Compose client pointers, all non-secret: host, config dir,
	// context, API-version pin, and the compose selectors. DOCKER_TLS_VERIFY
	// is a non-secret flag (default certs under ~/.docker via HOME still
	// work). DOCKER_CERT_PATH is DELIBERATELY excluded — it points at the
	// client's TLS private key, the same credential class as SSH_AUTH_SOCK;
	// a custom cert path is opted in via env_passthrough.
	"DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT",
	"DOCKER_TLS_VERIFY", "DOCKER_API_VERSION",
	"COMPOSE_FILE", "COMPOSE_PROJECT_NAME", "COMPOSE_PROFILES",
}

// buildEnv assembles the COMPLETE, scoped environment for a hook subprocess:
// the safe baseline + the operator-allowed passthrough + the DEPLOY_*
// context, each taken from lookup only if present. lookup is injected so this
// is a pure, deterministic function (tests do not depend on the real
// environment). Order matters: DEPLOY_* comes LAST and dedup is last-wins, so
// the per-deploy context is authoritative and a passthrough entry that
// collides with a DEPLOY_* name can never override the real deploy target.
// It also returns the passthrough names the operator declared but that are
// ABSENT from the environment — the caller warns about them (fail-loudly:
// a typo or a missing credential should be visible), without refusing, since
// some passthrough vars are legitimately optional.
func buildEnv(lookup func(string) (string, bool), hctx Context, passthrough []string) (env, missing []string) {
	var out []string
	add := func(key string) bool {
		if v, ok := lookup(key); ok {
			out = append(out, key+"="+v)
			return true
		}
		return false
	}
	for _, k := range baseline {
		add(k)
	}
	for _, k := range passthrough {
		if !add(k) {
			missing = append(missing, k)
		}
	}
	out = append(out, hctx.env()...) // DEPLOY_* last: authoritative
	return dedupEnv(out), missing
}

// dedupEnv keeps the last assignment of each key (POSIX exec semantics),
// preserving first-seen order of the surviving keys.
func dedupEnv(env []string) []string {
	last := make(map[string]string, len(env))
	var order []string
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if _, seen := last[key]; !seen {
			order = append(order, key)
		}
		last[key] = kv
	}
	out := make([]string, 0, len(order))
	for _, key := range order {
		out = append(out, last[key])
	}
	return out
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

// Resolve picks the hook to run. A hook that is declared but unusable is an
// error, never a silent skip: a repo hook that exists without the executable
// bit, or a configured global path that is missing, must block the
// deployment — a declared backup gate cannot silently disappear.
func Resolve(repoDir, repoRelPath, globalPath string) (Resolution, error) {
	repoHook := filepath.Join(repoDir, repoRelPath)
	if info, err := os.Stat(repoHook); err == nil && !info.IsDir() {
		if info.Mode()&0o111 == 0 {
			return Resolution{}, fmt.Errorf("repo hook %s exists but is not executable (missing chmod +x?)", repoRelPath)
		}
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
// The hook runs with a SCOPED environment (baseline + DEPLOY_* + the
// operator-allowed passthrough), never the daemon's full environment: a
// repo-controlled hook must not inherit SOPS_AGE_KEY_FILE or the notifier
// tokens.
func (r *Runner) Run(ctx context.Context, path string, hctx Context, timeout time.Duration, passthrough []string) (execx.Result, error) {
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	env, missing := buildEnv(os.LookupEnv, hctx, passthrough)
	if len(missing) > 0 {
		r.log.Warn("env_passthrough variables declared but absent from the environment; the hook will not receive them",
			"hook", path, "stack", hctx.Stack, "missing", missing)
	}
	res, err := r.runner.Run(runCtx, execx.Cmd{
		Name:     path,
		Dir:      hctx.Dir,
		Env:      env,
		CleanEnv: true,
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
