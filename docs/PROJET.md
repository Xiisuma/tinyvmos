# TinyVMOS — Distribution Linux d'hyperviseur de micro-VM

> Nom du projet : **TinyVMOS**. Ce document est le cahier des charges complet, à donner à Claude Code comme contexte de départ. Il regroupe toutes les décisions prises lors de la phase de conception.

---

## 0. Instructions pour Claude Code

- Lis ce document en entier avant d'écrire du code.
- Travaille **phase par phase** (section 13). Ne passe à la phase suivante que lorsque les critères d'acceptation de la phase en cours sont validés.
- Avant chaque phase : propose un plan court, puis implémente.
- **Réutilise l'existant** (noyau Linux, KVM, vrais services). N'écris pas de noyau, de pile TCP/IP ou de serveurs SSH/FTP/HTTP maison.
- Tout doit être **reproductible** : un build propre depuis zéro doit produire tous les livrables via un seul script/CI.
- Documente chaque composant (README par dossier) et versionne l'API (OpenAPI).
- Demande confirmation avant toute action destructive (formatage disque, suppression de volumes) et ne teste jamais les scripts d'écriture disque sur les disques réels de la machine de dev.
- Si un point du document est ambigu ou contradictoire avec la réalité technique, signale-le au lieu de deviner.

---

## 1. Vision

Créer **une distribution Linux sur mesure** qui transforme un ordinateur physique en **hyperviseur de micro-VM**.

- **1 VM = 1 utilité** : un service (SSH, FTP, Apache, DHCP, DNS…), une application, un coffre de données, une IA…
- **Décentralisé sur les VM, recentralisé dans une interface graphique unique.**
- Les vrais services et protocoles existants sont réutilisés (TCP, UDP, ICMP, HTTP/S, SSH, FTP, DHCP, DNS…), déjà testés et fiables.
- La valeur ajoutée du projet : **le plan de contrôle** (démon + API), **les zones réseau sécurisées**, **le catalogue de services**, **les interfaces PC et mobile**, et **la distribution bootable**.

### Philosophie (inspirée de Linux, pas de Windows)

- Un outil = une fonction.
- Configuration en **fichiers texte** (YAML/TOML), versionnables.
- Tout pilotable en **ligne de commande** ET via API.
- Composants **modulaires** et remplaçables.
- Windows n'est **que la machine de développement**, il n'apparaît pas dans le produit.

---

## 2. Décision clé : pas de « from scratch » total

| Élément | Décision |
|---|---|
| Noyau | **Linux** (avec KVM). Pas de noyau maison. |
| Hyperviseur | **KVM** + **Firecracker** ou **Cloud Hypervisor** (micro-VM). |
| Repli sans KVM | **QEMU** en émulation logicielle (très lent, pour tests uniquement). Firecracker et Cloud Hypervisor **ne fonctionnent pas sans KVM**. |
| OS invité | Linux minimal (**Alpine** ou Buildroot), un seul service par image. |
| Services | Implémentations existantes : OpenSSH, vsftpd/proftpd, Apache2, dnsmasq/Unbound, etc. |
| Réseau virtuel | Bridges Linux / VXLAN, éventuellement **Open vSwitch**, règles **nftables**. |
| Plan de contrôle | **Démon en Go ou Rust** + API REST/WebSocket. C'est le cœur à écrire. |
| OS hôte | **Buildroot** (reproductible) ou base Alpine, minimal, sans bureau ni paquets inutiles. |

*Piste optionnelle d'apprentissage (hors chemin critique)* : un noyau jouet en Rust (virtio, smoltcp) en projet parallèle. Le produit reste sur Linux.

---

## 3. Architecture en couches

```
┌──────────────────────────────────────────────────────────┐
│  Clients : UI locale (kiosque) │ Web PWA │ App Android    │
└───────────────▲──────────────────────────────────────────┘
                │  API REST + WebSocket (TLS, OpenAPI)
┌───────────────┴──────────────────────────────────────────┐
│  Plan de contrôle (démon Go/Rust)                         │
│  VM lifecycle · réseau/zones · stockage · secrets · audit │
└───────────────▲──────────────────────────────────────────┘
                │
┌───────────────┴──────────────────────────────────────────┐
│  OS hôte (Linux minimal) : noyau+KVM · Firecracker/CH ·   │
│  nftables/OVS · LUKS · WireGuard · détection matérielle   │
└───────────────▲──────────────────────────────────────────┘
                │
┌───────────────┴──────────────────────────────────────────┐
│  Micro-VM (1 utilité chacune), images du catalogue        │
└──────────────────────────────────────────────────────────┘
```

