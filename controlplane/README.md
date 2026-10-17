# controlplane

Le démon TinyVMOS, écrit en Go. C'est le cœur du projet : tout le reste n'en est qu'un client.

## Pourquoi Go

SDK Firecracker officiel maintenu par AWS, `vishvananda/netlink` et `google/nftables`
natifs, cross-compilation ARM64 en une variable d'environnement, binaire statique unique,
compilation en secondes. Le démon est un orchestrateur limité par les entrées/sorties,
pas un hyperviseur.

## Deux binaires

| Binaire | Rôle |
|---|---|
| `tinyvmosd` | Le démon. Linux seulement par nature : netlink, nftables, KVM, Firecracker. |
| `tinyvmosctl` | La commande. Ne parle que l'API, donc compile pour Linux, macOS et Windows. |

Un binaire unique rendrait impossible la compilation de la commande pour le poste d'un
administrateur, puisque la moitié démon tire des paquets Linux seulement. La séparation
verrouille aussi la règle du projet : aucun raccourci vers l'état n'est possible depuis
le client, il n'y a que l'API.

## État d'avancement

Phase 2, lots A, B1 et B2 : interface `VMM`, implémentation Firecracker, machine d'état,
supervision, magasin d'état, zones réseau, adressage double pile et pare-feu.
Pas encore d'API : le démon prend ses zones, ses flux et ses VM sur la ligne de commande.

| Lot | Contenu | État |
|---|---|---|
| A | `VMM`, Firecracker sous `jailer`, machine d'état, relance auto, magasin d'état | fait |
| B1 | Zones, bridges et tap par netlink, adressage double pile | fait |
| B2 | Pare-feu nftables « tout refuser », flux déclarés et publication de services | fait |
| C | Schéma `tinyvmos.yaml`, validation, `--dry-run` | à faire. La **boucle de réconciliation** est déjà en place, voir `reconcile/` |
| D | `openapi.yaml`, REST et WebSocket, TLS, authentification par appareil, rôles | à faire |
| E | Journal d'audit chaîné par hachage | à faire |
| F | `tinyvmosctl` | à faire |
| G | Image `dhcp-dns` et tests d'isolation entre zones | à faire |

## Sous-paquets

| Paquet | Rôle |
|---|---|
| `vm/` | Interface `VMM`, implémentation Firecracker, machine d'état, superviseur |
| `network/` | Zones, bridges, interfaces tap, adressage IPv4 et IPv6 |
| `state/` | Magasin d'état : fichier JSON réécrit de façon atomique |
| `cmd/tinyvmosd/` | Le démon |

## Magasin d'état

Un fichier JSON, réécrit de façon atomique : écriture dans un temporaire du même dossier,
`fsync` du fichier, `rename`, `fsync` du dossier. Le `rename` atomique donne les
transactions gratuitement — soit l'ancien état complet, soit le nouveau, jamais un mélange.

SQLite a été écarté : le pilote courant impose CGO, ce qui casse le binaire statique et la
compilation croisée ARM64, livrable du produit vérifié par la CI. La variante pure Go est
du C transpilé, plus lourde et plus lente. L'état est petit par construction, puisque
l'architecture est déclarative : une trentaine de VM représentent environ 50 Ko.

Deux règles rendent ce choix tenable, et valent de toute façon avec n'importe quel support :

1. **Une écriture par opération**, jamais une par VM.
2. **Les métriques ne sont pas persistées.** CPU, mémoire résidente et durée de vie
   restent en mémoire et partent par WebSocket. Seuls les faits durables sont écrits.

Un état écrit par une version plus récente du démon est **refusé** plutôt que chargé :
réécrire un fichier dont on ne connaît pas tous les champs les effacerait.

Le journal d'audit (ajout seul, chaîné par hachage) et les secrets (chiffrés) ont leurs
propres fichiers. Le journal d'audit ne peut pas vivre ici : le réécrire entièrement à
chaque entrée détruirait la propriété recherchée.

## Une identité confinée par VM

Chaque VM tourne sous son propre uid et son propre gid, pris dans la plage réservée
**60000-60999**, attribués et persistés comme les adresses IP.

Les partager — ce que faisait la première version — voudrait dire qu'une VM échappée de
Firecracker a les droits de **toutes les autres** : leurs images de travail, leurs jails,
leurs sockets. L'isolation réseau tiendrait, celle du système de fichiers tomberait d'un
coup. La documentation Firecracker recommande explicitement un identifiant par VM.

**Aucun utilisateur système n'est créé**, et c'est volontaire. `jailer` appelle `setuid`
avec une valeur numérique, `chown` aussi, et Firecracker confiné ne lit jamais
`/etc/passwd`. Créer mille comptes pour mille VM polluerait le système pour rien, et
chaque création serait une écriture de plus à faire échouer.

Droits sur les fichiers :

