# web

Interface web servie par le démon et embarquée dans son binaire : un seul artefact à déployer.

PWA responsive. Tableau de bord complet sur PC, onglets et grands boutons tactiles sur
mobile et tablette. Le client API est généré depuis `controlplane/api/openapi.yaml`,
jamais écrit à la main.

C'est aussi cette interface qui s'affiche en plein écran dans le kiosque local
(Cage plus navigateur) quand un écran est détecté au démarrage. Le kiosque ne doit
permettre ni de sortir vers un shell, ni de naviguer vers une autre adresse.
