# build

Scripts de build et d'intégration continue.

| Script | Rôle |
|---|---|
| `check-env.sh` | Vérifie l'environnement de développement (Phase 0) |
| `make-iso.sh` | ISO hybride x86_64, boot UEFI, mode live plus installateur |
| `make-img.sh` | Image brute ARM64 pour Raspberry Pi |
| `make-ova.sh` | Conversion VMDK, descripteur OVF, archive OVA |
| `run-firecracker.sh` | Lance une micro-VM de test, avec ou sans `jailer` |

Règle : un dépôt fraîchement cloné doit produire **tous** les livrables sans intervention
manuelle. Chaque livrable est accompagné de sa somme de contrôle SHA-256.
