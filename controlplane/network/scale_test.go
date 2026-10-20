package network

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Test à l'échelle du plan d'adressage et du pare-feu.
//
// Le cahier des charges parle de vingt à trente micro-VM sur une machine
// modeste. Tout avait été éprouvé à deux ou trois : une collision d'adresses
// ou un jeu de règles qui explose ne se voient pas à cette taille, et se
// découvriraient en production.
//
// Ce test ne démarre aucune micro-VM — il éprouve le plan d'adressage et la
// génération du pare-feu, qui sont la partie qui change de nature avec le
// nombre. Le démarrage réel de trente VM demande KVM et vit dans
// tests/, pas ici.

const (
	echelleZones = 5
	echelleVMs   = 30
)

func zonesEchelle(t *testing.T) []Zone {
	t.Helper()
	zones := make([]Zone, 0, echelleZones)
	for i := 0; i < echelleZones; i++ {
		zones = append(zones, Zone{
			Name: fmt.Sprintf("zone%d", i),
			IPv4: fmt.Sprintf("10.42.%d.0/24", 100+i),
		})
	}
	return zones
}

// Trente VM réparties sur cinq zones ne doivent produire aucune collision, ni
// d'adresse ni de nom d'interface. Une collision silencieuse ferait deux VM
// injoignables et des règles de pare-feu qui ne veulent plus rien dire.
func TestEchelleAucuneCollisionDAdresse(t *testing.T) {
	alloc, err := NewAllocation()
	if err != nil {
		t.Fatalf("NewAllocation : %v", err)
	}
	zones := zonesEchelle(t)
	for i := range zones {
		if err := alloc.ResolveZoneIPv6(&zones[i]); err != nil {
			t.Fatalf("ResolveZoneIPv6 : %v", err)
		}
	}

	vusV4 := map[string]string{}
	vusV6 := map[string]string{}
	vusTap := map[string]string{}

	for i := 0; i < echelleVMs; i++ {
		nom := fmt.Sprintf("vm%02d", i)
		zone := zones[i%len(zones)]

		bail, err := alloc.Allocate(zone, nom)
		if err != nil {
			t.Fatalf("attribution pour %s : %v", nom, err)
		}

		if precedent, dejaVu := vusV4[bail.IPv4]; dejaVu {
			t.Fatalf("collision IPv4 %s entre %s et %s", bail.IPv4, precedent, nom)
		}
		vusV4[bail.IPv4] = nom

		if bail.IPv6 == "" {
			t.Fatalf("%s n'a pas d'adresse IPv6 : une famille non configurée contourne les règles de l'autre", nom)
		}
		if precedent, dejaVu := vusV6[bail.IPv6]; dejaVu {
			t.Fatalf("collision IPv6 %s entre %s et %s", bail.IPv6, precedent, nom)
		}
		vusV6[bail.IPv6] = nom

		tap := TapName(nom)
		if precedent, dejaVu := vusTap[tap]; dejaVu {
			t.Fatalf("collision de nom d'interface %s entre %s et %s", tap, precedent, nom)
		}
		vusTap[tap] = nom

		// Le nom d'interface Linux est limité à quinze caractères. Un nom
		// tronqué entrerait en collision avec un autre sans prévenir.
		if len(tap) > 15 {
			t.Fatalf("nom d'interface trop long pour %s : %q fait %d caractères", nom, tap, len(tap))
		}
	}

	if len(vusV4) != echelleVMs {
		t.Fatalf("%d adresses IPv4 distinctes pour %d vm", len(vusV4), echelleVMs)
	}
}

