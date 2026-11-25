---
title: Moving a stack with data
icon: lucide/package-open
---

# Moving a stack with data

plico has no automatic handover between hosts, and this is deliberate. A stack
that owns data (a SQLite file in a named volume, a Postgres volume, uploads)
must be **stopped on one host before it runs on another**, and the data must be
restored before the first start. Two running copies that write to the same data
diverge silently; zero running copies means downtime. Only a human can choose
the moment to switch.

This page is the manual procedure. It applies to any stack whose state is not
fully rebuildable from Git.

!!! warning "Never run the same stack on two hosts at once"
    Two plico instances that both deploy a stack that writes to shared or
    copied data will each accept changes that the other never sees. If the
    application also drives an external system (a device, an API, a building
    controller), both copies can send conflicting commands to it. Do not rely
    on Git to prevent this: Git only says what *should* run, not what *is*
    running.

## Before you start

- The stack's data lives in **named volumes** or dedicated paths outside the
  worktree (see [System layout](layout.md#paths-and-what-to-back-up)).
- You have a backup tool that can produce a restorable snapshot of those
  volumes. The usual choice is restic, through the same
  [pre-deploy hook](../concepts/hooks.md) plico already runs on each deploy.
- The new host has the secrets the stack needs (age key, sops-encrypted files,
  registry credentials). Check this *before* the cutover, not during it.
- Both hosts' `config.toml` are at hand. The stack appears in exactly one of
  them at any time.

Plan a window. The stack is down from step 2 to step 6.

## Procedure

Each step has a checkpoint. Do not go to the next step until the checkpoint
passes.

### 1. Confirm the current state on the old host

```sh
plico status
sudo -u plico docker compose -f /opt/docker/<stack>/<compose_file> ps
```

Note the running state and the deployed SHA. This is the reference for the
rollback path below.

### 2. Decommission the stack on the old host

Stop the stack with plico itself, so it can never be redeployed from there:

```sh
sudo -u plico plico decommission --stack <stack>
```

This runs `docker compose down` without volumes, keeps the worktree and the
volumes, and records a tombstone. The stack must still be in `config.toml` at
this point. See the [CLI reference](../reference/cli.md#plico-decommission).

!!! danger "Never use `down -v`"
    `docker compose down -v` deletes named volumes. `plico decommission` never
    removes them. Data removal is a separate, deliberate act done only after the
    new host is verified (step 8).

**Checkpoint:** `docker compose -f /opt/docker/<stack>/<compose_file> ps -a`
shows no container for the stack.

### 3. Take a backup, after the stop

Back up the stack's volumes **while its containers are stopped**, so the copy
is consistent. Use the same backup tool and repository as your pre-deploy hook
(see [A backup pre-deploy hook](../guides/backup-hook.md)). Record the snapshot
ID.

**Checkpoint:** the snapshot exists and the backup tool reports it complete.

### 4. Remove the stack from the old host's configuration

Remove the stack from `config.toml` on the old host, then validate and restart
plico. The restart drops the decommission tombstone, so the old host forgets the
stack entirely:

```sh
sudo -u plico plico validate
sudo systemctl restart plico
plico status          # the stack must no longer be listed
```

The old host now neither runs nor deploys the stack. Its volumes are untouched.

**Checkpoint:** `plico status` no longer lists the stack, and `docker ps` still
shows no container for it.

### 5. Transfer and restore on the new host

On the new host, restore the snapshot **into the named volumes, before the
stack is configured there**:

```sh
# Restore the volumes from the snapshot (example, adapt to your backup tool).
restic restore <snapshot-id> --target /tmp/restore-<stack>
```

Copy the restored data into the volumes the compose file declares. Check the
ownership and permissions match what the container expects.

**Checkpoint:** the restored data passes a sanity check you define for the
application (row counts, a known record, an application-level integrity check).
Do not skip this: a restore that "looks fine" is the most common failure.

### 6. Start the stack on the new host

Add the stack to the new host's `config.toml`, validate, and deploy it once by
hand so the pre-deploy hook runs and takes a fresh backup first:

```sh
sudo -u plico plico validate
sudo systemctl restart plico
plico deploy-now --stack <stack>
```

**Checkpoint:** `plico status` shows the stack deployed on the new host, and the
container is healthy (`/healthz` or the application's own check).

### 7. Switch traffic

Only after step 6 passes, point clients, DNS or the reverse proxy at the new
host. Watch the application's logs and health for a while before you consider
the move done.

### 8. Clean up the old host (later, not now)

Keep the old host's volumes and the snapshot until the new host has run through
at least one full deploy cycle without incident. Then remove the old volumes
deliberately, with `docker volume rm`, after you have checked the snapshot can
still be restored.

## Rollback

If the new host fails at any checkpoint **before** step 7:

1. Decommission the stack on the new host and remove it from its configuration.
2. Re-add the stack to the old host's configuration and restart plico.
3. Deploy it there with `plico deploy-now --stack <stack> --force`.

The old volumes were never modified, so this restores the pre-move state. Do
not try to roll back by running both hosts at once.

After step 7, a rollback is a new move in the opposite direction, with the same
checkpoints.

## Why there is no automation

Automating the handover would mean plico decides when a stateful stack changes
host, and would need a distributed lock to guarantee that nothing runs twice.
Lock-based leader election is the job of an orchestrator, not of a pull-based
deployer for standalone Compose. plico keeps the procedure explicit, so the
human decides the moment and checks each checkpoint.

For stateless stacks (no volumes to restore), the move is simply: stop on the
old host (`plico decommission`), remove from its config, add to the new host's config. Nothing about
the data needs to be sequenced.
