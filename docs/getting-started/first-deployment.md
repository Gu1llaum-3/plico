---
title: Your first deployment
icon: lucide/rocket
---

# Your first deployment

With the daemon [installed](installation.md) and a
[minimal config](configuration.md) in place, this walks through a first deploy.

## 1. Start the daemon

```sh
sudo systemctl enable --now plico
```

plico immediately begins polling each stack every `poll_interval`. Nothing is
deployed until it sees a git delta between `origin/<ref>` and the last deployed
SHA (there is none yet, so the first observed revision deploys).

## 2. Watch it happen

```sh
plico status
```

```text
STACK    STATUS     DEPLOYED   UPDATED        NEXT RUN   PENDING
webapp   deploying  —          just now       on delta   a1b2c3d
```

Follow the structured logs (one correlation `run_id` per deployment):

```sh
journalctl -u plico -f -o cat
```

Each run walks the [deploy pipeline](../concepts/pipeline.md): fetch → diff →
pre-hooks → decrypt → pull → `up` → verify → post-hook → persist state.

## 3. Trigger a deploy on demand

You don't have to wait for a poll tick. The client CLI talks to the daemon over
its unix socket:

```sh
plico dry-run    --stack webapp   # show the delta + pending commits, no action
plico deploy-now --stack webapp   # deploy immediately, window or not
```

To redeploy the *current* revision (no git delta), add `--force`:

```sh
plico deploy-now --stack webapp --force
```

See the full [CLI reference](../reference/cli.md).

## 4. Confirm health

After `up`, plico verifies every service is `running` and `healthy` (or has no
healthcheck) within `verify_timeout`. A local semantic healthcheck is exposed:

```sh
curl -s http://127.0.0.1:9444/healthz
```

Between deployments, [drift detection](../concepts/monitoring.md) keeps watching
each stack and notifies on a regression.

## Where to go next

- Add a [backup pre-deploy hook](../guides/backup-hook.md) — the reason plico
  exists.
- [Encrypt your secrets with SOPS](../guides/sops-secrets.md).
- Restrict deploys to a [maintenance window](../guides/scheduling-windows.md).
