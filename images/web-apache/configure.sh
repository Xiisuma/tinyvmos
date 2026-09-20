#!/bin/sh
#
# Configuration d'Apache dans le chroot de l'image web-apache.
# Exécuté par images/build-image.sh, jamais directement.

set -eu

# Apache écoute sur toutes les interfaces de la VM. L'isolation est assurée
# par les zones et nftables sur l'hôte, pas par une écoute restreinte ici.
sed -i 's/^Listen .*/Listen 80/' /etc/apache2/httpd.conf

# Ne pas divulguer la version ni le système : information gratuite offerte à
# un attaquant, et cette image finira en zone Publique face à des joueurs de CTF.
sed -i 's/^ServerTokens .*/ServerTokens Prod/' /etc/apache2/conf.d/default.conf 2>/dev/null || true
{
    echo "ServerTokens Prod"
    echo "ServerSignature Off"
    echo "TraceEnable Off"
    echo "ServerName tinyvmos.local"
} >> /etc/apache2/httpd.conf

# Retirer les modules inutiles : moins de code chargé, moins de surface.
for mod in autoindex_module status_module info_module userdir_module cgi_module; do
    sed -i "s|^LoadModule ${mod}|#LoadModule ${mod}|" /etc/apache2/httpd.conf
done

# Apache tourne en httpd:httpd, jamais en root, hors du socket d'écoute.
grep -q '^User apache' /etc/apache2/httpd.conf || true

rm -rf /var/cache/apk/* /tmp/* 2>/dev/null || true

exit 0
