package network

// ExampleZones renvoie les cinq zones du cahier des charges, en configuration
// de départ.
//
// Elles n'ont rien de spécial : ce sont des zones ordinaires, que l'utilisateur
// peut renommer, supprimer ou compléter. Aucune n'a de règle cachée dans le
// code. « La zone IA n'a pas de sortie Internet » n'est pas une propriété de la
// zone IA, c'est simplement qu'aucun flux ne la relie à l'extérieur.
//
// L'IPv6 est laissé vide : un /64 est taillé pour chaque zone dans le préfixe
// local unique tiré au premier démarrage de l'installation. Mettre un préfixe
// en dur ici le rendrait identique sur toutes les machines TinyVMOS.
func ExampleZones() []Zone {
	return []Zone{
		{
			Name:    "gestion",
			IPv4:    "10.42.0.0/24",
			Comment: "Plan de contrôle et accès distant WireGuard.",
		},
		{
			Name:    "publique",
			IPv4:    "10.42.10.0/24",
			Comment: "Services exposés, derrière le reverse proxy. À considérer comme compromise.",
		},
		{
			Name:    "interne",
			IPv4:    "10.42.20.0/24",
			Comment: "Services d'infrastructure : DHCP, DNS, SSH, FTP.",
		},
		{
			Name:    "donnees",
			IPv4:    "10.42.30.0/24",
			Comment: "Coffres de données. Aucun flux vers l'extérieur n'est déclaré.",
		},
		{
			Name:    "ia",
			IPv4:    "10.42.40.0/24",
			Comment: "Moteur et clients d'IA, hors ligne. Aucun flux vers l'extérieur n'est déclaré.",
		},
	}
}
