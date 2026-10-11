package network

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strings"

	"github.com/vishvananda/netlink"
)

// Manager pose et retire les zones et les interfaces tap sur l'hôte.
//
// Tout passe par netlink, et non par des appels à ip ou brctl : pas de sortie
// de commande à analyser, des erreurs typées, et rien à installer sur l'hôte.
type Manager struct {
	Log *slog.Logger

	// TapOwner et TapGroup sont l'uid et le gid auxquels appartiennent les
	// interfaces tap.
	//
	// Ce n'est pas un détail de confort. Le noyau n'autorise l'attachement à un
	// tap qu'au propriétaire déclaré, ou à un processus doté de CAP_NET_ADMIN.
	// Firecracker tourne confiné sous jailer, sans capacité : si le tap
	// appartient à root, il répond « Operation not permitted » et la VM ne
	// démarre pas. Il faut donc le donner à l'utilisateur confiné.
	TapOwner uint32
	TapGroup uint32
}

// NewManager renvoie un gestionnaire de réseau.
//
// tapOwner et tapGroup sont l'uid et le gid de l'utilisateur sous lequel
// tournent les VM : c'est lui qui ouvrira les interfaces tap.
func NewManager(log *slog.Logger, tapOwner, tapGroup uint32) *Manager {
	if log == nil {
		log = slog.Default()
	}
	return &Manager{Log: log, TapOwner: tapOwner, TapGroup: tapGroup}
}

// EnsureZone crée le bridge de la zone s'il manque, et pose son adressage.
//
// L'opération est idempotente : la relancer sur une zone déjà en place ne
// change rien. C'est ce qui permettra à la boucle de réconciliation du lot C
// de l'appeler à chaque passe sans réfléchir.
func (m *Manager) EnsureZone(z Zone) error {
	if err := z.Validate(); err != nil {
		return err
	}
	br := z.BridgeName()

	// Un réseau déjà routé par une autre interface ferait disparaître le
	// trafic sans erreur. C'est le piège rencontré en Phase 1 ; il est refusé
	// ici plutôt que découvert au diagnostic.
	if err := m.checkNoConflictingRoute(z); err != nil {
		return err
	}

	link, err := netlink.LinkByName(br)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return fmt.Errorf("zone %s : lecture du bridge %s : %w", z.Name, br, err)
		}
		attrs := netlink.NewLinkAttrs()
		attrs.Name = br
		attrs.MTU = z.MTUOrDefault()
		bridge := &netlink.Bridge{LinkAttrs: attrs}
		if err := netlink.LinkAdd(bridge); err != nil {
			return fmt.Errorf("zone %s : création du bridge %s : %w", z.Name, br, err)
		}
		m.Log.Info("bridge de zone créé", "zone", z.Name, "bridge", br, "mtu", z.MTUOrDefault())
		if link, err = netlink.LinkByName(br); err != nil {
			return fmt.Errorf("zone %s : relecture du bridge %s : %w", z.Name, br, err)
		}
	}

	if link.Attrs().MTU != z.MTUOrDefault() {
		if err := netlink.LinkSetMTU(link, z.MTUOrDefault()); err != nil {
			return fmt.Errorf("zone %s : mtu du bridge %s : %w", z.Name, br, err)
		}
	}

	v4, err := z.IPv4Net()
	if err != nil {
		return err
	}
	if err := m.ensureAddr(link, v4, z.Name); err != nil {
		return err
	}

	v6, err := z.IPv6Net()
	if err != nil {
		return err
	}
	if v6 != nil {
		// Sans cela, le noyau refuse de poser une adresse IPv6 sur un bridge
		// qui n'a pas encore de port actif, et l'adresse reste en « tentative ».
		if err := writeSysctl(fmt.Sprintf("net/ipv6/conf/%s/accept_dad", br), "0"); err != nil {
			m.Log.Warn("accept_dad non réglé", "zone", z.Name, "bridge", br, "erreur", err)
		}
		// L'hôte ne doit pas accepter d'annonce de routeur venant d'une VM :
		// ce serait un détournement de passerelle depuis l'intérieur d'une zone.
		for _, key := range []string{"accept_ra", "accept_redirects"} {
			if err := writeSysctl(fmt.Sprintf("net/ipv6/conf/%s/%s", br, key), "0"); err != nil {
				m.Log.Warn("sysctl IPv6 non réglé", "zone", z.Name, "clé", key, "erreur", err)
			}
		}
		if err := m.ensureAddr(link, v6, z.Name); err != nil {
			return err
		}
	}

	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			return fmt.Errorf("zone %s : activation du bridge %s : %w", z.Name, br, err)
		}
	}
	return nil
}

