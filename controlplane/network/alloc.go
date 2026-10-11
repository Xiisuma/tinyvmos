package network

import (
	"fmt"
	"net/netip"
	"sort"
)

// Lease est l'adressage attribué à une VM dans une zone.
type Lease struct {
	Zone string `json:"zone"`
	VM   string `json:"vm"`

	// IPv4 et IPv6 sont notées en CIDR, prêtes à être posées sur l'interface
	// de l'invité.
	IPv4 string `json:"ipv4"`
	IPv6 string `json:"ipv6,omitempty"`

	// GatewayV4 et GatewayV6 sont les adresses de l'hôte dans la zone.
	GatewayV4 string `json:"gateway_v4"`
	GatewayV6 string `json:"gateway_v6,omitempty"`
}

// Allocation est l'ensemble des attributions, persisté avec l'état du démon.
//
// L'attribution est enregistrée plutôt que recalculée : une adresse dérivée
// d'un hachage du nom entrerait en collision tôt ou tard, et silencieusement.
// Une VM doit aussi retrouver la même adresse après un redémarrage du démon,
// sans quoi les règles de pare-feu et les baux ne voudraient plus rien dire.
type Allocation struct {
	// ULAPrefix est le préfixe local unique de l'installation, tiré une seule
	// fois. Il ne doit jamais changer : toutes les adresses IPv6 en dépendent.
	ULAPrefix string `json:"ula_prefix"`

	// ZoneIndex associe un indice stable à chaque zone, qui détermine son /64.
	ZoneIndex map[string]int `json:"zone_index"`

	// Leases est indexé par nom de VM.
	Leases map[string]*Lease `json:"leases"`
}

// NewAllocation renvoie une attribution vide, avec un préfixe fraîchement tiré.
func NewAllocation() (*Allocation, error) {
	ula, err := GenerateULAPrefix()
	if err != nil {
		return nil, err
	}
	return &Allocation{
		ULAPrefix: ula.String(),
		ZoneIndex: map[string]int{},
		Leases:    map[string]*Lease{},
	}, nil
}

// ensure complète une attribution relue d'un état ancien ou partiel.
func (a *Allocation) ensure() error {
	if a.ZoneIndex == nil {
		a.ZoneIndex = map[string]int{}
	}
	if a.Leases == nil {
		a.Leases = map[string]*Lease{}
	}
	if a.ULAPrefix == "" {
		ula, err := GenerateULAPrefix()
		if err != nil {
			return err
		}
		a.ULAPrefix = ula.String()
	}
	return nil
}

// ULA renvoie le préfixe local unique de l'installation.
func (a *Allocation) ULA() (netip.Prefix, error) {
	if err := a.ensure(); err != nil {
		return netip.Prefix{}, err
	}
	p, err := netip.ParsePrefix(a.ULAPrefix)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("préfixe local unique %q illisible : %w", a.ULAPrefix, err)
	}
	return p, nil
}

// IndexForZone renvoie l'indice stable d'une zone, en en attribuant un au
// besoin. Le plus petit indice libre est retenu, pour que les numéros restent
// compacts et lisibles.
func (a *Allocation) IndexForZone(zone string) (int, error) {
	if err := a.ensure(); err != nil {
		return 0, err
	}
	if idx, ok := a.ZoneIndex[zone]; ok {
		return idx, nil
	}

	used := map[int]bool{}
	for _, idx := range a.ZoneIndex {
		used[idx] = true
	}
	// L'indice 0 est réservé : un /64 en ...:0000:: se confond visuellement
	// avec le préfixe de l'installation lui-même.
	for idx := 1; idx <= 0xffff; idx++ {
		if !used[idx] {
			a.ZoneIndex[zone] = idx
			return idx, nil
		}
	}
	return 0, fmt.Errorf("plus d'indice de zone disponible : 65535 zones déclarées")
}

// ResolveZoneIPv6 complète l'IPv6 d'une zone qui n'en déclare pas, en taillant
// un /64 dans le préfixe de l'installation.
func (a *Allocation) ResolveZoneIPv6(z *Zone) error {
	if z.IPv6 != "" {
		return nil
	}
	ula, err := a.ULA()
	if err != nil {
		return err
	}
	idx, err := a.IndexForZone(z.Name)
	if err != nil {
		return err
	}
	p, err := ZoneIPv6(ula, idx)
	if err != nil {
		return err
	}
	z.IPv6 = p.String()
	return nil
}

