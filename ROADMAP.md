# Roadmap plico

> Philosophie constante : **orchestrer** `git`, `sops`, `docker compose` — ne rien
> réimplémenter. Chaque ajout doit rester compréhensible par un mainteneur seul.
> Pull-only assumé : pas de webhook, la fenêtre de déploiement est un choix, pas
> une contrainte.

## ✅ v0 — MVP (livré)

- Polling git par stack (clone/fetch incrémental, auth HTTPS par domaine via
  `GIT_ASKPASS` = le binaire lui-même, auto-réparation d'un clone corrompu)
- **Gate de backup pré-déploiement bloquant** (`.deploy/pre-deploy.sh` du repo
  prioritaire, fallback global ; `exit ≠ 0` → pas de déploiement, retry au tick
  suivant) + hook post-déploiement non bloquant
- Secrets SOPS **déchiffrés en mémoire** (`sops exec-env`, chiffrement partiel
  `.sops.yaml`/`encrypted_regex` supporté) ; mode tmpfs Linux en option
- `pull` + `up -d --remove-orphans` ; un pull qui échoue ne touche pas la stack
- Vérification post-up (healthchecks), verrou par stack, sémaphore global,
  arrêt gracieux qui draine les runs en vol
- `/healthz` sémantique, logs slog JSON avec `run_id`, state.json atomique,
  notifications ntfy dédupliquées (même échec = une seule alerte)
- Tooling : mise (tasks), lefthook, smoke test e2e (git + docker + sops/age)
  en CI GitHub Actions, releases GoReleaser (linux/darwin/freebsd × amd64/arm64)

## 🎯 v1 — Opérabilité (priorité 1 : le différenciateur)

C'est ce qui transforme le daemon en outil qu'on opère. Rien de tout ceci
n'existe dans doco-cd.

- [x] **Planning par stack** (F5, F7, F8) : expression cron + fenêtre horaire
      **stricte** (jamais de déploiement hors fenêtre ; fenêtre manquée =
      WARN, jamais rattrapée en retard), ancre persistée dans state.json
      (redémarrage dans une fenêtre ouverte → ré-ouverture ; fenêtre déjà
      honorée → pas de rejeu), `window >= poll_interval` imposé, surcharge du
      global par stack, opt-out `@poll`, timezone configurable, DST documenté
      (heure sautée = pas de run ; heure répétée = un seul run), `next_run`
      exposé dans /healthz
- [x] **Distinction check / apply** (F6) : `check = true` par stack (héritage
      du global) — hors fenêtre, fetch + diff à chaque tick et notification
      `deploy_queued` une seule fois par révision en attente (dédup persistée
      `last_queued_sha`, non ré-annoncée par l'apply de la fenêtre, nettoyée
      au succès) ; outcome `queued` visible dans /healthz
- [x] **CLI cliente** via socket unix (F24–F30) : `status`, `check-now`,
      `deploy-now` (`--force` pour redéployer la révision courante — la
      sortie de panne post-verify), `dry-run` (delta + commits en attente),
      `validate` ; `--skip-pre` interdit sans `--force` (imposé côté daemon)
      + notification `pre_hook_skipped` ; socket `/run/plico/plico.sock`
      recommandé (`<base_dir>/plico.sock` compatible), actions sérialisées
      par les verrous du deployer
- [x] **Installateur de release** : détection OS/architecture, latest ou
      version épinglée, binaire local, checksums, installation atomique,
      configuration systemd idempotente et rollback d'upgrade
- [ ] ~~config.d + deep-merge + SIGHUP (F21–F23)~~ **abandonné tel que
      spécifié** (juillet 2026 — raisonnement dans « Hors périmètre »).
      Ce qui remplace le besoin :
      - documenter « `systemctl restart` = reload » comme mécanisme officiel
        d'application d'un changement de config (déjà vrai et testé : drain
        des runs en vol, ancres de planning persistées, fenêtre encore
        ouverte ré-ouverte au redémarrage)
      - option future `stacks_dir = "/etc/plico/stacks.d"` : un fichier par
        stack contenant uniquement des blocs `[[stack]]` complets,
        concaténés puis validés globalement **fail-closed** — PAS de
        surcharge du global par fichier (l'héritage global→stack existe
        déjà dans `applyDefaults`). À faire seulement si le besoin
        d'automatisation multi-hôtes se matérialise
- [ ] **Multi-notifiers** (F31–F33) : webhook générique (Teams/Google Chat) +
      SMTP, filtrage par événement et par canal
- [ ] **Heartbeat Uptime Kuma** par stack (F36)

## 🔭 v1.x — Combler l'écart doco-cd (à la carte, dans cet ordre)

- [ ] **`path` par stack (monorepo)** : pointer un sous-répertoire du repo —
      effort faible, forte valeur, aligné avec le modèle existant
- [ ] **Détection de dérive (« reconciliation-lite »)** : re-check périodique
      de la santé des stacks entre les déploiements via `compose ps` →
      notification sur dérive (unhealthy, service arrêté à la main). **Pas de
      remédiation automatique** : backup + alerte + humain, toujours
- [ ] **Options compose fines par stack** : `profiles`, `env_files`
      additionnels, `remove_orphans` désactivable, args `up` supplémentaires
- [ ] **`plico healthcheck`** : sous-commande qui sonde son propre /healthz
      (HEALTHCHECK Docker, watchdog systemd)
- [ ] **`/metrics` Prometheus** : `deployments_total`, `deploy_errors_total`,
      `poll_duration_seconds`, `deployments_active`… (nommage inspiré de
      doco-cd)
- [ ] **Renovate/Dependabot** sur le repo (deps Go + versions mise épinglées)
- [ ] Clone shallow (`--depth 1`) en option ; `ref` = tag ou SHA épinglé

## 🌅 v2+ — Plus tard, peut-être

- [ ] **Podman** : nouvelle implémentation de l'interface `compose.Runtime`
      (prévue pour, rien d'autre à toucher)
- [ ] Rollback données assisté (restic restore guidé — jamais automatique)
- [ ] Image conteneur officielle (en assumant les compromis socket/volume)
- [ ] Quiesce standardisé avant dump (label/convention), si le besoin émerge
      des hooks réels

## 🚫 Hors périmètre — décisions, pas des oublis

| Écarté | Pourquoi |
|---|---|
| **Webhooks** | Pull-only assumé : sans rolling deploy, l'instantanéité n'apporte rien ; la fenêtre de déploiement est le vrai besoin |
| **Swarm** | Abandonné en amont, aucune nouveauté — aucun intérêt |
| **Reconciliation événementielle** (events Docker → redeploy auto) | Complexité forte, contraire à la philosophie backup + alerte + humain ; la détection de dérive en couvre l'essentiel |
| **Auto-discovery de stacks** | Magique ; tout doit être explicite dans la config |
| **Build d'images** | C'est le travail d'une CI, pas d'un déployeur |
| **Sources OCI, providers de secrets externes** (Vault, 1Password…) | sops + age suffisent ; chaque provider est une surface de maintenance |
| **Zéro-downtime dans plico** | Si nécessaire un jour : blue-green derrière Traefik/Caddy, orchestré par les hooks pre/post-deploy existants — hors du binaire |
| **SIGHUP (reload à chaud, F22)** | `systemctl restart` fait déjà tout, correctement : drain des runs en vol, ancres persistées, fenêtre ouverte ré-ouverte — durement acquis (3 rounds de revue). Rejouer cette logique en process vivant (stacks retirées pendant un run, schedule modifié fenêtre ouverte…) = la partie la plus délicate du code, pour un gain nul avec un poller à 60 s |
| **Deep-merge config.d (F21)** | L'héritage global→stack existe nativement (`applyDefaults` : schedule, window, check, hook_timeout) ; la sémantique de fusion scalaire/map/tableau + champs protégés est le point de complexité documenté de doco-cd. À l'échelle de quelques stacks, un seul fichier reste lisible ; si besoin un jour : `stacks_dir` concaténé sans merge (cf. v1) |
| **Stack invalide ignorée + alerte (F23)** | Contraire à « échouer bruyamment » : une stack silencieusement non gérée (alerte ratée = fenêtres manquées, ancre qui dérive) est pire qu'un daemon qui **refuse de démarrer** avec un message précis. Le filet est `plico validate` avant restart |

## Mémo sécurité (contexte, pas une tâche)

Le « jamais en clair sur disque » couvre le périmètre plico (base_dir,
worktrees, logs — ce que restic sauvegarde). Les valeurs interpolées vivent
ensuite dans la config des conteneurs (`/var/lib/docker`, `docker inspect`) :
propriété inhérente à l'injection par variables d'environnement, commune à
tous les outils du genre. Au reboot, `unless-stopped` **redémarre** les
conteneurs avec leur config existante — ni sops, ni la clé age, ni plico ne
sont sollicités ; la clé n'est nécessaire qu'au prochain déploiement.
