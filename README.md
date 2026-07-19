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

## How it works

Every `poll_interval`, for each stack:

1. `git fetch` + compare the `origin/<ref>` SHA with the last deployed SHA
   (persisted in `state.json`). No delta → silent no-op.
2. **Pre-deploy hook**: `.deploy/pre-deploy.sh` from the repo (takes
   precedence) or a configured global path. It receives `DEPLOY_STACK`,
   `DEPLOY_DIR`, `DEPLOY_GIT_REF`, `DEPLOY_OLD_SHA`, `DEPLOY_NEW_SHA`.
   **`exit != 0` → deployment aborted**, notification, retried on the next
   tick.
   ⚠️ The daemon runs with `UMask=0022` so cloned files stay readable by
   containers bind-mounting them under an arbitrary uid — hooks inherit it:
   a hook writing sensitive data (a database dump before backup, say) must
   set `umask 077` at the top of the script or write to a
   permission-restricted directory.
3. SOPS decryption in memory: `sops exec-env secrets.enc.env -- docker
   compose … up -d`. tmpfs mode (`/dev/shm`) available on Linux.
4. `docker compose pull` (`force_pull` option) then `up -d --remove-orphans`.
   A failed pull leaves the running stack untouched.
5. Post-up verification: every service `running` and `healthy` (or without a
   healthcheck) within `verify_timeout`; immediate failure on `unhealthy`.
6. Optional post-deploy hook (non-blocking), notification, state persisted.

A run still in flight at the next tick is **skipped**, never piled up.

### Per-stack scheduling

Without a `schedule`, a stack is deployed as soon as a git delta is seen.
With a `schedule` (cron, evaluated in `timezone`), each firing **opens a
deployment window** of `window` duration (default 1 h): during the window,
every poll tick may deploy; outside it, the stack is untouched.

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

**The window is authoritative**: plico never deploys outside it, give or
take one `poll_interval` of tolerance on the tick that discovers the firing
(ticker jitter must not turn a healthy firing into a missed window). A
firing whose window has entirely passed (daemon down, host paused, previous
run covering the whole window) is **logged as WARN and never replayed
late**. The schedule anchor (last accounted firing + the cron expression it
was computed under) is persisted in `state.json`: a restart *during* a
still-open window re-opens it; an already-honored window is not replayed;
**editing the `schedule` re-anchors at restart** (no phantom firings under
the new expression). The number of attempts within a window is roughly
`window / poll_interval` — size it generously if you want retries; a run
dispatched at the end of a window may outlive it and is attributed to the
window that launched it. `/healthz` exposes `next_run` per stack. **DST**: a
firing falling in the skipped hour does not run; in the repeated hour it
runs once (first occurrence).

## Installation

On the server (Linux/systemd):

```sh
curl -fsSLO https://raw.githubusercontent.com/Gu1llaum-3/plico/main/install.sh
less install.sh          # read before running
sudo sh install.sh
```

The installer downloads the latest release (checksums verified), creates the
`plico` user, the directories, the systemd unit and an example
configuration — it never starts a service without an active configuration.
To activate:

```sh
sudo cp /etc/plico/config.toml.example /etc/plico/config.toml
sudoedit /etc/plico/config.toml
sudo systemctl enable --now plico
plico status
```

Upgrading: re-run `sudo sh install.sh` (atomic binary replacement, restart
only when something changed, automatic rollback if the service does not come
back). Pinning a version, local/offline binaries, `--operator`, `--config`
for automation, macOS/FreeBSD, migrating from the historical layout and
diagnostics: see [the installation guide](docs/installation.md).

## Configuration

The commented reference is [`config.example.toml`](config.example.toml),
installed on the server as `/etc/plico/config.toml.example`. Secrets (git
tokens, ntfy) are referenced with `${ENV_VAR}` interpolation and provided
through `/etc/plico/plico.env` — a missing variable prevents the daemon from
starting and fails `plico validate`.

- `GET /healthz` (127.0.0.1:9444): **semantic** healthcheck — 503 when the
  scheduler stops ticking or a run exceeds `run_timeout`.
- Structured JSON logs (slog), one correlation `run_id` per deployment.

### Notifications

Three channels — `[ntfy]`, repeatable `[[webhook]]` (generic JSON, works
as-is with Google Chat and Teams), `[smtp]` — each with its own optional
`events` list. **The default is failure-oriented**: `pre_hook_failed`,
`pre_hook_skipped`, `deploy_failed`, `window_missed` (a scheduled window
produced no run), `git_sync_failed` (git fetch failing repeatedly — revoked
token, moved repo). `deploy_success`, `deploy_queued` and `deploy_start` are
opt-in per channel, and `events = ["all"]` selects everything:

```toml
[ntfy]
url = "https://ntfy.example.com/plico"
events = ["deploy_failed", "pre_hook_failed", "window_missed",
          "git_sync_failed", "deploy_success"]   # + success, for this channel

[[webhook]]
url = "https://chat.googleapis.com/v1/spaces/.../messages?key=..."
# no events key = failures only
```

A failing channel never breaks a deployment (the send error is logged
locally), and a failure alert survives even a run killed by `run_timeout`.

### System layout

