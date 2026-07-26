---
title: Authentication
icon: lucide/key-square
---

# Authentication

plico needs credentials in two places: to **fetch** a private git repo, and to
**pull** an image from a private container registry. Both are configured in
plico the same way — a per-host table with the token via `${ENV}` — and in both
cases the token is resolved on demand and **never written to disk**. (SSH git
remotes and, if you prefer, registries can instead be delegated to the host.)

| What | How plico handles it |
|---|---|
| Private git over HTTPS | **Configured in plico** — `[git.auths."<host>"]` |
| Private git over SSH | **Delegated** to the plico user's `~/.ssh` |
| Private container registry | **Configured in plico** — `[registry.auths."<host>"]` (or delegated to `docker login`) |

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

Configure registry credentials per host, exactly like git — a `[registry.auths]`
table keyed by the registry host, with the token via `${ENV}`:

```toml
# /etc/plico/config.toml
[registry.auths."ghcr.io"]
username = "my-user"
password = "${PLICO_GHCR_TOKEN}"

# Docker Hub: key it "docker.io" (plico maps it to the index URL docker expects).
[registry.auths."docker.io"]
username = "my-user"
password = "${PLICO_DOCKERHUB_TOKEN}"
```

```sh
# /etc/plico/plico.env
PLICO_GHCR_TOKEN=ghp_xxx
PLICO_DOCKERHUB_TOKEN=dckr_pat_xxx
```

How it works — the same idea as git's `GIT_ASKPASS`:

- **The token is never written to a docker config or credential store.** plico
  registers itself as a Docker **credential helper** (`docker-credential-plico`)
  and points the pull at a throwaway `DOCKER_CONFIG` holding only that *routing*
  — no credential. docker asks the helper on demand, which resolves the token
  from the config. As with any `${ENV}` secret in `plico.env` (git tokens
  included), the token does live in the daemon's environment; plico simply never
  serialises it to a file the way `docker login` does.
- **`docker.io` is the friendly key for Docker Hub;** plico translates it to the
  `https://index.docker.io/v1/` key docker actually uses.
- **Server-only.** Like `[git.auths]`, a repo manifest can never set a registry
  credential.
- **A missing token fails loudly** at config load (`plico validate`): a declared
  `[registry.auths]` entry with no username or password is an error, not a
  silent skip.

!!! warning "`[registry.auths]` is the source of truth for pull auth"
    When you configure **any** `[registry.auths]`, plico uses its own
    `DOCKER_CONFIG` for pulls, so the plico user's `~/.docker/config.json` is
    **not** consulted during a plico pull. Declare **every** private registry
    plico must pull from in `[registry.auths]` — don't mix it with a host
    `docker login` for pulls. (Registries you *don't* configure fall back to an
    anonymous pull.)

### Alternative: `docker login` on the host

If you'd rather not put registry tokens in plico's config — or a **hook** runs
its own `docker pull` — log in as the plico user instead. This is independent of
`[registry.auths]`:

```sh
echo "$REGISTRY_TOKEN" | sudo -u plico env HOME=/var/lib/plico \
  docker login ghcr.io -u my-user --password-stdin
```

That writes `/var/lib/plico/.docker/config.json` (owned by `plico`). Make sure
you log in **as `plico`**, not as your own user or `root` — a config under
`/root` or `/home/you` is invisible to the daemon.

!!! note "Hooks pull with the host config, not `[registry.auths]`"
    `[registry.auths]` covers plico's own `docker compose pull`/`up`. A
    [hook](hooks.md) that runs `docker pull` itself gets the host's
    `DOCKER_CONFIG`/`HOME` (passed through as non-secret pointers), so a
    hook that pulls a private image needs the `docker login` above.
