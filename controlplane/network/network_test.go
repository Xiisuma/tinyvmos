package network

import (
	"net/netip"
	"strings"
	"testing"
)

func TestZoneValidate(t *testing.T) {
	base := Zone{Name: "interne", IPv4: "10.42.20.0/24", IPv6: "fd42:1::/64"}
	if err := base.Validate(); err != nil {
		t.Fatalf("zone valide refusée : %v", err)
	}

	cases := []struct {
		nom     string
		modifie func(*Zone)
		attendu string
	}{
		{"nom vide", func(z *Zone) { z.Name = "" }, "nom de zone invalide"},
		{"nom en majuscules", func(z *Zone) { z.Name = "Interne" }, "nom de zone invalide"},
		{"nom trop long", func(z *Zone) { z.Name = "zone-beaucoup-trop-longue" }, "trop long"},
		{"ipv4 absent", func(z *Zone) { z.IPv4 = "" }, "ipv4"},
		{"ipv4 en adresse d'hôte", func(z *Zone) { z.IPv4 = "10.42.20.1/24" }, "adresse d'hôte"},
		{"ipv4 trop étroit", func(z *Zone) { z.IPv4 = "10.42.20.0/31" }, "trop étroit"},
		{"ipv6 en adresse d'hôte", func(z *Zone) { z.IPv6 = "fd42:1::1/64" }, "adresse d'hôte"},
		{"ipv6 qui est de l'ipv4", func(z *Zone) { z.IPv6 = "10.0.0.0/8" }, "n'est pas une adresse IPv6"},
		{"mtu absurde", func(z *Zone) { z.MTU = 42 }, "mtu"},
	}

	for _, tc := range cases {
		t.Run(tc.nom, func(t *testing.T) {
			z := base
			tc.modifie(&z)
			err := z.Validate()
			if err == nil {
				t.Fatal("zone invalide acceptée")
			}
			if !strings.Contains(err.Error(), tc.attendu) {
				t.Fatalf("message attendu contenant %q, obtenu %q", tc.attendu, err)
			}
		})
	}
}

// TestValidateZonesRefuseRecouvrement protège du piège rencontré en Phase 1 :
// deux bridges sur le même réseau font disparaître le trafic sans erreur.
func TestValidateZonesRefuseRecouvrement(t *testing.T) {
	cases := []struct {
		nom     string
		zones   []Zone
		attendu string
	}{
		{
			"recouvrement ipv4 exact",
			[]Zone{{Name: "a", IPv4: "10.42.10.0/24"}, {Name: "b", IPv4: "10.42.10.0/24"}},
			"se recouvrent en IPv4",
		},
		{
			"recouvrement ipv4 par inclusion",
			[]Zone{{Name: "a", IPv4: "10.42.0.0/16"}, {Name: "b", IPv4: "10.42.10.0/24"}},
			"se recouvrent en IPv4",
		},
		{
			"recouvrement ipv6",
			[]Zone{
				{Name: "a", IPv4: "10.42.10.0/24", IPv6: "fd42:1::/64"},
				{Name: "b", IPv4: "10.42.20.0/24", IPv6: "fd42:1::/64"},
			},
			"se recouvrent en IPv6",
		},
		{
			"nom dupliqué",
			[]Zone{{Name: "a", IPv4: "10.42.10.0/24"}, {Name: "a", IPv4: "10.42.20.0/24"}},
			"déclarée deux fois",
		},
		{
			"bridge dupliqué",
			[]Zone{
				{Name: "a", Bridge: "br-commun", IPv4: "10.42.10.0/24"},
				{Name: "b", Bridge: "br-commun", IPv4: "10.42.20.0/24"},
			},
			"partagent le même bridge",
		},
	}

	for _, tc := range cases {
		t.Run(tc.nom, func(t *testing.T) {
			err := ValidateZones(tc.zones)
			if err == nil {
				t.Fatal("configuration incohérente acceptée")
			}
			if !strings.Contains(err.Error(), tc.attendu) {
				t.Fatalf("message attendu contenant %q, obtenu %q", tc.attendu, err)
			}
		})
	}
}

func TestExampleZonesValides(t *testing.T) {
	zones := ExampleZones()
	if len(zones) != 5 {
		t.Fatalf("5 zones d'exemple attendues, obtenu %d", len(zones))
	}
	if err := ValidateZones(zones); err != nil {
		t.Fatalf("les zones d'exemple devraient être valides : %v", err)
	}
	for _, z := range zones {
		if z.Comment == "" {
			t.Errorf("zone %s sans commentaire : l'exemple doit s'expliquer", z.Name)
		}
		if z.IPv6 != "" {
			t.Errorf("zone %s porte un IPv6 en dur : il doit être taillé dans le préfixe tiré à l'installation", z.Name)
		}
	}
}

