---
title: Installation
icon: lucide/download
---

# Installation

plico runs as a systemd daemon on a Linux host. The binary is static, but it
orchestrates `git`, `docker compose` and — for stacks that use them — `sops`.
Make sure those commands work under the user that will run the daemon.

!!! warning "Docker socket access ≈ root"
    Access to the Docker socket group is practically equivalent to root access.
    Treat the host running plico accordingly.

## Prerequisites

- Linux with systemd (full service setup is Linux/systemd only).
- `git` and `docker compose` available on the host.
- `sops` and an `age` key, only for stacks that use encrypted secrets.

## Install a release

Download the installer, **read it**, then run it:

```sh
curl -fsSLO https://raw.githubusercontent.com/Gu1llaum-3/plico/main/install.sh
less install.sh          # read before running
sudo sh install.sh
```

The installer downloads the latest stable GitHub release (checksums verified),
creates the `plico` user, the directories, the systemd unit, and an example
configuration. **It never starts a service without an active configuration.**

Pinned versions and local binaries are supported:

```sh
sudo sh install.sh --version v1.2.3
sudo sh install.sh --binary ./plico
sudo sh install.sh --binary ./plico --sha256 <binary-sha256>
```

Platforms: Linux, Darwin and FreeBSD on amd64/arm64. Outside Linux/systemd, or
with `--binary-only`, only the binary is installed. The full option list lives
in [Operations → Upgrades & rollback](../operations/upgrades.md).

## Activate the service

```sh
sudo cp /etc/plico/config.toml.example /etc/plico/config.toml
sudoedit /etc/plico/config.toml
sudo systemctl enable --now plico
plico status
```

An existing `config.toml` or `plico.env` is never replaced by the installer;
the `.example` template is managed by plico and may be refreshed on upgrades.

## Next steps

- [Configuration](configuration.md) — write your first `config.toml`.
- [Your first deployment](first-deployment.md) — watch a stack deploy.
- [Building from source](../operations/diagnostics.md) and contributing: see
  the repository's `CLAUDE.md`.
