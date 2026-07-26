---
title: Supervision (systemd)
icon: lucide/server-cog
---

# Supervision (systemd)

Process babysitting is delegated to the supervisor; plico keeps the scheduling
internal. The installer deploys `packaging/plico.service`.

## The unit

- `RuntimeDirectory=plico` creates `/run/plico`.
- `StateDirectory=plico` prepares `/var/lib/plico`.
- `EnvironmentFile=-/etc/plico/plico.env` keeps secrets out of the unit. The
  leading `-` makes the file optional on a fresh install.
- `UMask=0022` so cloned files stay readable by containers bind-mounting them
  under an arbitrary uid (relevant to [hooks](../concepts/hooks.md)).

## Applying a config change: restart is the reload

```sh
plico validate
sudo systemctl restart plico
```

A restart drains in-flight runs, and the persisted schedule anchors re-open a
still-open deployment window. **A restart is the reload mechanism — there is no
SIGHUP.**

This was a deliberate choice: a graceful restart already gives a clean,
well-defined reload, and re-anchoring on restart avoids phantom firings when the
schedule expression itself changed. See
[Scheduling](../concepts/scheduling.md#anchors-survive-restarts) and
[Design principles](../concepts/design-principles.md#restart-is-the-reload-mechanism).

## Enable / start / stop

```sh
sudo systemctl enable --now plico     # start now and on boot
sudo systemctl restart plico          # apply a config change
sudo systemctl stop plico             # stop the daemon
systemctl status plico --no-pager -l  # current state
```

The enabled/disabled state and operator group memberships are preserved across
upgrades.