// Allocate attribue une adresse à une VM dans une zone, ou renvoie celle
// qu'elle possède déjà.
//
// La plus petite adresse libre est retenue. L'adresse de l'hôte, qui sert de
// passerelle, est sautée, de même que l'adresse de diffusion en IPv4.
func (a *Allocation) Allocate(z Zone, vmName string) (*Lease, error) {
	if err := a.ensure(); err != nil {
		return nil, err
	}
	if existing, ok := a.Leases[vmName]; ok {
		if existing.Zone != z.Name {
			return nil, fmt.Errorf("vm %s déjà adressée dans la zone %s : la déplacer demande de la supprimer d'abord",
				vmName, existing.Zone)
		}
		return existing, nil
	}

	p4, err := parsePrefix(z.IPv4)
	if err != nil {
		return nil, err
	}
	hostV4, err := z.HostIPv4()
	if err != nil {
		return nil, err
	}

	taken := map[netip.Addr]bool{hostV4: true}
	for _, l := range a.Leases {
		if l.Zone != z.Name {
			continue
		}
		if pfx, err := netip.ParsePrefix(l.IPv4); err == nil {
			taken[pfx.Addr()] = true
		}
	}

	addr4, err := nextFreeIPv4(p4, hostV4, taken)
	if err != nil {
		return nil, fmt.Errorf("zone %s : %w", z.Name, err)
	}

	lease := &Lease{
		Zone:      z.Name,
		VM:        vmName,
		IPv4:      netip.PrefixFrom(addr4, p4.Bits()).String(),
		GatewayV4: hostV4.String(),
	}

	if z.IPv6 != "" {
		p6, err := parsePrefix(z.IPv6)
		if err != nil {
			return nil, err
		}
		hostV6, err := z.HostIPv6()
		if err != nil {
			return nil, err
		}

		// L'identifiant d'interface IPv6 reprend le dernier octet de l'adresse
		// IPv4 : les deux adresses d'une même VM se lisent ainsi du même coup
		// d'œil, ce qui compte quand on diagnostique un pare-feu à deux familles.
		v4bytes := addr4.As4()
		addr6, err := ipv6WithHostID(p6, uint64(v4bytes[3]))
		if err != nil {
			return nil, fmt.Errorf("zone %s : %w", z.Name, err)
		}

		lease.IPv6 = netip.PrefixFrom(addr6, p6.Bits()).String()
		lease.GatewayV6 = hostV6.String()
	}

	a.Leases[vmName] = lease
	return lease, nil
}

// Release libère l'adresse d'une VM.
func (a *Allocation) Release(vmName string) {
	if a.Leases != nil {
		delete(a.Leases, vmName)
	}
}

// LeasesInZone renvoie les attributions d'une zone, triées par adresse IPv4.
func (a *Allocation) LeasesInZone(zone string) []Lease {
	out := []Lease{}
	for _, l := range a.Leases {
		if l.Zone == zone {
			out = append(out, *l)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ai, erri := netip.ParsePrefix(out[i].IPv4)
		aj, errj := netip.ParsePrefix(out[j].IPv4)
		if erri != nil || errj != nil {
			return out[i].VM < out[j].VM
		}
		return ai.Addr().Less(aj.Addr())
	})
	return out
}

// nextFreeIPv4 cherche la plus petite adresse libre du réseau, en évitant
// l'adresse de réseau, celle de l'hôte et celle de diffusion.
func nextFreeIPv4(p netip.Prefix, host netip.Addr, taken map[netip.Addr]bool) (netip.Addr, error) {
	broadcast := ipv4Broadcast(p)

	addr := p.Addr().Next() // on saute l'adresse de réseau
	for p.Contains(addr) {
		if addr != broadcast && addr != host && !taken[addr] {
			return addr, nil
		}
		addr = addr.Next()
	}
	return netip.Addr{}, fmt.Errorf("plus d'adresse IPv4 libre dans %s", p)
}

// ipv4Broadcast renvoie l'adresse de diffusion d'un réseau IPv4.
func ipv4Broadcast(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	mask := ^uint32(0) >> uint(p.Bits())
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	v |= mask
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// ipv6WithHostID pose un identifiant d'hôte dans un préfixe IPv6.
func ipv6WithHostID(p netip.Prefix, id uint64) (netip.Addr, error) {
	if id == 0 {
		return netip.Addr{}, fmt.Errorf("identifiant d'hôte IPv6 nul : l'adressse serait celle du réseau")
	}
	addr := p.Addr().As16()
	for i := 0; i < 8; i++ {
		addr[15-i] |= byte(id >> (8 * i))
	}
	out := netip.AddrFrom16(addr)
	if !p.Contains(out) {
		return netip.Addr{}, fmt.Errorf("identifiant d'hôte %d hors du préfixe %s", id, p)
	}
	return out, nil
}
