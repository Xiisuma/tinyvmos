# kernel

Noyau invité commun à toutes les micro-VM.

Firecracker démarre le noyau **directement** : pas d'UEFI, pas de bootloader, pas de
BIOS. Il lui faut un `vmlinux` non compressé, et l'image racine est passée en
`root=/dev/vda` sur la ligne de commande. C'est une contrainte structurelle de
Firecracker, et c'est aussi ce qui lui donne sa petite surface d'attaque.

## Récupération

```bash
./images/kernel/fetch-kernel.sh
```

Le script télécharge le noyau, **vérifie son empreinte SHA-256** et pose un lien stable
`output/kernel/vmlinux`. Un fichier déjà en cache et valide n'est pas retéléchargé.

## Version actuelle

| Élément | Valeur |
|---|---|
| Version | 6.1.155 |
| Origine | chaîne d'intégration continue de Firecracker, `firecracker-ci/v1.15/x86_64` |
| SHA-256 | `e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2` |

## À faire en Phase 4

Ce noyau est celui de la CI de Firecracker : il fonctionne, mais il n'est pas taillé
pour TinyVMOS. Il sera remplacé par un noyau construit sur mesure, réduit au strict
nécessaire :

- virtio-net, virtio-blk, virtio-vsock
- ext4
- aucun module, tout en statique
- pas de pilote de matériel physique, pas de son, pas de graphique

Moins de code dans l'invité, c'est moins de surface exposée à un participant qui aurait
compromis un service — et un démarrage plus rapide.

## Ligne de commande du noyau

Telle que passée par `build/run-firecracker.sh` :

```
console=ttyS0 reboot=k panic=1 pci=off
i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd
root=/dev/vda rw
ip=<adresse>::<passerelle>:255.255.255.0::eth0:off
tinyvmos.ip=<adresse>/<préfixe> tinyvmos.gw=<passerelle>
```

- `pci=off` : Firecracker n'expose aucun bus PCI. C'est aussi pourquoi le passthrough
  GPU est structurellement impossible, décision assumée.
- `panic=1` et `reboot=k` : une VM qui panique s'arrête au lieu de rester bloquée.
- `ip=` est interprété par le noyau, `tinyvmos.ip=` est relu par le service
  `tinyvmos-net` de l'image. Les deux disent la même chose, l'un pour le noyau, l'autre
  pour OpenRC. La même image sert ainsi plusieurs VM sans être reconstruite.
