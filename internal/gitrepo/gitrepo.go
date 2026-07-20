// Package gitrepo drives the git CLI: incremental clone/fetch per stack and
// per-domain HTTPS authentication without ever writing a secret to disk (F4).
//
// Auth mechanism: GIT_ASKPASS points back to the plico binary itself, which
// answers git's username/password prompts from environment variables that
// exist only in the git subprocess (see AskpassEnvFlag handling in main).
package gitrepo

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/Gu1llaum-3/plico/internal/config"
	"github.com/Gu1llaum-3/plico/internal/execx"
)

// Environment contract between gitrepo and the askpass mode of main().
const (
	AskpassEnvFlag = "PLICO_INTERNAL_ASKPASS"
	EnvUsername    = "PLICO_GIT_USERNAME"
	EnvPassword    = "PLICO_GIT_PASSWORD"
)

type Client struct {
	runner execx.Runner
	auths  map[string]config.GitAuth // key = host
	log    *slog.Logger
}

func New(r execx.Runner, auths map[string]config.GitAuth, log *slog.Logger) *Client {
	return &Client{runner: r, auths: auths, log: log}
}

// SyncAndResolve makes dir an up-to-date clone of repoURL (clone on first
// run, fetch afterwards) and returns the SHA of origin/<ref>. A corrupted
// local clone is wiped and re-cloned: it is only a cache, origin is the
// source of truth.
func (c *Client) SyncAndResolve(ctx context.Context, repoURL, ref, dir string) (string, error) {
	sha, err := c.syncOnce(ctx, repoURL, ref, dir)
	if err != nil && isCorruption(err) && ctx.Err() == nil {
		c.log.Warn("local clone looks corrupted, re-cloning", "dir", dir, "error", err)
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return "", fmt.Errorf("removing corrupted clone: %w", rmErr)
		}
		sha, err = c.syncOnce(ctx, repoURL, ref, dir)
	}
	return sha, err
}

func (c *Client) syncOnce(ctx context.Context, repoURL, ref, dir string) (string, error) {
	if _, statErr := os.Stat(filepath.Join(dir, ".git")); statErr != nil {
		// The clone dir is only a cache. A leftover dir without .git (an
		// interrupted first clone, manual debris) would make `git clone`
		// refuse "destination path already exists": wipe it first.
		if _, err := os.Stat(dir); err == nil {
			c.log.Warn("removing leftover non-git directory before clone", "dir", dir)
			if err := os.RemoveAll(dir); err != nil {
				return "", fmt.Errorf("removing leftover dir: %w", err)
			}
		}
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			return "", err
		}
		if _, err := c.git(ctx, "", repoURL, "clone", "--no-checkout", repoURL, dir); err != nil {
			return "", err
		}
	}
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", ref, ref)
	if _, err := c.git(ctx, dir, repoURL, "fetch", "--prune", "origin", refspec); err != nil {
		return "", err
	}
	res, err := c.git(ctx, dir, repoURL, "rev-parse", "refs/remotes/origin/"+ref)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// LogRange returns the one-line log of commits in (old, new], for dry-run
// reports.
func (c *Client) LogRange(ctx context.Context, repoURL, dir, oldSHA, newSHA string) ([]string, error) {
	res, err := c.git(ctx, dir, repoURL, "log", "--oneline", "--no-decorate", oldSHA+".."+newSHA)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		if line != "" {
			out = append(out, line)
		}
	}
	return out, nil
}

// PathChanged reports whether any file under repoRelPath differs between
// oldSHA and newSHA. It backs the monorepo scoping: a stack rooted at a
// subdirectory must not redeploy when a commit only touched a sibling
// subtree. It is a local object-store operation (no worktree, no network),
// so it is safe to call before the checkout. An error (e.g. oldSHA gone
// after an upstream force-push) is returned so the caller can fail open.
func (c *Client) PathChanged(ctx context.Context, repoURL, dir, oldSHA, newSHA, repoRelPath string) (bool, error) {
	// `:(literal)` disables pathspec glob magic, so a directory whose name
	// contains *, ?, [ ] or a leading : is matched verbatim. Without it such a
	// name would be read as a glob and could silently match nothing — a missed
	// deploy, the one failure mode this whole feature must never have.
	pathspec := ":(literal)" + repoRelPath
	res, err := c.git(ctx, dir, repoURL, "diff", "--name-only", oldSHA+".."+newSHA, "--", pathspec)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(res.Stdout)) != "", nil
}

// CheckoutDetached puts the worktree at sha (F: versioned hooks and compose
// files come from the exact revision being deployed).
func (c *Client) CheckoutDetached(ctx context.Context, repoURL, dir, sha string) error {
	_, err := c.git(ctx, dir, repoURL, "checkout", "--quiet", "--force", "--detach", sha)
	return err
}

func (c *Client) git(ctx context.Context, dir, repoURL string, args ...string) (execx.Result, error) {
	return c.runner.Run(ctx, execx.Cmd{
		Name: "git",
		Args: args,
		Dir:  dir,
		Env:  c.authEnv(repoURL),
	})
}

// authEnv returns the askpass environment for repoURL, or nil when no auth
// is configured for its host (public repo) or the URL is not HTTPS (ssh is
// delegated to the system agent).
func (c *Client) authEnv(repoURL string) []string {
	u, err := url.Parse(repoURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil
	}
	auth, ok := c.auths[u.Hostname()]
	if !ok {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		c.log.Error("cannot resolve own executable for GIT_ASKPASS", "error", err)
		return nil
	}
	return []string{
		"GIT_ASKPASS=" + exe,
		"GIT_TERMINAL_PROMPT=0",
		AskpassEnvFlag + "=1",
		EnvUsername + "=" + auth.Username,
		EnvPassword + "=" + auth.Password,
	}
}

var corruptionMarkers = []string{
	"not a git repository",
	"object file",
	"loose object",
	"is corrupt",
	"bad object",
	"unable to read tree",
}

func isCorruption(err error) bool {
	msg := strings.ToLower(err.Error())
	for _, m := range corruptionMarkers {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}