// ensureAddr pose une adresse si elle n'est pas déjà présente.
func (m *Manager) ensureAddr(link netlink.Link, ipnet *net.IPNet, zone string) error {
	family := netlink.FAMILY_V4
	if ipnet.IP.To4() == nil {
		family = netlink.FAMILY_V6
	}

	existing, err := netlink.AddrList(link, family)
	if err != nil {
		return fmt.Errorf("zone %s : lecture des adresses de %s : %w", zone, link.Attrs().Name, err)
	}
	for _, a := range existing {
		if a.IPNet != nil && a.IPNet.String() == ipnet.String() {
			return nil
		}
	}

	if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: ipnet}); err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("zone %s : ajout de %s sur %s : %w", zone, ipnet, link.Attrs().Name, err)
	}
	m.Log.Info("adresse de zone posée", "zone", zone, "interface", link.Attrs().Name, "adresse", ipnet.String())
	return nil
}

// checkNoConflictingRoute refuse une zone dont le réseau est déjà routé par une
// autre interface.
func (m *Manager) checkNoConflictingRoute(z Zone) error {
	br := z.BridgeName()

	for _, spec := range []struct {
		cidr   string
		family int
	}{
		{z.IPv4, netlink.FAMILY_V4},
		{z.IPv6, netlink.FAMILY_V6},
	} {
		if spec.cidr == "" {
			continue
		}
		p, err := parsePrefix(spec.cidr)
		if err != nil {
			return err
		}

		routes, err := netlink.RouteList(nil, spec.family)
		if err != nil {
			return fmt.Errorf("zone %s : lecture des routes : %w", z.Name, err)
		}
		for _, r := range routes {
			if r.Dst == nil || r.Dst.String() != p.String() {
				continue
			}
			if r.LinkIndex == 0 {
				continue
			}
			link, err := netlink.LinkByIndex(r.LinkIndex)
			if err != nil {
				continue
			}
			if link.Attrs().Name == br {
				continue
			}
			return fmt.Errorf("zone %s : le réseau %s est déjà routé par %s. Libérer cette interface ou donner un autre réseau à la zone : sinon le trafic partirait vers la mauvaise interface sans aucune erreur",
				z.Name, p, link.Attrs().Name)
		}
	}
	return nil
}

// RemoveZone retire le bridge d'une zone, à condition qu'aucune interface ne
// lui soit encore rattachée.
func (m *Manager) RemoveZone(z Zone) error {
	br := z.BridgeName()
	link, err := netlink.LinkByName(br)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("zone %s : lecture du bridge %s : %w", z.Name, br, err)
	}

	members, err := m.zoneMembers(link.Attrs().Index)
	if err != nil {
		return err
	}
	if len(members) > 0 {
		return fmt.Errorf("zone %s : le bridge %s porte encore %d interface(s) : %s",
			z.Name, br, len(members), strings.Join(members, ", "))
	}

	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("zone %s : suppression du bridge %s : %w", z.Name, br, err)
	}
	m.Log.Info("bridge de zone supprimé", "zone", z.Name, "bridge", br)
	return nil
}

// zoneMembers liste les interfaces rattachées à un bridge.
func (m *Manager) zoneMembers(bridgeIndex int) ([]string, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("lecture des interfaces : %w", err)
	}
	var out []string
	for _, l := range links {
		if l.Attrs().MasterIndex == bridgeIndex {
			out = append(out, l.Attrs().Name)
		}
	}
	return out, nil
}

// TapName renvoie le nom de l'interface tap d'une VM.
//
// Le nom doit tenir dans la limite du noyau, donc un nom de VM long est
// tronqué. La troncature garde le début, qui est la partie lisible.
func TapName(vmName string) string {
	name := "tap-" + vmName
	if len(name) > 15 {
		name = name[:15]
	}
	return name
}

