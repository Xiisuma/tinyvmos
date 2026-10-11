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

## Règles qui ne se négocient pas

- **Toute VM est lancée via `jailer`.** Jamais Firecracker en direct, même pour tester.
  Le mode direct existe dans `build/run-firecracker.sh`, pour le diagnostic, pas ici.
- Le filtre seccomp n'est jamais désactivé : c'est la dernière barrière entre une VM
  compromise et l'hôte.
- **Chaque VM reçoit sa propre copie de l'image.** Le disque racine est monté en écriture
  et `jailer` installe les artefacts par liens durs : deux VM issues de la même image
  partageraient le même inode et s'écraseraient. La copie passe par
  `cp --reflink=auto --sparse=always`, donc instantanée et sans coût disque sur btrfs ou
  xfs, et limitée à l'occupation réelle ailleurs.
- L'image du catalogue n'est jamais modifiée. C'est ce qui rend les images jetables.
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

## Liens durs de jailer : réglé

`jailer` installe les artefacts dans la jail par **liens durs**, qui exigent que la jail et
les artefacts soient sur le même système de fichiers. Deux mesures :

- le défaut de `-jail-dir` est un dossier `jail/` à côté du fichier d'état, donc sur le
  même système de fichiers que lui ;
- si les chemins sont donnés explicitement et tombent sur deux systèmes de fichiers
  différents, `Create` échoue **avec un message qui le dit**. Sans cette vérification,
  l'échec arrivait bien plus loin sous la forme d'un « Invalid argument » de `jailer` qui
  ne renseignait sur rien.