| Chemin | Mode | Pourquoi |
|---|---|---|
| `<run-dir>` | 0711 | Traversable, pas listable : une VM atteint son dossier, sans découvrir les autres |
| `<run-dir>/<vm>` | 0700 | Seule cette VM y entre |
| `<run-dir>/<vm>/rootfs.ext4` | 0600 | Son image de travail |
| `<run-dir>/<vm>/vmlinux` | 0600 | Sa copie du noyau |

Le 0711 du dossier parent n'est pas un détail. En 0750 root, une VM ne pouvait même pas
traverser jusqu'à ses propres fichiers. Ça fonctionnait — `jailer` installe les artefacts
en tant que root — mais la séparation reposait alors sur l'inaccessibilité générale plutôt
que sur la propriété des fichiers, et on ne savait pas laquelle des deux protégeait.

`tests/phase2d-identites.sh` le prouve en prenant l'identité d'une VM avec `setpriv` pour
tenter de lire les fichiers d'une autre, et vérifie aussi qu'elle lit bien les siens.

## Rien ne démarre sans que son empreinte ait été vérifiée

Une micro-VM démarre sur deux fichiers : l'image de son système de fichiers et le noyau
invité. Les deux sont du code, exécuté avec tous les droits à l'intérieur de la VM. Un
fichier modifié entre la construction et le démarrage donne à qui l'a modifié le contenu
entier de la VM, et le pare-feu n'y change rien : une VM est autorisée à faire ce que sa
zone permet, quel que soit le code qu'elle exécute.

Les deux niveaux de contrôle ne valent pas la même chose, et c'est le point à comprendre.

**Les métadonnées de construction** (`output/images/<service>.json`) détectent une image
corrompue — copie tronquée, disque fatigué, construction interrompue — ou remplacée par
quelqu'un qui ignore leur existence. Elles ne protègent pas contre un attaquant attentif :
le fichier de métadonnées se modifie aussi facilement que l'image qu'il décrit. Elles ne
sont donc pas l'autorité.

**L'empreinte retenue dans l'état du démon** l'est. Elle est enregistrée à la première
utilisation de l'artefact, puis comparée à chaque démarrage suivant. Elle vit dans le
fichier d'état, écrit par le démon et lisible de lui seul : modifier une image dans le
catalogue ne suffit plus, il faut aussi atteindre l'état.

C'est de la confiance à la première utilisation, le même compromis que les clés d'hôte
SSH, avec la même limite : la première fois, on fait confiance. Une signature du catalogue
la lèverait ; elle demande une clé, donc une gestion de clé, et c'est le sujet de la
distribution des images.

La vérification a lieu dans le gestionnaire, **avant** `vmm.Create`, donc avant que
l'artefact ne soit copié dans le dossier de travail de la VM : c'est le fichier du
catalogue qui est vérifié, celui qui serait modifié. Vérifier la copie n'aurait rien
prouvé, puisque c'est nous qui venons de la faire.

Conséquence voulue : **une image reconstruite ne démarre plus.** Son empreinte a changé,
et c'est exactement ce que le dispositif doit signaler.

```bash
sudo ./output/bin/tinyvmosd -state /var/lib/tinyvmos/state.json \
  -trust /chemin/vers/output/images/web-apache.ext4
```

Sans cette échappatoire, la seule issue serait de supprimer l'état, ce qui emporterait
aussi les adresses attribuées et les identités confinées. `-trust` se passe démon arrêté :
un démon en marche réécrirait l'état depuis sa copie en mémoire.

`tinyvmosd -list` affiche les empreintes retenues et leur origine — métadonnées de
construction, première utilisation, ou acceptation par l'opérateur. Sans cette
information, impossible de savoir ce que la vérification vaut.

Le schéma d'état passe en version 2. Le champ est optionnel et aurait pu être ajouté en
silence ; monter le numéro fait qu'un démon antérieur refuse de repartir sur cet état, au
lieu de le lire sans vérifier aucune empreinte. Un retour en arrière qui désactive la
vérification sans le dire est précisément ce qu'il faut éviter.

## Règles qui ne se négocient pas

- **Toute VM est lancée via `jailer`, sous sa propre identité.** Jamais Firecracker en
  direct, jamais une identité partagée entre deux VM, même pour tester.
  Le mode direct existe dans `build/run-firecracker.sh`, pour le diagnostic, pas ici.
- Le filtre seccomp n'est jamais désactivé : c'est la dernière barrière entre une VM
  compromise et l'hôte.
- **Chaque VM reçoit sa propre copie de l'image.** Le disque racine est monté en écriture
  et `jailer` installe les artefacts par liens durs : deux VM issues de la même image
  partageraient le même inode et s'écraseraient. La copie passe par
  `cp --reflink=auto --sparse=always`, donc instantanée et sans coût disque sur btrfs ou
  xfs, et limitée à l'occupation réelle ailleurs.
