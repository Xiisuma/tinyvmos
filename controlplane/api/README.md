# api

`openapi.yaml` est le contrat, écrit **avant** le code. Les types Go et les stubs de
routes en sont générés avec `oapi-codegen` ; le client de la web UI et celui de
l'application Flutter en sont générés aussi. La logique n'est décrite qu'une fois.

- Racine `/api/v1`, versionnée.
- TLS obligatoire, certificat auto-signé généré au premier démarrage.
- Authentification par appareil (certificat client), rôles `lecture`, `opérateur`, `admin`.
- Limitation de débit et taille maximale des requêtes.
- WebSocket pour les logs, les changements d'état, les notifications et les consoles série.
