# os

OS hôte TinyVMOS : Linux minimal construit avec Buildroot, avec KVM, virtio, nftables,
WireGuard et LUKS. Sans bureau, sans compilateur, sans paquet superflu.

- `buildroot/` — configuration Buildroot, overlays rootfs, versions figées
- `hardware-detect/` — établissement du profil matériel au démarrage
- `installer/` — installation sur disque interne, avec chiffrement LUKS optionnel

Le profil matériel est écrit en JSON et consommé par le plan de contrôle. Il détermine
notamment si la machine démarre en mode kiosque (écran détecté) ou en mode headless.
