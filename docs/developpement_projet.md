# TinyVMOS — Plan de développement, du début à la fin

> Document de travail dérivé de `PROJET.md`. Liste exhaustive et ordonnée de tout ce qu'il y a à faire, point par point, de la préparation de l'environnement jusqu'au démontage après l'événement.
>
> Chaque point est une case à cocher. On ne passe à la phase suivante que lorsque ses critères d'acceptation sont validés.

---

## Décisions actées

| Sujet | Décision | Raison courte |
|---|---|---|
| Langage du plan de contrôle | **Go** | SDK Firecracker officiel, `netlink` et `nftables` natifs, cross-compilation ARM64 gratuite, itération rapide |
| Hyperviseur | **Firecracker** | Surface d'attaque minimale + `jailer` (namespace, chroot, cgroup, seccomp) : dernière barrière après compromission d'une VM en CTF |
| Abstraction | Interface Go `VMM` (`Create` / `Start` / `Stop` / `Delete` / `Console` / `Metrics`) | Cloud Hypervisor ajoutable plus tard si le passthrough GPU devient nécessaire, sans toucher au plan de contrôle |
| OS hôte | Buildroot | Build reproductible, empreinte minimale |
| OS invité | Alpine minimal, un service par image | Images jetables, volumes persistants séparés |
| Nom du fichier de config déclarative | `tinyvmos.yaml` | Source de vérité réconciliée par le démon |

---

## Contexte de déploiement réel

- La machine cible est **une unité centrale de bureau** (écran, clavier, souris) qui tournait déjà sous Linux. Le mode « avec écran » est donc le **mode de production réel**, pas un cas secondaire. Le mode headless reste supporté mais n'est pas le scénario principal.
- **La machine cible est nettement moins puissante que la machine de développement.** C'est une contrainte structurante, pas un détail : tout le dimensionnement se fait sur la machine cible, jamais sur le portable de dev. Voir « Contrainte de dimensionnement » ci-dessous.
- L'événement est un **CTF** se déroulant dans un **IUT**, pour les étudiants présents.
- Les participants sont, par définition de l'épreuve, des **utilisateurs hostiles et compétents**. Toute mesure de blocage doit résister à un contournement actif, pas seulement à un usage naïf.

### Chaîne réseau de l'événement

```
Wi-Fi interne de l'IUT
        │  (routeur connecté en client / mode WISP)
        ▼
  Routeur de l'événement ── Wi-Fi participants + pare-feu (blocage des sites d'IA)
        │  LAN filaire
        ▼
  Port physique du serveur TinyVMOS  ← « IP principale » de la machine
        │
        ▼
  Bridges internes (un par zone)
        │
        ▼
  Micro-VM, chacune avec sa propre IP
```

### Contrainte de dimensionnement

La machine de développement (Ryzen 7 260, 15,29 Go de RAM, RTX 5050 8 Go) n'est **pas** représentative de la machine cible, qui est plus faible. Règles qui en découlent :

- **Budget mémoire par micro-VM fixé bas dès la conception.** Ordre de grandeur visé : 64 à 128 Mio pour un service d'infrastructure (SSH, DNS, FTP, reverse proxy), 256 Mio pour Apache. Ces valeurs sont déclarées dans `tinyvmos.yaml` et vérifiées en test, pas laissées au hasard.
- **Le démon doit refuser un déploiement** qui dépasse la RAM disponible de la machine, au lieu de laisser l'OOM killer trancher en pleine épreuve.
- **L'IA est le poste le plus lourd et le plus incertain.** Sans GPU sur la cible et avec peu de RAM, un modèle 7–8B peut être hors de portée. Prévoir une descente en gamme : 3B quantifié, voire 1,5B, voire abandon de l'IA sur la machine de l'événement si le budget ne passe pas. **Ce n'est décidable qu'une fois les caractéristiques de la cible connues.**
- **Mesurer, pas supposer** : chaque image du catalogue est profilée (RAM au repos, RAM en charge, temps de démarrage) et le résultat consigné. Un tableau de budget mémoire est tenu à jour dans `docs/`.
- **Le GPU de la machine de dev sert uniquement au dev** (llama.cpp dans WSL2 pour itérer vite). Il ne doit jamais devenir une hypothèse de production.

---

## Phase 0 — Préparation de l'environnement de développement

### 0.1 Vérification matérielle de la machine de dev — **relevé le 2026-09-21**
- [x] Version de Windows : **Windows 11 Famille, build 26200**.
- [x] GPU : **NVIDIA RTX 5050 Laptop, 8151 Mio de VRAM**, pilote 610.71 (+ iGPU Radeon 780M). Le cahier des charges supposait une 5060 — c'est une 5050, mais les 8 Go de VRAM attendus sont là, un modèle 7–8B quantifié passe.
- [x] CPU : **AMD Ryzen 7 260**, 8 cœurs / 16 threads.
- [x] RAM réelle : **15,29 Go**.
- [x] Espace disque : **320 Go libres sur C:**, 340 Go sur D: — largement au-dessus des 150 Go nécessaires.
- [x] **AMD-V déjà activé dans le BIOS** (`VirtualizationFirmwareEnabled : True`). Rien à toucher.