func TestZoneAdresseHote(t *testing.T) {
	z := Zone{Name: "interne", IPv4: "10.42.20.0/24", IPv6: "fd42:14::/64"}

	v4, err := z.HostIPv4()
	if err != nil {
		t.Fatalf("HostIPv4 : %v", err)
	}
	if v4.String() != "10.42.20.1" {
		t.Fatalf("passerelle IPv4 attendue 10.42.20.1, obtenue %s", v4)
	}

	v6, err := z.HostIPv6()
	if err != nil {
		t.Fatalf("HostIPv6 : %v", err)
	}
	if v6.String() != "fd42:14::1" {
		t.Fatalf("passerelle IPv6 attendue fd42:14::1, obtenue %s", v6)
	}
}

// TestULAPrefixConforme vérifie la conformité RFC 4193 et le tirage au sort.
func TestULAPrefixConforme(t *testing.T) {
	p1, err := GenerateULAPrefix()
	if err != nil {
		t.Fatalf("GenerateULAPrefix : %v", err)
	}
	if p1.Bits() != ULAPrefixBits {
		t.Fatalf("/%d attendu, obtenu /%d", ULAPrefixBits, p1.Bits())
	}
	if b := p1.Addr().As16(); b[0] != 0xfd {
		t.Fatalf("premier octet 0xfd attendu (RFC 4193), obtenu %#x", b[0])
	}
	if p1.Masked() != p1 {
		t.Fatalf("%s devrait être un préfixe réseau", p1)
	}

	p2, err := GenerateULAPrefix()
	if err != nil {
		t.Fatalf("GenerateULAPrefix : %v", err)
	}
	if p1 == p2 {
		t.Fatal("deux tirages identiques : le préfixe doit être aléatoire, sinon deux installations entreraient en collision")
	}
}

func TestZoneIPv6Decoupe(t *testing.T) {
	ula := netip.MustParsePrefix("fd12:3456:789a::/48")

	p1, err := ZoneIPv6(ula, 1)
	if err != nil {
		t.Fatalf("ZoneIPv6 : %v", err)
	}
	if p1.Bits() != ZonePrefixBits {
		t.Fatalf("/%d attendu, obtenu /%d", ZonePrefixBits, p1.Bits())
	}
	if p1.String() != "fd12:3456:789a:1::/64" {
		t.Fatalf("fd12:3456:789a:1::/64 attendu, obtenu %s", p1)
	}

	p2, _ := ZoneIPv6(ula, 2)
	if p1.Overlaps(p2) {
		t.Fatal("deux zones différentes ne doivent pas se recouvrir")
	}

	if _, err := ZoneIPv6(ula, 0x10000); err == nil {
		t.Fatal("un indice hors bornes devrait être refusé")
	}
	if _, err := ZoneIPv6(netip.MustParsePrefix("fd00::/32"), 1); err == nil {
		t.Fatal("un préfixe d'installation qui n'est pas un /48 devrait être refusé")
	}
}

func TestAllocationIndicesStables(t *testing.T) {
	a, err := NewAllocation()
	if err != nil {
		t.Fatalf("NewAllocation : %v", err)
	}

	i1, _ := a.IndexForZone("publique")
	i2, _ := a.IndexForZone("interne")
	i1bis, _ := a.IndexForZone("publique")

	if i1 != i1bis {
		t.Fatalf("l'indice d'une zone doit rester stable : %d puis %d", i1, i1bis)
	}
	if i1 == i2 {
		t.Fatalf("deux zones ne doivent pas partager un indice (%d)", i1)
	}
	if i1 == 0 || i2 == 0 {
		t.Fatal("l'indice 0 est réservé")
	}
}

