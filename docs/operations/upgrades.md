---
title: Upgrades & rollback
icon: lucide/refresh-cw
---

# Upgrades & rollback

## Upgrading

Re-run the installer with `latest`, `--version` or `--binary`:

```sh
sudo sh install.sh                      # latest stable release
sudo sh install.sh --version v1.2.3     # pin a version
sudo sh install.sh --binary ./plico     # a local/offline binary
```

The binary replacement is **atomic**. If the service was active, it is only
restarted when the binary or the unit changed. The installer then waits for the
CLI to actually reach the socket — a bare `systemctl restart` success is not
enough. **On failure, the previous binary and unit are restored.** The
enabled/disabled state and operator memberships are preserved.

## Installer options

```text
--version VERSION    install a specific release
--binary PATH        install a local binary instead of downloading
--sha256 HASH        verify a local binary against this hash (recommended)
--config PATH        provide a config.toml (enables + starts the service)
--env-file PATH      provide plico.env alongside --config
--operator USER      add USER to the plico group (0660 socket access)
--binary-only        install only the binary, no service setup
--no-start           set everything up but do not start the daemon
```

Official downloads are always verified against `checksums.txt`. With a local
binary, `--sha256` is optional (the file is already within your trust boundary)
but recommended. `--operator USER` requires the user to log back in after the
group change.

## Rollback

**v1 rollback is manual, by design** — plico does not auto-roll-back application
state.

- **Code / compose configuration:** `git revert` in the stack repo; plico
  redeploys the previous revision on the next tick. Or force it with
  `plico deploy-now --stack X --force`.
- **Data:** restore from the backup taken by the [pre-deploy hook](../concepts/hooks.md)
  (dump + restic). No automatic restore in v1.

After a failed post-up verification, plico still records the new SHA, so a broken
revision is **not redeployed in a loop** — recovery goes through `git revert` or
a forced redeploy. See the [deploy pipeline](../concepts/pipeline.md#failure-handling).
