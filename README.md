# plico

> *plicare* "to fold" → *plico* "I unfold" — literally, to deploy.

**plico** is a *pull-based* GitOps deployer for standalone Docker Compose
stacks — FluxCD-style, but for Compose. Its reason to exist next to
[doco-cd](https://github.com/kimdre/doco-cd): a **blocking pre-deployment
backup gate** (if the backup fails, nothing is deployed), a readable
host-path layout (`/opt/docker/<stack>/`) that is easy to back up with
restic, and SOPS secrets that are **never written to disk in cleartext**.

plico **orchestrates** mature CLIs — `git`, `sops`, `docker compose` — via
`os/exec`. It reimplements neither Git, nor Compose, nor SOPS.

## 📖 Documentation

**Full documentation → <https://gu1llaum-3.github.io/plico/>**

- [Getting started](https://gu1llaum-3.github.io/plico/getting-started/installation/)
  — install, configure, and run your first deployment.
- [Concepts](https://gu1llaum-3.github.io/plico/concepts/pipeline/) — the deploy
  pipeline, scheduling & windows, layered hooks, secrets, drift detection.
- [Reference](https://gu1llaum-3.github.io/plico/reference/config/) — every
  config key, per-stack option, repo manifest, and CLI command.
- [Operations](https://gu1llaum-3.github.io/plico/operations/layout/) — system
  layout, systemd supervision, upgrades, and diagnostics.

## Quick install

On the server (Linux/systemd):

```sh
curl -fsSLO https://raw.githubusercontent.com/Gu1llaum-3/plico/main/install.sh
less install.sh          # read before running
sudo sh install.sh
```

Then activate the service:

```sh
sudo cp /etc/plico/config.toml.example /etc/plico/config.toml
sudoedit /etc/plico/config.toml
sudo systemctl enable --now plico
plico status
```

See the [installation guide](https://gu1llaum-3.github.io/plico/getting-started/installation/)
for pinned versions, offline binaries, and the full option list.

## Development

To contribute or build from source:

```sh
mise install          # go, lefthook, golangci-lint, sops, age (pinned in mise.toml)
mise run build        # → bin/plico
```

Available tasks: `mise run build | test | lint | fmt | smoke | install-test |
release-test | shellcheck`, plus `docs | docs-serve` for the documentation site.
Working conventions live in [`CLAUDE.md`](CLAUDE.md); scope decisions and
rejected alternatives in [`ROADMAP.md`](ROADMAP.md).

`mise run smoke` runs [`test/smoke.sh`](test/smoke.sh): a complete local GitOps
environment (`file://` git repo, real Docker stack, **age + sops** encrypted
secrets) verifying the nominal deployment, in-memory secret injection (no
cleartext on disk or in logs), then the backup gate blocking a bad revision.
Requires a Docker daemon.
