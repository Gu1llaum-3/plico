---
title: Design principles
icon: lucide/compass
---

# Design principles

plico is opinionated. These principles explain why it behaves the way it does —
and why some "obvious" features were deliberately rejected.

## Orchestrate CLIs, never reimplement

plico shells out to `git`, `sops` and `docker compose` via `os/exec`. It does
**not** embed go-git, compose-go, the sops library, or the Docker SDK. This is
the whole design thesis: it is what keeps plico small enough for two people to
hold in their heads, and it means plico inherits the exact behaviour, bug-fixes
and flags of the tools you already trust — no reimplementation drift.

## Fail loudly, never silently

A declared-but-unusable thing must **block or refuse**, not skip:

- a pre-hook missing its `+x` bit,
- a configured hook that has gone missing,
- an invalid config, an unknown manifest key, a referenced-but-undefined
  `${ENV}` variable.

Silently ignoring a broken stack would let a misconfiguration masquerade as a
healthy no-op. plico would rather stop and tell you.

## The pre-deploy hook is a blocking gate

`pre-deploy` exit `!= 0` ⇒ **no deploy, ever** — except the deliberate
`deploy-now --skip-pre --force` (loud and notified). Backup is the headline use
case, but the gate is general: it can quiesce a service, run a migration, or
check a precondition. See [Layered hooks](hooks.md).

## Secrets never hit disk in cleartext

sops decrypts into the process environment (`sops exec-env`) or a tmpfs — never
a file under `base_dir`. The smoke test asserts no secret leaks into
`base_dir`, logs, or notifications. See [Secrets](secrets.md).

## A failing notifier never breaks a deployment

Notification sends are bounded, emitted asynchronously, and fall back to local
logging on error. A dead ntfy server or a broken webhook can never stall or fail
a deploy.

## Detection, not auto-remediation

plico alerts a human; it never blindly re-applies desired state. A re-`up` over
a failed migration or a corrupted volume can destroy data.
[Drift detection](monitoring.md) is deliberately detection-only.

## Restart is the reload mechanism

Config changes apply via `systemctl restart`, which drains in-flight runs and
restores persisted schedule anchors. There is no SIGHUP — it was considered and
rejected, because a graceful restart already gives a clean, well-defined reload.

---

These are not incidental implementation details; they are the contract. The
repository's `ROADMAP.md` records the features that were rejected to keep them
intact.
