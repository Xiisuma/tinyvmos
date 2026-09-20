# controlplane

Le démon TinyVMOS, écrit en Go. C'est le cœur du projet : tout le reste n'en est qu'un client.

## Pourquoi Go

SDK Firecracker officiel maintenu par AWS, `vishvananda/netlink` et `google/nftables`
natifs, cross-compilation ARM64 en une variable d'environnement, binaire statique unique,
compilation en secondes. Le démon est un orchestrateur limité par les entrées/sorties,
pas un hyperviseur.

## Sous-paquets

| Paquet | Rôle |
|---|---|
| `api/` | REST et WebSocket, contrat `openapi.yaml` écrit avant le code |
| `vm/` | Interface `VMM` et implémentation Firecracker, lancée sous `jailer` |
| `network/` | Bridges par zone, interfaces tap, règles nftables |
| `storage/` | Volumes persistants, snapshots, chiffrement LUKS |
| `secrets/` | Stockage chiffré — jamais dans une image, jamais dans un log |
| `audit/` | Journal inaltérable, chaîné par hachage |
| `pairing/` | QR code, certificat par appareil, révocation |
| `config/` | Schéma et validation de `tinyvmos.yaml` |
| `state/` | Boucle de réconciliation entre état voulu et état réel |

## Règles

- Le contrat OpenAPI est la source de vérité : les types et les stubs en sont générés.
- Toute VM est lancée via `jailer`, avec un uid et un gid dédiés. Jamais en direct.
- Politique réseau par défaut : **tout refuser**. Chaque flux autorisé est déclaré.
- Toute modification manuelle des règles nftables est écrasée à la réconciliation suivante.
- Un déploiement qui dépasse la RAM disponible est refusé, plutôt que laissé à l'OOM killer.
