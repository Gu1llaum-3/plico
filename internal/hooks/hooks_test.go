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

	"plico/internal/execx"
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

	t.Run("non-executable repo hook is ignored", func(t *testing.T) {
		t.Parallel()
		repo := t.TempDir()
		writeScript(t, filepath.Join(repo, RepoPreDeploy), "exit 0", 0o644)
		res, err := Resolve(repo, RepoPreDeploy, "")
		if err != nil {
			t.Fatal(err)
		}
		if res.Path != "" {
			t.Errorf("non-executable hook resolved: %+v", res)
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
		Context{Stack: "web", Dir: dir, GitRef: "main", OldSHA: "old1", NewSHA: "new2"}, time.Minute)
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
	res, err := r.Run(context.Background(), hook, Context{Dir: dir}, time.Minute)
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
	writeScript(t, hook, "sleep 30", 0o755)

	r := New(execx.NewRunner(discard()), discard())
	start := time.Now()
	_, err := r.Run(context.Background(), hook, Context{Dir: dir}, 200*time.Millisecond)
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
