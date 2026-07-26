---
title: Layered hooks
icon: lucide/layers
---

# Layered hooks

Hooks are plico's general pre/post-deploy mechanism. Backup is the headline use
case — the **blocking pre-deploy gate** that gives plico its reason to exist —
but a hook can equally quiesce a service, run a migration, check a precondition,
or anything else. Generic hooks are precisely what a distroless, shell-less
deployer cannot offer.

## Two layers: the operator brackets the repo

- **Global operator hooks** — defined in the server config
  (`[hooks].pre_deploy` / `post_deploy`, absolute paths). They run on **every**
  stack, **in addition to** the repo's hooks, regardless of what the repo
  declares. This is the "backup is sacred" gate that survives a repo forgetting
  or replacing it.
- **Repo hooks** — the stack repo's own scripts: the manifest `pre_deploy` /
  `post_deploy` lists, or the default `.plico/pre-deploy.sh` /
  `.plico/post-deploy.sh`.

The execution order — the operator brackets the repo:

```
global pre  →  repo pre  →  DEPLOY  →  repo post  →  global post
└── blocking gate ──────┘             └── non-blocking ─────┘
```

Pre-hooks are a **blocking gate**: the first non-zero exit aborts the
deployment, fires a notification, and the run is retried on the next tick.
Post-hooks are **non-blocking**.

## Opt-outs

| Setting | Effect |
|---|---|
| `global_hooks = false` | This stack skips the **global** operator hooks. |
| `repo_hooks = false`   | Ignore the **repo's** own hooks (manifest lists **and** the default `.plico/*.sh`) while keeping the rest of its recipe. |
| `ignore_manifest = true` | Full kill-switch: drop the **entire** repo side — recipe *and* hooks. The global `[hooks]` still run. See [Server vs repo config](trust-boundary.md). |
| `deploy-now --skip-pre --force` | The operator's explicit escape hatch: skip **all** pre-hooks (global included) for one run. Loud and notified. |

## The hook environment is scoped

Hooks run with a **safe baseline** environment and nothing else:

- `PATH`, `HOME`, locale, and `DOCKER_HOST` / `DOCKER_CONFIG` / `DOCKER_CONTEXT`.
- The `DEPLOY_*` context variables: `DEPLOY_STACK`, `DEPLOY_DIR`,
  `DEPLOY_GIT_REF`, `DEPLOY_OLD_SHA`, `DEPLOY_NEW_SHA`.

The daemon's own secrets — `SOPS_AGE_KEY_FILE`, notifier and git tokens — are
**withheld**, so a repo-controlled hook cannot exfiltrate them.

A hook that genuinely needs another variable (e.g. `RESTIC_PASSWORD`, AWS
credentials) opts in by **name** via `[hooks].env_passthrough` (global) or a
stack's `env_passthrough` (added to the global list). `SSH_AUTH_SOCK` is not in
the baseline — add it via passthrough for a backup-over-ssh hook.

!!! warning "umask and sensitive hook output"
    The daemon runs with `UMask=0022` so cloned files stay readable by
    containers bind-mounting them under an arbitrary uid — and hooks inherit it.
    A hook writing sensitive data (a database dump before backup, say) must set
    `umask 077` at the top of the script, or write to a permission-restricted
    directory.

!!! note "Global hooks are server scripts"
    A global hook's working directory is the stack's checked-out content root,
    but it applies to *every* stack. Reference the deploy tree via `$DEPLOY_DIR`
    rather than relying on the working directory.

See the [backup pre-deploy hook](../guides/backup-hook.md) guide for a concrete
script, and [Reference → Per-stack options](../reference/stack-options.md) for
the exact toggles.