func TestAllocationAdressage(t *testing.T) {
	a, err := NewAllocation()
	if err != nil {
		t.Fatalf("NewAllocation : %v", err)
	}
	z := Zone{Name: "publique", IPv4: "10.42.10.0/24"}
	if err := a.ResolveZoneIPv6(&z); err != nil {
		t.Fatalf("ResolveZoneIPv6 : %v", err)
	}
	if z.IPv6 == "" {
		t.Fatal("la zone devrait avoir reçu un IPv6")
	}
	if err := z.Validate(); err != nil {
		t.Fatalf("la zone complétée devrait être valide : %v", err)
	}

	l1, err := a.Allocate(z, "web1")
	if err != nil {
		t.Fatalf("Allocate : %v", err)
	}
	// 10.42.10.1 est l'hôte : la première VM prend la suivante.
	if l1.IPv4 != "10.42.10.2/24" {
		t.Fatalf("10.42.10.2/24 attendu, obtenu %s", l1.IPv4)
	}
	if l1.GatewayV4 != "10.42.10.1" {
		t.Fatalf("passerelle 10.42.10.1 attendue, obtenue %s", l1.GatewayV4)
	}
	if l1.IPv6 == "" || l1.GatewayV6 == "" {
		t.Fatalf("adressage IPv6 attendu, obtenu %+v", l1)
	}

	// L'identifiant IPv6 reprend le dernier octet de l'IPv4, pour que les deux
	// adresses d'une VM se lisent du même coup d'œil.
	if !strings.HasSuffix(l1.IPv6, "::2/64") {
		t.Fatalf("l'IPv6 devrait finir en ::2/64 pour refléter .2 en IPv4, obtenu %s", l1.IPv6)
	}

	l2, err := a.Allocate(z, "web2")
	if err != nil {
		t.Fatalf("Allocate : %v", err)
	}
	if l2.IPv4 != "10.42.10.3/24" {
		t.Fatalf("10.42.10.3/24 attendu, obtenu %s", l2.IPv4)
	}

	// Rappeler Allocate doit rendre le même bail, pas en consommer un nouveau.
	again, err := a.Allocate(z, "web1")
	if err != nil {
		t.Fatalf("Allocate : %v", err)
	}
	if again.IPv4 != l1.IPv4 {
		t.Fatalf("l'adresse d'une vm doit être stable : %s puis %s", l1.IPv4, again.IPv4)
	}

	// Après libération, l'adresse doit être réutilisée.
	a.Release("web1")
	l3, err := a.Allocate(z, "web3")
	if err != nil {
		t.Fatalf("Allocate : %v", err)
	}
	if l3.IPv4 != "10.42.10.2/24" {
		t.Fatalf("l'adresse libérée devrait être réutilisée, obtenu %s", l3.IPv4)
	}
}

func TestAllocationRefuseChangementDeZone(t *testing.T) {
	a, _ := NewAllocation()
	z1 := Zone{Name: "publique", IPv4: "10.42.10.0/24"}
	z2 := Zone{Name: "interne", IPv4: "10.42.20.0/24"}

	if _, err := a.Allocate(z1, "web1"); err != nil {
		t.Fatalf("Allocate : %v", err)
	}
	_, err := a.Allocate(z2, "web1")
	if err == nil {
		t.Fatal("déplacer une vm de zone en silence devrait être refusé")
	}
	if !strings.Contains(err.Error(), "déjà adressée") {
		t.Fatalf("message inattendu : %v", err)
	}
}

func TestAllocationEpuisement(t *testing.T) {
	a, _ := NewAllocation()
	// /29 : 8 adresses, moins le réseau, l'hôte et la diffusion, il en reste 5.
	z := Zone{Name: "petite", IPv4: "10.42.50.0/29"}

	for i := 0; i < 5; i++ {
		if _, err := a.Allocate(z, string(rune('a'+i))); err != nil {
			t.Fatalf("attribution %d refusée : %v", i, err)
		}
	}
	if _, err := a.Allocate(z, "trop"); err == nil {
		t.Fatal("un réseau épuisé devrait être signalé")
	}
}

// TestAllocationEviteDiffusionEtHote : l'adresse de réseau, celle de l'hôte et
// celle de diffusion ne doivent jamais être attribuées.
func TestAllocationEviteDiffusionEtHote(t *testing.T) {
	a, _ := NewAllocation()
	z := Zone{Name: "petite", IPv4: "10.42.50.0/29"}

	interdites := map[string]bool{
		"10.42.50.0": true, // réseau
		"10.42.50.1": true, // hôte
		"10.42.50.7": true, // diffusion
	}
	for i := 0; i < 5; i++ {
		l, err := a.Allocate(z, string(rune('a'+i)))
		if err != nil {
			t.Fatalf("attribution %d : %v", i, err)
		}
		addr := netip.MustParsePrefix(l.IPv4).Addr().String()
		if interdites[addr] {
			t.Fatalf("adresse réservée attribuée : %s", addr)
		}
	}
}

func TestTapNameTronque(t *testing.T) {
	if got := TapName("web1"); got != "tap-web1" {
		t.Fatalf("tap-web1 attendu, obtenu %s", got)
	}
	long := TapName("une-vm-avec-un-nom-tres-long")
	if len(long) > 15 {
		t.Fatalf("nom d'interface trop long pour le noyau : %q fait %d caractères", long, len(long))
	}
	if !strings.HasPrefix(long, "tap-") {
		t.Fatalf("préfixe tap- attendu, obtenu %q", long)
	}
}
