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

	"plico/internal/config"
	"plico/internal/execx"
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
