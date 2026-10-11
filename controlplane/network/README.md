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

## Pare-feu

Tout trafic qui entre ou sort d'une zone est refusé, sauf les flux déclarés. Une seule
table de famille `inet` couvre IPv4 et IPv6 : une règle sans son équivalent dans l'autre
famille serait un trou qui ne se voit pas.

### Les flux, et les deux zones fictives

Un flux autorise un trafic d'une zone vers une autre. Deux zones fictives rendent le
modèle uniforme : `internet` pour ce qui est derrière l'interface de sortie, `host` pour
les services que la machine rend aux VM.

Conséquence : « cette zone a une sortie Internet » n'est pas un drapeau sur la zone, c'est
un flux déclaré comme les autres. La promesse « aucune donnée ne sort » se vérifie donc en
lisant la liste des flux, pas en fouillant le code.

Un accès **entrant** depuis Internet n'est pas un flux : il désigne **une** VM précise et
demande une redirection de port. Le déclarer comme flux est refusé à la validation, avec un
message qui renvoie vers les publications, décrites plus bas. Les deux concepts sont
séparés parce que les mélanger aurait donné un type aux champs à moitié vides selon les cas.

### Texte plutôt que netlink

Le jeu de règles est engendré en texte puis appliqué par `nft -f`, et non construit par
netlink comme le prévoyait le plan. Trois raisons :

- **`nft -f` est atomique** : le fichier passe en entier ou pas du tout. Un pare-feu à
  moitié appliqué serait pire que pas de pare-feu.
- **Le résultat est lisible.** Sur une machine déployée, un opérateur doit pouvoir
  constater ce qui est installé, sans outil et sans confiance aveugle. Même raisonnement
  que pour l'état en JSON. Le jeu de règles est écrit dans `run-dir`, et `-print-ruleset`
  l'affiche sans rien installer ni exiger de droits.
- Construire à la main le bytecode des expressions nftables pour une politique à deux
  familles est long et facile à rater d'un rien. Sur un composant de sécurité, la
  lisibilité du résultat vaut plus que l'élégance du moyen.

### Aucune chaîne en policy drop

C'est le point le plus contre-intuitif, et il est volontaire.

Une politique `drop` sur `forward` s'appliquerait à **tout** le trafic routé de la machine,
y compris celui d'un conteneur ou d'un tunnel. Dans nftables, une politique de chaîne ne se
laisse pas contredire par une autre table : notre table casserait le reste de la machine.
Sur `input`, ce serait encore plus grave — elle couperait l'accès d'administration, SSH
compris, dès le premier démarrage.

Le refus passe donc par des règles qui **nomment nos interfaces** :

```
iifname @zones counter drop
oifname @zones counter drop
```

La garantie est la même — un paquet qui entre ou sort d'une zone sans règle l'autorisant
est jeté, et un `drop` ne se laisse pas annuler par une autre table — mais un paquet qui ne
concerne aucune zone traverse la chaîne sans être regardé. Deux tests gardent cette
propriété : l'absence de `policy drop`, et la survie d'une table nftables étrangère à
l'application comme au retrait.

### ICMP, à ne pas jeter sans réfléchir

- **ICMPv6 de découverte de voisins** : accepté explicitement. Un `drop` global casse
  purement et simplement IPv6, la résolution d'adresse en dépendant.
- **ICMP de découverte du MTU** (`destination-unreachable`, `time-exceeded`,
  `parameter-problem`) : accepté. Le jeter produit des connexions qui s'établissent puis se
  figent, symptôme très coûteux à diagnostiquer.

Les refus sont journalisés à débit limité : un refus doit se voir sans pouvoir remplir le
disque.

### Traduction d'adresses

Seules les zones ayant un flux déclaré vers `internet` ont une entrée de traduction. Une
zone sans ce flux n'en a pas : même si une route existait, ses paquets sortiraient avec une
adresse privée et ne reviendraient jamais. Deux barrières valent mieux qu'une pour une
promesse du genre « aucune donnée ne sort ».

## Publication de services

Une publication expose un port d'une VM sur l'interface de sortie de la machine. C'est ce
qui rend un service joignable depuis le réseau, par exemple le serveur web d'un événement.

```json
[{"vm": "web1", "port": 80, "host_port": 8080, "comment": "serveur web"}]
```

Deux règles sont produites : la redirection elle-même, dans une chaîne `prerouting`, et
l'autorisation du trafic redirigé à traverser vers la VM, dans `forward`.

### `ct status dnat` n'est pas décoratif

La règle de `forward` porte `ct status dnat`, et c'est une condition de sécurité, pas une
précision de style.

Sans elle, la règle accepte aussi un paquet venu de l'extérieur **visant directement
l'adresse privée de la VM**, sans passer par la redirection. Sur un réseau plat où
quelqu'un ajoute une route vers la plage privée, le service entier redeviendrait joignable
en contournant le port publié — et le pare-feu afficherait pourtant une configuration
correcte. Le trou a été trouvé par le test, pas par relecture.

### IPv4 seulement, et c'est délibéré

Les adresses IPv6 des zones sont des adresses locales uniques. Les publier par redirection
supposerait du NAT66, pratique déconseillée qui casse la traçabilité des connexions.
Publier correctement un service en IPv6 demande un préfixe routé depuis l'extérieur, ce que
la machine n'a pas aujourd'hui. Mieux vaut ne rien faire que faire quelque chose de bancal
sur ce terrain.

### Validation en deux temps

Ce qui ne dépend pas de l'état de la machine — syntaxe, bornes de ports, protocole,
doublons de port exposé, existence de la VM parmi celles déclarées — est vérifié **au
chargement de la configuration**, avant qu'un bridge ne soit posé ou qu'une VM ne démarre.
Le reste, qui a besoin des adresses attribuées, est vérifié à l'application du pare-feu.

Deux publications sur le même port exposé sont refusées : le résultat dépendrait de l'ordre
des règles, exactement le genre d'ambiguïté qui se découvre en production.

## Un piège IPv6 qui ne se voit pas

Le routage IPv6 se règle **par interface**. Une interface créée après l'écriture de
`net.ipv6.conf.all.forwarding` hérite de `default`, pas de `all`.

Conséquence observée : le routage IPv6 entre zones ne fonctionnait pas alors que l'IPv4
fonctionnait. Une panne dissymétrique de ce genre coûte cher à diagnostiquer, parce que
tout semble correct. Le réglage est donc posé sur chaque bridge au moment de sa création,
et `default/forwarding` est écrit en plus.
