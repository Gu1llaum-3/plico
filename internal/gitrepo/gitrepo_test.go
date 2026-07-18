package gitrepo

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"plico/internal/config"
	"plico/internal/execx"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func mkClone(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "stack")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSyncAndResolveFetchesWhenCloneExists(t *testing.T) {
	t.Parallel()
	dir := mkClone(t)
	fake := &execx.FakeRunner{Script: []execx.Response{
		{Result: execx.Result{}},                           // fetch
		{Result: execx.Result{Stdout: []byte("abc123\n")}}, // rev-parse
	}}
	c := New(fake, nil, discard())
	sha, err := c.SyncAndResolve(context.Background(), "https://example.com/r.git", "main", dir)
	if err != nil {
		t.Fatal(err)
	}
	if sha != "abc123" {
		t.Errorf("sha = %q", sha)
	}
	if len(fake.Calls) != 2 {
		t.Fatalf("want fetch+rev-parse, got %d calls", len(fake.Calls))
	}
	if fake.Calls[0].Args[0] != "fetch" || !slices.Contains(fake.Calls[0].Args, "+refs/heads/main:refs/remotes/origin/main") {
		t.Errorf("fetch call = %v", fake.Calls[0].Args)
	}
	if fake.Calls[1].Args[0] != "rev-parse" || fake.Calls[1].Args[1] != "refs/remotes/origin/main" {
		t.Errorf("rev-parse call = %v", fake.Calls[1].Args)
	}
}

func TestSyncAndResolveClonesFirstTime(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "stack") // no .git
	fake := &execx.FakeRunner{Script: []execx.Response{
		{Result: execx.Result{}},                           // clone
		{Result: execx.Result{}},                           // fetch
		{Result: execx.Result{Stdout: []byte("def456\n")}}, // rev-parse
	}}
	c := New(fake, nil, discard())
	sha, err := c.SyncAndResolve(context.Background(), "https://example.com/r.git", "main", dir)
	if err != nil {
		t.Fatal(err)
	}
	if sha != "def456" {
		t.Errorf("sha = %q", sha)
	}
	if fake.Calls[0].Args[0] != "clone" {
		t.Errorf("first call should be clone, got %v", fake.Calls[0].Args)
	}
}

func TestAskpassEnvForMatchingHost(t *testing.T) {
	t.Parallel()
	dir := mkClone(t)
	fake := &execx.FakeRunner{Script: []execx.Response{
		{Result: execx.Result{}},
		{Result: execx.Result{Stdout: []byte("abc\n")}},
	}}
	auths := map[string]config.GitAuth{
		"bitbucket.org": {Username: "bot", Password: "apppass"},
	}
	c := New(fake, auths, discard())
	if _, err := c.SyncAndResolve(context.Background(), "https://bitbucket.org/acme/r.git", "main", dir); err != nil {
		t.Fatal(err)
	}
	env := fake.Calls[0].Env
	for _, want := range []string{
		"GIT_TERMINAL_PROMPT=0",
		AskpassEnvFlag + "=1",
		EnvUsername + "=bot",
		EnvPassword + "=apppass",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %q: %v", want, execx.Redact(env))
		}
	}
	hasAskpass := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "GIT_ASKPASS=") && len(kv) > len("GIT_ASKPASS=") {
			hasAskpass = true
		}
	}
	if !hasAskpass {
		t.Error("GIT_ASKPASS not set to own executable")
	}
}

func TestNoAuthEnvForUnknownHostOrSSH(t *testing.T) {
	t.Parallel()
	auths := map[string]config.GitAuth{"bitbucket.org": {Username: "u", Password: "p"}}
	c := New(&execx.FakeRunner{}, auths, discard())
	for _, url := range []string{
		"https://github.com/acme/r.git", // host not configured
		"git@bitbucket.org:acme/r.git",  // ssh shorthand
		"ssh://git@bitbucket.org/acme/r.git",
	} {
		if env := c.authEnv(url); env != nil {
			t.Errorf("authEnv(%q) should be nil, got %v", url, execx.Redact(env))
		}
	}
}

func TestCorruptedCloneIsWipedAndRecloned(t *testing.T) {
	t.Parallel()
	dir := mkClone(t)
	marker := filepath.Join(dir, ".git", "MARKER")
	if err := os.WriteFile(marker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &execx.FakeRunner{Script: []execx.Response{
		{Result: execx.Result{ExitCode: 128}, Err: errors.New("fatal: not a git repository (or any of the parent directories)")}, // fetch fails
		{Result: execx.Result{}},                           // re-clone
		{Result: execx.Result{}},                           // fetch
		{Result: execx.Result{Stdout: []byte("aaa111\n")}}, // rev-parse
	}}
	c := New(fake, nil, discard())
	sha, err := c.SyncAndResolve(context.Background(), "https://example.com/r.git", "main", dir)
	if err != nil {
		t.Fatal(err)
	}
	if sha != "aaa111" {
		t.Errorf("sha = %q", sha)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Error("corrupted clone was not wiped")
	}
	if fake.Calls[1].Args[0] != "clone" {
		t.Errorf("second call should be re-clone, got %v", fake.Calls[1].Args)
	}
}

func TestNonCorruptionErrorIsNotRetried(t *testing.T) {
	t.Parallel()
	dir := mkClone(t)
	fake := &execx.FakeRunner{Script: []execx.Response{
		{Result: execx.Result{ExitCode: 128}, Err: errors.New("fatal: could not resolve host bitbucket.org")},
	}}
	c := New(fake, nil, discard())
	if _, err := c.SyncAndResolve(context.Background(), "https://example.com/r.git", "main", dir); err == nil {
		t.Fatal("want network error to propagate")
	}
	if len(fake.Calls) != 1 {
		t.Errorf("network error must not trigger re-clone, got %d calls", len(fake.Calls))
	}
}
