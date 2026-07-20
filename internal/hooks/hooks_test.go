package hooks

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gu1llaum-3/plico/internal/execx"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func writeScript(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), mode); err != nil {
		t.Fatal(err)
	}
}

func TestResolve(t *testing.T) {
	t.Parallel()

	t.Run("repo hook wins over global", func(t *testing.T) {
		t.Parallel()
		repo, global := t.TempDir(), filepath.Join(t.TempDir(), "global.sh")
		writeScript(t, filepath.Join(repo, RepoPreDeploy), "exit 0", 0o755)
		writeScript(t, global, "exit 0", 0o755)
		res, err := Resolve(repo, RepoPreDeploy, global)
		if err != nil {
			t.Fatal(err)
		}
		if res.Source != "repo" {
			t.Errorf("source = %q, want repo", res.Source)
		}
	})

	t.Run("falls back to global", func(t *testing.T) {
		t.Parallel()
		repo, global := t.TempDir(), filepath.Join(t.TempDir(), "global.sh")
		writeScript(t, global, "exit 0", 0o755)
		res, err := Resolve(repo, RepoPreDeploy, global)
		if err != nil {
			t.Fatal(err)
		}
		if res.Source != "global" || res.Path != global {
			t.Errorf("res = %+v", res)
		}
	})

	t.Run("non-executable repo hook is an error, not a silent skip", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		writeScript(t, filepath.Join(repo, RepoPreDeploy), "exit 0", 0o644)
		_, err := Resolve(repo, RepoPreDeploy, "")
		if err == nil {
			t.Fatal("a committed hook without +x must block the deploy, not bypass the gate")
		}
		if !strings.Contains(err.Error(), "not executable") {
			t.Errorf("error = %v", err)
		}
	})

	t.Run("non-executable repo hook errors even with a global fallback", func(t *testing.T) {
		t.Parallel()
		repo, global := t.TempDir(), filepath.Join(t.TempDir(), "global.sh")
		writeScript(t, filepath.Join(repo, RepoPreDeploy), "exit 0", 0o644)
		writeScript(t, global, "exit 0", 0o755)
		if _, err := Resolve(repo, RepoPreDeploy, global); err == nil {
			t.Fatal("the repo-declared gate must not be silently replaced by the global hook")
		}
	})

	t.Run("no hook anywhere", func(t *testing.T) {
		t.Parallel()
		res, err := Resolve(t.TempDir(), RepoPreDeploy, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Path != "" {
			t.Errorf("res = %+v", res)
		}
	})

	t.Run("configured global missing is an error", func(t *testing.T) {
		t.Parallel()
		if _, err := Resolve(t.TempDir(), RepoPreDeploy, "/nonexistent/hook.sh"); err == nil {
			t.Fatal("a declared but missing backup gate must be an error, not a skip")
		}
	})
}

func TestRunPassesDeployContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hook := filepath.Join(dir, "hook.sh")
	writeScript(t, hook, `printf '%s|%s|%s|%s|%s' "$DEPLOY_STACK" "$DEPLOY_DIR" "$DEPLOY_GIT_REF" "$DEPLOY_OLD_SHA" "$DEPLOY_NEW_SHA"`, 0o755)

	r := New(execx.NewRunner(discard()), discard())
	res, err := r.Run(context.Background(), hook,
		Context{Stack: "web", Dir: dir, GitRef: "main", OldSHA: "old1", NewSHA: "new2"}, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "web|" + dir + "|main|old1|new2"
	if string(res.Stdout) != want {
		t.Errorf("hook env = %q, want %q", res.Stdout, want)
	}
}

func TestRunNonZeroExitFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hook := filepath.Join(dir, "hook.sh")
	writeScript(t, hook, "echo 'backup failed: disk full' >&2\nexit 1", 0o755)

	r := New(execx.NewRunner(discard()), discard())
	res, err := r.Run(context.Background(), hook, Context{Dir: dir}, time.Minute, nil)
	if err == nil {
		t.Fatal("want error on exit 1")
	}
	if !strings.Contains(string(res.Stderr), "disk full") {
		t.Errorf("stderr not captured: %q", res.Stderr)
	}
}

