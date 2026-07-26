---
title: Migrating older installs
icon: lucide/move-right
---

# Migrating from the historical layout

Older configurations without `state_file` or `[api].socket` keep the exact
historical layout under `base_dir`. An upgrade **never moves anything
automatically**. To migrate to the split [system layout](layout.md):

1. **Stop plico.**

   ```sh
   sudo systemctl stop plico
   ```

2. **Move the state file**, preserving ownership and mode:

   ```sh
   sudo install -o plico -g plico -m 0600 \
     <base_dir>/state.json /var/lib/plico/state.json
   ```

3. **Add `state_file`** to the config:

   ```toml
   state_file = "/var/lib/plico/state.json"
   ```

4. **Add the socket:**

   ```toml
   [api]
   socket = "/run/plico/plico.sock"
   ```

5. **Check the unit** contains `RuntimeDirectory=plico` and
   `StateDirectory=plico`.

6. **Restart and verify** before removing the old runtime files:

   ```sh
   plico validate
   sudo systemctl start plico
   plico status
   ```

!!! danger "Never start on an empty state"
    During the migration, never let plico start against an empty state: every
    SHA would be considered undeployed and hooks could be replayed. Move
    `state.json` *before* the first start on the new layout.
