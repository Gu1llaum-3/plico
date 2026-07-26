---
title: Scheduling & windows
icon: lucide/calendar-clock
---

# Scheduling & windows

Without a `schedule`, a stack deploys as soon as a git delta is seen. With a
`schedule` (cron, evaluated in `timezone`), each firing **opens a deployment
window** of `window` duration (default 1 h): during the window, every poll tick
may deploy; outside it, the stack is untouched.

```toml
schedule = "0 22 * * *"   # global: a 22:00 window for every stack
window = "2h"

[[stack]]
name = "critical"
schedule = "0 4 * * *"    # override: this one at 4 AM
window = "30m"

[[stack]]
name = "dev"
schedule = "@poll"        # opt-out: deploy on every tick, as without a schedule

[[stack]]
name = "watched"
schedule = "0 22 * * *"
check = true              # outside the window: fetch + diff on every tick and
                          # a "deployment queued" notification (once per
                          # revision) — without deploying anything
```

## The window is authoritative

plico **never deploys outside the window**, give or take one `poll_interval` of
tolerance on the tick that discovers the firing (ticker jitter must not turn a
healthy firing into a missed window).

A firing whose window has entirely passed — daemon down, host paused, or a
previous run covering the whole window — is **logged as WARN and never replayed
late**. It fires a `window_missed` notification instead.

## Anchors survive restarts

The schedule anchor (the last accounted firing plus the cron expression it was
computed under) is persisted in `state.json`:

- A restart *during* a still-open window **re-opens** it.
- An already-honored window is **not replayed**.
- **Editing the `schedule` re-anchors at restart** — no phantom firings under
  the new expression.

## Retries and attribution

The number of attempts within a window is roughly `window / poll_interval` —
size the window generously if you want retries. A run dispatched at the end of a
window may outlive it and is **attributed to the window that launched it**.
`/healthz` exposes `next_run` per stack.

## DST

A firing falling in the **skipped** hour does not run. A firing in the
**repeated** hour runs once (the first occurrence).

!!! note "The scheduler is the sharp edge"
    This window state machine is the subtlest code in plico, hardened over four
    rounds of adversarial review. The invariants above — window authoritative,
    missed windows never replayed, anchors persisted, attribution at dispatch —
    are load-bearing.
