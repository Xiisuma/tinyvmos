# mobile

Application Flutter, livrée en APK signé. Elle sert de télécommande de la machine.

**L'OS ne tourne pas sur le téléphone** (bootloader verrouillé, pas de KVM) : le téléphone
pilote un PC, un mini-PC ou un Raspberry Pi.

Appairage : l'OS affiche un QR code au premier lancement, l'application le scanne,
un certificat par appareil est émis et stocké dans le magasin sécurisé du téléphone.
Hors réseau local, l'accès passe uniquement par WireGuard auto-hébergé.

**La clé de signature de l'APK est conservée hors dépôt, en lieu sûr.** Sa perte empêche
définitivement toute mise à jour de l'application.