- L'image du catalogue n'est jamais modifiée. C'est ce qui rend les images jetables.
- **Aucun artefact ne démarre sans que son empreinte ait été vérifiée**, image comme
  noyau. Un fichier dont l'empreinte a changé arrête le démon, il ne produit pas un
  avertissement.
- Le contrat `api/openapi.yaml` sera écrit **avant** le code, et les clients en seront
  générés. Aucun client d'API écrit à la main.
- Un déploiement qui dépasse la RAM disponible sera refusé, plutôt que laissé à l'OOM killer.

## Trois pièges d'intégration, et leur raison

Documentés parce qu'ils se reproduiront sur un autre hôte ou une autre version du SDK.

1. **`JailerConfig.NumaNode` nul fait paniquer le SDK.** `jail()` le déréférence sans
   vérifier. Nœud 0 par défaut.
2. **`CgroupVersion` vide fait échouer `jailer`** avec `CgroupHierarchyMissing` sur tout
   hôte en cgroup v2 pur, ce qui inclut WSL2 et la plupart des distributions récentes. La
   version est **détectée** à l'exécution par la présence de
   `/sys/fs/cgroup/cgroup.controllers`, et non codée en dur : l'OS doit démarrer sur un
   parc hétérogène.
3. **Un lien dur vers un lien symbolique reste un lien symbolique.** `output/kernel/vmlinux`
   en est un ; sa cible n'existe pas dans le chroot, et Firecracker répond « The kernel
   file cannot be opened ». Les chemins sont résolus avant d'être confiés au SDK.

## Développement

```bash
cd controlplane
gofmt -l .                 # doit ne rien renvoyer
go vet ./...
go test -race ./...
GOOS=linux GOARCH=arm64 go build ./...   # le livrable Raspberry Pi ne doit jamais casser
go build -o ../output/bin/tinyvmosd ./cmd/tinyvmosd
```

Les tests unitaires utilisent `vm.FakeVMM`, un hyperviseur en mémoire. Ils exercent la
machine d'état, la supervision et la temporisation des relances **sans KVM ni
Firecracker**, donc aussi dans la CI où `/dev/kvm` n'existe pas.

Ce qu'un simulacre ne peut pas prouver est couvert par `tests/phase2a-demon.sh`, qui fait
tourner une vraie micro-VM sous `jailer`.

## Essai manuel

```bash
sudo ./build/net-setup.sh up
cd controlplane && go build -o ../output/bin/tinyvmosd ./cmd/tinyvmosd && cd ..

sudo ./output/bin/tinyvmosd \
  -state output/run/state.json \
  -run-dir output/run \
  -jail-dir output/jail \
  -vm name=web1,service=web-apache,tap=tap0,ip=10.42.10.2/24,gw=10.42.10.1

# dans un autre terminal
curl http://10.42.10.2/
sudo pkill -9 -x firecracker     # la vm doit se relancer seule
./output/bin/tinyvmosd -state output/run/state.json -list
```

La forme `-vm clé=valeur` est provisoire : elle disparaîtra au profit de `tinyvmos.yaml`
au lot C. Elle n'existe que pour exercer le démon de bout en bout dès maintenant.

## Chemins : une seule contrainte, satisfaite par construction

`jailer` installe dans la jail **tous** les artefacts par liens durs : l'image comme le
noyau. Les liens durs exigent le même système de fichiers.

Les deux sont donc copiés dans le dossier de travail de la VM, et la jail est placée juste
à côté. La seule contrainte restante — dossier de travail et jail sur le même système de
fichiers — est satisfaite par les chemins par défaut, et vérifiée explicitement avec un
message clair si elle est violée par des chemins donnés à la main.

Les défauts étaient d'ailleurs incohérents entre eux : `run-dir` valait `/run/tinyvmos`,
en tmpfs, et la jail se plaçait à côté du fichier d'état, sur disque. Désormais
`/var/lib/tinyvmos/run` et `/var/lib/tinyvmos/jail`. Garder des copies d'image en tmpfs
aurait de toute façon consommé de la mémoire vive par VM, ce qu'il ne faut surtout pas
faire sur une machine modeste.

## Liens durs de jailer : réglé

`jailer` installe les artefacts dans la jail par **liens durs**, qui exigent que la jail et
les artefacts soient sur le même système de fichiers. Deux mesures :

- le défaut de `-jail-dir` est un dossier `jail/` à côté du fichier d'état, donc sur le
  même système de fichiers que lui ;
- si les chemins sont donnés explicitement et tombent sur deux systèmes de fichiers
  différents, `Create` échoue **avec un message qui le dit**. Sans cette vérification,
  l'échec arrivait bien plus loin sous la forme d'un « Invalid argument » de `jailer` qui
  ne renseignait sur rien.
