# web-apache

Apache2 seul, sur Alpine minimal. Première image du catalogue : elle sert de référence
aux suivantes et c'est elle qui valide la chaîne complète en Phase 1.

## Construction

```bash
sudo ./images/build-image.sh web-apache
```

Produit `output/images/web-apache.ext4` et son fichier de métadonnées `.json`
(empreinte SHA-256, occupation réelle, budget mémoire déclaré).

## Lancement

```bash
sudo ./build/net-setup.sh up
sudo ./build/run-firecracker.sh --name web1 --service web-apache --jailer
curl http://10.42.10.2/
sudo ./build/run-firecracker.sh --name web1 --stop
sudo ./build/net-setup.sh down
```

## Mesures

Relevées sur la machine de développement, image du 2026-09-21 :

| Grandeur | Valeur |
|---|---|
| Occupation du rootfs | 14 Mio |
| Taille de l'image | 64 Mio |
| Budget mémoire déclaré | 256 Mio |
| Démarrage jusqu'à la première réponse HTTP | environ 2,9 s |

Les 2,9 secondes ne viennent pas de Firecracker, qui démarre le noyau en une centaine
de millisecondes, mais d'OpenRC puis d'Apache. C'est le poste à réduire si le temps de
démarrage devient un critère.

**La machine cible est plus faible que la machine de développement.** Ces valeurs seront
reprises sur la cible avant d'être considérées comme acquises.

## Durcissement

- `ServerTokens Prod` et `ServerSignature Off` : l'en-tête `Server` ne divulgue ni la
  version d'Apache, ni le système. Cette image finira en zone Publique face à des
  joueurs de CTF, à qui il est inutile d'offrir gratuitement la surface.
- `TraceEnable Off`.
- Modules `autoindex`, `status`, `info`, `userdir` et `cgi` désactivés : moins de code
  chargé, moins de surface.
- Le compte `root` de l'image est verrouillé. Le seul accès est la console série, qui
  n'est joignable que depuis l'hôte.

## Limites connues

- Apache écoute en clair sur le port 80. La terminaison TLS reviendra au `reverse-proxy`,
  pas à cette image.
- L'adressage est statique, passé par la ligne de commande du noyau. La découverte par
  DHCP arrive en Phase 2 avec la VM `dhcp-dns`.
