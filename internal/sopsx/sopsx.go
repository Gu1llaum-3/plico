// Package sopsx builds the SOPS decryption plumbing. Secrets are never
// written in clear to persistent storage (F16): the default mode wraps the
// compose command with `sops exec-env`, the fallback decrypts to a tmpfs.
package sopsx

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"plico/internal/execx"
)

// Prefix returns the argv prefix chaining `sops exec-env` for each file, to
// be placed before the compose command:
//
//	sops exec-env a.enc.env -- sops exec-env b.enc.env -- docker compose ...
//
// Each level enriches the environment passed to the next; on duplicate keys
// the LAST file of the list wins. Returns nil when files is empty.
func Prefix(files []string) []string {
	var p []string
	for _, f := range files {
		p = append(p, "sops", "exec-env", f, "--")
	}
	return p
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
