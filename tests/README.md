# tests

Tests d'intégration. Les plus importants sont ceux qui prouvent l'isolation — ce sont eux
qui garantissent la promesse du projet, et ils tournent dans la CI.

- Depuis la zone Publique, tenter d'atteindre chaque autre zone : tout doit échouer
  sauf les flux explicitement déclarés.
- Depuis les zones Données et IA, tenter de joindre Internet par IP et par nom :
  doit échouer. Capture `tcpdump` sur le port physique à l'appui.
- `web-evenement` ne peut joindre ni les coffres, ni les VM d'IA.
- Flux vrac vers propre de bout en bout, avec conservation du brut.
- Injection de prompt : des données contenant des instructions malveillantes ne doivent
  déclencher aucune action de `ia-tri`, qui n'a ni outil ni accès réseau.
- Tenue de la contrainte mémoire de la machine cible, plus faible que celle de dev.
