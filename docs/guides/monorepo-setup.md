---
title: Deploy a monorepo
icon: lucide/folder-tree
---

# Deploy a monorepo

!!! note "Guide in progress"
    A full walkthrough is coming.

This guide will set up several stacks from one repository, each in its own
subdirectory, and demonstrate that a commit touching one subtree redeploys only
that stack.

**Meanwhile:**

- [Concepts → Monorepos](../concepts/monorepo.md) — how `path` and subtree-scoped
  change detection work, plus the limits.
- [Reference → Per-stack options](../reference/stack-options.md#monorepo-example)
  — the `path` key.
