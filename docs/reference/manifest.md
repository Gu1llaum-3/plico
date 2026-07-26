---
title: Repo manifest
icon: lucide/file-code
---

# Repo manifest (`.plico.toml`)

A stack's git repo can carry its own **non-sensitive** deployment recipe, so the
server config stays minimal and the *how* lives with the code, versioned at the
deployed revision. It is checked out at the deployed SHA. The
[server-vs-repo boundary](../concepts/trust-boundary.md) explains what a repo may
and may not declare; this page is the key-by-key reference.

## Two layouts

Under the stack's content root (respects `path` for [monorepos](../concepts/monorepo.md)):

- a single file **`.plico.toml`**, or
- a **`.plico/`** directory holding `plico.toml` plus scripts.

!!! warning "Do not use both"
    A `.plico.toml` file and a `.plico/` directory present at the same time is
    an ambiguity error. Pick one.

## `[compose]`

```toml
[compose]
compose_file   = "docker-compose.prod.yml"
profiles       = ["web", "worker"]   # --profile, applied to pull AND up
env_files      = ["config/app.env"]  # extra --env-file (interpolation)
remove_orphans = false               # default true
up_args        = ["--wait"]          # appended after `up -d`
```

`up_args` is an **allowlist** of safe long-form flags (`--wait`, `--pull=…`,
`--scale=…`, `--build`, …). A repo cannot reclaim `-p` / `-f` or the project
identity that isolates stacks.

## `[sops]` — encrypted env

```toml
[sops]
files = ["secrets/app.enc.env"]   # injected as ${VAR} for compose interpolation
mode  = "exec-env"                # or "tmpfs"
```

Decrypted in memory, never on disk. The server's `sops_files`, if set, override
this wholesale. See [Secrets](../concepts/secrets.md).

## `[secrets]` — encrypted files

```toml
[secrets]
files = ["secrets/tls.enc.key"]   # → bind-mount via ${PLICO_SECRETS_DIR}/tls.key
```

Decrypted onto a per-stack tmpfs exposed as `$PLICO_SECRETS_DIR`; the `.enc`
marker is stripped from the target name. **Linux-only**; cleared on reboot and
re-materialised on the next deploy.

## `pre_deploy` / `post_deploy` — ordered hooks

```toml
# .plico/plico.toml (directory form: manifest + scripts)
pre_deploy  = [".plico/backup.sh", ".plico/migrate.sh"]  # ordered; blocking gate
post_deploy = [".plico/notify.sh"]                       # ordered; non-blocking
```

Pre-hooks are a blocking gate (first non-zero exit aborts); post-hooks are
non-blocking. They run in the same server-scoped environment as the default
`.plico/*.sh` hooks. If unset, the default single `.plico/pre-deploy.sh` /
`.plico/post-deploy.sh` still apply. See [Layered hooks](../concepts/hooks.md).

## Hook environment variables

Hooks receive: `DEPLOY_STACK`, `DEPLOY_DIR`, `DEPLOY_GIT_REF`, `DEPLOY_OLD_SHA`,
`DEPLOY_NEW_SHA`, and — when `[secrets]` is used — `PLICO_SECRETS_DIR`.

## Guardrails

- Paths (`compose_file`, `env_files`, `[sops]`, `[secrets]`, hooks) must be
  lexically **repo-relative** — absolute paths and `..` are rejected.
- The manifest file must **not be a symlink**.
- A decrypted secret file lands only in the plico-owned tmpfs, never a
  repo-chosen path.
- **Unknown keys and missing files/hooks are hard errors** (fail loudly).
- The manifest is **not** `${ENV}`-interpolated — a `${VAR}` is left for docker
  compose.

No `.plico.toml` ⇒ behaviour is exactly as before the feature existed.
