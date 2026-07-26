---
title: System layout
icon: lucide/hard-drive
---

# System layout

New installations separate persistent data from runtime.

## Paths and what to back up

| Path | Content | Back up |
|---|---|---|
| `/opt/docker/<stack>` | Git worktrees, rebuildable | optional |
| `/var/lib/plico/state.json` | SHAs, failures, queued revisions, cron anchors | **yes** |
| `/run/plico/plico.sock*` | volatile socket and lock | no |
| `/etc/plico` | configuration, environment, age key | **yes** |

!!! danger "Never store data inside a worktree"
    plico may delete and re-clone `/opt/docker/<stack>` to repair Git. Databases
    and irreplaceable uploads must live in named volumes or dedicated external
    paths — never inside a worktree.

## Permissions

```text
/usr/local/bin/plico          root:root    0755
/etc/plico                    root:plico   0750
/etc/plico/config.toml        root:plico   0640
/etc/plico/plico.env          root:plico   0600
/var/lib/plico                plico:plico  0750
/var/lib/plico/state.json     plico:plico  0600
/run/plico                    plico:plico  0750  (created by systemd)
/run/plico/plico.sock         plico:plico  0660
/opt/docker                   plico:plico  0750
```

The `0660` socket is reachable by members of the `plico` group — add an operator
with the installer's `--operator USER` (see [Upgrades](upgrades.md)).

## Older layouts

Configurations without `state_file` or `[api].socket` keep the exact historical
layout under `base_dir`. To move to the split layout, see
[Migrating older installs](migration.md).
