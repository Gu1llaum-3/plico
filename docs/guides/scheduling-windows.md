---
title: Restrict deploys to a window
icon: lucide/calendar-clock
---

# Restrict deploys to a maintenance window

!!! note "Guide in progress"
    A full walkthrough is coming.

This guide will restrict a stack's deployments to a nightly window, add
out-of-window `check` notifications, and explain what happens when a window is
missed.

**Meanwhile:**

- [Concepts → Scheduling & windows](../concepts/scheduling.md) — cron firings,
  the authoritative window, anchors, retries, and DST.
- [Reference → Server configuration](../reference/config.md#top-level-keys) — the
  `schedule` / `window` / `check` keys.
