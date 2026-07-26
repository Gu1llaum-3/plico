---
title: Monitoring & health
icon: lucide/activity
---

# Monitoring & health

plico gives you three complementary signals: an inbound healthcheck for the
daemon, an outbound heartbeat for when you can't reach it, and drift detection
for the stacks' containers between deployments.

## `/healthz` — the daemon's semantic health

```
GET http://127.0.0.1:9444/healthz
```

A **semantic** healthcheck: it returns 503 when the scheduler stops ticking or a
run exceeds `run_timeout` — not a mere "process is up" probe. It exposes
`next_run` per stack. Logs are structured JSON (slog) with one correlation
`run_id` per deployment.

## Liveness heartbeat — the outbound dead-man's switch

`[heartbeat]` turns plico into an outbound dead-man's switch: while the daemon is
healthy (same predicate as `/healthz`) it GETs the configured URL every
`interval`; when degraded or dead it stays **silent**, so the monitor alerts on
the *missing* beat.

```toml
[heartbeat]
url = "https://kuma.example.com/api/push/aBcD1234?status=up&msg=OK"
interval = "30s"
```

It is the outbound mirror of `/healthz`: it works when the monitor cannot reach
the host (behind NAT/firewall), and the silence covers both "plico degraded" and
"plico dead" (a dead process cannot send a status). Paste the push URL your
monitor gives you (Uptime Kuma push, Healthchecks.io, …) verbatim.

- A **failed deployment does not stop the beat** — the daemon is alive and
  scheduling; deploy failures are the notifiers' job.
- plico beats immediately on start, then every `interval`.

!!! tip "Grace period"
    Set your monitor's grace period to **≥ 2× `interval`** so a single missed
    beat (restart, GC pause, brief blip) does not raise a false alarm.

## Drift detection (reconciliation-lite)

plico verifies a stack's health once, right after `up`. Between deployments —
which a schedule can space days apart — a container can crash, be OOM-killed, or
go `unhealthy` and nothing would notice (`/healthz` and the heartbeat watch the
*daemon*, not the stacks). Drift detection closes that gap.

On by default, every `drift_interval` (2 m) plico re-reads each deployed stack's
`docker compose ps` and, on a **regression** (a service unhealthy, dead, crashed
or crash-looping), sends a `drift_detected` notification — and a `drift_resolved`
one when it recovers.

```toml
drift_check = true    # global default; set false to disable, or per stack
drift_interval = "2m"
```

!!! danger "Detection only — plico never auto-remediates"
    Unlike Flux/Argo it does **not** re-apply desired state: a blind re-`up` on a
    failed migration or a corrupted volume can destroy data. The rule stands —
    **backup + alert + human**.

### It is quiet in every ambiguous case

It only checks a stack whose last deploy succeeded, skips a stack a deploy
currently owns, and swallows a transient `docker` error rather than cry wolf.
One alert per drift episode (deduped), then one on recovery — except a
successful redeploy closes an open episode silently. Tearing a drifted stack
down (`compose down`) is not treated as recovery.

`drift_detected` is failure-oriented (on by default per channel);
`drift_resolved` is a recovery signal, opt-in like `deploy_success`.

**Known limit:** a service **stopped by hand** (`compose stop` → exited 0,
indistinguishable from a one-shot init container) and a **fully downed** stack
(0 services) are not flagged — only unhealthy/dead/crashed services are.
