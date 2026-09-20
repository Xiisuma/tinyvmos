## Objectif

<!-- Pourquoi ce changement. Le diff dit déjà le comment. -->

## Phase concernée

<!-- Phase 0 à 9 du plan, et le point précis. Exemple : Phase 2, point 2.4 -->

## Changements

-

## Vérification

<!-- Comment un relecteur reproduit le résultat. Commandes exactes. -->

```bash
```

## Sécurité et isolation

<!--
Cocher ce qui s'applique. Une PR qui touche au réseau, au pare-feu ou à
l'isolation doit citer les tests exécutés et leur résultat.
-->

- [ ] Ce changement ne touche ni au réseau, ni au pare-feu, ni à l'isolation
- [ ] Les tests d'isolation ont été exécutés et sont au vert
- [ ] Aucun flux réseau n'a été ouvert sans déclaration explicite dans `tinyvmos.yaml`
- [ ] Aucun secret, clé, certificat ou jeton n'est ajouté au dépôt
- [ ] Les VM restent lancées via `jailer`

## Dimensionnement

<!--
La machine cible est plus faible que la machine de développement.
À renseigner dès qu'une image ou une VM est touchée.
-->

- [ ] Sans objet
- [ ] Budget mémoire déclaré et mesuré : <!-- image, RAM au repos, RAM en charge, temps de démarrage -->

## Points d'attention pour la relecture

<!-- Ce dont tu n'es pas sûr, les compromis assumés, ce qui mérite un second avis. -->
