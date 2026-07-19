# Installing plico

## Prerequisites

The plico binary is static, but it orchestrates `git`, `docker compose` and,
for the stacks that use them, `sops`. Check those commands under the user
that will run the daemon. Access to the Docker socket group is practically
equivalent to root access.

## Installing a release

Download and inspect the script before running it:

```sh
curl -fsSLO https://raw.githubusercontent.com/Gu1llaum-3/plico/main/install.sh
less install.sh
sudo sh install.sh
```

`latest` means the latest stable GitHub release. A pinned version and a
local binary are supported:

```sh
sudo sh install.sh --version v1.2.3
sudo sh install.sh --binary ./plico
sudo sh install.sh --binary ./plico --sha256 <binary-sha256>
```

Official downloads are always verified against `checksums.txt`. With a local
binary, `--sha256` is optional since the file is already within the
operator's trust boundary, but it remains recommended.

Platforms: Linux, Darwin and FreeBSD on amd64/arm64. Full service setup is
Linux/systemd only; elsewhere, or with `--binary-only`, only the binary is
installed.

## Initial configuration

Without `--config`, the script installs `/etc/plico/config.toml.example`,
creates an empty `plico.env` and does not start any daemon. Two ways to
activate the service:

**Manually** (the simple path):

```sh
sudo cp /etc/plico/config.toml.example /etc/plico/config.toml
sudoedit /etc/plico/config.toml
sudo systemctl enable --now plico
plico status
```

**Through the installer** (automation / provisioning): prepare a
configuration file, then

```sh
sudo sh install.sh --config ./config.toml --env-file ./plico.env
```

This second pass enables and starts the service, waits for the CLI to
actually reach the socket, and rolls back on failure. It also serves to
retry a first start attempt that failed.

An existing `config.toml` or `plico.env` is never replaced. The `.example`
template is managed by the installer and may be refreshed on upgrades.

Useful options:

```text
--version VERSION
--binary PATH
--sha256 HASH
--config PATH
--env-file PATH
--operator USER
--binary-only
--no-start
```

`--operator USER` adds the user to the `plico` group to access the `0660`
socket. Logging back in is required after a group change.

## Layout and permissions

```text
/usr/local/bin/plico          root:root   0755
/etc/plico                    root:plico  0750
/etc/plico/config.toml        root:plico  0640
/etc/plico/plico.env          root:plico  0600
/var/lib/plico                plico:plico 0750
/var/lib/plico/state.json     plico:plico 0600
/run/plico                    plico:plico 0750 (created by systemd)
/run/plico/plico.sock         plico:plico 0660
/opt/docker                   plico:plico 0750
```

The `/opt/docker/<stack>` worktrees are rebuildable from Git. Application
data must live in named volumes or dedicated external paths, never inside a
worktree that may be re-cloned.

## Upgrade and rollback

Re-run the installer with `latest`, `--version` or `--binary`. The binary
replacement is atomic. If the service was active, it is only restarted when
the binary or the unit changed. The installer then waits for the CLI to
actually reach the socket; a bare `systemctl restart` success is not enough.
On failure, the previous binary and unit are restored.

The enabled/disabled state and operator memberships are preserved.

## Migrating from the historical layout

An upgrade never moves anything automatically. To migrate:

1. Stop plico.
2. Copy `<base_dir>/state.json` to `/var/lib/plico/state.json`, owner
   `plico:plico`, mode `0600`.
3. Add `state_file = "/var/lib/plico/state.json"`.
4. Add `[api] socket = "/run/plico/plico.sock"`.
5. Check that the unit contains `RuntimeDirectory=plico` and
   `StateDirectory=plico`.
6. Restart and check `plico status` before removing the old runtime files.

Never start on an empty state during the migration: every SHA would be
considered undeployed and hooks could be replayed.

## Diagnostics

```sh
systemctl status plico --no-pager -l
journalctl -u plico -n 100 --no-pager -o cat
sudo -u plico docker info
sudo -u plico docker compose version
plico status
```

`plico status` does not require the daemon's secrets: it only reads the
socket path from the configuration.