New installations separate persistent data from runtime:

| Path | Content | Back up |
|---|---|---|
| `/opt/docker/<stack>` | Git worktrees, rebuildable | optional |
| `/var/lib/plico/state.json` | SHAs, failures, queued revisions, cron anchors | **yes** |
| `/run/plico/plico.sock*` | volatile socket and lock | no |
| `/etc/plico` | configuration, environment, age key | **yes** |

Never place a database or irreplaceable uploads inside a worktree: plico may
delete and re-clone it to repair Git. Older configurations without
`state_file` or `[api].socket` keep the exact historical layout under
`base_dir`.

### SOPS secrets: partial encryption recommended

plico decrypts through `sops exec-env`: the repo's `.sops.yaml` is only used
for encryption, the decryption metadata being embedded in the file itself.
**Partial encryption** (only sensitive values encrypted, the rest readable
in git diffs) is therefore supported natively:

```yaml
# .sops.yaml at the stack repo root
creation_rules:
  - path_regex: \.deploy/.*\.enc\.env$
    encrypted_regex: "(SECRET|PASSWORD|TOKEN|KEY)"
    mac_only_encrypted: true
    age: age1...   # recipient(s)
```

```sh
sops encrypt --in-place .deploy/secrets.enc.env   # or: sops edit
```

⚠️ Without `mac_only_encrypted: true`, the MAC also covers cleartext values:
a hand edit (outside `sops edit`/`sops set`) breaks decryption — plico will
fail at the `sops` stage with "MAC mismatch".

### Client CLI

The daemon exposes a local API on a unix socket (`[api] socket`, recommended
at `/run/plico/plico.sock`; historical fallback `<base_dir>/plico.sock`).
Commands go through the daemon's locks — never a deployment concurrent with
the scheduler:

```sh
plico status                      # per stack: status, SHA, pending, next window
plico check-now  --stack X|--all  # immediate fetch + diff, notifies without deploying
plico deploy-now --stack X|--all  # immediate deployment, window or not
plico deploy-now --stack X --force            # redeploy the current revision
plico deploy-now --stack X --force --skip-pre # skip the backup gate (loud, notified)
plico dry-run    --stack X        # delta + pending commits, without acting
plico validate                    # check the configuration without starting
```

Client commands only load `base_dir` and `[api].socket`: they do not need
the Git/ntfy tokens that live only in the systemd environment. `--socket`
skips reading the configuration entirely.

`--skip-pre` is refused without `--force` — on the client **and** the daemon
side — and fires a `pre_hook_skipped` notification. Every command accepts
`-c` (config, to locate the socket) or `--socket`.

### Git auth

Per-host HTTPS via `[git.auths."<host>"]`: plico passes itself as
`GIT_ASKPASS` to the git subprocess — the token appears **neither on disk,
nor in argv, nor in `.git/config`**. SSH remotes use the system agent.

### Supervision (systemd)

Process babysitting is delegated to the supervisor; plico keeps the
scheduling internal.

The installer deploys [`packaging/plico.service`](packaging/plico.service).
`RuntimeDirectory=plico` creates `/run/plico`, `StateDirectory=plico`
prepares `/var/lib/plico`, and `EnvironmentFile=-/etc/plico/plico.env` keeps
secrets out of the unit. The `-` makes the file optional on a fresh install.

Applying a configuration change is `plico validate` then
`sudo systemctl restart plico`: the restart drains in-flight runs and the
persisted schedule anchors re-open a still-open deployment window — a
restart **is** the reload mechanism, there is no SIGHUP.

## Rollback (v1 = manual, by design)

- **Code / compose configuration**: `git revert` in the stack repo; plico
  redeploys the previous revision on the next tick.
- **Data**: the backup taken by the pre-deploy hook (dump + restic) is
  restored manually. No automatic restore in v1 — that is a choice.

After a failed post-up verification, plico still records the new SHA so a
broken revision is not redeployed in a loop: recovery goes through
`git revert` (or `plico deploy-now --force`).

## Development

To contribute or build from source:

```sh
mise install          # go, lefthook, golangci-lint, sops, age (pinned in mise.toml)
mise run build        # → bin/plico
```

Available tasks: `mise run build | test | lint | fmt | smoke | install-test
| release-test | shellcheck`.

`mise run smoke` runs [`test/smoke.sh`](test/smoke.sh): a complete local
GitOps environment (`file://` git repo, real Docker stack, **age + sops**
encrypted secrets) verifying the nominal deployment, in-memory secret
injection (no cleartext on disk or in logs), then the backup-gate blocking a
bad revision. Requires a Docker daemon.

## Roadmap

- **v1**: webhook + SMTP notifiers, Uptime Kuma heartbeat. Already shipped:
  per-stack cron scheduling and windows, check/apply distinction, the client
  CLI over a unix socket. Config changes are applied with
  `systemctl restart plico` — a graceful drain with persisted schedule
  anchors, so a restart is the official reload mechanism (SIGHUP and
  config.d deep-merge were considered and rejected, see
  [ROADMAP](ROADMAP.md)).
- **Later**: **Podman** support (the runtime already sits behind a
  `compose.Runtime` interface), Prometheus metrics, container image,
  webhook ingestion.