func TestRunTimeout(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hook := filepath.Join(dir, "hook.sh")
	// `sleep` is not a shell builtin: this hook only works if PATH survived
	// the env scoping — the canary for the baseline.
	writeScript(t, hook, "sleep 30", 0o755)

	r := New(execx.NewRunner(discard()), discard())
	start := time.Now()
	_, err := r.Run(context.Background(), hook, Context{Dir: dir}, 200*time.Millisecond, nil)
	if err == nil {
		t.Fatal("want timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %v, want timeout message", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Error("hook was not killed promptly")
	}
}

func TestBuildEnv(t *testing.T) {
	t.Parallel()
	env := map[string]string{
		"PATH":              "/usr/bin:/bin",
		"HOME":              "/home/plico",
		"LANG":              "en_US.UTF-8",
		"DOCKER_HOST":       "unix:///var/run/docker.sock",
		"SSH_AUTH_SOCK":     "/run/agent.sock",
		"SOPS_AGE_KEY_FILE": "/etc/plico/age.key",
		"PLICO_NTFY_TOKEN":  "tk_secret",
		"AWS_SECRET_ACCESS": "shhh",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	hctx := Context{Stack: "web", Dir: "/opt/docker/web", GitRef: "main", OldSHA: "old", NewSHA: "new"}

	got := map[string]string{}
	built, _ := buildEnv(lookup, hctx, []string{"AWS_SECRET_ACCESS"})
	for _, kv := range built {
		k, v, _ := strings.Cut(kv, "=")
		got[k] = v
	}

	for _, k := range []string{"PATH", "HOME", "LANG", "DOCKER_HOST"} {
		if _, ok := got[k]; !ok {
			t.Errorf("baseline var %s missing", k)
		}
	}
	if got["DEPLOY_STACK"] != "web" || got["DEPLOY_NEW_SHA"] != "new" {
		t.Errorf("DEPLOY_* not injected: %v", got)
	}
	for _, k := range []string{"SOPS_AGE_KEY_FILE", "PLICO_NTFY_TOKEN"} {
		if _, ok := got[k]; ok {
			t.Errorf("secret %s leaked into the hook environment", k)
		}
	}
	if _, ok := got["SSH_AUTH_SOCK"]; ok {
		t.Error("SSH_AUTH_SOCK must be opt-in, not in the default baseline")
	}
	if got["AWS_SECRET_ACCESS"] != "shhh" {
		t.Error("passthrough var was not allowed through")
	}
}

func TestBuildEnvSkipsAbsentAndDedups(t *testing.T) {
	t.Parallel()
	env := map[string]string{"PATH": "/bin"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	out, _ := buildEnv(lookup, Context{Stack: "web"}, nil)

	seen := map[string]int{}
	for _, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		seen[k]++
	}
	if seen["HOME"] != 0 {
		t.Error("absent HOME must not be fabricated")
	}
	if seen["DEPLOY_STACK"] != 1 {
		t.Errorf("DEPLOY_STACK count = %d, want 1", seen["DEPLOY_STACK"])
	}
}

// The DEPLOY_* context is authoritative: a passthrough entry colliding with a
// DEPLOY_* name (permitted by the name regex) must NOT override the real
// per-deploy value the hook is told to trust.
func TestBuildEnvContextWinsOverColludingPassthrough(t *testing.T) {
	t.Parallel()
	// The daemon happens to have DEPLOY_STACK set to a stale value, and the
	// operator (mistakenly) passes it through.
	env := map[string]string{"PATH": "/bin", "DEPLOY_STACK": "stale-from-daemon"}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	out, _ := buildEnv(lookup, Context{Stack: "real-target"}, []string{"DEPLOY_STACK"})

	var stackVal string
	count := 0
	for _, kv := range out {
		if k, v, _ := strings.Cut(kv, "="); k == "DEPLOY_STACK" {
			stackVal = v
			count++
		}
	}
	if count != 1 {
		t.Fatalf("DEPLOY_STACK appears %d times, want 1", count)
	}
	if stackVal != "real-target" {
		t.Errorf("context overridden by passthrough: DEPLOY_STACK=%q, want the real deploy target", stackVal)
	}
}

func TestBuildEnvBaselineCoversDockerClients(t *testing.T) {
	t.Parallel()
	// Rootless (XDG_RUNTIME_DIR), API-version pin and compose selectors must
	// keep working after scoping — but DOCKER_CERT_PATH (the TLS private-key
	// location) must be WITHHELD like SSH_AUTH_SOCK, opted in via passthrough.
	daemon := map[string]string{
		"PATH":                 "/bin",
		"XDG_RUNTIME_DIR":      "/run/user/1000",
		"DOCKER_HOST":          "tcp://h:2376",
		"DOCKER_TLS_VERIFY":    "1",
		"DOCKER_API_VERSION":   "1.45",
		"COMPOSE_PROFILES":     "prod",
		"COMPOSE_PROJECT_NAME": "web",
		"DOCKER_CERT_PATH":     "/etc/docker/certs", // holds key.pem — sensitive
	}
	lookup := func(k string) (string, bool) { v, ok := daemon[k]; return v, ok }
	got := map[string]bool{}
	out, _ := buildEnv(lookup, Context{Stack: "web"}, nil)
	for _, kv := range out {
		k, _, _ := strings.Cut(kv, "=")
		got[k] = true
	}
	for _, k := range []string{"XDG_RUNTIME_DIR", "DOCKER_HOST", "DOCKER_TLS_VERIFY", "DOCKER_API_VERSION", "COMPOSE_PROFILES", "COMPOSE_PROJECT_NAME"} {
		if !got[k] {
			t.Errorf("baseline missing %s — a real docker hook would break", k)
		}
	}
	if got["DOCKER_CERT_PATH"] {
		t.Error("DOCKER_CERT_PATH (TLS private-key path) must not be in the baseline; it is opt-in via passthrough")
	}
}

func TestBuildEnvReportsMissingPassthrough(t *testing.T) {
	t.Parallel()
	// PRESENT is set, ABSENT_TYPO is not: the operator's declared-but-absent
	// var is reported so a typo / missing credential is visible (fail-loudly),
	// without refusing (some passthrough vars are optional).
	daemon := map[string]string{"PATH": "/bin", "PRESENT": "v"}
	lookup := func(k string) (string, bool) { v, ok := daemon[k]; return v, ok }
	out, missing := buildEnv(lookup, Context{Stack: "web"}, []string{"PRESENT", "ABSENT_TYPO"})

	if len(missing) != 1 || missing[0] != "ABSENT_TYPO" {
		t.Errorf("missing = %v, want [ABSENT_TYPO]", missing)
	}
	// The present one is still injected.
	found := false
	for _, kv := range out {
		if kv == "PRESENT=v" {
			found = true
		}
	}
	if !found {
		t.Error("a present passthrough var must still be injected")
	}
}

// TestRunScopesEnvironment proves end to end that a hook does NOT see a
// daemon secret by default, but DOES when passed through.
func TestRunScopesEnvironment(t *testing.T) {
	t.Setenv("SOPS_AGE_KEY_FILE", "/etc/plico/age.key")
	dir := t.TempDir()
	hook := filepath.Join(dir, "hook.sh")
	writeScript(t, hook, `printf 'age=[%s] stack=[%s]' "$SOPS_AGE_KEY_FILE" "$DEPLOY_STACK"`, 0o755)
	r := New(execx.NewRunner(discard()), discard())
	hctx := Context{Stack: "web", Dir: dir}

	res, err := r.Run(context.Background(), hook, hctx, time.Minute, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != "age=[] stack=[web]" {
		t.Errorf("scoped hook saw = %q, want age withheld + DEPLOY_STACK present", got)
	}

	res, err = r.Run(context.Background(), hook, hctx, time.Minute, []string{"SOPS_AGE_KEY_FILE"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != "age=[/etc/plico/age.key] stack=[web]" {
		t.Errorf("passthrough hook saw = %q, want the age key present", got)
	}
}
