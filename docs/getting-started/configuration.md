---
title: Configuration
icon: lucide/settings
---

# Configuration

plico reads a single TOML file, by default `/etc/plico/config.toml`. The
commented reference template is installed alongside it as
`config.toml.example`; the full key-by-key reference is
[Reference → Server configuration](../reference/config.md).

## A minimal config

```toml
base_dir      = "/opt/docker"
state_file    = "/var/lib/plico/state.json"
timezone      = "Europe/Paris"
poll_interval = "60s"

[api]
socket = "/run/plico/plico.sock"

[[stack]]
name         = "webapp"                       # directory AND compose project name
repo         = "https://github.com/acme/webapp-deploy.git"
ref          = "main"                         # branch to follow
compose_file = "docker-compose.yml"
force_pull   = true
```

That is enough to poll `webapp` every 60 s and deploy `main` whenever it moves.

## Secrets in the config: `${ENV}` interpolation

Never write a token literally in `config.toml`. Reference it with
`${ENV_VAR}` and provide the value through `/etc/plico/plico.env`:

```toml
[git.auths."github.com"]
username = "deploy-bot"
password = "${PLICO_GITHUB_TOKEN}"
```

```sh
# /etc/plico/plico.env  (loaded by systemd, kept out of the unit)
PLICO_GITHUB_TOKEN=ghp_xxx
```

!!! danger "Missing variables fail loudly"
    A referenced-but-undefined `${VAR}` prevents the daemon from starting and
    fails `plico validate`. plico never starts half-configured.

## A fuller example

A more realistic setup: a nightly global window with per-stack overrides, a
private git host, a global backup gate, one plain stack, one private monorepo
stack with encrypted env, and a third-party repo whose own hooks are disabled.

```toml
# ── Global ───────────────────────────────────────────────────────────
base_dir      = "/opt/docker"
state_file    = "/var/lib/plico/state.json"
timezone      = "Europe/Paris"
poll_interval = "60s"
run_timeout   = "30m"
max_concurrent_deploys = 2

# Default deployment window (overridable, or opt out, per stack).
schedule = "0 3 * * *"          # every day at 03:00…
window   = "2h"                 # …open for 2 hours

drift_interval = "2m"           # re-check deployed stacks for regressions

[api]
socket = "/run/plico/plico.sock"

[health]
listen = "127.0.0.1:9444"       # GET /healthz

[heartbeat]                     # outbound dead-man's switch
url      = "https://kuma.example.com/api/push/aBcD1234?status=up&msg=OK"
interval = "30s"

[ntfy]                          # failure-oriented by default
url   = "https://ntfy.example.com/plico"
token = "${PLICO_NTFY_TOKEN}"

# Global operator backup gate: runs before EVERY stack, blocking.
[hooks]
pre_deploy      = ["/opt/plico/hooks/backup.sh"]
env_passthrough = ["RESTIC_PASSWORD", "RESTIC_REPOSITORY"]

# Private git over HTTPS, keyed by exact hostname. Token via plico.env.
[git.auths."github.com"]
username = "deploy-bot"
password = "${PLICO_GITHUB_TOKEN}"

# Private container registry, keyed by host (the analog of [git.auths]). The
# token reaches docker via plico's credential helper, never written to disk.
[registry.auths."ghcr.io"]
username = "my-user"
password = "${PLICO_GHCR_TOKEN}"

# ── Stacks ───────────────────────────────────────────────────────────
# 1) Plain public stack; ignores the global window, deploys as soon as main moves.
[[stack]]
name     = "blog"
repo     = "https://github.com/acme/blog-deploy.git"
ref      = "main"
schedule = "@poll"              # opt out of the global window

# 2) Private monorepo stack: one subdir of a shared infra repo (uses the
#    [git.auths] above), encrypted env, its own 30-min window, private image.
[[stack]]
name            = "nextcloud"
repo            = "https://github.com/acme/infra.git"
ref             = "prod"
path            = "nextcloud"                    # <repo>/nextcloud/…
sops_files      = ["secrets/nextcloud.enc.env"]  # decrypted in memory
schedule        = "0 4 * * *"                    # override: 04:00…
window          = "30m"
env_passthrough = ["RESTIC_PASSWORD"]            # extra var for this stack's hook

# 3) Third-party repo: keep its compose recipe, but run none of its hooks.
#    The global backup gate above still applies.
[[stack]]
name       = "dashboard"
repo       = "https://gitea.example.com/tools/dashboard.git"
ref        = "main"
repo_hooks = false
```

Each block has a deep dive:

- [Server configuration](../reference/config.md) — every top-level key and table.
- [Per-stack options](../reference/stack-options.md) — the full `[[stack]]` set.
- [Authentication](../concepts/authentication.md) — private git and registries.
- [Scheduling & windows](../concepts/scheduling.md) — the `schedule`/`window` model.
- [Layered hooks](../concepts/hooks.md) — the global backup gate and `repo_hooks`.

The exhaustive annotated template ships in the repo as `config.example.toml`
(installed as `/etc/plico/config.toml.example`).

## Validate before you restart

```sh
plico validate            # checks the config without starting the daemon
sudo systemctl restart plico
```

A **restart is the reload mechanism** — there is no SIGHUP. It drains in-flight
runs and re-opens any still-open schedule window from the persisted anchor. See
[Operations → Supervision](../operations/supervision.md).

## Private repositories & registries

If your stack lives in a **private git repo** or pulls from a **private
container registry**, set that up before the first deploy — see
[Concepts → Authentication](../concepts/authentication.md). Registry credentials
in particular are not a plico config key: you `docker login` as the plico user.

## What to configure next

- [Scheduling & windows](../concepts/scheduling.md) — restrict deploys to a
  nightly window.
- [Notifications](../guides/notifications.md) — ntfy, webhook, SMTP.
- [Secrets](../concepts/secrets.md) — SOPS-encrypted env and files.
- [Layered hooks](../concepts/hooks.md) — the backup gate and operator hooks.