### 0.2 Mise en place de WSL2
- [ ] Installer WSL2 avec Ubuntu 24.04.
- [ ] Créer `C:\Users\axell\.wslconfig` avec `memory=10GB`, `processors=8`, `nestedVirtualization=true`, `swap=8GB`.
- [ ] Redémarrer WSL (`wsl --shutdown`) et vérifier la prise en compte.
- [ ] Vérifier `ls /dev/kvm` → le fichier doit exister.
- [ ] Si absent : appliquer le plan B (VM Ubuntu sous VMware avec AMD-V exposé, dédiée au dev).

### 0.3 Emplacement du projet
- [ ] Créer le dépôt dans le système de fichiers Linux : `~/tinyvmos`. **Jamais dans `/mnt/c/`** (builds Buildroot plusieurs fois plus lents, symlinks et permissions cassés).
- [ ] Conserver `C:\Users\axell\Claude\OS\PROJET.md` et ce document comme documents de référence, copiés dans `~/tinyvmos/docs/`.
- [ ] Relancer Claude Code depuis WSL2, dans `~/tinyvmos`.

### 0.4 Installation des outils
- [ ] Dépendances Buildroot : `build-essential`, `bc`, `bison`, `flex`, `libssl-dev`, `libelf-dev`, `rsync`, `cpio`, `unzip`, `wget`, `file`, `python3`.
- [ ] Outils d'image : `qemu-system-x86`, `qemu-utils`, `xorriso`, `mtools`, `dosfstools`, `squashfs-tools`.
- [ ] Réseau : `iproute2`, `bridge-utils`, `nftables`, `dnsmasq`, `tcpdump`, `iperf3`.
- [ ] Go (dernière version stable) + `golangci-lint`.
- [ ] Firecracker : télécharger le binaire statique et le `jailer` correspondants, vérifier les sommes de contrôle.
- [ ] Git, et initialiser le dépôt.

### 0.5 Script de vérification d'environnement
- [ ] Écrire `build/check-env.sh` qui contrôle : présence de `/dev/kvm` et droits, architecture, RAM, espace disque, présence et versions de tous les outils ci-dessus, `nestedVirtualization`, accès Internet pour le téléchargement des sources.
- [ ] Le script sort en code non nul et affiche un message clair par élément manquant.

### 0.6 Squelette du dépôt
- [ ] Créer l'arborescence de la section 14 de `PROJET.md` : `docs/`, `os/`, `images/`, `controlplane/`, `cli/`, `web/`, `mobile/`, `build/`, `tests/`, `.github/workflows/`.
- [ ] Un `README.md` par dossier expliquant son rôle.
- [ ] `.gitignore` (artefacts de build, `output/`, binaires, clés).
- [ ] `LICENSE` et `README.md` racine.

**Critère d'acceptation Phase 0** : `build/check-env.sh` passe intégralement, le dépôt est initialisé dans `~/tinyvmos`, et `ls /dev/kvm` répond.

---

## Phase 1 — Validation de la chaîne (micro-VM Apache)

