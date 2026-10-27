# Récapitulatif temporaire - DOAL

Mise à jour : 27 octobre 2026. Ce fichier permet de reprendre le projet dans un nouveau chat.
Il est temporaire et peut être supprimé après transmission.

## Portée de cette discussion

La demande actuelle porte sur la transmission, le nettoyage justifié, les commits/pushes
et la consolidation des branches vers main. Les informations de produit ci-dessous
sont tirées des sources, de la documentation et de l'historique du dépôt ; elles ne
prétendent pas reconstituer toutes les conversations antérieures.

Révision de départ : `b2f3e6c10de911274996124bfa96d4a3a5ccc2a1`. Inventaire initial : 166 fichiers suivis.

## Produit et fonctionnement

Client local de test des trackers BitTorrent écrit en Go, avec interface web embarquée et simulation de trafic.

Binaire Go autonome, modules de configuration/trackers/annonces et contrôle HTTP/WebSocket. Les assets Tailwind et Lucide sont construits par npm run build:web puis embarqués dans le binaire. scripts/start-windows.ps1 est le lanceur Windows ; les données d'utilisateur vivent dans %LOCALAPPDATA%/DOAL.

## Choix de design à préserver

Interface locale de pilotage avec graphiques dont les dimensions sont contrôlées et icônes Lucide. Les assets web/static/app.css et lucide.min.js sont des sorties embarquées nécessaires, à conserver et synchroniser avec le build.

## Déjà réalisé avant ce nettoyage

- Correction des dimensions des graphiques et du lanceur Windows (b2f3e6c).
- Sérialisation des lots de trackers et callbacks d'annonce (efd9e63).
- Durcissement de la confiance proxy/réseau et des accès sortants (083b6d2, e760241).

## Nettoyage et préservation

Conserver les profils clients, les tests de protocoles et la configuration runtime locale. Ne jamais publier DOAL_SECRET_TOKEN. Le serveur administratif doit rester lié à 127.0.0.1.

L'inventaire distingue fichiers suivis, non suivis, ignorés et worktrees. Les candidats
à suppression sont vérifiés par rôle et consommateurs ; aucune suppression massive
de tests, documentation, médias, dépendances, secrets ou données personnelles.
Les sauvegardes des fichiers retirés et les diffs initiaux sont hors des dépôts, dans
le dossier local `Documents/Codex/cleanup-2026-10-27/apps/DOAL`.
Une liste de fichiers ne constitue pas une revue comportementale de chaque ligne.

## Vérification et livraison

Commandes de référence : npm ci ; npm run build:web ; npm audit ; go test ./... ; go vet ./... ; go build. Les tests doivent rester isolés et ne pas annoncer à des trackers externes.

Vérifié le 27 octobre : build des assets et syntaxe JavaScript réussis, npm audit à zéro, go vet ./... réussi. La suite Go complète passe avec GOMAXPROCS=2 (10 packages). Une première exécution parallèle a planté dans le runtime Go/Windows après PASS du package web ; aucun fichier Go ni go.mod n'a changé. Les mêmes assertions ont été rejouées intégralement avec le parallélisme borné, sans test désactivé.

## Consolidation de ce passage

Les pins Actions et Lucide sont réunis. Le verrouillage transitif source-map-js a été corrigé vers une version sans l'avis connu, sans changer les fonctionnalités.

## Restant à traiter ou à confirmer

- Les deux branches Dependabot sont réunies et les assets suivis reconstruits ; la suppression des branches distantes suit seulement la preuve de publication sur main.
- Le workflow .github/workflows/release.yml réagit aux pushes main ; vérifier son environnement de publication avant le push.
- Le fonctionnement du lanceur et du binaire installé reste une preuve distincte de la compilation.

## Reprendre sans perdre le contexte

Lire ce fichier puis les consignes AGENTS.md/CLAUDE.md, README et les documents de domaine
encore présents. Contrôler `git status`, `git branch -avv`, `git worktree list` et les
refs distantes actuelles avant de reprendre. Garder les sources, la CI, le déploiement,
le runtime chargé et la validation utilisateur comme preuves distinctes.
Aucun redémarrage/rechargement de service, activation de release ou changement de base
n'est autorisé par le seul nettoyage Git.
