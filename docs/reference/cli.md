---
title: CLI
icon: lucide/terminal
---

# CLI reference

`plico` is a single binary. `serve` runs the daemon; the other commands are a
thin client that talks to it over a unix socket, going through the daemon's
locks so a manual action never races the scheduler.

## `plico serve`

Run the daemon: the polling loop plus `/healthz`.

| Flag | Default | Meaning |
|---|---|---|
| `-c, --config` | `/etc/plico/config.toml` | Path to the config file. |

Normally started by systemd, not by hand — see
[Supervision](../operations/supervision.md).

## `plico status`

Per-stack table: status, deployed SHA, last update, next window, pending
revision.

| Flag | Default | Meaning |
|---|---|---|
| `-c, --config` | `/etc/plico/config.toml` | Locate the socket from this config. |
| `--socket` | — | Connect to this socket directly, skipping the config. |

## `plico check-now`

Force an immediate fetch + diff, notify, but **do not deploy** — the client-side
equivalent of an out-of-window `check`.

| Flag | Meaning |
|---|---|
| `--stack <name>` | Target a single stack. |
| `--all` | Target every stack. (`--stack` / `--all` are mutually exclusive.) |
| `-c, --config` / `--socket` | Connection, as above. |

## `plico deploy-now`

Force an immediate deployment, **bypassing the schedule window**.

| Flag | Meaning |
|---|---|
| `--stack <name>` / `--all` | Target selection (mutually exclusive). |
| `--force` | Deploy even without a git delta (redeploy the current revision). |
| `--skip-pre` | **Dangerous:** skip the pre-deploy backup gate. Requires `--force`; fires `pre_hook_skipped`. |
| `--skip-post` | Skip the post-deploy hook (low risk). |
| `-c, --config` / `--socket` | Connection. |

```sh
plico deploy-now --stack webapp                 # deploy now, window or not
plico deploy-now --stack webapp --force         # redeploy the current revision
plico deploy-now --stack webapp --force --skip-pre  # skip the gate (loud, notified)
```

!!! danger "`--skip-pre` is refused without `--force`"
    Enforced on both the client **and** the daemon side, and it fires a
    `pre_hook_skipped` notification. It skips **all** pre-hooks, global included.

## `plico dry-run`

Show what would be deployed — the git delta and pending commits — without
acting.

| Flag | Meaning |
|---|---|
| `--stack <name>` | Stack to inspect (**required**). |
| `-c, --config` / `--socket` | Connection. |

## `plico validate`

Validate the configuration without starting the daemon. Prints how many stacks
are configured and how many are scheduled. Run it before every
`systemctl restart`.

| Flag | Default | Meaning |
|---|---|---|
| `-c, --config` | `/etc/plico/config.toml` | Config to validate. |

## `plico version`

Print the plico version.

## Socket resolution

Client commands only read `base_dir` and `[api].socket` — they do **not** need
the git/ntfy tokens that live in the systemd environment. `--socket` bypasses
the config entirely. The recommended socket is `/run/plico/plico.sock`; the
historical fallback is `<base_dir>/plico.sock`.
