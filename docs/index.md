---
title: plico
icon: lucide/ship
---

# plico

> *plicare* "to fold" → *plico* "I unfold" — literally, to deploy.

**plico** is a *pull-based* GitOps deployer for standalone Docker Compose
stacks — FluxCD-style, but for Compose. It watches your git repositories and,
when a stack's tracked ref moves, deploys it: back up, decrypt secrets in
memory, pull, `up`, verify, notify.

plico **orchestrates** mature CLIs — `git`, `sops`, `docker compose` — via
`os/exec`. It reimplements neither Git, nor Compose, nor SOPS. That is the whole
design thesis: a small tool two people can hold in their heads. See
[Design principles](concepts/design-principles.md).

## Why plico

Its reason to exist next to [doco-cd](https://github.com/kimdre/doco-cd):

- **A blocking pre-deployment backup gate.** If the backup hook fails, *nothing*
  is deployed. Backup is a first-class, non-negotiable step — not an
  afterthought. See [Layered hooks](concepts/hooks.md).
- **A readable host-path layout** (`/opt/docker/<stack>/`) that is trivial to
  back up with restic.
- **SOPS secrets that never touch disk in cleartext** — decrypted into the
  process environment or a tmpfs, never a file under `base_dir`. See
  [Secrets](concepts/secrets.md).
- **Generic pre/post-deploy hooks** with a scoped environment — backup is the
  headline use case, but a hook can equally quiesce a service, run a migration,
  or check a precondition. This is precisely what a distroless, shell-less
  deployer cannot do.

## Core guarantees

- **Fail loudly, never silently.** A declared-but-unusable thing (a missing
  hook, an invalid config) blocks the deploy — it is never skipped.
- **The window is authoritative.** A scheduled stack never deploys outside its
  window, and a missed window is logged and notified, never replayed late.
- **A failing notifier never breaks a deployment.**
- **Detection, not auto-remediation.** [Drift detection](concepts/monitoring.md)
  alerts a human; plico never blindly re-applies desired state over a failed
  migration or a corrupted volume.

## Where to next

- **[Getting started](getting-started/installation.md)** — install, configure,
  and run your first deployment.
- **[Concepts](concepts/pipeline.md)** — how the pipeline, scheduling, hooks,
  and secrets actually work.
- **[Reference](reference/config.md)** — every config key, per-stack option, and
  CLI command.
- **[Operations](operations/layout.md)** — layout, systemd, upgrades, and
  diagnostics.
