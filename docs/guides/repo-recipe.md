---
title: Write a repo-side recipe
icon: lucide/scroll-text
---

# Write a repo-side recipe (`.plico.toml`)

!!! note "Guide in progress"
    A full walkthrough is coming. The reference below is complete in the
    meantime.

This guide will build a `.plico.toml` from scratch: choosing the file vs
directory layout, adding `[compose]` options, `[sops]` and `[secrets]` blocks,
and ordered hook lists — with the guardrails explained inline.

**Meanwhile:**

- [Reference → Repo manifest](../reference/manifest.md) — every key and guardrail.
- [Concepts → Server vs repo config](../concepts/trust-boundary.md) — what a repo
  may and may not declare.
