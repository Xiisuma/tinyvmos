# hardware-detect

Établit le profil matériel au démarrage : architecture, présence et droits sur `/dev/kvm`,
RAM, cœurs CPU, disques, cartes réseau, GPU, écran connecté.

Sortie : un fichier JSON lu par le plan de contrôle.

Deux cas méritent un message clair à l'écran, pas seulement dans les logs :

- **KVM absent** — Firecracker ne peut pas fonctionner. Repli QEMU en émulation logicielle,
  signalé explicitement comme réservé aux tests.
- **Ressources insuffisantes** — le nombre de VM déployables est plafonné et l'utilisateur averti.