Principe : **l'API est le cœur**. L'interface PC, la PWA et l'app Android ne sont que des clients de la même API. La logique n'est écrite qu'une fois.

---

## 4. Détection matérielle et adaptation

Au démarrage, l'OS établit un **profil matériel** :

- Architecture : **x86_64** ou **ARM64**.
- Présence de **KVM** (`/dev/kvm`), sinon message clair + mode dégradé QEMU.
- RAM, CPU (cœurs), disques, cartes réseau, GPU éventuel.
- **Écran connecté ou non**.

Adaptation :

- **Sans écran** → mode *headless* : interface accessible uniquement à distance.
- **Avec écran** → interface locale plein écran (kiosque Wayland type **Cage** + navigateur).
- **Ressources limitées** → limite du nombre de VM, images plus légères, avertissements.
- Builds requis : **x86_64** (prioritaire) et **ARM64** (Raspberry Pi, plus tard).

---

## 5. Réseau : zones et flux

Le réseau est par **zones**, avec des **règles de flux explicites** (défaut : tout est bloqué).

| Zone | Contenu | Accès sortant Internet |
|---|---|---|
| **Publique** | Serveur web événement, reverse proxy | Non (sauf nécessité explicite) |
| **Interne / services** | DHCP, DNS, SSH, FTP, etc. | Selon besoin |
| **Données** | Coffres (vrac, propre) | **Non** |
| **IA** | IA-moteur, IA-collecte, IA-tri | **Non, jamais** |
| **Gestion** | Plan de contrôle, WireGuard | Non |

Briques réseau :

- **VM DHCP/DNS** interne qui sert les autres VM (découverte par nom).
- **VM routeur/firewall** virtuelle pour les flux entre zones et vers l'extérieur.
- **Reverse proxy** devant le serveur web public.
- Le serveur web public n'a **aucun accès direct** aux coffres ni aux VM IA.
- Chaque flux autorisé entre zones est déclaré dans la config du plan de contrôle et appliqué par nftables. Tout le reste est refusé.

---

## 6. Catalogue de services (images de VM)

Chaque image = Alpine + **un seul service**, générée par script de build, configurable par fichier.

**Services d'infrastructure**
- `ssh` (OpenSSH)
- `ftp` (vsftpd ou proftpd)
- `web-apache` (Apache2)
- `dhcp` / `dns` (dnsmasq ou Unbound + Kea)
- `router-firewall`
- `reverse-proxy`

**Services de données**
- `coffre-vrac` et `coffre-propre` : **dossiers de fichiers Markdown** (un « coffre » Obsidian n'est qu'un dossier). Hébergés via **WebDAV, Samba, Syncthing ou Git**. Obsidian s'ouvre côté client (PC/téléphone) en pointant sur le partage.

**Services d'IA (100 % locale)** — voir section 7
- `ia-moteur` (llama.cpp/Ollama, sans internet)
- `ia-collecte` (client léger)
- `ia-tri` (client léger)

