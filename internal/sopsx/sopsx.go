// Package sopsx builds the SOPS decryption plumbing. Secrets are never
// written in clear to persistent storage (F16): the default mode wraps the
// compose command with `sops exec-env`, the fallback decrypts to a tmpfs.
package sopsx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

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

// DecryptToDir decrypts each sops-encrypted file into destDir under fixed
// names (a `.enc` marker in the basename is stripped), overwriting on each
// call. Unlike DecryptToTmpfs it is a STABLE, per-stack directory with NO
// cleanup: the decrypted files must outlive the deploy (a container bind-mounts
// them for its whole lifetime). destDir must be a tmpfs path (RAM) so cleartext
// never lands on persistent disk. Files are 0644 (readable by the container's
// user) inside a 0700 dir (host-private). Returns the destination dir.
func DecryptToDir(ctx context.Context, r execx.Runner, files []string, tmpfsRoot, stack string) (string, error) {
	// The per-user base (<root>/plico-<uid>) is created/verified owned-by-us
	// and 0700 (see plicoTmpfsDir). Then recreate the per-stack leaf FRESH each
	// deploy: defeats a planted leaf (the base is plico-controlled, so the ops
	// are race-free) AND clears any stale cleartext from a previous deploy.
	destDir, err := plicoTmpfsDir(tmpfsRoot, stack+"-secrets")
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(destDir); err != nil {
		return "", fmt.Errorf("clearing secrets dir: %w", err)
	}
	if err := os.Mkdir(destDir, 0o700); err != nil {
		return "", err
	}
	for _, f := range files {
		out := filepath.Join(destDir, decryptedName(f))
		if _, err := r.Run(ctx, execx.Cmd{
			Name: "sops",
			Args: []string{"decrypt", "--output", out, f},
		}); err != nil {
			return "", fmt.Errorf("sops decrypt %s: %w", f, err)
		}
		if err := os.Chmod(out, 0o644); err != nil {
			return "", err
		}
	}
	return destDir, nil
}

// decryptedName is the target basename for a decrypted secret file: the source
// basename with a single ".enc" marker removed (server.enc.key -> server.key,
// tls.enc -> tls). Names without ".enc" are kept as-is. Kept in sync with
// config.decryptedSecretName, which validates it at manifest load.
func decryptedName(f string) string {
	base := filepath.Base(f)
	if strings.HasSuffix(base, ".enc") { // tls.enc -> tls
		return base[:len(base)-len(".enc")]
	}
	return strings.Replace(base, ".enc.", ".", 1) // server.enc.key -> server.key
}

// ensureSafeDir makes p a mode-0700 directory, refusing a symlink or a
// wrong-permission dir. In a world-writable tmpfs (/dev/shm is 1777) such a
// path could be planted to redirect cleartext onto disk or expose it to
// another user, so anything unexpected is a hard error, never silently reused.
func ensureOwnedDir(p string) error {
	if err := os.Mkdir(p, 0o700); err == nil {
		return nil // we just created it, we own it
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink; refusing (possible planted secrets path)", p)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", p)
	}
	if fi.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s must be mode 0700, got %o (possible planted secrets path)", p, fi.Mode().Perm())
	}
	if stat, ok := fi.Sys().(*syscall.Stat_t); ok && int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s is not owned by the current user (possible planted secrets path)", p)
	}
	return nil
}

// plicoTmpfsDir returns <tmpfsRoot>/plico-<uid>/<leaf>, ensuring the per-user
// base is a 0700 directory we own (created fresh or verified). The uid scoping
// avoids collisions between plico instances run by different users on one host,
// and the ownership check defeats a path planted in the world-writable tmpfs.
func plicoTmpfsDir(tmpfsRoot, leaf string) (string, error) {
	base := filepath.Join(tmpfsRoot, fmt.Sprintf("plico-%d", os.Getuid()))
	if err := ensureOwnedDir(base); err != nil {
		return "", err
	}
	return filepath.Join(base, leaf), nil
}

// RemoveStackSecrets deletes a stack's decrypted-secrets directory (best
// effort). Called when a stack no longer declares [secrets] so a revoked
// secret does not linger as cleartext in tmpfs until reboot.
func RemoveStackSecrets(tmpfsRoot, stack string) error {
	return os.RemoveAll(filepath.Join(tmpfsRoot, fmt.Sprintf("plico-%d", os.Getuid()), stack+"-secrets"))
}

// DecryptToTmpfs decrypts each file to a per-run directory under tmpfsRoot
// (normally /dev/shm, verified by CheckTmpfs). Decryption goes through
// `sops decrypt --output`: the cleartext never transits a shell.
func DecryptToTmpfs(ctx context.Context, r execx.Runner, files []string, tmpfsRoot, stack, runID string) (EnvFiles, error) {
	dir, err := plicoTmpfsDir(tmpfsRoot, stack+"-"+runID)
	if err != nil {
		return EnvFiles{}, err
	}
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
