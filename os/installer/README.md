# installer

Installation de TinyVMOS sur un disque interne : partitionnement GPT, ESP, racine,
volume de données avec chiffrement LUKS optionnel, bootloader UEFI, configuration initiale.

Règles de sécurité, sans exception :

- Double confirmation avant toute écriture disque, avec rappel du disque ciblé.
- Interdiction d'installer sur le disque depuis lequel on a démarré.
- **Ne jamais tester ces scripts sur les disques réels de la machine de développement.**
  Uniquement sur disques virtuels.
