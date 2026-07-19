# Installation de plico

## Prérequis

Le binaire plico est statique, mais orchestre `git`, `docker compose` et, pour
les stacks concernées, `sops`. Vérifier ces commandes sous l'utilisateur qui
fera tourner le daemon. L'accès au groupe du socket Docker équivaut
pratiquement à un accès root.

## Installer une release

Télécharger et inspecter le script avant de l'exécuter :

```sh
curl -fsSLO https://raw.githubusercontent.com/Gu1llaum-3/plico/main/install.sh
less install.sh
sudo sh install.sh
```

`latest` désigne la dernière release GitHub stable. Une version précise et un
binaire local sont supportés :

```sh
sudo sh install.sh --version v1.2.3
sudo sh install.sh --binary ./plico
sudo sh install.sh --binary ./plico --sha256 <sha256-du-binaire>
```

Le téléchargement officiel vérifie toujours `checksums.txt`. Avec un binaire
local, `--sha256` est facultatif car le fichier est déjà dans le périmètre de
confiance de l'opérateur, mais reste recommandé.

Plateformes : Linux, Darwin et FreeBSD sur amd64/arm64. La configuration
complète du service est réservée à Linux/systemd ; ailleurs, ou avec
`--binary-only`, seul le binaire est installé.

## Configuration initiale

Sans `--config`, le script installe `/etc/plico/config.toml.example`, crée un
`plico.env` vide et ne démarre pas de daemon. Préparer une configuration puis :

```sh
sudo sh install.sh --config ./config.toml --env-file ./plico.env
```

Ce second passage active et démarre le service. Il sert aussi à reprendre une
première tentative de démarrage qui aurait échoué.

Un `config.toml` ou `plico.env` existant n'est jamais remplacé. Le template
`.example` est géré par l'installateur et peut être actualisé aux upgrades.

Options utiles :

```text
--version VERSION
--binary PATH
--sha256 HASH
--config PATH
--env-file PATH
--operator USER
--binary-only
--no-start
```

`--operator USER` ajoute l'utilisateur au groupe `plico` pour accéder au
socket `0660`. Une reconnexion est nécessaire après un changement de groupe.

## Layout et permissions

```text
/usr/local/bin/plico          root:root   0755
/etc/plico                    root:plico  0750
/etc/plico/config.toml        root:plico  0640
/etc/plico/plico.env          root:plico  0600
/var/lib/plico                plico:plico 0750
/var/lib/plico/state.json     plico:plico 0600
/run/plico                    plico:plico 0750 (créé par systemd)
/run/plico/plico.sock         plico:plico 0660
/opt/docker                   plico:plico 0750
```

Les worktrees `/opt/docker/<stack>` sont reconstructibles depuis Git. Les
données applicatives doivent vivre dans des volumes nommés ou des chemins
externes dédiés, jamais dans le worktree susceptible d'être recloné.

## Mise à jour et rollback

Relancer l'installateur avec `latest`, `--version` ou `--binary`. Le remplacement
du binaire est atomique. Si le service était actif, il n'est redémarré que si
le binaire ou l'unité a changé. L'installateur attend ensuite que la CLI
rejoigne réellement la socket ; un simple succès de `systemctl restart` ne
suffit pas. En cas d'échec, le binaire et l'unité précédents sont restaurés.

L'état enabled/disabled et les fichiers opérateur sont préservés.

## Migration depuis le layout historique

Une mise à jour ne déplace rien automatiquement. Pour migrer :

1. Arrêter plico.
2. Copier `<base_dir>/state.json` vers `/var/lib/plico/state.json` avec
   propriétaire `plico:plico` et mode `0600`.
3. Ajouter `state_file = "/var/lib/plico/state.json"`.
4. Ajouter `[api] socket = "/run/plico/plico.sock"`.
5. Vérifier que l'unité contient `RuntimeDirectory=plico` et
   `StateDirectory=plico`.
6. Redémarrer et vérifier `plico status` avant de supprimer les anciens
   fichiers runtime.

Ne jamais démarrer sur un état vide pendant la migration : les SHA seraient
considérés non déployés et les hooks pourraient être rejoués.

## Diagnostic

```sh
systemctl status plico --no-pager -l
journalctl -u plico -n 100 --no-pager -o cat
sudo -u plico docker info
sudo -u plico docker compose version
plico status
```

`plico status` ne requiert pas les secrets du daemon : il lit uniquement le
chemin de socket dans la configuration.
