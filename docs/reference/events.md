---
title: Notification events
icon: lucide/bell
---

# Notification events

Each notification channel (`[ntfy]`, `[[webhook]]`, `[smtp]`) takes an optional
`events` list. Omitting it selects the **failure-oriented default**. Listing
`"all"` selects everything. An unknown event name is a hard error — a typo
cannot ride along undetected.

```toml
[ntfy]
url = "https://ntfy.example.com/plico"
events = ["deploy_failed", "pre_hook_failed", "window_missed",
          "git_sync_failed", "deploy_success"]   # + success for this channel

[[webhook]]
url = "https://chat.googleapis.com/v1/spaces/.../messages?key=..."
# no events key = failures only
```

## The catalog

| Event | In default set? | Fires when |
|---|---|---|
| `pre_hook_failed` | ✅ default | A pre-deploy hook exited non-zero — the deploy was aborted. |
| `pre_hook_skipped` | ✅ default | Pre-hooks were skipped via `deploy-now --skip-pre --force`. |
| `deploy_failed` | ✅ default | A deployment failed (pull, up, or verify). Survives a `run_timeout` kill. |
| `window_missed` | ✅ default | A scheduled window produced no run. |
| `git_sync_failed` | ✅ default | Git fetch failed repeatedly (see `git_sync_alert_after`). |
| `drift_detected` | ✅ default | A deployed stack regressed between deploys (unhealthy/dead/crashed). |
| `deploy_queued` | opt-in | Out-of-window `check`: a new revision is pending, not deployed. |
| `deploy_start` | opt-in | A deployment run started. |
| `deploy_success` | opt-in | A deployment completed and verified. |
| `drift_resolved` | opt-in | A previously-drifted stack recovered. |

## Behaviour

- **A failing channel never breaks a deployment** — the send error is logged
  locally and the deploy continues. A failure alert survives even a run killed
  by `run_timeout`.
- `git_sync_failed` fires after `git_sync_alert_after` consecutive fetch
  failures for a stack (default 5; `0` disables).
- Webhooks send a generic JSON body with a human `text` field, accepted as-is by
  Google Chat and Microsoft Teams incoming webhooks.

See [Configure notifications](../guides/notifications.md) for channel setup, and
[Monitoring](../concepts/monitoring.md) for the drift signals.
