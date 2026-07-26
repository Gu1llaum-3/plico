---
title: Server configuration
icon: lucide/file-cog
---

# Server configuration reference

plico reads one TOML file (default `/etc/plico/config.toml`). The commented
template is installed as `config.toml.example`. Secrets are referenced with
`${ENV_VAR}` and provided through `/etc/plico/plico.env`; a missing variable
prevents startup and fails `plico validate`.

## Top-level keys

| Key | Type | Default | Meaning |
|---|---|---|---|
| `base_dir` | path | — | Where git worktrees live (`<base_dir>/<stack>`). Rebuildable; never store irreplaceable data here. |
| `state_file` | path | — | Persistent deployment + schedule state. **Back this up.** |
| `timezone` | string | — | IANA zone in which `schedule` cron expressions are evaluated. |
| `poll_interval` | duration | `60s` | How often each stack's ref is checked. |
| `schedule` | cron / `@…` | — | Optional global deployment schedule (overridable per stack). See [Scheduling](../concepts/scheduling.md). |
| `window` | duration | `1h` | Duration of the window opened by each firing. |
| `check` | bool | `false` | Out-of-window: fetch + diff each tick and notify "queued" without deploying. |
| `drift_check` | bool | `true` | Enable [drift detection](../concepts/monitoring.md) (overridable per stack). |
| `drift_interval` | duration | `2m` | How often deployed stacks are re-checked for drift (min 5 s). |
| `run_timeout` | duration | `30m` | Hard ceiling for one full deployment run. |
| `max_concurrent_deploys` | int | `2` | How many stacks may deploy at the same time. |

## `[log]`

```toml
[log]
# path = "/var/log/plico/plico.log"   # empty = stderr; rotate via logrotate
level = "info"                        # debug | info | warn | error
```

## `[health]`

```toml
[health]
listen = "127.0.0.1:9444"   # GET /healthz — semantic healthcheck, local by default
```

## `[heartbeat]`

Outbound dead-man's switch. See [Monitoring](../concepts/monitoring.md).

```toml
[heartbeat]
# url = "https://kuma.example.com/api/push/aBcD1234?status=up&msg=OK"
# interval = "30s"
```

## `[api]`

```toml
[api]
socket = "/run/plico/plico.sock"   # volatile unix socket for the client CLI
```

## Notifications

Three channel types, each with an optional `events` list (see
[Notification events](events.md)). Omitting `events` selects the
failure-oriented default.

```toml
[ntfy]
# url = "https://ntfy.example.com/plico"
# token = "${PLICO_NTFY_TOKEN}"
# events = ["deploy_failed", "pre_hook_failed", "window_missed", "git_sync_failed"]

# [[webhook]]                 # repeatable; generic JSON (Google Chat / Teams)
# url = "https://chat.googleapis.com/v1/spaces/.../messages?key=..."
# events = ["all"]

# [smtp]
# host = "mail.example.com"
# port = 587
# from = "plico@example.com"
# to = ["ops@example.com"]
# username = "plico@example.com"
# password = "${PLICO_SMTP_PASSWORD}"

# git_sync_alert_after = 5    # fire git_sync_failed after N consecutive fetch failures; 0 disables
```

A failing channel never breaks a deployment — the send error is logged locally.

## `[hooks]`

Global operator hooks and the shared hook environment. See
[Layered hooks](../concepts/hooks.md).

```toml
[hooks]
# pre_deploy  = ["/opt/plico/hooks/backup.sh"]   # absolute; run on EVERY stack, blocking, before repo hooks
# post_deploy = ["/opt/plico/hooks/notify.sh"]   # absolute; after repo hooks, non-blocking
timeout = "10m"
# env_passthrough = ["AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"]   # extra hook env, by NAME
```

## `[git.auths."<host>"]`

Per-host HTTPS credentials, keyed by **exact hostname** (`github.com` does not
match `www.github.com`; no port or path matching). plico passes itself as
`GIT_ASKPASS`, so the token appears **neither on disk, nor in argv, nor in
`.git/config`**. A private repo with no matching entry fails fast rather than
hanging (`GIT_TERMINAL_PROMPT=0`).

```toml
[git.auths."bitbucket.org"]
username = "deploy-bot"
password = "${PLICO_BITBUCKET_TOKEN}"
```

**SSH remotes** are delegated to the plico user's `~/.ssh` (agent, deploy key,
`known_hosts`) — plico configures nothing. See
[Concepts → Authentication](../concepts/authentication.md).

## `[registry.auths."<host>"]`

Per-host **container registry** credentials, the analog of `[git.auths]`, keyed
by the registry host (`ghcr.io`, `registry.example.com:5000`, or `docker.io` for
Docker Hub). plico registers itself as a Docker credential helper for these
hosts, so the token is resolved on demand and **never written to disk**. A
declared entry missing its username or password fails `plico validate`.

```toml
[registry.auths."ghcr.io"]
username = "my-user"
password = "${PLICO_GHCR_TOKEN}"
```

Configuring **any** `[registry.auths]` makes it authoritative for plico's pulls:
the plico user's `~/.docker/config.json` is not consulted during a plico pull, so
declare every private registry there. See
[Concepts → Authentication](../concepts/authentication.md) for the mechanism and
the `docker login` alternative.

## `[[stack]]`

The repeated stack table is documented on its own page:
[Per-stack options](stack-options.md).
