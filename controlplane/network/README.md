# network

Zones réseau : un bridge par zone, une interface tap par micro-VM, l'adressage des deux
familles IP. Tout passe par netlink, jamais par des appels à `ip` ou `brctl` : pas de
sortie de commande à analyser, des erreurs typées, rien à installer sur l'hôte.

## Les zones sont déclarées, pas codées en dur

Une zone est un groupe de VM qui partagent un réseau. L'utilisateur déclare celles qu'il
veut ; les cinq du cahier des charges — gestion, publique, interne, données, IA — ne sont
qu'une configuration d'exemple, visible avec `tinyvmosd -print-zones`.

Conséquence importante : **aucune zone n'a de comportement magique**. « La zone IA n'a pas
de sortie Internet » n'est pas une règle attachée au nom « ia » quelque part dans le code,
c'est simplement qu'aucun flux ne la relie à l'extérieur. Une règle cachée dans le code
serait invisible à la relecture de la configuration, et c'est exactement ce qu'on ne veut
pas d'un pare-feu.

## IPv6

Les deux familles sont gérées. C'était un choix : un IPv6 laissé sans configuration n'est
pas neutre, il reste actif et contourne les règles IPv4.

- Le préfixe de l'installation est un **/48 local unique** tiré au hasard au premier
  démarrage, conformément à la RFC 4193 (`fd` puis 40 bits aléatoires). Le tirage est
  voulu : un préfixe en dur serait identique sur toutes les installations, et deux machines
  TinyVMOS reliées un jour par un tunnel entreraient en collision.
- Il est **conservé dans l'état**. S'il changeait à chaque démarrage, toutes les adresses
  déjà attribuées deviendraient fausses.
- Chaque zone reçoit un **/64** taillé dedans, indexé sur les 16 bits intermédiaires. Un
  /64 n'est pas un choix de confort : c'est la taille que supposent la découverte de
  voisins et l'autoconfiguration. Plus étroit, des comportements IPv6 standard de l'invité
  cassent.
- L'identifiant d'interface d'une VM **reprend le dernier octet de son IPv4**. `10.42.10.2`
  donne `…:1::2`. Les deux adresses d'une VM se lisent du même coup d'œil, ce qui compte
  quand on diagnostique un pare-feu à deux familles.

Trois réglages noyau sont posés explicitement, parce que leur défaut varie selon les
distributions et qu'ils ont une conséquence de sécurité :

| Réglage | Valeur | Pourquoi |
|---|---|---|
| `accept_ra` sur bridges et taps | 0 | Une VM ne doit pas pouvoir annoncer une passerelle à l'hôte. Ce serait un détournement depuis l'intérieur d'une zone. |
| `accept_redirects` | 0 | Même raison, pour les redirections. |
| `accept_dad` sur le bridge | 0 | Sans cela, le noyau refuse de poser une adresse IPv6 sur un bridge sans port actif, et l'adresse reste en « tentative ». |

## Adressage

Les attributions sont **enregistrées**, pas recalculées. Une adresse dérivée d'un hachage
du nom entrerait en collision tôt ou tard, et sans bruit. Une VM doit aussi retrouver la
même adresse après un redémarrage du démon, sinon les règles de pare-feu et les baux ne
voudraient plus rien dire.

L'attribution prend la plus petite adresse libre, en sautant l'adresse de réseau, celle de
l'hôte — qui sert de passerelle — et celle de diffusion en IPv4.

## Ce qui est refusé, et pourquoi

`ValidateZones` refuse deux zones qui se recouvrent, même par inclusion.

Ce n'est pas du zèle. Deux interfaces portant le même réseau font **disparaître le trafic
sans le moindre message d'erreur** : le noyau garde une seule route et l'autre interface ne
reçoit rien. Le piège a déjà coûté une heure de diagnostic en Phase 1, avec deux bridges
laissés sur `10.42.10.0/24`. Il est maintenant refusé à la validation, et `EnsureZone`
vérifie en plus qu'aucune autre interface ne route déjà le réseau de la zone, en nommant
la coupable.

Sont refusés aussi : un nom de zone trop long pour que le nom de bridge tienne dans la
limite du noyau, un CIDR qui désigne une adresse d'hôte (`10.42.20.1/24` au lieu de
`10.42.20.0/24` — erreur de saisie fréquente, corrigée en silence ce serait pire), un
réseau plus étroit qu'un /30, et deux zones partageant un bridge.

## Un piège qui coûte cher

**L'interface tap doit appartenir à l'utilisateur confiné**, pas à root.

Le noyau n'autorise l'attachement à un tap qu'à son propriétaire déclaré, ou à un processus
doté de `CAP_NET_ADMIN`. Firecracker tourne sous `jailer`, en utilisateur `tinyvmos`, sans
aucune capacité. Si le tap appartient à root, Firecracker répond :

```
Open tap device failed: Operation not permitted (os error 1)
```

À noter : la bibliothèque netlink appelle **toujours** `TUNSETOWNER` avec la valeur du
champ `Owner`. Le laisser à zéro ne veut donc pas dire « pas de propriétaire », mais
« propriétaire root », ce qui est plus restrictif que de ne rien régler. D'où `TapOwner` et
`TapGroup` sur le gestionnaire.

## Routage

`EnableForwarding` active le routage pour les deux familles. Le routage seul **n'ouvre
rien** : la politique du pare-feu reste « tout refuser » et seuls les flux déclarés passent.
Sans routage en revanche, aucun flux déclaré entre deux zones ne pourrait fonctionner.

## Ce que ce paquet ne fait pas encore

Le pare-feu. Les zones existent, sont adressées et routées — donc elles se parlent. C'est
ce que le lot B2 viendra couper, avec une table nftables de famille `inet`, qui couvre les
deux familles d'un seul jeu de règles.

Un point à ne pas oublier à ce moment-là : **ne jamais jeter l'ICMPv6 en bloc.** La
découverte de voisins en dépend, et un `drop` global casse purement et simplement IPv6.
