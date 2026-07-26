---
title: A backup pre-deploy hook
icon: lucide/database-backup
---

# A backup pre-deploy hook

!!! note "Guide in progress"
    A full walkthrough with a real restic script is coming.

This is the reason plico exists: a **blocking** backup that runs before every
deploy, so a bad revision can never overwrite un-backed-up data. This guide will
show a complete pre-deploy hook (database dump + restic push), the `umask 077`
and `env_passthrough` details, and how to make it a global operator hook that
runs on every stack.

**Meanwhile:**

- [Concepts → Layered hooks](../concepts/hooks.md) — the gate, the scoped
  environment, `umask`, and `env_passthrough`.
- [Reference → Server configuration](../reference/config.md#hooks) — the global
  `[hooks].pre_deploy` keys.
