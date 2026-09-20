# TinyVMOS

Distribution Linux qui transforme un ordinateur physique en hyperviseur de micro-VM.
Une VM par utilité : un service, une application, un coffre de données, une IA.
Décentralisé sur les VM, recentralisé dans une interface unique.

## Décisions techniques

| Sujet | Choix |
|---|---|
| Noyau | Linux avec KVM |
| Hyperviseur | Firecracker, derrière une interface Go `VMM` qui le rend remplaçable |
| Plan de contrôle | Démon en **Go**, API REST et WebSocket décrite en OpenAPI |
| OS hôte | Buildroot, minimal, reproductible |
| OS invité | Alpine minimal, un seul service par image |
| Réseau | Bridges Linux par zone, règles nftables en politique « tout refuser » |

## Démarrage

```bash
./build/check-env.sh
```

Ce script vérifie que la machine de développement a tout ce qu'il faut.
Il doit passer avant toute autre chose.

## Documents de référence

- `docs/PROJET.md` — cahier des charges complet
- `docs/developpement_projet.md` — plan de développement, phase par phase

## Arborescence

| Dossier | Rôle |
|---|---|
| `docs/` | Architecture, sécurité, guides |
| `os/` | OS hôte : Buildroot, détection matérielle, installateur |
| `images/` | Catalogue : une image de micro-VM par service |
| `controlplane/` | Démon Go — le cœur du projet |
| `cli/` | `tinyvmosctl`, client en ligne de commande |
| `web/` | Interface web et PWA |
| `mobile/` | Application Flutter, livrée en APK |
| `build/` | Scripts de build : ISO, IMG, OVA, CI |
| `tests/` | Tests d'intégration, isolation réseau, bout en bout |
