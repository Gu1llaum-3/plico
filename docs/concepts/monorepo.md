---
title: Monorepos
icon: lucide/folder-tree
---

# Monorepos: several stacks in one repo

By default one repo is one stack, cloned into `<base_dir>/<name>`. Set a `path`
to point a stack at a **subdirectory** of its repo instead — the `compose_file`,
`sops_files` and the `.plico/` hooks resolve under it, and `docker compose` runs
with it as its working directory (so relative `build:`/`env_file:` paths inside
the compose file resolve correctly).

```toml
[[stack]]
name = "traefik"
repo = "https://github.com/acme/infra.git"
path = "traefik"      # <repo>/traefik/{docker-compose.yml,.plico/pre-deploy.sh,…}

[[stack]]
name = "nextcloud"
repo = "https://github.com/acme/infra.git"   # same repo, different subdir
path = "nextcloud"
```

## Change detection is scoped to the subdirectory

A commit that touches only `traefik/` redeploys `traefik` alone — `nextcloud`
sees the moved repo HEAD but, finding nothing changed under its own subtree,
stays untouched (no hook, no pull, no notification).

This is decided by `git diff --name-only <old>..<new> -- <path>`. Two
deliberate escape hatches:

- A **forced run** (`deploy-now --force`) and the **very first deploy** bypass
  the subtree check.
- If the diff **cannot be computed** (e.g. the old revision was dropped by an
  upstream force-push), plico deploys to be safe rather than miss a real change.

## Notes and limits

- Each stack still gets its **own clone** — a few MB duplicated per stack.
  plico does not share one checkout across stacks.
- `$DEPLOY_DIR` handed to hooks is the **subdirectory**, not the repo root.
- A change to a file **outside** every stack's `path` (a shared file at the
  repo root) triggers no redeploy, since compose/sops/hooks all live under
  `path`.

See the [Deploy a monorepo](../guides/monorepo-setup.md) guide for a worked
example.
