# images

Catalogue des images de micro-VM. Une image égale un service, sans exception.

Chaque image est un Alpine minimal plus un seul service, produite par `build-image.sh`
et configurable par fichier. Les images sont jetables ; les données vivent sur des
volumes persistants séparés et chiffrés.

| Image | Rôle | Zone |
|---|---|---|
| `kernel` | Noyau invité `vmlinux` commun à toutes les VM | — |
| `ssh` | OpenSSH, authentification par clé uniquement | Interne |
| `ftp` | vsftpd, utilisateurs cloisonnés | Interne |
| `web-apache` | Apache2 | Interne ou Publique |
| `dhcp-dns` | dnsmasq : baux, résolution, filtrage de domaines | Interne |
| `router-firewall` | Routage entre zones, nftables | Interne |
| `reverse-proxy` | Terminaison TLS devant le web public | Publique |
| `coffre` | `coffre-vrac` et `coffre-propre`, fichiers Markdown | Données |
| `ia-moteur` | llama.cpp ou Ollama, modèle intégré, hors ligne | IA |
| `ia-client` | Base commune de `ia-collecte` et `ia-tri` | IA |
| `web-evenement` | Serveur web du CTF — à considérer comme compromis | Publique |

**Contrainte de dimensionnement** : la machine cible est nettement plus faible que la
machine de développement. Chaque image est profilée (RAM au repos, RAM en charge, temps
de démarrage) et son budget déclaré. Ordre de grandeur visé : 64 à 128 Mio pour un service
d'infrastructure, 256 Mio pour Apache.
