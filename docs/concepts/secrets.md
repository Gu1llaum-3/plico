---
title: Secrets
icon: lucide/lock
---

# Secrets

plico's rule is absolute: **secrets never hit disk in cleartext under
`base_dir`**. Encrypted files live in the git repo; only the age key stays on
the server. plico decrypts through the `sops` CLI into the process environment
or a tmpfs, never a file plico writes to persistent storage.

## Two mechanisms

### `[sops]` — encrypted env, injected in memory

sops-encrypted dotenv files, committed in the repo, decrypted **in memory** and
injected as environment for `${VAR}` interpolation by docker compose.

```toml
# .plico.toml (in the stack repo)
[sops]
files = ["secrets/app.enc.env"]
mode  = "exec-env"   # or "tmpfs"
```

- **`exec-env`** — `sops exec-env file.enc.env -- docker compose … up -d`. The
  values exist only in the compose process environment.
- **`tmpfs`** (Linux only) — decrypted to an `--env-file` on `/dev/shm`.

The server's `sops_files`, if set, override the repo's `[sops]` wholesale.

### `[secrets]` — encrypted files, bind-mounted from tmpfs

For secrets an app reads as **files** (TLS certs, keys) rather than env. plico
decrypts them onto a stable per-stack **tmpfs** directory exposed as
`$PLICO_SECRETS_DIR`, so the compose file can bind-mount them read-only.

```toml
[secrets]
files = ["secrets/tls.enc.key"]   # → ${PLICO_SECRETS_DIR}/tls.key
```

```yaml
# docker-compose.yml
services:
  web:
    volumes:
      - ${PLICO_SECRETS_DIR}/tls.key:/etc/ssl/private/tls.key:ro
```

Cleartext lives only in RAM. The `.enc` marker is stripped from the target
name. **Linux-only** (needs a real tmpfs), and a reboot clears it — the stack
re-materialises the files on its next deploy. The tmpfs directory is per-uid
and owner-checked.

## Partial encryption is recommended

plico decrypts through `sops exec-env`: the repo's `.sops.yaml` is only used at
*encryption* time (decryption metadata is embedded in the file). So **partial
encryption** — only sensitive values encrypted, the rest readable in git
diffs — works natively:

```yaml
# .sops.yaml at the stack repo root
creation_rules:
  - path_regex: secrets/.*\.enc\.env$
    encrypted_regex: "(SECRET|PASSWORD|TOKEN|KEY)"
    mac_only_encrypted: true
    age: age1...   # recipient(s)
```

```sh
sops encrypt --in-place secrets/app.enc.env   # or: sops edit
```

!!! warning "MAC mismatch on hand edits"
    Without `mac_only_encrypted: true`, the MAC also covers cleartext values: a
    hand edit (outside `sops edit` / `sops set`) breaks decryption — plico fails
    at the `sops` stage with "MAC mismatch". Always edit through sops.

## Where the guarantee is enforced

The smoke test asserts no secret leaks into `base_dir`, logs, or notifications.
The hook environment also **withholds** `SOPS_AGE_KEY_FILE` from repo hooks (see
[Layered hooks](hooks.md)). Follow the [SOPS guide](../guides/sops-secrets.md)
to set it all up.
