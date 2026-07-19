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
   ⚠️ Le daemon tourne avec `UMask=0022` pour que les fichiers des clones
   restent lisibles par les conteneurs bind-montés (uid arbitraire) — les
   hooks en héritent : un hook qui écrit des données sensibles (dump de base
   avant backup, par exemple) doit poser `umask 077` en tête de script ou
   écrire dans un répertoire aux permissions restreintes.
3. Déchiffrement SOPS en mémoire : `sops exec-env secrets.enc.env -- docker
   compose … up -d`. Mode `tmpfs` (`/dev/shm`) disponible sur Linux.
4. `docker compose pull` (option `force_pull`) puis `up -d --remove-orphans`.
   Un pull qui échoue laisse la stack en place.
5. Vérification post-up : tous les services `running` et `healthy` (ou sans
   healthcheck) dans `verify_timeout` ; échec immédiat sur `unhealthy`.
6. Hook post-déploiement optionnel (non bloquant), notification, état persisté.

Un run encore en cours au tick suivant est **sauté**, jamais empilé.

### Planning par stack

Sans `schedule`, une stack est déployée dès qu'un delta git est détecté. Avec
un `schedule` (cron, évalué dans `timezone`), chaque déclenchement **ouvre une
fenêtre de déploiement** de durée `window` (défaut 1 h) : pendant la fenêtre,
chaque tick de polling peut déployer ; en dehors, la stack n'est pas touchée.

```toml
schedule = "0 22 * * *"   # global : fenêtre à 22h pour toutes les stacks
window = "2h"

[[stack]]
name = "critique"
schedule = "0 4 * * *"    # surcharge : celle-ci à 4h du matin
window = "30m"

[[stack]]
name = "dev"
schedule = "@poll"        # opt-out : déploie à chaque tick, comme sans planning

[[stack]]
name = "surveillee"
schedule = "0 22 * * *"
check = true              # hors fenêtre : fetch + diff à chaque tick, et
                          # notification « déploiement en attente » (une seule
                          # fois par révision) — sans rien appliquer
```

**La fenêtre fait autorité** : plico ne déploie jamais en dehors, à une
tolérance près d'un `poll_interval` sur le tick qui découvre le déclenchement
(le jitter du ticker ne transforme pas un déclenchement sain en fenêtre
manquée). Un déclenchement dont la fenêtre est entièrement passée (daemon
arrêté, hôte en pause, run précédent couvrant toute la fenêtre) est **loggé
en WARN et jamais rattrapé en retard**. L'ancre de planning (dernier
déclenchement traité + l'expression cron utilisée) est persistée dans
`state.json` : un redémarrage *pendant* une fenêtre encore ouverte la
ré-ouvre ; une fenêtre déjà honorée n'est pas rejouée ; **modifier le
`schedule` ré-ancre au redémarrage** (pas de déclenchements fantômes sous la
nouvelle expression). Le nombre de tentatives dans une fenêtre vaut environ
`window / poll_interval` — dimensionner large si on veut des retries ; un run
lancé en fin de fenêtre peut la déborder, il est attribué à la fenêtre qui
l'a lancé. `/healthz` expose `next_run` par stack. **DST** : un déclenchement
tombant dans l'heure sautée ne s'exécute pas ; dans l'heure répétée, il
s'exécute une seule fois (première occurrence).

## Installation

Dernière version stable :

```sh
curl -fsSLO https://raw.githubusercontent.com/Gu1llaum-3/plico/main/install.sh
less install.sh
sudo sh install.sh
```

Version précise ou binaire déjà téléchargé :

```sh
sudo sh install.sh --version v1.2.3
sudo sh install.sh --binary ./plico --sha256 <sha256>
```

