# cli

`tinyvmosctl` — client en ligne de commande, basé sur cobra.

Il consomme **la même API** que l'interface web et l'application mobile : aucune logique
métier ici. Sortie lisible par défaut, `--json` pour le scriptage.

Commandes prévues : `vm`, `zone`, `flow`, `volume`, `snapshot`, `image`, `device`,
`audit`, et `apply -f tinyvmos.yaml` qui reproduit l'état complet depuis zéro.
