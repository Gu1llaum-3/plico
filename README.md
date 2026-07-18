# plico

> *plicare* « plier » → *plico* « je déplie » — déployer, littéralement.

**plico** est un déployeur GitOps *pull-based* pour stacks Docker Compose, à la
FluxCD mais pour Compose standalone. Sa raison d'être face à
[doco-cd](https://github.com/kimdre/doco-cd) : un **gate de sauvegarde
pré-déploiement bloquant** (si le backup échoue, on ne déploie pas), un layout
host-path lisible (`/opt/docker/<stack>/`) facile à sauvegarder avec restic, et
des secrets SOPS **jamais écrits en clair sur disque**.

plico **orchestre** des CLIs matures — `git`, `sops`, `docker compose` — via
`os/exec`. Il ne réimplémente ni Git, ni Compose, ni SOPS.

## Fonctionnement

À chaque `poll_interval`, pour chaque stack :

1. `git fetch` + comparaison du SHA `origin/<ref>` avec le dernier SHA déployé
   (persisté dans `state.json`). Pas de delta → no-op silencieux.
2. **Hook pré-déploiement** : `.deploy/pre-deploy.sh` du repo (prioritaire) ou
   chemin global configuré. Il reçoit `DEPLOY_STACK`, `DEPLOY_DIR`,
   `DEPLOY_GIT_REF`, `DEPLOY_OLD_SHA`, `DEPLOY_NEW_SHA`.
   **`exit != 0` → déploiement abandonné**, notification, retry au tick suivant.
3. Déchiffrement SOPS en mémoire : `sops exec-env secrets.enc.env -- docker
   compose … up -d`. Mode `tmpfs` (`/dev/shm`) disponible sur Linux.
4. `docker compose pull` (option `force_pull`) puis `up -d --remove-orphans`.
   Un pull qui échoue laisse la stack en place.
5. Vérification post-up : tous les services `running` et `healthy` (ou sans
   healthcheck) dans `verify_timeout` ; échec immédiat sur `unhealthy`.
6. Hook post-déploiement optionnel (non bloquant), notification, état persisté.

Un run encore en cours au tick suivant est **sauté**, jamais empilé.

## Installation

```sh
mise install          # go, lefthook, golangci-lint, sops, age (versions dans mise.toml)
mise run build        # → bin/plico
```

Tâches disponibles : `mise run build | test | lint | fmt | smoke`.

`mise run smoke` exécute [`test/smoke.sh`](test/smoke.sh) : un environnement
GitOps complet en local (repo git `file://`, vraie stack Docker, secrets
chiffrés **age + sops**) qui vérifie le déploiement nominal, l'injection des
secrets en mémoire (aucun clair sur disque ni dans les logs), puis le blocage
par le gate de backup. Nécessite un daemon Docker.

## Configuration

Copier [`config.example.toml`](config.example.toml) vers
`/etc/plico/config.toml`. Les secrets (tokens git, ntfy) passent par
interpolation `${ENV_VAR}` — une variable absente empêche le démarrage.

```sh
bin/plico serve --config /etc/plico/config.toml
```

- `GET /healthz` (127.0.0.1:9444) : healthcheck **sémantique** — 503 si le
  scheduler ne tick plus ou si un run dépasse `run_timeout`.
- Logs JSON structurés (slog), un `run_id` de corrélation par déploiement.
- Notifications ntfy orientées échec : `deploy_queued`, `deploy_start`,
  `pre_hook_failed`, `pre_hook_skipped`, `deploy_failed`, `deploy_success`.

### Secrets SOPS : chiffrement partiel recommandé

plico déchiffre via `sops exec-env` : le `.sops.yaml` du repo ne sert qu'au
chiffrement, les métadonnées de déchiffrement étant embarquées dans le
fichier. Le **chiffrement partiel** (seules les valeurs sensibles sont
chiffrées, le reste lisible en diff git) est donc supporté nativement :

```yaml
# .sops.yaml à la racine du repo de stack
creation_rules:
  - path_regex: \.deploy/.*\.enc\.env$
    encrypted_regex: "(SECRET|PASSWORD|TOKEN|KEY)"
    mac_only_encrypted: true
    age: age1...   # destinataire(s)
```

```sh
sops encrypt --in-place .deploy/secrets.enc.env   # ou: sops edit
```

⚠️ Sans `mac_only_encrypted: true`, le MAC couvre aussi les valeurs en
clair : une édition à la main (hors `sops edit`/`sops set`) casse le
déchiffrement — plico échouera au stage `sops` avec « MAC mismatch ».

### Auth Git

HTTPS par domaine via `[git.auths."<host>"]` : plico se passe lui-même en
`GIT_ASKPASS` au sous-processus git — le token n'apparaît **ni sur disque, ni
dans l'argv, ni dans `.git/config`**. Les remotes SSH utilisent l'agent du
système.

### Supervision (systemd)

Le baby-sitting du process est délégué au superviseur ; plico garde le
scheduling interne.

```ini
[Unit]
Description=plico GitOps deployer
After=network-online.target docker.service

[Service]
ExecStart=/usr/local/bin/plico serve --config /etc/plico/config.toml
Restart=always
RestartSec=5
Environment=SOPS_AGE_KEY_FILE=/etc/plico/age.key
Environment=PLICO_BITBUCKET_TOKEN=…   # ou EnvironmentFile=
# SIGHUP réservé au rechargement de conf (v1)

[Install]
WantedBy=multi-user.target
```

## Rollback (v1 = manuel, assumé)

- **Code / config compose** : `git revert` dans le repo de la stack ; plico
  redéploie la révision précédente au tick suivant.
- **Données** : le backup pris par le hook pré-déploiement (dump + restic) se
  restaure manuellement. Pas de restore automatique en v1 — c'est un choix.

Après un échec de vérification post-up, plico enregistre quand même le nouveau
SHA pour ne pas redéployer en boucle une révision cassée : la sortie de panne
passe par `git revert` (ou `deploy-now --force`, v1).

## Feuille de route

- **v1** : `config.d/<stack>.toml` + rechargement SIGHUP, planning cron et
  fenêtre par stack, CLI cliente (`status`, `check-now`, `deploy-now`,
  `dry-run`, `validate`) via socket unix, notifiers webhook + SMTP, heartbeat
  Uptime Kuma.
- **Plus tard** : support **Podman** (le runtime est déjà derrière une
  interface `compose.Runtime`), métriques Prometheus, image conteneur,
  réception de webhooks.
