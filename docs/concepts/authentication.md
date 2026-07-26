---
title: Authentication
icon: lucide/key-square
---

# Authentication

plico needs credentials in two places: to **fetch** a private git repo, and to
**pull** an image from a private container registry. It handles the two very
differently — one is configured in plico, the other is delegated to the host's
Docker setup.

| What | How plico handles it |
|---|---|
| Private git over HTTPS | **Configured in plico** — `[git.auths."<host>"]` |
| Private git over SSH | **Delegated** to the plico user's `~/.ssh` |
| Private container registry | **Delegated** to `docker login` as the plico user (native `[registry.auths]` planned) |

`repo` and `[git.auths]` are [server-only](trust-boundary.md) — bootstrap and
credentials never come from a repo.

## Private git over HTTPS

Add one table per host, keyed by **exact hostname**, and reference the token
through `${ENV}` interpolation (never inline it):

```toml
# /etc/plico/config.toml
[git.auths."github.com"]
username = "deploy-bot"
password = "${PLICO_GITHUB_TOKEN}"

[[stack]]
name = "webapp"
repo = "https://github.com/acme/webapp-deploy.git"
ref  = "main"
```

```sh
# /etc/plico/plico.env   (loaded by systemd, mode 0600)
PLICO_GITHUB_TOKEN=ghp_xxx
```

How it works and what to watch for:

- **Exact hostname match.** The key must equal the repo URL's host exactly —
  `github.com` does **not** match `www.github.com`, and there is no port or path
  matching. A miss means the repo is treated as public (and a private fetch then
  fails).
- **The token never touches disk, argv, or `.git/config`.** plico passes itself
  as `GIT_ASKPASS`; the credential reaches git only over the askpass channel,
  from environment variables that exist solely in the git subprocess.
- **Missing auth fails fast.** `GIT_TERMINAL_PROMPT=0` means a private repo with
  no matching `[git.auths]` errors immediately instead of hanging on a prompt.
  Watch for a `git_sync_failed` notification.

Use a scoped **deploy token** (read-only, repo-scoped) rather than a personal
token where your host supports it (GitHub fine-grained tokens, GitLab deploy
tokens, Bitbucket app passwords, Gitea tokens).

## Private git over SSH

For an `ssh://` or `git@host:...` remote, plico configures **nothing** — no key,
no `known_hosts`, no `StrictHostKeyChecking`. The git subprocess inherits the
daemon's environment, so SSH auth is entirely the **plico user's `~/.ssh`
setup**. Set it up as that user:

```sh
# Home is /var/lib/plico; create the .ssh dir as the plico user
sudo -u plico install -d -m 0700 /var/lib/plico/.ssh

# Install a read-only deploy key (private key readable ONLY by plico)
sudo install -o plico -g plico -m 0600 ./deploy_key /var/lib/plico/.ssh/id_ed25519

# Pre-seed the host key so the first fetch doesn't fail on an unknown host
sudo -u plico sh -c 'ssh-keyscan github.com >> /var/lib/plico/.ssh/known_hosts'
```

Register the **public** key as a deploy key on the git host. Because plico does
no host-key handling, an unseeded `known_hosts` will make the first fetch fail
(the daemon is non-interactive) — the `ssh-keyscan` step above avoids that.

## Private container registry

plico does **not** manage registry credentials *itself* today. `docker compose
pull` runs with the daemon's inherited environment, so it authenticates as the
**plico user** via the `docker` CLI's own `config.json`.

This is a deliberate **default for simplicity, not a limitation**: native
per-host `[registry.auths]` (mirroring `[git.auths]`) is a *planned* feature —
it is feasible via the same credential-helper mechanism plico already uses for
git, without reimplementing registry login (docker still does the real pull).
It is tracked in the repository's `ROADMAP.md`. Until then, use `docker login`.

Log in **once, as the plico user**, so the credentials land where the daemon
will read them (`$HOME/.docker/config.json`, and plico's home is
`/var/lib/plico`):

```sh
echo "$REGISTRY_TOKEN" | sudo -u plico env HOME=/var/lib/plico \
  docker login ghcr.io -u my-user --password-stdin
```

That writes `/var/lib/plico/.docker/config.json` (owned by `plico`), and every
subsequent `docker compose pull` uses it automatically — no plico config needed.

!!! tip "Prefer a shared config dir?"
    Instead of the plico user's home, you can point Docker at a fixed directory
    by adding `DOCKER_CONFIG=/etc/plico/docker` to `/etc/plico/plico.env`, then
    `sudo -u plico env DOCKER_CONFIG=/etc/plico/docker docker login …`. The
    daemon inherits `DOCKER_CONFIG` from `plico.env`, and hooks receive it too
    (see below).

!!! warning "Registry pull fails after a login that looked fine?"
    Make sure you logged in **as `plico`**, not as your own user or `root` — a
    `~/.docker/config.json` under `/root` or `/home/you` is invisible to the
    daemon. Verify with `sudo -u plico env HOME=/var/lib/plico docker pull <img>`.

### Hooks and the registry

A [hook](hooks.md) that runs its own `docker` commands finds the same registry
config: `DOCKER_CONFIG`, `HOME`, and `DOCKER_CONTEXT` are passed through the
otherwise-scrubbed hook environment as non-secret pointers, so a hook's `docker`
CLI reads the same `config.json` the daemon does.