Sous Linux/systemd, l'installateur prépare l'utilisateur de service, les
répertoires, `/etc/plico/plico.env` et l'unité systemd. Sans configuration
active il n'active pas le service : copier et adapter
`/etc/plico/config.toml.example`, puis relancer l'installateur avec
`--config`. Sur Darwin et FreeBSD, seule l'installation du binaire est faite.
Voir [le guide d'installation](docs/installation.md) pour les mises à jour,
le mode hors-ligne, les permissions Docker et la migration des chemins.

Installation depuis les sources pour contribuer :

```sh
mise install
mise run build        # → bin/plico
```

Tâches disponibles : `mise run build | test | lint | fmt | smoke |
install-test | release-test | shellcheck`.

`mise run smoke` exécute [`test/smoke.sh`](test/smoke.sh) : un environnement
GitOps complet en local (repo git `file://`, vraie stack Docker, secrets
chiffrés **age + sops**) qui vérifie le déploiement nominal, l'injection des
secrets en mémoire (aucun clair sur disque ni dans les logs), puis le blocage
par le gate de backup. Nécessite un daemon Docker.

## Configuration

Copier [`config.example.toml`](config.example.toml) vers
`/etc/plico/config.toml`. Les secrets (tokens git, ntfy) passent par
interpolation `${ENV_VAR}` — une variable absente empêche le démarrage du
daemon et la commande `validate`.

```sh
bin/plico serve --config /etc/plico/config.toml
```

- `GET /healthz` (127.0.0.1:9444) : healthcheck **sémantique** — 503 si le
  scheduler ne tick plus ou si un run dépasse `run_timeout`.
- Logs JSON structurés (slog), un `run_id` de corrélation par déploiement.
- Notifications ntfy orientées échec : `deploy_queued`, `deploy_start`,
  `pre_hook_failed`, `pre_hook_skipped`, `deploy_failed`, `deploy_success`.

### Layout système

Les nouvelles installations séparent données persistantes et runtime :

| Chemin | Contenu | Sauvegarde |
|---|---|---|
| `/opt/docker/<stack>` | worktrees Git, reconstructibles | optionnelle |
| `/var/lib/plico/state.json` | SHA, échecs, files d'attente, ancres cron | **oui** |
| `/run/plico/plico.sock*` | socket et verrou volatils | non |
| `/etc/plico` | configuration, environnement, clé age | **oui** |

Ne jamais placer une base ou des uploads irremplaçables dans un worktree :
plico peut le supprimer et le recloner pour réparer Git. Les anciennes
configurations sans `state_file` ni `[api].socket` conservent exactement le
layout historique sous `base_dir`.

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

### CLI cliente

Le daemon expose une API locale sur un socket unix (`[api] socket`, recommandé
à `/run/plico/plico.sock`; fallback historique `<base_dir>/plico.sock`). Les commandes passent par les
verrous du daemon — jamais de déploiement concurrent au scheduler :

```sh
plico status                      # par stack : statut, SHA, en attente, prochaine fenêtre
plico check-now  --stack X|--all  # fetch + diff immédiat, notifie sans déployer
plico deploy-now --stack X|--all  # déploiement immédiat, hors fenêtre
plico deploy-now --stack X --force            # redéploie la révision courante
plico deploy-now --stack X --force --skip-pre # saute le gate de backup (bruyant, notifié)
plico dry-run    --stack X        # delta + commits en attente, sans agir
plico validate                    # vérifie la config sans démarrer
```

Les commandes clientes ne chargent que `base_dir` et `[api].socket` : elles
n'ont pas besoin des tokens Git/ntfy présents uniquement dans l'environnement
systemd. `--socket` évite entièrement la lecture de la configuration.

`--skip-pre` est refusé sans `--force` — côté client **et** côté daemon — et
déclenche une notification `pre_hook_skipped` (F30). Toutes les commandes
acceptent `-c` (config, pour localiser le socket) ou `--socket`.

### Auth Git

HTTPS par domaine via `[git.auths."<host>"]` : plico se passe lui-même en
`GIT_ASKPASS` au sous-processus git — le token n'apparaît **ni sur disque, ni
dans l'argv, ni dans `.git/config`**. Les remotes SSH utilisent l'agent du
système.

### Supervision (systemd)

Le baby-sitting du process est délégué au superviseur ; plico garde le
scheduling interne.

L'installateur déploie [`packaging/plico.service`](packaging/plico.service).
`RuntimeDirectory=plico` crée `/run/plico`, `StateDirectory=plico` prépare
`/var/lib/plico`, et `EnvironmentFile=-/etc/plico/plico.env` garde les secrets
hors de l'unité. Le `-` rend le fichier optionnel sur une installation neuve.

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
