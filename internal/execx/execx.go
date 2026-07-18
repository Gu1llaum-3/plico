// Package execx runs external commands (git, sops, docker) behind a small
// interface so every caller can be tested without real binaries.
package execx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// MaxCapture bounds how much stdout/stderr is kept per command.
const MaxCapture = 4 << 20 // 4 MiB

// Cmd describes one external command invocation.
type Cmd struct {
	Name  string // binary name, e.g. "git", "sops", "docker"
	Args  []string
	Dir   string   // working directory ("" = inherit)
	Env   []string // appended to os.Environ(), never a replacement
	Stdin io.Reader
}

// Result holds the captured output of a finished command.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Runner executes commands. The real implementation honors ctx cancellation
// by killing the whole process group.
type Runner interface {
	// Run returns a non-nil error when the command could not start, was
	// cancelled, or exited non-zero. Result is populated in all cases.
	Run(ctx context.Context, c Cmd) (Result, error)
}

type osRunner struct {
	log *slog.Logger
}

// NewRunner returns the real Runner backed by os/exec.
func NewRunner(log *slog.Logger) Runner {
	return &osRunner{log: log}
}

func (r *osRunner) Run(ctx context.Context, c Cmd) (Result, error) {
	// Retry ETXTBSY: on Linux, exec-ing a freshly written script can race a
	// concurrent fork that inherited the (already closed) write descriptor
	// (golang/go#22315). Transient by nature; a short retry absorbs it.
	for attempt := 0; ; attempt++ {
		res, err := r.runOnce(ctx, c)
		if err != nil && errors.Is(err, syscall.ETXTBSY) && attempt < 3 && ctx.Err() == nil {
			time.Sleep(time.Duration(10<<attempt) * time.Millisecond)
			continue
		}
		return res, err
	}
}

func (r *osRunner) runOnce(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Stdin = c.Stdin

	var stdout, stderr boundedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// New process group so that cancellation kills children too
	// (a timed-out `docker compose pull` must not leave zombies behind).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 5 * time.Second

	start := time.Now()
	err := cmd.Run()

	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: -1}
	if cmd.ProcessState != nil {
		res.ExitCode = cmd.ProcessState.ExitCode()
	}

	r.log.Debug("command finished",
		"cmd", c.Name,
		"args", strings.Join(c.Args, " "),
		"dir", c.Dir,
		"env", Redact(c.Env),
		"exit_code", res.ExitCode,
		"duration", time.Since(start).Round(time.Millisecond).String(),
	)

	if err != nil {
		// Prefer the context error: callers distinguish timeouts from failures.
		if ctx.Err() != nil {
			return res, fmt.Errorf("%s %s: %w", c.Name, firstArg(c.Args), ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return res, fmt.Errorf("%s %s: exit %d: %s", c.Name, firstArg(c.Args), res.ExitCode, Tail(res.Stderr, 512))
		}
		return res, fmt.Errorf("%s %s: %w", c.Name, firstArg(c.Args), err)
	}
	return res, nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// Tail returns the last max bytes of b, trimmed, for human-facing messages.
func Tail(b []byte, max int) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

// Redact masks values of env entries whose key looks secret. Always use it
// when logging Cmd.Env.
func Redact(env []string) []string {
	out := make([]string, len(env))
	for i, kv := range env {
		key, _, ok := strings.Cut(kv, "=")
		up := strings.ToUpper(key)
		if ok && (strings.Contains(up, "PASSWORD") || strings.Contains(up, "TOKEN") || strings.Contains(up, "SECRET") || strings.Contains(up, "KEY")) {
			out[i] = key + "=***"
		} else {
			out[i] = kv
		}
	}
	return out
}

// boundedBuffer keeps at most MaxCapture bytes and drops the rest.
type boundedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	room := MaxCapture - b.buf.Len()
	if room <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > room {
		b.truncated = true
		b.buf.Write(p[:room])
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *boundedBuffer) Bytes() []byte { return b.buf.Bytes() }