**Service événement**
- `web-evenement` (serveur web pour l'événement)

Le plan de contrôle gère ce catalogue : lister, déployer, configurer, mettre à jour, supprimer une image/VM.

---

## 7. Cas d'usage principal : événement, flux « vrac → propre » avec IA locale

### Contrainte absolue
**Aucune donnée ne doit jamais sortir de la machine.** La sécurité des données de l'événement (données de participants, RGPD) est prioritaire. Donc :

- **IA 100 % locale.** Pas d'API externe, pas de clé cloud, aucun service cloud dans le catalogue.
- La garantie est **imposée par l'architecture** (zone sans route sortante), pas seulement par la configuration.
- Les modèles sont **téléchargés en amont** puis intégrés à l'image (mode hors ligne).

### Composants

1. **`ia-moteur`** : une seule VM qui héberge le modèle (llama.cpp ou Ollama), **sans aucun accès internet**. Un seul modèle chargé pour économiser la RAM.
2. **`ia-collecte`** : petit service qui envoie ses requêtes au moteur. Droits : **écrit uniquement dans `coffre-vrac`**.
3. **`ia-tri`** : petit service qui envoie ses requêtes au moteur. Droits : **lit `coffre-vrac`, écrit dans `coffre-propre`**. Aucun autre droit, aucun outil.
4. `ia-collecte` et `ia-tri` partagent la **même image de base**, avec deux configurations différentes.

### Flux

```
Sources → ia-collecte → [coffre-vrac] → ia-tri → [coffre-propre]
                                 ↑ (brut conservé)
```

- Le tri **copie avant de supprimer** : le brut est conservé tant que le résultat n'est pas validé.
- Chaque étape est journalisée.

### Modèle et ressources
- Modèle quantifié **3–8 milliards de paramètres** (Qwen, Llama, Mistral, Gemma), suffisant pour trier/classer.
- Par défaut **CPU** (une micro-VM Firecracker ne supporte pas le passthrough GPU). Le GPU en VM (QEMU/Cloud Hypervisor + VFIO) est une **option future**.
- **Machine cible : 32 Go de RAM recommandés** (16 Go minimum).

### Durcissement
- **Injection de prompt** : les données collectées peuvent contenir des instructions malveillantes. `ia-tri` n'a **aucun outil**, aucun accès réseau, et ne peut écrire que dans `coffre-propre`. Sa sortie est traitée comme des données, jamais comme des ordres.
- Étiquetage de sensibilité des données possible (évolution future).

---

## 8. Sécurité (exigences transverses)

- **Chiffrement des volumes de données** (LUKS) : illisibles si la machine est volée.
- **Volumes persistants séparés des images de VM** (images = jetables), avec **snapshots et sauvegardes**.
- **Pare-feu par défaut « tout refuser »** entre zones.
- **API** : TLS obligatoire, authentification **par appareil**, rôles (lecture seule, opérateur, admin), limitation de débit.
- **Secrets** : stockés chiffrés, jamais dans les images ni les logs.
- **Journal d'audit** inaltérable de tous les accès et actions (utile pour prouver la conformité RGPD).
- **Accès distant** : **WireGuard auto-hébergé**, sans relais tiers. **Pas de Tailscale** (serveur de coordination externe). Pas de port ouvert sur la box.
- Images minimales, mises à jour contrôlées, services non-root quand possible.
- Isolation : chaque VM dans sa zone, communications explicitement déclarées.

---

## 9. Plan de contrôle (démon + API)

**Langage** : Go ou Rust (à trancher en début de Phase 2, justifier le choix).

**Responsabilités**
- Cycle de vie des VM : créer, démarrer, arrêter, supprimer, redémarrer.
- Supervision : état, CPU/RAM, redémarrage automatique.
- Gestion des images/catalogue.
- Réseau : création des zones, règles nftables, DHCP/DNS internes.
- Stockage : volumes persistants, snapshots, chiffrement.
- Secrets, appairage d'appareils, rôles, audit.
- Temps réel via **WebSocket** : logs, états, notifications (VM tombée, service arrêté).
- Console de chaque VM (série/SSH) accessible depuis l'UI.

**API**
- REST + WebSocket, **versionnée** (`/api/v1`), documentée en **OpenAPI**.
- Sert aussi l'interface web.
- Un client CLI (`tinyvmosctl`) consomme la même API.

**Configuration déclarative** : un fichier YAML décrit VM, zones, flux, volumes, droits. Le démon réconcilie l'état réel avec cet état voulu.

---

## 10. Interfaces

### 10.1 Interface locale PC
- Kiosque plein écran (Cage + navigateur) affichant la web UI, lancé seulement si un écran est détecté.
- Aussi accessible en web depuis le réseau local (via WireGuard si à distance).

### 10.2 Web UI (PWA responsive)
- Servie par le démon. Détecte taille/type d'écran : dashboard complet sur PC, navigation par onglets et gros boutons tactiles sur mobile/tablette.
- Fonctions : liste des VM et état, topologie réseau/zones, logs, console, catalogue, déploiement, volumes/snapshots, audit, gestion des appareils.

### 10.3 Application Android (APK)
- **Flutter** (ou React Native) : app native avec scan de QR, notifications fiables.
- Parle à la **même API**. Sert de « télécommande » de la machine.
- **L'OS ne tourne pas sur le téléphone/tablette** (bootloader verrouillé, pas de KVM) : le téléphone pilote un PC/mini-PC/Raspberry Pi.

### 10.4 Appairage
1. Au premier lancement, l'OS affiche un **QR code**.
2. L'app le scanne : échange de clés, **certificat par appareil**.
3. Le téléphone est enregistré comme appareil de confiance (révocable depuis l'UI).
4. Accès hors réseau local **uniquement via WireGuard** auto-hébergé.

---

## 11. Livrables

Tous issus **d'un seul build reproductible** : on produit une image disque, puis on la convertit.

| Livrable | Usage | Détails |
|---|---|---|
| `tinyvmos-x86_64.iso` | Clé USB via **Rufus** | ISO hybride, boot **UEFI** (+ BIOS legacy optionnel), mode live + **installateur** vers disque interne |
| `tinyvmos-arm64.img` | Raspberry Pi / ARM64 | Image brute (Rufus/balenaEtcher) |
| `tinyvmos.ova` (+ `.ovf`/`.vmdk`) | Test sous **VMware Workstation** | Conversion `qemu-img convert -O vmdk`, descripteur OVF, archive OVA |
| `tinyvmos-app.apk` | Android tablette/téléphone | `flutter build apk`, **signé** (clé de signature à conserver précieusement) ; AAB pour Play Store plus tard |

**Points importants**
- **Virtualisation imbriquée** : dans VMware, activer *Virtualize Intel VT-x/EPT ou AMD-V*. L'OS détecte l'absence de KVM au boot et affiche un message clair.
- **Installateur** : sans lui, l'OS ne tourne que depuis la clé USB. Il doit gérer partitionnement, chiffrement LUKS optionnel, copie sur disque, bootloader.
- Un script/CI (ex. GitHub Actions) produit à chaque version : ISO, IMG, OVA, APK + sommes de contrôle.

---

## 12. Environnement de développement

**Machine de dev** : ASUS TUF Gaming A18 — Windows (10 ou 11, **à confirmer**), AMD Ryzen 7, **16 Go RAM**, 500 Go libres, GPU NVIDIA RTX (probablement **RTX 5060**, 8 Go VRAM, à confirmer), NPU (à ignorer, support Linux quasi inexistant).

**Setup recommandé**
- **Build de l'OS** : **WSL2 Ubuntu**, projet dans le système de fichiers Linux (`~/tinyvmos`), **pas dans `C:\`** (builds beaucoup plus lents).
- **Claude Code** : lancé **dans WSL2**, pour accéder aux mêmes outils que le build.
- **APK** : Flutter + Android Studio + SDK directement sous **Windows**.
- **Tests quotidiens de l'ISO** : **QEMU dans WSL2**.
- **Test final de l'OVA** : VMware Workstation, idéalement hyperviseur Windows désactivé (WSL2 et VMware se gênent sur AMD).

`C:\Users\<NOM>\.wslconfig` :
```
[wsl2]
memory=10GB
processors=8
nestedVirtualization=true
swap=8GB
```

**Prérequis à vérifier**
- **AMD-V / SVM Mode** activé dans le BIOS.
- `ls /dev/kvm` dans WSL2 → doit exister. Sinon **plan B** : une VM Ubuntu dans VMware avec AMD-V exposé, dédiée au dev.
- Pilote NVIDIA à jour sous Windows + CUDA récent dans WSL2 (la RTX série 50 est récente).

**IA en dev** : faire tourner le modèle **directement dans WSL2 avec le GPU** (llama.cpp/Ollama), pas dans une micro-VM. Un modèle 7–8B quantifié passe dans 8 Go de VRAM. Pour les tests d'intégration complets avec 16 Go de RAM : utiliser un **petit modèle (3B)**. Ne pas lancer VMware en même temps qu'un gros build.

---

## 13. Roadmap (phases et critères d'acceptation)

### Phase 0 — Préparation environnement
- WSL2 configuré, `/dev/kvm` disponible, outils installés (Buildroot deps, QEMU, xorriso, qemu-img, Go/Rust).
- **Critère** : `ls /dev/kvm` OK et un script de vérification d'environnement passe.

### Phase 1 — Validation de la chaîne (micro-VM Apache)
- Lancer une micro-VM **Firecracker** avec Alpine + Apache via un script.
- **Critère** : la page Apache répond depuis l'hôte via un réseau virtuel (tap + bridge).

### Phase 2 — Démon + API + réseau virtuel
- Démon (Go/Rust) : cycle de vie de plusieurs VM, zones réseau, DHCP/DNS internes, règles nftables.
- API `/api/v1` + OpenAPI + CLI `tinyvmosctl`. Config déclarative YAML.
- **Critère** : déployer 3 VM (dhcp/dns, web, ssh) par API, avec isolation entre zones prouvée par test.

### Phase 3 — Interface web PC
- Web UI (dashboard, VM, logs temps réel, console, topologie).
- **Critère** : piloter tout le cycle de vie depuis le navigateur.

### Phase 4 — Image bootable + OVA
- Build reproductible de l'OS hôte, détection matérielle, mode headless/écran.
- ISO hybride UEFI, conversion VMDK/OVA.
- **Critère** : l'OS boote sous QEMU puis VMware, détecte l'absence de KVM avec un message clair, et lance ses VM quand KVM est actif.

### Phase 5 — Installateur sur disque
- Installateur (partitionnement, LUKS optionnel, bootloader).
- **Critère** : installation réussie sur un disque virtuel puis reboot autonome.

### Phase 6 — Catalogue événement + IA locale
- Images : coffre-vrac, coffre-propre, ia-moteur, ia-collecte, ia-tri, web-evenement, reverse-proxy.
- Zones sans sortie, volumes chiffrés, droits minimaux, journal d'audit.
- **Critère** : le flux vrac → propre fonctionne de bout en bout ; un test prouve qu'**aucun paquet ne sort** des zones Données/IA ; le brut est conservé ; le serveur web public ne peut pas joindre les coffres.

### Phase 7 — Appairage + application Android
- WireGuard auto-hébergé, QR code, certificats par appareil, révocation.
- App Flutter (état, actions, logs, notifications) → **APK signé**.
- **Critère** : téléphone appairé, pilotage à distance via WireGuard.

### Phase 8 — Durcissement, CI et release
- Pipeline CI produisant ISO/IMG/OVA/APK + checksums.
- Audit sécurité, sauvegardes/snapshots, documentation utilisateur.
- **Critère** : une release complète générée depuis zéro par la CI.

---

## 14. Structure de dépôt proposée

```
tinyvmos/
├── README.md
├── docs/                  # architecture, sécurité, guides
├── os/                    # Buildroot/Alpine : config, overlays, scripts de boot
│   ├── hardware-detect/
│   └── installer/
├── images/                # catalogue : une image par service
│   ├── ssh/ ftp/ web-apache/ dhcp-dns/ router-firewall/ reverse-proxy/
│   ├── coffre/ ia-moteur/ ia-client/ web-evenement/
├── controlplane/          # démon (Go/Rust)
│   ├── api/  openapi.yaml
│   ├── vm/  network/  storage/  audit/  pairing/
├── cli/                   # tinyvmosctl
├── web/                   # PWA / interface PC
├── mobile/                # app Flutter -> APK
├── build/                 # scripts : ISO, IMG, OVA, CI
├── tests/                 # tests d'intégration, isolation réseau, e2e
└── .github/workflows/
```

---

## 15. Contraintes et risques connus

| Risque | Mitigation |
|---|---|
| KVM imbriqué capricieux (WSL2/VMware sur AMD) | Plan B : VM Ubuntu dans VMware avec AMD-V exposé ; QEMU en repli |
| 16 Go RAM sur la machine de dev | `.wslconfig` ajusté, modèle 3B pour les tests, IA sur GPU dans WSL2 |
| Pas de GPU dans une micro-VM Firecracker | IA sur CPU par défaut ; GPU via QEMU/Cloud Hypervisor + VFIO en option |
| Fuite de données via l'IA | Zone IA/Données sans route sortante, modèles intégrés hors ligne, tests réseau automatisés |
| Injection de prompt dans les données collectées | `ia-tri` sans outils ni réseau, écriture limitée à `coffre-propre` |
| Vol de la machine | Volumes LUKS |
| Compromission de l'API | TLS, auth par appareil, rôles, audit, WireGuard uniquement |
| Perte de la clé de signature APK | Sauvegarde sécurisée hors dépôt |
| Serveur web public compromis | Zone isolée, reverse proxy, aucune route vers les coffres |

---

## 16. Points ouverts à confirmer

1. Windows **10 ou 11** sur la machine de dev.
2. GPU exact (**RTX 5060** supposée) et RAM réelle.
3. **Machine cible** de l'événement : CPU, RAM (32 Go recommandés), GPU éventuel, écran ou headless.
4. Choix **Go vs Rust** pour le démon.
5. Choix **Firecracker vs Cloud Hypervisor**.
6. Protocole du coffre : **WebDAV, Samba, Syncthing ou Git**.
7. Accès distant : réseau local seul, ou aussi à distance (WireGuard) dès la v1.
8. Nature exacte des données de l'événement (niveau de sensibilité, durée de conservation, obligations RGPD).

---

## 17. Premier pas concret

Dans WSL2 Ubuntu :

```bash
ls /dev/kvm
```

- Si le fichier existe → lancer la **Phase 1** (micro-VM Apache avec Firecracker).
- Sinon → régler la config (BIOS AMD-V, `.wslconfig`, ou plan B VM Ubuntu) avant de continuer.
