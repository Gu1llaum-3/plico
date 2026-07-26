---
title: Encrypt secrets with SOPS
icon: lucide/key-round
---

# Encrypt secrets with SOPS

!!! note "Guide in progress"
    A full step-by-step walkthrough is coming. For now, the reference material
    below covers the whole flow.

This guide will walk through generating an age key, writing a `.sops.yaml`,
encrypting a dotenv, committing it to the stack repo, and wiring it into a
`[sops]` block — end to end.

**Meanwhile:**

- [Concepts → Secrets](../concepts/secrets.md) — the model, `exec-env` vs
  `tmpfs`, and the partial-encryption `.sops.yaml` example.
- [Reference → Repo manifest](../reference/manifest.md#sops-encrypted-env) — the
  `[sops]` keys.
- [Reference → Per-stack options](../reference/stack-options.md) — the server-side
  `sops_files` / `sops_mode` overrides.
