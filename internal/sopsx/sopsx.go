// Package sopsx builds the SOPS decryption plumbing. Secrets are never
// written in clear to persistent storage (F16): the default mode wraps the
// compose command with `sops exec-env`, the fallback decrypts to a tmpfs.
package sopsx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Gu1llaum-3/plico/internal/execx"
)

// ExecEnvArgv wraps argv so it runs with the decrypted content of files in
// its environment. `sops exec-env` takes exactly two arguments — the file
// and the command as ONE string, executed via `sh -c` — so the wrapped
// command is shell-quoted, and multiple files nest:
//
//	sops exec-env a.enc.env 'sops exec-env b.enc.env '\''docker compose … up -d'\'''
//
// Each nesting level enriches the environment of the next; on duplicate
// keys the LAST file of the list wins (innermost level). Returns argv
// unchanged when files is empty.
func ExecEnvArgv(files, argv []string) []string {
	if len(files) == 0 {
		return argv
	}
	cmd := shJoin(argv)
	// Wrap from the last file (innermost) up to the second one; the first
	// file becomes the real argv so no shell is involved at the top level.
	for i := len(files) - 1; i >= 1; i-- {
		cmd = "sops exec-env " + shQuote(files[i]) + " " + shQuote(cmd)
	}
	return []string{"sops", "exec-env", files[0], cmd}
}

// shQuote single-quotes s for POSIX sh, escaping embedded single quotes.
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func shJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = shQuote(a)
	}
	return strings.Join(quoted, " ")
}

// EnvFiles is the tmpfs-mode result: --env-file arguments for compose plus a
// cleanup function that must run unconditionally (success, failure, timeout).
type EnvFiles struct {
	Args    []string
	Cleanup func()
}

// DecryptToTmpfs decrypts each file to a per-run directory under tmpfsRoot
// (normally /dev/shm, verified by CheckTmpfs at startup). Decryption goes
// through `sops decrypt --output`: the cleartext never transits a shell.
func DecryptToTmpfs(ctx context.Context, r execx.Runner, files []string, tmpfsRoot, stack, runID string) (EnvFiles, error) {
	dir := filepath.Join(tmpfsRoot, "plico", stack+"-"+runID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return EnvFiles{}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }

	var args []string
	for i, f := range files {
		out := filepath.Join(dir, fmt.Sprintf("secrets-%d.env", i))
		if _, err := r.Run(ctx, execx.Cmd{
			Name: "sops",
			Args: []string{"decrypt", "--output", out, f},
		}); err != nil {
			cleanup()
			return EnvFiles{}, fmt.Errorf("sops decrypt %s: %w", f, err)
		}
		if err := os.Chmod(out, 0o600); err != nil {
			cleanup()
			return EnvFiles{}, err
		}
		args = append(args, "--env-file", out)
	}
	return EnvFiles{Args: args, Cleanup: cleanup}, nil
}
