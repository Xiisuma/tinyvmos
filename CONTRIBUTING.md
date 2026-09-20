# Contribuer à TinyVMOS

## Flux de travail

`main` est protégée. Aucun commit direct : tout passe par une branche et une pull request.

```bash
git switch main
git pull
git switch -c feat/ma-fonctionnalite
# ... travail, commits ...
git push -u origin feat/ma-fonctionnalite
gh pr create --fill
```

Une fois la CI au vert et la relecture faite, la PR est fusionnée en **squash**, puis
la branche est supprimée. L'historique de `main` reste linéaire : un commit par changement.

## Nommage des branches

| Préfixe | Usage |
|---|---|
| `feat/` | Nouvelle fonctionnalité |
| `fix/` | Correction de bug |
| `chore/` | Outillage, dépendances, tâches sans effet fonctionnel |
| `docs/` | Documentation seule |
| `refactor/` | Restructuration sans changement de comportement |
| `test/` | Ajout ou correction de tests |
| `ci/` | Intégration continue |
| `sec/` | Durcissement ou correction de sécurité |

Le reste du nom est en minuscules, mots séparés par des tirets : `feat/vmm-interface-firecracker`.

## Messages de commit

Format [Conventional Commits](https://www.conventionalcommits.org/), en français.

```
<type>(<portée facultative>): <sujet à l'impératif, 50 caractères maximum>

<corps facultatif : le pourquoi, pas le comment — le diff dit déjà le comment>

<pied facultatif : Refs #12, BREAKING CHANGE: ...>
```

Types : `feat`, `fix`, `chore`, `docs`, `refactor`, `test`, `ci`, `perf`, `sec`.

Exemple :

```
feat(vm): ajoute l interface VMM et l implementation Firecracker

Le VMM passe derriere une interface pour que Cloud Hypervisor reste
ajoutable si le passthrough GPU devient necessaire en Phase 6.
Toute VM est lancee via jailer, avec un uid et un gid dedies.
```

## Pull requests

- Une PR répond à **un** objectif. Si elle en fait deux, elle en devient deux.
- Le titre suit la même convention que les commits.
- La description explique le pourquoi et comment vérifier. Le modèle de PR guide la rédaction.
- La CI doit être verte avant toute fusion.
- Une PR qui touche au réseau, au pare-feu ou à l'isolation **doit** citer les tests
  d'isolation exécutés et leur résultat.

## Règles non négociables

- **Aucun secret dans le dépôt.** Ni clé, ni certificat, ni jeton, ni mot de passe,
  ni configuration WireGuard. La CI échoue si un tel fichier est suivi.
- **Aucune politique réseau permissive par défaut.** Tout flux autorisé est déclaré
  explicitement dans `tinyvmos.yaml`.
- **Toute VM est lancée via `jailer`.** Jamais Firecracker en direct, même « juste pour tester ».
- **Les scripts d'écriture disque ne se testent jamais sur les disques réels** de la
  machine de développement. Uniquement sur disques virtuels.
- **Le dimensionnement se fait sur la machine cible**, qui est plus faible que la machine
  de développement. Chaque image déclare son budget mémoire et il est vérifié.
- Le contrat `controlplane/api/openapi.yaml` est écrit **avant** le code, et les clients
  en sont générés. Aucun client d'API écrit à la main.

## Environnement

Avant toute chose :

```bash
./build/check-env.sh
```

Le développement se fait dans WSL2, dépôt dans `~/tinyvmos`, jamais sous `/mnt/c/`.
