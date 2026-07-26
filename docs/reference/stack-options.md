---
title: Per-stack options
icon: lucide/boxes
---

# Per-stack options (`[[stack]]`)

Each `[[stack]]` table declares one stack. `name`, `repo` and `ref` are the
minimum. Many keys override a global default for this stack only.

```toml
[[stack]]
name         = "webapp"        # directory name AND compose project name
repo         = "https://bitbucket.org/acme/webapp-deploy.git"
ref          = "main"          # branch to follow
compose_file = "docker-compose.yml"
force_pull   = true
sops_files   = ["secrets/app.enc.env"]
sops_mode    = "exec-env"
verify_timeout = "90s"
```

## Reference

| Key | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | — | **Required.** Directory under `base_dir` and the compose project name. |
| `repo` | URL | — | **Required.** Git remote. Server-only (never from the repo recipe). |
| `ref` | string | — | **Required.** Branch to follow. |
| `path` | string | — | [Monorepo](../concepts/monorepo.md) subdirectory holding this stack. Empty = repo root. |
| `compose_file` | string | `docker-compose.yml` | Compose file, relative to `path`. |
| `force_pull` | bool | `true` | Run `compose pull` before `up`. |
| `sops_files` | list | — | SOPS-encrypted dotenv files (repo-relative). Overrides the repo `[sops]`. |
| `sops_mode` | string | `exec-env` | `exec-env` or `tmpfs` (Linux only). Server-only. |
| `verify_timeout` | duration | `90s` | Post-up healthcheck budget. |
| `hook_timeout` | duration | `[hooks].timeout` | Override the hook timeout for this stack. |
| `env_passthrough` | list | — | Extra hook env variable **names**, added to the global list. Server-only. |
| `schedule` | cron / `@poll` | global | Override the global schedule, or `@poll` to opt out. |
| `window` | duration | global | Override the global window. |
| `check` | bool | global | Out-of-window fetch+diff+notify without deploying. |
| `drift_check` | bool | `true` | Opt this stack out of [drift detection](../concepts/monitoring.md). |
| `global_hooks` | bool | `true` | `false` skips the global `[hooks]` for this stack. |
| `repo_hooks` | bool | `true` | `false` ignores the repo's own hooks (manifest lists **and** `.plico/*.sh`) while keeping the rest of its recipe. |
| `ignore_manifest` | bool | `false` | `true` is a full kill-switch: ignore the entire repo side (recipe **and** hooks). Global `[hooks]` still run. |

## Trust and precedence

Bootstrap/trust keys — `repo`, `ref`, git `auth`, `base_dir`, `env_passthrough`,
`sops_mode`, `schedule` — live **only** here, never in a repo recipe, and the
server always overrides the repo.

Note the asymmetry: the repo-only compose options (`profiles`, `env_files`,
`up_args`, `remove_orphans`, `[secrets]`) have **no per-key server override** —
the server's only lever over them is the wholesale `ignore_manifest` kill-switch.
The full who-sets-what and precedence tables are in
[Server vs repo config](../concepts/trust-boundary.md).

## Monorepo example

```toml
[[stack]]
name = "traefik"
repo = "https://github.com/acme/infra.git"
path = "traefik"      # <repo>/traefik/{docker-compose.yml,.plico/…}

[[stack]]
name = "nextcloud"
repo = "https://github.com/acme/infra.git"
path = "nextcloud"
```

A commit touching only `traefik/` redeploys `traefik` alone.
