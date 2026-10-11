# reconcile

Ramène l'état réel de la machine vers l'état voulu, en continu.

## Pourquoi

Avant ce paquet, le démon posait son réseau et son pare-feu **une seule fois** au
démarrage, puis ne corrigeait plus rien.

Conséquence : un `nft flush ruleset` lancé par n'importe quel autre outil détruisait toute
l'isolation, **définitivement et sans un mot**. C'était la faiblesse la plus grave du plan
de contrôle, et la documentation affirmait pourtant le contraire — « toute retouche est
écrasée à la prochaine réconciliation ». Cette phrase est maintenant vraie.

## Ce qui est réparé

| Écart | Réparation |
|---|---|
| Table nftables disparue, vidée ou remplacée | Le pare-feu est réappliqué |
| Politique différente de celle voulue | Le pare-feu est réappliqué |
| Bridge de zone supprimé | Il est recréé, avec son adressage |
| Interface tap manquante, VM à l'arrêt | Elle est recréée |
| Interface tap manquante, VM en marche | **Signalé, pas réparé** — voir plus bas |

L'ordre compte : le pare-feu passe en premier. Si l'isolation est tombée, c'est ce qu'il
faut rétablir avant tout le reste.

## Détecter sans réécrire

Réappliquer le pare-feu à chaque passe serait simple, et faux : chaque application
supprime puis recrée la table, donc **remet tous les compteurs à zéro**. On perdrait la
trace des refus, qui est précisément ce qu'on regarde pour comprendre un problème.

La détection repose sur une **règle témoin** posée dans la table :

```
counter comment "tinyvmos-empreinte-b40202a2bf7d"
```

Elle ne rend aucun verdict — un test le vérifie, sinon elle modifierait la politique
qu'elle sert à identifier. L'empreinte est celle du jeu de règles, donc trois écarts se
voient : la table a disparu, la règle témoin a disparu, ou la politique a changé. Le
nombre de règles est comparé en plus, pour attraper une retouche manuelle.

La lecture se fait sur la **sortie JSON** de `nft`, pas sur son affichage textuel : le
texte est fait pour les humains et sa mise en forme change d'une version à l'autre, ce qui
ferait de la vérification un piège.

### Le piège de l'auto-référence

L'empreinte porte sur le jeu de règles **sans la règle témoin**, puisqu'elle y est ensuite
insérée. La recalculer sur le texte final donne une autre valeur.

La première version faisait exactement cette erreur. Résultat : la comparaison échouait à
chaque passe, le pare-feu était réécrit toutes les deux secondes, et tous les compteurs
repartaient de zéro en boucle. Le symptôme visible était trompeur — « dérive corrigée »
dans les journaux, en apparence le bon comportement.

Un test vérifie maintenant qu'au repos la table **n'est pas recréée**, en comparant le
handle des règles avant et après plusieurs passes.

## L'interface d'une VM en marche n'est pas réparée

C'est délibéré. Firecracker détient un descripteur sur l'ancienne interface ; recréer une
interface du même nom ne le rebranche pas. La VM resterait sans réseau, et seule une
relance la rétablirait.

Relancer une VM de production parce qu'une interface a disparu est une décision qui mérite
d'être prise explicitement, pas déclenchée par une boucle de fond. L'écart est donc
signalé, avec un message qui dit ce qu'il faudrait faire.

## Intervalle

Dix secondes par défaut. Assez court pour qu'une isolation détruite soit rétablie avant
qu'on ait eu le temps d'en profiter, assez long pour que la vérification ne pèse rien :
tant qu'aucune dérive n'est constatée, une passe ne fait que des lectures.

`-reconcile-interval 0` désactive la réparation. Le démon le dit alors explicitement dans
ses journaux, et un test vérifie qu'avec cette valeur **rien n'est réparé** — sinon le
drapeau ne voudrait rien dire.
