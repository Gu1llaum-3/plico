package execx

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
)

func discard() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestRunCapturesOutputAndExitCode(t *testing.T) {
	t.Parallel()
	r := NewRunner(discard())
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo out; echo err >&2"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); got != "out" {
		t.Errorf("stdout = %q, want %q", got, "out")
	}
	if got := strings.TrimSpace(string(res.Stderr)); got != "err" {
		t.Errorf("stderr = %q, want %q", got, "err")
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
}

func TestRunNonZeroExitIsError(t *testing.T) {
	t.Parallel()
	r := NewRunner(discard())
	res, err := r.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo boom >&2; exit 3"}})
	if err == nil {
		t.Fatal("want error on exit 3")
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("error should carry stderr tail, got: %v", err)
	}
}

func TestRunTimeoutKillsProcessGroup(t *testing.T) {
	t.Parallel()
	r := NewRunner(discard())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	// The child spawns its own child; both must die with the group.
	_, err := r.Run(ctx, Cmd{Name: "sh", Args: []string{"-c", "sleep 30 & wait"}})
	if err == nil {
		t.Fatal("want error on timeout")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("run did not return promptly after cancel: %s", elapsed)
	}
}

func TestRunAppendsEnv(t *testing.T) {
	t.Setenv("PLICO_INHERITED_VAR", "from-daemon")
	r := NewRunner(discard())
	res, err := r.Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", "printf '%s|%s' \"$PLICO_TEST_VAR\" \"$PLICO_INHERITED_VAR\""},
		Env: []string{"PLICO_TEST_VAR=hello"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Default: appended to os.Environ() — both the explicit and the
	// inherited var are visible (git/sops/compose rely on this).
	if got := string(res.Stdout); got != "hello|from-daemon" {
		t.Errorf("default env = %q, want %q", got, "hello|from-daemon")
	}
}

func TestRunCleanEnvDoesNotInherit(t *testing.T) {
	t.Setenv("PLICO_INHERITED_VAR", "from-daemon")
	r := NewRunner(discard())
	res, err := r.Run(context.Background(), Cmd{
		Name: "sh", Args: []string{"-c", "printf '%s|%s' \"$PLICO_ONLY\" \"$PLICO_INHERITED_VAR\""},
		Env:      []string{"PLICO_ONLY=x", "PATH=" + os.Getenv("PATH")},
		CleanEnv: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// CleanEnv: the command sees ONLY what Env provides — the daemon's
	// inherited var must be absent (this is how hooks are scoped).
	if got := string(res.Stdout); got != "x|" {
		t.Errorf("clean env = %q, want %q (inherited var must be absent)", got, "x|")
	}
}

func TestRunCleanEnvNilStillScopes(t *testing.T) {
	t.Setenv("PLICO_INHERITED_VAR", "from-daemon")
	r := NewRunner(discard())
	// A nil Env with CleanEnv MUST yield an empty environment, not inherit
	// the daemon's (Go's os/exec treats nil cmd.Env as "inherit").
	res, err := r.Run(context.Background(), Cmd{
		Name: "/bin/sh", Args: []string{"-c", "printf '%s' \"$PLICO_INHERITED_VAR\""},
		Env:      nil,
		CleanEnv: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(res.Stdout); got != "" {
		t.Errorf("nil clean env leaked the daemon environment: %q", got)
	}
}

func TestEnvNames(t *testing.T) {
	t.Parallel()
	// Only keys, never values — a secret whose name looks innocent
	// (DATABASE_URL) must not have its value logged.
	got := EnvNames([]string{"DATABASE_URL=postgres://u:p@h/db", "PATH=/bin"})
	if len(got) != 2 || got[0] != "DATABASE_URL" || got[1] != "PATH" {
		t.Errorf("EnvNames = %v", got)
	}
	for _, s := range got {
		if strings.Contains(s, "=") {
			t.Errorf("EnvNames leaked a value: %q", s)
		}
	}
}

func TestRedact(t *testing.T) {
	t.Parallel()
	in := []string{
		"PLICO_GIT_PASSWORD=s3cret",
		"MY_TOKEN=abc",
		"API_SECRET=x",
		"SOPS_AGE_KEY=k",
		"PLAIN=ok",
	}
	out := Redact(in)
	for i, kv := range out[:4] {
		if !strings.HasSuffix(kv, "=***") {
			t.Errorf("entry %d not redacted: %q", i, kv)
		}
	}
	if out[4] != "PLAIN=ok" {
		t.Errorf("non-secret entry modified: %q", out[4])
	}
	if in[0] != "PLICO_GIT_PASSWORD=s3cret" {
		t.Error("Redact must not mutate its input")
	}
}

func TestTail(t *testing.T) {
	t.Parallel()
	if got := Tail([]byte("  short \n"), 100); got != "short" {
		t.Errorf("Tail = %q", got)
	}
	long := strings.Repeat("a", 50) + "END"
	if got := Tail([]byte(long), 10); got != "…"+long[len(long)-10:] {
		t.Errorf("Tail = %q", got)
	}
}

func TestFakeRunnerScriptAndRecording(t *testing.T) {
	t.Parallel()
	f := &FakeRunner{Script: []Response{
		{Result: Result{Stdout: []byte("one")}},
		{Result: Result{ExitCode: 1}, Err: context.DeadlineExceeded},
	}}
	res, err := f.Run(context.Background(), Cmd{Name: "git", Args: []string{"fetch"}})
	if err != nil || string(res.Stdout) != "one" {
		t.Fatalf("first scripted call: res=%v err=%v", res, err)
	}
	if _, err := f.Run(context.Background(), Cmd{Name: "git"}); err == nil {
		t.Fatal("second scripted call should error")
	}
	if _, err := f.Run(context.Background(), Cmd{Name: "git"}); err == nil {
		t.Fatal("exhausted script should error")
	}
	if len(f.Calls) != 3 || f.Calls[0].Args[0] != "fetch" {
		t.Errorf("calls not recorded: %+v", f.Calls)
	}
}