// Une attribution rendue doit être réutilisée : sans cela, une zone en /24
// s'épuiserait au fil des créations et suppressions, bien avant d'héberger
// deux cent cinquante VM simultanées.
func TestEchelleLesAdressesRenduesSontReprises(t *testing.T) {
	alloc, err := NewAllocation()
	if err != nil {
		t.Fatalf("NewAllocation : %v", err)
	}
	zone := zonesEchelle(t)[0]
	if err := alloc.ResolveZoneIPv6(&zone); err != nil {
		t.Fatalf("ResolveZoneIPv6 : %v", err)
	}

	premier, err := alloc.Allocate(zone, "ephemere")
	if err != nil {
		t.Fatalf("attribution : %v", err)
	}
	adresse := premier.IPv4

	for tour := 0; tour < 300; tour++ {
		alloc.Release("ephemere")
		bail, err := alloc.Allocate(zone, "ephemere")
		if err != nil {
			t.Fatalf("tour %d : %v", tour, err)
		}
		if bail.IPv4 != adresse {
			t.Fatalf("tour %d : adresse %s au lieu de %s, la plage se consomme", tour, bail.IPv4, adresse)
		}
	}
}

// Le jeu de règles doit rester lisible et rapide à produire à cette taille.
// Il est engendré à chaque passe de réconciliation, toutes les dix secondes :
// s'il prenait une seconde, la machine passerait son temps là-dedans.
func TestEchelleJeuDeReglesTenable(t *testing.T) {
	zones := zonesEchelle(t)
	alloc, err := NewAllocation()
	if err != nil {
		t.Fatalf("NewAllocation : %v", err)
	}
	for i := range zones {
		if err := alloc.ResolveZoneIPv6(&zones[i]); err != nil {
			t.Fatalf("ResolveZoneIPv6 : %v", err)
		}
	}

	leases := map[string]Lease{}
	pubs := []Publication{}
	for i := 0; i < echelleVMs; i++ {
		nom := fmt.Sprintf("vm%02d", i)
		bail, err := alloc.Allocate(zones[i%len(zones)], nom)
		if err != nil {
			t.Fatalf("attribution : %v", err)
		}
		leases[nom] = *bail
		pubs = append(pubs, Publication{VM: nom, Port: 80, HostPort: 10000 + i})
	}

	// Un flux entre chaque paire de zones, plus la sortie et l'hôte : de quoi
	// produire un jeu de règles réaliste pour une installation chargée.
	flows := []Flow{}
	for i := range zones {
		for j := range zones {
			if i == j {
				continue
			}
			flows = append(flows, Flow{
				From: zones[i].Name, To: zones[j].Name,
				Proto: ProtoTCP, Ports: []string{"443"},
			})
		}
		flows = append(flows,
			Flow{From: zones[i].Name, To: ZoneInternet, Proto: ProtoTCP, Ports: []string{"80", "443"}},
			Flow{From: zones[i].Name, To: ZoneHost, Proto: ProtoUDP, Ports: []string{"53"}},
			Flow{From: ZoneHost, To: zones[i].Name, Proto: ProtoTCP, Ports: []string{"9100"}},
		)
	}

	p := Policy{Zones: zones, Flows: flows, Publications: pubs, Leases: leases}
	fw := NewFirewall(quiet(), "eth0", "")

	debut := time.Now()
	ruleset, err := fw.Ruleset(p)
	if err != nil {
		t.Fatalf("Ruleset à l'échelle : %v", err)
	}
	duree := time.Since(debut)

	// Large, volontairement : ce test doit attraper un effondrement, pas une
	// variation de quelques millisecondes sur une machine chargée.
	if duree > 250*time.Millisecond {
		t.Errorf("génération du jeu de règles en %s, trop long pour une passe toutes les dix secondes", duree)
	}
	if len(ruleset) > 256*1024 {
		t.Errorf("jeu de règles de %d octets : plus personne ne le relira", len(ruleset))
	}

	// Les quatre chaînes doivent être là, quelle que soit la taille.
	for _, chaine := range []string{"chain forward", "chain input", "chain output", "chain prerouting"} {
		if !strings.Contains(ruleset, chaine) {
			t.Errorf("chaîne absente à l'échelle : %s", chaine)
		}
	}

	// Et la règle témoin, sans laquelle la réconciliation ne verrait plus
	// aucune dérive.
	if !strings.Contains(ruleset, fingerprintPrefix) {
		t.Error("la règle témoin a disparu du jeu de règles à l'échelle")
	}
}