// EnsureTap crée l'interface tap d'une VM et la rattache au bridge de sa zone.
//
// Idempotent, comme EnsureZone.
func (m *Manager) EnsureTap(z Zone, vmName string) (string, error) {
	tap := TapName(vmName)
	br := z.BridgeName()

	bridge, err := netlink.LinkByName(br)
	if err != nil {
		return "", fmt.Errorf("vm %s : bridge %s de la zone %s absent : créer la zone d'abord (%w)", vmName, br, z.Name, err)
	}

	link, err := netlink.LinkByName(tap)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if !errors.As(err, &notFound) {
			return "", fmt.Errorf("vm %s : lecture de %s : %w", vmName, tap, err)
		}
		attrs := netlink.NewLinkAttrs()
		attrs.Name = tap
		attrs.MTU = z.MTUOrDefault()
		dev := &netlink.Tuntap{
			LinkAttrs: attrs,
			Mode:      netlink.TUNTAP_MODE_TAP,
			// Sans propriétaire explicite, le tap appartient à root et
			// Firecracker, confiné sous jailer et sans CAP_NET_ADMIN, ne peut
			// pas s'y attacher.
			Owner: m.TapOwner,
			Group: m.TapGroup,
		}
		if err := netlink.LinkAdd(dev); err != nil {
			return "", fmt.Errorf("vm %s : création de %s : %w", vmName, tap, err)
		}
		m.Log.Info("interface tap créée",
			"vm", vmName, "tap", tap, "zone", z.Name,
			"propriétaire", m.TapOwner, "groupe", m.TapGroup)
		if link, err = netlink.LinkByName(tap); err != nil {
			return "", fmt.Errorf("vm %s : relecture de %s : %w", vmName, tap, err)
		}
	}

	if link.Attrs().MasterIndex != bridge.Attrs().Index {
		if err := netlink.LinkSetMaster(link, bridge); err != nil {
			return "", fmt.Errorf("vm %s : rattachement de %s au bridge %s : %w", vmName, tap, br, err)
		}
	}

	// Une interface tap ne doit jamais accepter d'annonce de routeur ni poser
	// d'adresse automatique côté hôte : c'est l'invité qui s'adresse, et une
	// VM ne doit pas pouvoir annoncer une passerelle à l'hôte.
	for _, key := range []string{"accept_ra", "accept_redirects", "autoconf"} {
		if err := writeSysctl(fmt.Sprintf("net/ipv6/conf/%s/%s", tap, key), "0"); err != nil {
			m.Log.Debug("sysctl IPv6 non réglé sur le tap", "tap", tap, "clé", key, "erreur", err)
		}
	}

	if link.Attrs().Flags&net.FlagUp == 0 {
		if err := netlink.LinkSetUp(link); err != nil {
			return "", fmt.Errorf("vm %s : activation de %s : %w", vmName, tap, err)
		}
	}
	return tap, nil
}

// RemoveTap retire l'interface tap d'une VM.
func (m *Manager) RemoveTap(vmName string) error {
	tap := TapName(vmName)
	link, err := netlink.LinkByName(tap)
	if err != nil {
		var notFound netlink.LinkNotFoundError
		if errors.As(err, &notFound) {
			return nil
		}
		return fmt.Errorf("vm %s : lecture de %s : %w", vmName, tap, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("vm %s : suppression de %s : %w", vmName, tap, err)
	}
	m.Log.Info("interface tap supprimée", "vm", vmName, "tap", tap)
	return nil
}

// EnableForwarding active le routage IP sur l'hôte, pour les deux familles.
//
// Le routage seul n'ouvre rien : la politique du pare-feu reste « tout
// refuser », et seuls les flux déclarés passent. Sans routage, en revanche,
// aucun flux déclaré ne pourrait fonctionner entre deux zones.
func (m *Manager) EnableForwarding() error {
	for _, key := range []string{"net/ipv4/ip_forward", "net/ipv6/conf/all/forwarding"} {
		if err := writeSysctl(key, "1"); err != nil {
			return fmt.Errorf("activation du routage (%s) : %w", key, err)
		}
	}
	// L'hôte ne doit pas se laisser reconfigurer par une annonce venue d'une
	// zone. Explicite, parce que le défaut varie selon les distributions.
	for _, key := range []string{"net/ipv6/conf/all/accept_ra", "net/ipv6/conf/all/accept_redirects"} {
		if err := writeSysctl(key, "0"); err != nil {
			m.Log.Warn("sysctl IPv6 global non réglé", "clé", key, "erreur", err)
		}
	}
	m.Log.Info("routage IP activé", "ipv4", true, "ipv6", true)
	return nil
}

func writeSysctl(key, value string) error {
	path := "/proc/sys/" + key
	return os.WriteFile(path, []byte(value), 0o644)
}
