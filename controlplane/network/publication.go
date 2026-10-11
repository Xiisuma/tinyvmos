package network

import (
	"fmt"
	"net/netip"
	"sort"
)

// Publication expose un port d'une VM sur l'interface de sortie de la machine.
//
// Ce n'est pas un flux entre zones, et c'est pour cela que les deux concepts
// sont séparés : un flux relie deux zones et se lit symétriquement, une
// publication désigne **une** VM précise et demande une redirection de port.
// Les mélanger aurait donné un type aux champs à moitié vides selon les cas.
type Publication struct {
	// VM est le nom de la micro-VM qui rend le service.
	VM string `json:"vm"`

	// Port est le port sur lequel la VM écoute.
	Port int `json:"port"`

	// HostPort est le port exposé à l'extérieur. Vide, il reprend Port.
	HostPort int `json:"host_port,omitempty"`

	// Proto vaut tcp ou udp. Vide, tcp.
	Proto string `json:"proto,omitempty"`

	Comment string `json:"comment,omitempty"`
}

// ProtoOrTCP renvoie le protocole effectif.
func (p Publication) ProtoOrTCP() string {
	if p.Proto == "" {
		return ProtoTCP
	}
	return p.Proto
}

// HostPortOrPort renvoie le port exposé.
func (p Publication) HostPortOrPort() int {
	if p.HostPort == 0 {
		return p.Port
	}
	return p.HostPort
}

// ValidateSyntax vérifie ce qui ne dépend pas de l'état de la machine.
//
// Séparé de Validate parce que ces contrôles peuvent se faire au chargement de
// la configuration, avant d'avoir touché à quoi que ce soit. Une configuration
// manifestement fausse doit être refusée avant qu'une VM ne démarre, pas après.
func (p Publication) ValidateSyntax() error {
	if p.VM == "" {
		return fmt.Errorf("publication incomplète : vm est obligatoire")
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("publication de %s : port %d hors bornes 1 à 65535", p.VM, p.Port)
	}
	if hp := p.HostPortOrPort(); hp < 1 || hp > 65535 {
		return fmt.Errorf("publication de %s : port d'exposition %d hors bornes 1 à 65535", p.VM, hp)
	}
	switch p.ProtoOrTCP() {
	case ProtoTCP, ProtoUDP:
	default:
		return fmt.Errorf("publication de %s : protocole %q invalide, tcp ou udp attendu", p.VM, p.Proto)
	}
	return nil
}

// ValidatePublicationsSyntax vérifie un ensemble sans consulter l'état :
// syntaxe de chaque entrée, et absence de doublon de port exposé.
//
// knownVMs, s'il est fourni, sert à refuser tout de suite une publication qui
// désigne une VM dont il n'est pas question. C'est le genre de faute de frappe
// qui, détectée trop tard, laisse un service non exposé sans que personne ne
// s'en aperçoive.
func ValidatePublicationsSyntax(pubs []Publication, knownVMs map[string]bool) error {
	taken := map[string]string{}
	for _, p := range pubs {
		if err := p.ValidateSyntax(); err != nil {
			return err
		}
		if knownVMs != nil && !knownVMs[p.VM] {
			return fmt.Errorf("publication de %s : cette vm n'est pas déclarée. La déclarer avant de publier un de ses services", p.VM)
		}
		key := fmt.Sprintf("%s/%d", p.ProtoOrTCP(), p.HostPortOrPort())
		if other, dup := taken[key]; dup {
			return fmt.Errorf("port exposé %s déjà pris par la publication de %s : deux redirections sur le même port donneraient un résultat dépendant de l'ordre des règles",
				key, other)
		}
		taken[key] = p.VM
	}
	return nil
}

// Validate refuse une publication incohérente.
//
// leases associe un nom de VM à son adressage : une publication vers une VM
// sans adresse attribuée ne pourrait produire aucune règle, et une règle
// manquante dans un pare-feu ne se voit pas.
func (p Publication) Validate(leases map[string]Lease, zones []Zone) error {
	if p.VM == "" {
		return fmt.Errorf("publication incomplète : vm est obligatoire")
	}
	if p.Port < 1 || p.Port > 65535 {
		return fmt.Errorf("publication de %s : port %d hors bornes 1 à 65535", p.VM, p.Port)
	}
	if hp := p.HostPortOrPort(); hp < 1 || hp > 65535 {
		return fmt.Errorf("publication de %s : port d'exposition %d hors bornes 1 à 65535", p.VM, hp)
	}
	switch p.ProtoOrTCP() {
	case ProtoTCP, ProtoUDP:
	default:
		return fmt.Errorf("publication de %s : protocole %q invalide, tcp ou udp attendu", p.VM, p.Proto)
	}

	lease, ok := leases[p.VM]
	if !ok {
		return fmt.Errorf("publication de %s : cette vm n'a pas d'adresse attribuée. La déclarer avant de publier un de ses services", p.VM)
	}
	if lease.IPv4 == "" {
		return fmt.Errorf("publication de %s : cette vm n'a pas d'adresse IPv4, et la redirection de port s'appuie dessus", p.VM)
	}
	if _, err := netip.ParsePrefix(lease.IPv4); err != nil {
		return fmt.Errorf("publication de %s : adresse %q illisible : %w", p.VM, lease.IPv4, err)
	}

	known := false
	for _, z := range zones {
		if z.Name == lease.Zone {
			known = true
			break
		}
	}
	if !known {
		return fmt.Errorf("publication de %s : sa zone %q n'est pas déclarée", p.VM, lease.Zone)
	}
	return nil
}

// ValidatePublications vérifie un ensemble et refuse deux publications sur le
// même port exposé.
//
// Deux redirections sur le même port produiraient un résultat dépendant de
// l'ordre des règles : le genre d'ambiguïté qui se découvre en production.
func ValidatePublications(pubs []Publication, leases map[string]Lease, zones []Zone) error {
	taken := map[string]string{}
	for _, p := range pubs {
		if err := p.Validate(leases, zones); err != nil {
			return err
		}
		key := fmt.Sprintf("%s/%d", p.ProtoOrTCP(), p.HostPortOrPort())
		if other, dup := taken[key]; dup {
			return fmt.Errorf("port exposé %s déjà pris par la publication de %s : deux redirections sur le même port donneraient un résultat dépendant de l'ordre des règles",
				key, other)
		}
		taken[key] = p.VM
	}
	return nil
}

// sortedPublications renvoie les publications triées, pour que le jeu de
// règles produit soit identique d'une exécution à l'autre.
func sortedPublications(pubs []Publication) []Publication {
	out := make([]Publication, len(pubs))
	copy(out, pubs)
	sort.Slice(out, func(i, j int) bool {
		if out[i].HostPortOrPort() != out[j].HostPortOrPort() {
			return out[i].HostPortOrPort() < out[j].HostPortOrPort()
		}
		return out[i].ProtoOrTCP() < out[j].ProtoOrTCP()
	})
	return out
}
