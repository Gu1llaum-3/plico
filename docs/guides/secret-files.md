---
title: Bind-mount a secret file
icon: lucide/file-key
---

# Bind-mount a decrypted secret file

!!! note "Guide in progress"
    A full walkthrough is coming.

This guide will take an encrypted TLS key from the repo, expose it through
`$PLICO_SECRETS_DIR`, and bind-mount it read-only into a container — with
cleartext living only in tmpfs.

**Meanwhile:**

- [Concepts → Secrets](../concepts/secrets.md#secrets-encrypted-files-bind-mounted-from-tmpfs)
  — the `[secrets]` mechanism and its Linux-only / reboot caveats.
- [Reference → Repo manifest](../reference/manifest.md#secrets-encrypted-files)
  — the `[secrets]` keys.
