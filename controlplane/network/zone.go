// Package network gère les zones réseau : un bridge par zone, une interface
// tap par micro-VM, et l'adressage des deux familles IP.
//
// Une zone est un groupe de VM qui partagent un réseau. Les zones ne sont pas
// codées en dur : l'utilisateur déclare celles qu'il veut. Les cinq du cahier
// des charges — gestion, publique, interne, données, IA — ne sont qu'une
// configuration d'exemple.
//
// Conséquence de ce choix : « cette zone n'a pas de sortie Internet » n'est pas
// une règle spéciale attachée à un nom de zone, c'est simplement l'absence de
// flux déclaré. Aucune zone n'a de comportement magique.
package network

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
)

// MaxZoneNameLen borne le nom d'une zone pour que le nom de bridge dérivé
// tienne dans la limite du noyau (IFNAMSIZ, 15 caractères utiles).
// « br- » occupe 3 caractères.
const MaxZoneNameLen = 12

var zoneNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Zone décrit un réseau de VM.
//
// Les deux familles d'adresses sont gérées. IPv6 est en service, pas ignoré :
// un IPv6 laissé sans configuration n'est pas neutre, il contourne les règles
// IPv4. Autant le piloter explicitement.
type Zone struct {
	Name string `json:"name"`

	// Bridge est le nom de l'interface sur l'hôte. Dérivé du nom de la zone
	// s'il n'est pas renseigné.
	Bridge string `json:"bridge,omitempty"`

	// IPv4 est le réseau de la zone en CIDR, par exemple 10.42.10.0/24.
	// La première adresse utilisable revient à l'hôte, qui sert de passerelle.
	IPv4 string `json:"ipv4"`

	// IPv6 est le réseau de la zone, en principe un /64 taillé dans le préfixe
	// local unique de l'installation. Vide, il est dérivé automatiquement.
	IPv6 string `json:"ipv6,omitempty"`

	// MTU vaut 1500 si laissé à zéro.
	MTU int `json:"mtu,omitempty"`

	// Comment est une note libre, reprise dans la configuration d'exemple.
	Comment string `json:"comment,omitempty"`
}

// BridgeName renvoie le nom d'interface de la zone.
func (z Zone) BridgeName() string {
	if z.Bridge != "" {
		return z.Bridge
	}
	return "br-" + z.Name
}

// MTUOrDefault renvoie le MTU effectif.
func (z Zone) MTUOrDefault() int {
	if z.MTU > 0 {
		return z.MTU
	}
	return 1500
}

// Validate refuse une zone incohérente avant de toucher au noyau.
func (z Zone) Validate() error {
	if !zoneNameRe.MatchString(z.Name) {
		return fmt.Errorf("nom de zone invalide %q : minuscules, chiffres et tirets, sans tiret en début ni en fin", z.Name)
	}
	if len(z.Name) > MaxZoneNameLen {
		return fmt.Errorf("nom de zone %q trop long : %d caractères pour %d au maximum, sinon le nom de bridge dépasse la limite du noyau",
			z.Name, len(z.Name), MaxZoneNameLen)
	}
	if len(z.BridgeName()) > 15 {
		return fmt.Errorf("nom de bridge %q trop long : 15 caractères au maximum (IFNAMSIZ)", z.BridgeName())
	}

	v4, err := parsePrefix(z.IPv4)
	if err != nil {
		return fmt.Errorf("zone %s : ipv4 %q invalide : %w", z.Name, z.IPv4, err)
	}
	if !v4.Addr().Is4() {
		return fmt.Errorf("zone %s : ipv4 %q n'est pas une adresse IPv4", z.Name, z.IPv4)
	}
	if v4.Bits() > 30 {
		return fmt.Errorf("zone %s : ipv4 %q trop étroit : /30 au maximum, sinon il ne reste pas d'adresse pour une VM", z.Name, z.IPv4)
	}

	if z.IPv6 != "" {
		v6, err := parsePrefix(z.IPv6)
		if err != nil {
			return fmt.Errorf("zone %s : ipv6 %q invalide : %w", z.Name, z.IPv6, err)
		}
		if !v6.Addr().Is6() || v6.Addr().Is4In6() {
			return fmt.Errorf("zone %s : ipv6 %q n'est pas une adresse IPv6", z.Name, z.IPv6)
		}
		if v6.Bits() > 120 {
			return fmt.Errorf("zone %s : ipv6 %q trop étroit : /120 au maximum", z.Name, z.IPv6)
		}
	}

	if z.MTU != 0 && (z.MTU < 576 || z.MTU > 9000) {
		return fmt.Errorf("zone %s : mtu %d hors des bornes raisonnables 576 à 9000", z.Name, z.MTU)
	}
	return nil
}

