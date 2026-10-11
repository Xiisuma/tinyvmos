package network

import (
	"crypto/rand"
	"fmt"
	"net/netip"
)

// ULAPrefixBits est la taille du préfixe local unique tiré pour une
// installation : un /48, dans lequel chaque zone reçoit un /64.
const ULAPrefixBits = 48

// ZonePrefixBits est la taille du réseau IPv6 attribué à une zone.
//
// Un /64 n'est pas un choix de confort : c'est la taille que supposent la
// découverte de voisins et l'autoconfiguration d'adresse. Un préfixe plus
// étroit casse des comportements IPv6 standard de l'invité.
const ZonePrefixBits = 64

// GenerateULAPrefix tire un préfixe local unique /48 au hasard, conformément à
// la RFC 4193 : premier octet 0xfd, puis 40 bits aléatoires.
//
// Le tirage au sort est voulu. Un préfixe en dur serait identique sur toutes
// les installations, et deux machines TinyVMOS reliées un jour par un tunnel
// entreraient en collision. Il est tiré une fois puis conservé dans l'état :
// un préfixe qui changerait à chaque démarrage invaliderait toutes les adresses
// déjà attribuées.
func GenerateULAPrefix() (netip.Prefix, error) {
	var b [6]byte
	if _, err := rand.Read(b[1:]); err != nil {
		return netip.Prefix{}, fmt.Errorf("tirage du préfixe local unique : %w", err)
	}
	b[0] = 0xfd

	var addr [16]byte
	copy(addr[:], b[:])

	return netip.PrefixFrom(netip.AddrFrom16(addr), ULAPrefixBits), nil
}

// ZoneIPv6 taille un /64 dans le préfixe de l'installation pour la zone
// d'indice donné.
//
// L'indice occupe les 16 bits entre le /48 et le /64, ce qui laisse 65 536
// zones : largement assez, et la lecture d'une adresse donne directement le
// numéro de zone.
func ZoneIPv6(ula netip.Prefix, index int) (netip.Prefix, error) {
	if ula.Bits() != ULAPrefixBits {
		return netip.Prefix{}, fmt.Errorf("préfixe d'installation attendu en /%d, reçu /%d", ULAPrefixBits, ula.Bits())
	}
	if !ula.Addr().Is6() {
		return netip.Prefix{}, fmt.Errorf("préfixe d'installation %s n'est pas IPv6", ula)
	}
	if index < 0 || index > 0xffff {
		return netip.Prefix{}, fmt.Errorf("indice de zone %d hors bornes : 0 à 65535", index)
	}

	addr := ula.Addr().As16()
	addr[6] = byte(index >> 8)
	addr[7] = byte(index)
	// Les 64 bits de poids faible appartiennent aux hôtes de la zone.
	for i := 8; i < 16; i++ {
		addr[i] = 0
	}

	return netip.PrefixFrom(netip.AddrFrom16(addr), ZonePrefixBits), nil
}
