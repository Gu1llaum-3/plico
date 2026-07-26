---
title: Diagnostics
icon: lucide/stethoscope
---

# Diagnostics

## Inspecting a running daemon

```sh
systemctl status plico --no-pager -l
journalctl -u plico -n 100 --no-pager -o cat
sudo -u plico docker info
sudo -u plico docker compose version
plico status
```

Run docker as the `plico` user — a stack that deploys by hand as `root` but
fails under the daemon is almost always a Docker permission or context
difference.

!!! tip "`plico status` needs no secrets"
    It only reads the socket path from the configuration — not the git/ntfy
    tokens in the systemd environment. Safe to run as any operator in the
    `plico` group.

## Health and logs

- `GET http://127.0.0.1:9444/healthz` — semantic health; 503 when the scheduler
  stalls or a run exceeds `run_timeout`. See
  [Monitoring](../concepts/monitoring.md).
- Structured JSON logs (slog), one correlation `run_id` per deployment — grep by
  `run_id` to follow a single run end to end.

## Common checks

| Symptom | First thing to check |
|---|---|
| Stack never deploys | `plico dry-run --stack X` — is there a delta? Right `ref`/`path`? |
| Deploy aborts at the gate | A pre-hook exited non-zero — see [Layered hooks](../concepts/hooks.md). |
| "MAC mismatch" at the sops stage | A hand-edited encrypted file — see [Secrets](../concepts/secrets.md). |
| Client can't reach the socket | `[api].socket` path, and operator group membership (`--operator`). |

## Building from source

To build or contribute:

```sh
mise install          # go, lefthook, golangci-lint, sops, age (pinned in mise.toml)
mise run build        # → bin/plico
```

Tasks: `mise run build | test | lint | fmt | smoke | install-test | release-test
| shellcheck`, plus `docs | docs-serve` for this site.

`mise run smoke` stands up a complete local GitOps environment (`file://` git
repo, real Docker stack, age + sops secrets) and verifies the nominal
deployment, in-memory secret injection (no cleartext on disk or in logs), and the
backup gate blocking a bad revision. It needs a Docker daemon. The repository's
`CLAUDE.md` documents the working conventions.