// parsePrefix accepte un CIDR et vérifie qu'il désigne bien un réseau, pas une
// adresse d'hôte. 10.42.10.1/24 est une erreur de saisie fréquente pour
// 10.42.10.0/24 ; la signaler vaut mieux que de la corriger en silence.
func parsePrefix(s string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(strings.TrimSpace(s))
	if err != nil {
		return netip.Prefix{}, err
	}
	if p.Masked() != p {
		return netip.Prefix{}, fmt.Errorf("%s désigne une adresse d'hôte, pas un réseau : écrire %s", s, p.Masked())
	}
	return p, nil
}

// HostIPv4 renvoie l'adresse de l'hôte dans la zone, qui sert de passerelle aux
// VM : la première adresse utilisable du réseau.
func (z Zone) HostIPv4() (netip.Addr, error) {
	p, err := parsePrefix(z.IPv4)
	if err != nil {
		return netip.Addr{}, err
	}
	return p.Addr().Next(), nil
}

// HostIPv6 renvoie l'adresse IPv6 de l'hôte dans la zone, ou une adresse vide
// si la zone n'a pas d'IPv6.
func (z Zone) HostIPv6() (netip.Addr, error) {
	if z.IPv6 == "" {
		return netip.Addr{}, nil
	}
	p, err := parsePrefix(z.IPv6)
	if err != nil {
		return netip.Addr{}, err
	}
	return p.Addr().Next(), nil
}

// IPv4Net renvoie le réseau IPv4 de la zone sous la forme attendue par netlink.
func (z Zone) IPv4Net() (*net.IPNet, error) {
	p, err := parsePrefix(z.IPv4)
	if err != nil {
		return nil, err
	}
	host, err := z.HostIPv4()
	if err != nil {
		return nil, err
	}
	return &net.IPNet{
		IP:   net.IP(host.AsSlice()),
		Mask: net.CIDRMask(p.Bits(), 32),
	}, nil
}

// IPv6Net renvoie le réseau IPv6 de la zone, ou nil si elle n'en a pas.
func (z Zone) IPv6Net() (*net.IPNet, error) {
	if z.IPv6 == "" {
		return nil, nil
	}
	p, err := parsePrefix(z.IPv6)
	if err != nil {
		return nil, err
	}
	host, err := z.HostIPv6()
	if err != nil {
		return nil, err
	}
	return &net.IPNet{
		IP:   net.IP(host.AsSlice()),
		Mask: net.CIDRMask(p.Bits(), 128),
	}, nil
}

// ValidateZones vérifie un ensemble de zones : noms uniques, bridges uniques,
// et aucun recouvrement d'adressage.
//
// Deux zones qui se recouvrent produiraient un conflit de routes, et le trafic
// disparaîtrait sans le moindre message d'erreur. C'est exactement le piège
// rencontré en Phase 1 avec deux bridges sur le même sous-réseau, et il coûte
// une heure de diagnostic à chaque fois. Autant refuser.
func ValidateZones(zones []Zone) error {
	seenName := map[string]bool{}
	seenBridge := map[string]string{}
	prefixes4 := map[string]netip.Prefix{}
	prefixes6 := map[string]netip.Prefix{}

	for _, z := range zones {
		if err := z.Validate(); err != nil {
			return err
		}
		if seenName[z.Name] {
			return fmt.Errorf("zone %s déclarée deux fois", z.Name)
		}
		seenName[z.Name] = true

		br := z.BridgeName()
		if other, dup := seenBridge[br]; dup {
			return fmt.Errorf("zones %s et %s partagent le même bridge %s", other, z.Name, br)
		}
		seenBridge[br] = z.Name

		p4, _ := parsePrefix(z.IPv4)
		for other, op := range prefixes4 {
			if p4.Overlaps(op) {
				return fmt.Errorf("les zones %s (%s) et %s (%s) se recouvrent en IPv4 : le trafic partirait vers la mauvaise interface sans aucune erreur",
					other, op, z.Name, p4)
			}
		}
		prefixes4[z.Name] = p4

		if z.IPv6 != "" {
			p6, _ := parsePrefix(z.IPv6)
			for other, op := range prefixes6 {
				if p6.Overlaps(op) {
					return fmt.Errorf("les zones %s (%s) et %s (%s) se recouvrent en IPv6",
						other, op, z.Name, p6)
				}
			}
			prefixes6[z.Name] = p6
		}
	}
	return nil
}
