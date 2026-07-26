---
title: Server vs repo config
icon: lucide/scale
---

# Server vs repo configuration

plico has **two configuration surfaces**, and knowing which one owns a setting
is the key to a predictable deploy:

- The **server config** — `/etc/plico/config.toml`, written by the operator. It
  holds bootstrap, trust, and policy: which repos, which credentials, schedules,
  notifiers, and per-stack overrides.
- The **repo manifest** — an optional [`.plico.toml`](../reference/manifest.md)
  inside each stack's git repo. It holds the *non-sensitive* deployment recipe
  (compose options, encrypted env/files, hook lists), versioned with the code at
  the deployed revision.

The split is deliberate and strict: **the server can express everything; the
repo can express only orchestration, and the server always wins.**

## Who sets what

| Server-only | Overlapping (server wins) | Repo-only |
|---|---|---|
| `name`, `repo`, `ref`, `path` | `compose_file` | `[compose].profiles` |
| `sops_files`, `sops_mode` | `force_pull` | `[compose].env_files` |
| `schedule`, `window`, `check` | `[sops]` `files` + `mode` | `[compose].up_args` |
| `drift_check`, `verify_timeout`, `hook_timeout` | | `[compose].remove_orphans` |
| `env_passthrough` | | `[secrets].files` |
| `ignore_manifest`, `global_hooks`, `repo_hooks` | | `pre_deploy` / `post_deploy` lists |
| plus file-level: `base_dir`, `git.auths`, `[hooks]`, notifiers, `timezone`, `poll_interval`, … | | (the repo's own `.plico/*.sh` default hooks) |

## Precedence for overlapping keys

When both surfaces set the same thing, the server wins:

| Setting | Resolution |
|---|---|
| `compose_file` | Server value if set, else repo, else `docker-compose.yml`. |
| `force_pull` | Server value if set, else repo, else `true`. |
| `[sops]` files + mode | **Wholesale**: if the server sets `sops_files`, the server's files *and* mode win entirely; otherwise the repo's `[sops]` applies. Not a per-field merge. |

!!! warning "The server has no per-key override for repo-only options"
    `profiles`, `env_files`, `up_args`, `remove_orphans` and `[secrets]` have
    **no** server-side equivalent. The operator cannot, say, add one profile from
    the server side — the repo owns those keys outright. The server's only lever
    over them is the wholesale `ignore_manifest` kill-switch below.

## The kill-switches

Three per-stack toggles, all **server-only**, decide how much of the repo side
runs:

| Setting | Repo recipe (compose/sops/secrets) | Repo hooks (lists + `.plico/*.sh`) | Global `[hooks]` |
|---|---|---|---|
| `ignore_manifest = true` | **dropped** — manifest never loaded | dropped | run (if `global_hooks`) |
| `repo_hooks = false` | **kept** | dropped | run (if `global_hooks`) |
| `global_hooks = false` | kept | kept (if `repo_hooks`) | **dropped** |

In words: `ignore_manifest` means "don't trust this repo's recipe at all";
`repo_hooks = false` is the finer tool — "trust the recipe, but don't run the
repo's scripts"; `global_hooks = false` only affects the operator's own global
[hooks](hooks.md). See [Per-stack options](../reference/stack-options.md) for the
exact keys.

## Why letting the repo choose compose flags is safe

The repo already runs arbitrary code via its hooks. A recipe choosing more
compose flags grants **no new privilege**, and every hook still runs in the same
server-scoped [environment](hooks.md#the-hook-environment-is-scoped). The recipe
is guarded regardless:

- Paths (`compose_file`, `env_files`, `[sops]`, `[secrets]`, hooks) must be
  lexically **repo-relative** — absolute paths and `..` segments are rejected.
- The manifest file itself must **not be a symlink** (no escaping the repo, no
  leaking a host file into a parse-error notification).
- A plico-decrypted secret file lands only in the **plico-owned tmpfs** dir,
  never a repo-chosen path.
- `up_args` is an **allowlist** of safe long-form `up` flags (`--wait`,
  `--pull=…`, `--scale=…`, `--build`, …) — a repo cannot reclaim `-p`/`-f` or
  the project identity that isolates stacks.
- Unknown keys and missing files/hooks are **hard errors** (fail loudly), and
  the manifest is **not** `${ENV}`-interpolated (a `${VAR}` is left for docker
  compose).

No `.plico.toml` ⇒ behaviour is exactly as if the feature did not exist. See the
[manifest reference](../reference/manifest.md) for every key, and
[Authentication](authentication.md) for how the server-only `repo` / `git.auths`
bootstrap reaches private repos and registries.