### 1.1 Noyau invité
- [ ] Construire ou récupérer un noyau Linux invité minimal au format `vmlinux` (démarrage direct, pas d'UEFI — contrainte Firecracker sur x86_64).
- [ ] Configuration noyau réduite : virtio-net, virtio-blk, virtio-vsock, ext4, pas de modules inutiles.
- [ ] Documenter la configuration dans `images/kernel/README.md`.

### 1.2 Rootfs Alpine + Apache
- [ ] Script `images/web-apache/build.sh` : téléchargement du minirootfs Alpine, `apk add apache2`, configuration, création d'une image ext4.
- [ ] Service Apache démarré par OpenRC au boot, page de test servie.
- [ ] Image reproductible : même entrée, même somme de contrôle en sortie.

### 1.3 Réseau virtuel minimal
- [ ] Script créant un bridge hôte `br-test` et une interface `tap` par VM.
- [ ] Attribution d'IP statique à la VM, route et NAT vers l'hôte.
- [ ] Nettoyage automatique des interfaces en fin de script.

### 1.4 Lancement Firecracker
- [ ] Script `build/run-firecracker.sh` : configuration JSON (noyau, rootfs, tap, vCPU, RAM), démarrage via l'API sur socket Unix.
- [ ] Récupérer la console série dans un fichier de log.
- [ ] Mesurer le temps de démarrage.

### 1.5 Premier contact avec le `jailer`
- [ ] Relancer la même VM via le `jailer` (chroot, namespaces, cgroup, uid/gid dédiés).
- [ ] Documenter les différences de configuration entre lancement direct et `jailer`.

**Critère d'acceptation Phase 1** : `curl http://<ip-vm>` depuis l'hôte renvoie la page Apache, la VM démarre sous `jailer`, et le script de nettoyage ne laisse aucune interface résiduelle.

---

## Phase 2 — Démon, API et réseau virtuel

### 2.1 Fondations du démon
- [ ] Initialiser le module Go `controlplane/`.
- [ ] Arborescence : `api/`, `vm/`, `network/`, `storage/`, `secrets/`, `audit/`, `pairing/`, `config/`, `state/`.
- [ ] Journalisation structurée (`log/slog`), niveaux configurables.
- [ ] Chargement de configuration, gestion des signaux, arrêt propre.
- [ ] Unité systemd (ou script OpenRC selon la base de l'hôte) pour le démon.

### 2.2 Interface VMM et implémentation Firecracker
- [ ] Définir l'interface Go `VMM` : `Create`, `Start`, `Stop`, `Delete`, `Restart`, `Console`, `Metrics`, `Snapshot`.
- [ ] Implémenter `firecracker.go` en s'appuyant sur `firecracker-go-sdk`.
- [ ] Lancement systématique via `jailer`, avec un uid/gid par VM.
- [ ] Machine d'état explicite de la VM : `Created`, `Starting`, `Running`, `Stopping`, `Stopped`, `Failed`.
- [ ] Redémarrage automatique sur crash, avec temporisation croissante et plafond.
- [ ] Tests unitaires avec un VMM simulé.

### 2.3 Plan d'adressage et zones
- [ ] Figer le plan d'adressage interne :

| Zone | Bridge | Sous-réseau | Sortie Internet |
|---|---|---|---|
| Gestion | `br-mgmt` | `10.42.0.0/24` | Non |
| Publique | `br-pub` | `10.42.10.0/24` | Non (sauf exception déclarée) |
| Interne / services | `br-svc` | `10.42.20.0/24` | Selon besoin déclaré |
| Données | `br-data` | `10.42.30.0/24` | **Non** |
| IA | `br-ia` | `10.42.40.0/24` | **Non, jamais** |

- [ ] Le port physique du serveur reçoit son IP par DHCP depuis le routeur de l'événement : c'est l'« IP principale ».
- [ ] Création programmatique des bridges et des tap via `vishvananda/netlink`.
- [ ] Une IP par micro-VM, attribuée de façon déterministe à partir de son nom.

### 2.4 Pare-feu et flux
- [ ] Générer les règles nftables via `google/nftables` (pas d'appel au binaire `nft`).
- [ ] Politique par défaut : **tout refuser**, dans les deux sens, entre toutes les zones.
- [ ] Chaque flux autorisé est déclaré dans `tinyvmos.yaml` (zone source, zone destination, protocole, port) et traduit en règle.
- [ ] Vérifier qu'aucune route sortante n'existe depuis les zones Données et IA, au niveau du routage **et** du pare-feu.
- [ ] Règle de sécurité : toute modification manuelle des règles est écrasée à la réconciliation suivante.

### 2.5 VM DHCP et DNS interne
- [ ] Image `images/dhcp-dns/` basée sur dnsmasq (ou Unbound + Kea si la séparation est préférée).
- [ ] Sert les baux et la résolution de noms aux autres VM (découverte par nom : `ssh.tinyvmos.lan`, etc.).
- [ ] Préparer les listes de blocage de domaines (voir Phase 9) — le même service servira au blocage des sites d'IA pour les participants.
- [ ] Journalisation des requêtes, avec rotation.

### 2.6 Configuration déclarative et réconciliation
- [ ] Schéma `tinyvmos.yaml` : VM, zones, flux, volumes, droits, images.
- [ ] Validation stricte du schéma avec messages d'erreur exploitables.
- [ ] Boucle de réconciliation : comparer l'état voulu et l'état réel, appliquer l'écart, journaliser chaque action.
- [ ] Mode `--dry-run` qui affiche le plan sans l'appliquer.

### 2.7 API
- [ ] Écrire `controlplane/api/openapi.yaml` **avant** le code (contrat d'abord).
- [ ] Générer les types et les stubs avec `oapi-codegen`.
- [ ] Routes `/api/v1` : VM, images, zones, flux, volumes, snapshots, secrets, appareils, audit, santé.
- [ ] WebSocket : flux de logs, changements d'état, notifications.
- [ ] Console série de chaque VM exposée par WebSocket.
- [ ] TLS obligatoire, certificat auto-signé généré au premier démarrage.
- [ ] Authentification par appareil (certificat client), rôles `lecture`, `opérateur`, `admin`.
- [ ] Limitation de débit et taille maximale des requêtes.

### 2.8 Journal d'audit
- [ ] Toute action et tout accès journalisés : horodatage, appareil, rôle, action, résultat.
- [ ] Chaînage par hachage (chaque entrée contient le hachage de la précédente) pour détecter toute altération.
- [ ] Stockage sur volume séparé, en écriture seule pour le démon.

### 2.9 CLI `tinyvmosctl`
- [ ] Client Go basé sur `cobra`, consommant **la même API** que l'UI.
- [ ] Commandes : `vm list/create/start/stop/delete/console/logs`, `zone`, `flow`, `volume`, `snapshot`, `image`, `apply -f tinyvmos.yaml`, `audit`, `device`.
- [ ] Sortie lisible par défaut, `--json` pour le scriptage.

### 2.10 Tests d'isolation
- [ ] Test automatisé : depuis une VM de la zone Publique, tenter d'atteindre chaque autre zone → tout doit échouer sauf les flux déclarés.
- [ ] Test automatisé : depuis les zones Données et IA, tenter de joindre Internet (IP publique directe et par nom) → doit échouer.
- [ ] Capture `tcpdump` sur le port physique pendant les tests, pour prouver qu'aucun paquet ne sort.

**Critère d'acceptation Phase 2** : trois VM (dhcp-dns, web, ssh) déployées uniquement par API, isolation entre zones prouvée par la suite de tests, `tinyvmosctl apply -f tinyvmos.yaml` reproduit l'état complet depuis zéro.

---

## Phase 3 — Interface web PC

### 3.1 Base technique
- [ ] Choisir la pile front (React ou Svelte) et la figer.
- [ ] Client API généré depuis `openapi.yaml` — jamais écrit à la main.
- [ ] Build intégré au binaire du démon (fichiers embarqués) pour n'avoir qu'un seul artefact à déployer.

### 3.2 Écrans
- [ ] Tableau de bord : état global, VM actives, CPU/RAM, alertes.
- [ ] Liste des VM : état, zone, IP, actions (démarrer, arrêter, redémarrer, supprimer).
- [ ] Détail d'une VM : ressources, logs en direct, console série interactive, volumes attachés.
- [ ] Topologie réseau : zones, VM, flux autorisés, rendu graphique.
- [ ] Catalogue : images disponibles, déploiement en quelques clics, configuration par formulaire.
- [ ] Volumes et snapshots : créer, restaurer, supprimer, état du chiffrement.
- [ ] Journal d'audit : consultation, filtres, export.
- [ ] Gestion des appareils appairés : liste, rôle, révocation.

### 3.3 Qualité
- [ ] Responsive : tableau de bord complet sur PC, onglets et grands boutons tactiles sur mobile et tablette.
- [ ] PWA : manifeste, service worker, installable.
- [ ] Reconnexion automatique du WebSocket avec temporisation croissante.
- [ ] Accessibilité au clavier sur toutes les actions.
- [ ] Confirmation explicite sur toute action destructive.

**Critère d'acceptation Phase 3** : l'intégralité du cycle de vie des VM est pilotable depuis le navigateur, sans passer par la CLI.

---

## Phase 4 — Image bootable et OVA

### 4.1 OS hôte Buildroot
- [ ] Configuration Buildroot : noyau avec KVM, virtio, bridge, VLAN, nftables, WireGuard, LUKS (dm-crypt), ext4.
- [ ] Overlay rootfs : démon, `tinyvmosctl`, Firecracker, `jailer`, dnsmasq, scripts de démarrage.
- [ ] Aucun paquet superflu, aucun bureau, aucun compilateur dans l'image finale.
- [ ] Build reproductible : versions figées, sommes de contrôle vérifiées.

### 4.2 Détection matérielle
- [ ] Module `os/hardware-detect/` : architecture, présence et droits sur `/dev/kvm`, RAM, cœurs CPU, disques, cartes réseau, GPU, écran connecté.
- [ ] Profil matériel écrit en JSON, consommé par le démon.
- [ ] **Absence de KVM** : message clair à l'écran et dans les logs, passage en mode dégradé QEMU explicitement signalé comme « tests uniquement ».
- [ ] **Ressources limitées** : plafonner le nombre de VM, avertir l'utilisateur.

### 4.3 Mode écran et mode headless
- [ ] Écran détecté → démarrage du kiosque Wayland (Cage + navigateur en plein écran sur la web UI locale).
- [ ] Aucun écran → mode headless, interface accessible uniquement à distance.
- [ ] Le kiosque ne doit pas permettre de sortir vers un shell ni vers une autre URL.
- [ ] Gestion clavier et souris dans le kiosque (c'est le mode de production réel : le serveur a écran, clavier et souris).

### 4.4 Livrables image
- [ ] `build/make-iso.sh` : ISO hybride, boot UEFI (BIOS legacy en option), mode live.
- [ ] `build/make-img.sh` : image brute ARM64 pour Raspberry Pi.
- [ ] `build/make-ova.sh` : `qemu-img convert -O vmdk`, descripteur `.ovf`, archive `.ova`.
- [ ] Sommes de contrôle SHA-256 pour chaque livrable.

### 4.5 Tests de démarrage
- [ ] Démarrage sous QEMU dans WSL2 (test quotidien).
- [ ] Démarrage de l'OVA sous VMware Workstation, avec *Virtualize Intel VT-x/EPT ou AMD-V* activé.
- [ ] Vérifier le message d'absence de KVM quand la virtualisation imbriquée est désactivée.
- [ ] Démarrage sur la machine physique cible depuis une clé USB écrite avec Rufus.

**Critère d'acceptation Phase 4** : l'OS démarre sous QEMU et sous VMware, détecte correctement la présence ou l'absence de KVM, affiche le kiosque quand un écran est branché, et lance ses VM quand KVM est actif.

---

## Phase 5 — Installateur sur disque

### 5.1 Fonctionnalités
- [ ] Détection et listage des disques, avec taille, modèle et présence de données existantes.
- [ ] Partitionnement GPT : ESP (FAT32) + racine + volume de données.
- [ ] **Chiffrement LUKS du volume de données** (optionnel mais proposé par défaut).
- [ ] Copie du système, installation du bootloader UEFI.
- [ ] Configuration initiale : nom de la machine, réseau, mot de passe administrateur, fuseau horaire.
- [ ] Génération du QR code d'appairage à la fin.

### 5.2 Sécurité de l'installateur
- [ ] Double confirmation avant toute écriture disque, avec rappel explicite du disque ciblé.
- [ ] **Jamais de test des scripts d'écriture disque sur les disques réels de la machine de dev** — uniquement sur disques virtuels.
- [ ] Interdiction d'installer sur le disque depuis lequel on a démarré.
- [ ] Journal complet de l'installation, conservé sur le système installé.

### 5.3 Tests
- [ ] Installation sur disque virtuel QEMU, puis redémarrage autonome sans la clé USB.
- [ ] Installation avec LUKS activé, puis déverrouillage au démarrage.
- [ ] Test d'interruption en cours d'installation : l'état doit rester détectable et récupérable.

**Critère d'acceptation Phase 5** : installation réussie sur un disque virtuel, redémarrage autonome, volume chiffré déverrouillable.

---

## Phase 6 — Catalogue de l'événement et IA locale

### 6.1 Chaîne de construction d'images
- [ ] Script générique `images/build-image.sh` : Alpine minirootfs + un service + configuration, sortie en image ext4.
- [ ] Une image = un service, aucune exception.
- [ ] Services non-root chaque fois que possible.
- [ ] Sommes de contrôle et reproductibilité vérifiées pour chaque image.

### 6.2 Images d'infrastructure
- [ ] `ssh` — OpenSSH, authentification par clé uniquement, mot de passe désactivé.
- [ ] `ftp` — vsftpd, utilisateurs cloisonnés dans leur dossier.
- [ ] `web-apache` — Apache2.
- [ ] `dhcp-dns` — dnsmasq (affiné en Phase 9 pour le blocage des sites d'IA).
- [ ] `router-firewall` — routage entre zones, nftables.
- [ ] `reverse-proxy` — devant le serveur web public, terminaison TLS.

### 6.3 Images de données
- [ ] `coffre-vrac` et `coffre-propre` : dossiers de fichiers Markdown exposés par le réseau.
- [ ] **Trancher le protocole de partage** : WebDAV, Samba, Syncthing ou Git. Recommandation : WebDAV (simple, authentifié, supporté nativement par Obsidian via greffon, et strictement orienté fichiers).
- [ ] Volumes persistants **chiffrés LUKS**, séparés des images.
- [ ] Snapshots automatiques programmables.
- [ ] Aucun partage de dossier hôte vers la VM (Firecracker ne propose pas virtio-fs, et c'est un avantage : pas de chemin d'évasion supplémentaire).

### 6.4 Images d'IA
- [ ] `ia-moteur` — llama.cpp ou Ollama, modèle quantifié 3 à 8 milliards de paramètres (Qwen, Llama, Mistral ou Gemma).
- [ ] **Modèle téléchargé en amont et intégré à l'image** — aucun téléchargement à l'exécution, fonctionnement hors ligne total.
- [ ] Un seul modèle chargé à la fois, pour économiser la RAM.
- [ ] Exécution sur CPU (Firecracker n'offre pas de passthrough GPU — décision assumée).
- [ ] `ia-collecte` et `ia-tri` : **même image de base, deux configurations**.
- [ ] `ia-collecte` : écrit **uniquement** dans `coffre-vrac`.
- [ ] `ia-tri` : lit `coffre-vrac`, écrit **uniquement** dans `coffre-propre`. Aucun outil, aucun accès réseau sortant.
- [ ] La sortie du modèle est traitée comme **des données, jamais comme des ordres** (protection contre l'injection de prompt via les données collectées).

### 6.5 Image de l'événement
- [ ] `web-evenement` — serveur web du CTF, derrière le reverse proxy, en zone Publique.
- [ ] **Aucune route** depuis la zone Publique vers les zones Données et IA.
- [ ] Cette VM est celle qui sera attaquée : la traiter comme compromise par défaut dans la conception.

### 6.6 Flux « vrac → propre »
- [ ] Implémenter le flux : sources → `ia-collecte` → `coffre-vrac` → `ia-tri` → `coffre-propre`.
- [ ] Le tri **copie avant de supprimer** : le brut est conservé tant que le résultat n'est pas validé.
- [ ] Chaque étape journalisée dans le journal d'audit.

### 6.7 Tests de la Phase 6
- [ ] Test de bout en bout du flux vrac → propre.
- [ ] Test prouvant qu'**aucun paquet ne sort** des zones Données et IA (capture sur le port physique pendant une charge).
- [ ] Test prouvant que le brut est conservé après un tri.
- [ ] Test prouvant que `web-evenement` ne peut joindre ni les coffres ni les VM d'IA.
- [ ] Test d'injection de prompt : données contenant des instructions malveillantes → `ia-tri` ne doit exécuter aucune action, seulement classer.

**Critère d'acceptation Phase 6** : tous les tests ci-dessus passent en automatique dans la CI.

---

## Phase 7 — Appairage et application Android

### 7.1 WireGuard auto-hébergé
- [ ] Serveur WireGuard sur l'hôte, en zone Gestion.
- [ ] Génération des clés, une paire par appareil.
- [ ] **Aucun relais tiers, pas de Tailscale**, aucun port ouvert sur la box.
- [ ] Révocation d'un appareil depuis l'UI.

### 7.2 Appairage
- [ ] Au premier lancement, affichage d'un **QR code** sur l'écran local.
- [ ] Échange de clés, émission d'un **certificat client par appareil**.
- [ ] Enregistrement de l'appareil comme appareil de confiance, avec un rôle.
- [ ] Expiration du QR code après un délai court.
- [ ] Révocation testée : l'appareil révoqué perd immédiatement l'accès.

### 7.3 Application Flutter
- [ ] Projet Flutter dans `mobile/`, client API généré depuis `openapi.yaml`.
- [ ] Scan du QR code, stockage du certificat dans le magasin sécurisé de l'appareil.
- [ ] Écrans : état des VM, actions, logs en direct, notifications.
- [ ] Notifications fiables (VM tombée, service arrêté, alerte de sécurité).
- [ ] **Rappel** : l'OS ne tourne pas sur le téléphone. Le téléphone est une télécommande.

### 7.4 Livrable APK
- [ ] `flutter build apk --release`, **signé**.
- [ ] **Clé de signature sauvegardée hors dépôt, en lieu sûr** — sa perte empêche toute mise à jour ultérieure.
- [ ] AAB pour le Play Store, plus tard uniquement.

**Critère d'acceptation Phase 7** : un téléphone appairé pilote la machine à distance via WireGuard, et sa révocation coupe l'accès immédiatement.

---

## Phase 8 — Durcissement, intégration continue et publication

### 8.1 Intégration continue
- [ ] Workflow GitHub Actions produisant à chaque version : `tinyvmos-x86_64.iso`, `tinyvmos-arm64.img`, `tinyvmos.ova`, `tinyvmos-app.apk`, plus les sommes de contrôle.
- [ ] Exécution de toute la suite de tests, y compris les tests d'isolation réseau.
- [ ] Build **depuis zéro** : un dépôt fraîchement cloné doit produire tous les livrables sans intervention manuelle.
- [ ] Cache des sources Buildroot pour garder des durées raisonnables.

### 8.2 Audit de sécurité
- [ ] Revue des règles nftables générées, zone par zone.
- [ ] Revue de la configuration `jailer` et des filtres seccomp.
- [ ] Vérification qu'aucun secret n'apparaît dans une image, un log ou le dépôt.
- [ ] Analyse des dépendances Go (`govulncheck`) et des paquets Alpine.
- [ ] Test de l'immuabilité du journal d'audit (tentative d'altération détectée).
- [ ] Vérification que tous les services tournent non-root quand c'est possible.

### 8.3 Sauvegardes
- [ ] Snapshots programmés des volumes persistants.
- [ ] Procédure d'export et de restauration testée réellement, pas seulement documentée.
- [ ] Sauvegarde hors machine des éléments critiques : clé de signature APK, clés WireGuard, en-têtes LUKS.

### 8.4 Documentation
- [ ] Guide d'installation.
- [ ] Guide d'exploitation (créer une VM, une zone, un flux).
- [ ] Guide de sécurité (modèle de menace, ce qui est garanti et ce qui ne l'est pas).
- [ ] Guide de dépannage.
- [ ] Référence de l'API publiée depuis `openapi.yaml`.

**Critère d'acceptation Phase 8** : une version complète est générée depuis zéro par la CI, tous les tests au vert.

---

## Phase 9 — Préparation et déroulé de l'événement CTF

Cette phase n'existe pas dans `PROJET.md` : elle découle du contexte réel de déploiement.

### 9.1 Points à verrouiller avec l'IUT — **à faire en premier, c'est le risque principal**
- [ ] **Déterminer le type de sécurité du Wi-Fi de l'IUT.** S'il est en **WPA2/WPA3-Enterprise (802.1X, type eduroam)**, la majorité des routeurs grand public **ne savent pas s'y connecter en mode client**. C'est un blocage total de la chaîne réseau prévue.
- [ ] Solutions de repli à valider, par ordre de préférence :
  1. Obtenir une **prise Ethernet murale** dédiée auprès du service informatique de l'IUT (de loin le plus fiable).
  2. Utiliser un routeur sous **OpenWrt**, qui sait se connecter en client à un réseau Enterprise.
  3. Partage de connexion depuis un téléphone (secours dégradé, à ne pas privilégier).
- [ ] Vérifier la présence d'un **portail captif** sur le Wi-Fi de l'IUT, qui casserait toute connexion automatique.
- [ ] Prévenir le service informatique de l'IUT de la tenue de l'événement et de la mise en place d'un routeur — sinon risque de blocage pour point d'accès non autorisé.
- [ ] Confirmer la **machine cible** : CPU, RAM (32 Go recommandés, 16 Go minimum), disques, écran présent.

### 9.2 Configuration du routeur
- [ ] Connexion au Wi-Fi de l'IUT en mode client, création du LAN de l'événement.
- [ ] Wi-Fi participants : SSID, WPA2/WPA3, mot de passe communiqué sur place.
- [ ] Bail DHCP fixe pour le port physique du serveur TinyVMOS (l'IP principale ne doit jamais changer pendant l'épreuve).
- [ ] **Serveur DNS annoncé par DHCP = la VM DNS de TinyVMOS**, et non le DNS de l'IUT.
- [ ] Noter la conséquence : on est en **double NAT** (IUT → routeur → serveur). Aucun flux entrant depuis le réseau de l'IUT n'est possible, et ce n'est pas gênant : tous les participants sont sur le LAN du routeur.

### 9.3 Blocage des sites d'IA — traiter les contournements
Le blocage DNS seul ne tient pas face à des participants de CTF. Le pare-feu du routeur doit compléter :

- [ ] **Filtrage DNS** sur la VM DNS : listes de domaines des services d'IA (bloqués par réponse NXDOMAIN ou redirection vers une page d'information).
- [ ] **Redirection forcée du port 53** (DNAT) de tout le LAN vers la VM DNS : impossible d'utiliser un autre résolveur.
- [ ] **Blocage de DoT** (port 853 en TCP et UDP).
- [ ] **Blocage de DoH** : liste des IP des résolveurs connus (Cloudflare, Google, Quad9, NextDNS, AdGuard…) sur le port 443.
- [ ] **Blocage de QUIC** (UDP 443) si une réponse en HTTP/3 permet de contourner le filtrage.
- [ ] **Blocage par IP et par plage d'adresses** des fournisseurs d'IA, en complément des noms de domaine.
- [ ] **Blocage des protocoles de VPN** courants (WireGuard sur 51820, OpenVPN sur 1194, IPsec) — sauf le WireGuard d'administration, qui doit rester joignable depuis la zone Gestion uniquement.
- [ ] Accepter la limite honnêtement : un participant avec un forfait 4G contourne tout. Le blocage réseau est une **mesure de dissuasion et d'équité**, pas une garantie absolue. Le prévoir dans le règlement de l'épreuve.
- [ ] Tester chaque contournement soi-même avant l'événement, et documenter le résultat.

### 9.4 Exposition des services de l'événement
- [ ] `web-evenement` accessible aux participants via le reverse proxy, publié sur l'IP principale du serveur par redirection de port.
- [ ] VM DNS joignable depuis le LAN du routeur sur le port 53 (flux explicitement déclaré, unique exception entrante vers la zone Interne).
- [ ] **Aucun autre port** exposé vers le LAN des participants.
- [ ] Vérifier par scan complet depuis un poste participant : seuls les ports attendus répondent.

### 9.5 Répétition générale
- [ ] Monter l'intégralité de la chaîne en conditions réelles (même routeur, même serveur, même configuration) au moins **une semaine avant** l'événement.
- [ ] Test de charge : nombre de participants attendus en simultané sur `web-evenement`.
- [ ] Test de panne : couper l'alimentation du serveur, vérifier que tout redémarre seul dans l'état voulu.
- [ ] Test de panne : couper le Wi-Fi de l'IUT, vérifier que le LAN interne continue de fonctionner.
- [ ] Mesurer le temps de redémarrage complet de la machine, du boot aux VM opérationnelles.

### 9.6 Jour J
- [ ] Sauvegarde complète des volumes **avant** le début de l'épreuve.
- [ ] Vérification du journal d'audit et de l'espace disque.
- [ ] Tablette ou téléphone appairé à portée de main pour la supervision.
- [ ] Procédure écrite pour les incidents courants : VM tombée, participant bloqué, saturation réseau.
- [ ] Surveillance en direct des logs et des alertes pendant toute l'épreuve.

### 9.7 Après l'événement
- [ ] Export du journal d'audit complet.
- [ ] Sauvegarde puis **effacement sécurisé** des données de participants, selon la durée de conservation décidée.
- [ ] Retour d'expérience écrit : ce qui a tenu, ce qui a été contourné, ce qui est à corriger.

---

## Points restant à trancher

| # | Point | Quand le trancher | Impact |
|---|---|---|---|
| 1 | ~~Version de Windows sur la machine de dev~~ | ~~Phase 0~~ | **Tranché** : Windows 11 Famille build 26200 |
| 2 | ~~GPU exact et RAM réelle de la machine de dev~~ | ~~Phase 0~~ | **Tranché** : RTX 5050 Laptop 8 Go, 15,29 Go de RAM |
| 3 | **Machine cible : CPU, RAM, disque, GPU éventuel** — elle est nettement plus faible que la machine de dev | **Immédiatement** | **Fort** : budget mémoire par VM, nombre de VM, taille du modèle d'IA, voire abandon de l'IA |
| 4 | Type de sécurité du Wi-Fi de l'IUT | **Immédiatement** | Bloquant pour toute la chaîne réseau |
| 5 | Protocole des coffres (WebDAV, Samba, Syncthing, Git) | Phase 6 | Moyen — recommandation : WebDAV |
| 6 | Pile front (React ou Svelte) | Phase 3 | Faible |
| 7 | Accès distant WireGuard dès la v1, ou réseau local seul | Phase 7 | Moyen |
| 8 | Nature et sensibilité des données de l'événement, durée de conservation, obligations RGPD | **Avant la Phase 6** | Fort — conditionne le chiffrement et l'audit |
| 9 | Nombre de participants attendus | Phase 9 | Dimensionnement |

---

## Risques et parades

| Risque | Parade |
|---|---|
| **Wi-Fi de l'IUT en WPA2-Enterprise** — le routeur ne peut pas s'y connecter | Prise Ethernet dédiée demandée à l'IUT, ou routeur sous OpenWrt. **À vérifier en premier.** |
| Portail captif sur le réseau de l'IUT | Identification en amont, ou passage en filaire |
| Blocage de l'IA contourné (DoH, VPN, 4G) | Défense en profondeur (§ 9.3) + règlement écrit de l'épreuve |
| KVM imbriqué capricieux sous WSL2/VMware sur AMD | Plan B : VM Ubuntu sous VMware avec AMD-V exposé ; QEMU en repli |
| 15,29 Go de RAM sur la machine de dev | `.wslconfig` ajusté à 10 Go, modèle 3B pour les tests, IA sur GPU dans WSL2 |
| **Machine cible plus faible que la machine de dev** — ce qui tourne en dev ne tournera pas forcément sur la cible | Budget mémoire par VM fixé bas et déclaré dans `tinyvmos.yaml`, refus de déploiement si la RAM manque, profilage systématique de chaque image, tests d'intégration exécutés sous la contrainte mémoire de la cible |
| **IA hors budget sur la machine cible** (pas de GPU, peu de RAM) | Descente en gamme planifiée : 3B, puis 1,5B, puis abandon de l'IA sur la machine de l'événement. Décision prise une fois la cible relevée, avant la Phase 6 |
| Pas de GPU dans une micro-VM Firecracker | IA sur CPU assumée ; interface `VMM` permettant d'ajouter Cloud Hypervisor + VFIO plus tard |
| Fuite de données par l'IA | Zones Données et IA sans route sortante, modèles intégrés hors ligne, tests réseau automatisés |
| Injection de prompt dans les données collectées | `ia-tri` sans outils ni réseau, écriture limitée à `coffre-propre`, sortie traitée comme donnée |
| Vol de la machine | Volumes LUKS |
| Compromission de l'API | TLS, authentification par appareil, rôles, audit, WireGuard uniquement |
| Perte de la clé de signature APK | Sauvegarde sécurisée hors dépôt |
| `web-evenement` compromis (c'est le but du CTF) | Zone isolée, reverse proxy, aucune route vers les coffres, `jailer` + seccomp comme dernière barrière |
| Panne de courant en pleine épreuve | Redémarrage automatique dans l'état voulu, testé ; onduleur si possible |

---

## Prochaine action

Lancer la **Phase 0**, en commençant par `build/check-env.sh`.

En parallèle et sans attendre : vérifier le **point 4** (type de sécurité du Wi-Fi de l'IUT). C'est le seul risque capable de remettre en cause l'architecture réseau entière, et il ne dépend pas du code.
