package network

import (
	"io"
	"log/slog"
	"strings"
	"testing"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testPolicy() Policy {
	zones := []Zone{
		{Name: "publique", IPv4: "10.42.10.0/24", IPv6: "fd42:1::/64"},
		{Name: "interne", IPv4: "10.42.20.0/24", IPv6: "fd42:2::/64"},
		{Name: "donnees", IPv4: "10.42.30.0/24", IPv6: "fd42:3::/64"},
	}
	flows := []Flow{
		{From: "publique", To: "interne", Proto: ProtoUDP, Ports: []string{"53"}, Comment: "dns"},
		{From: "interne", To: ZoneInternet, Proto: ProtoTCP, Ports: []string{"80", "443"}},
		{From: "interne", To: ZoneHost, Proto: ProtoUDP, Ports: []string{"53"}},
	}
	return Policy{Zones: zones, Flows: flows}
}

func TestFlowValidate(t *testing.T) {
	zones := testPolicy().Zones

	bons := []Flow{
		{From: "publique", To: "interne", Proto: ProtoTCP, Ports: []string{"80"}},
		{From: "publique", To: "interne", Proto: ProtoTCP, Ports: []string{"8000-8100"}},
		{From: "interne", To: ZoneInternet, Proto: ProtoTCP, Ports: []string{"443"}},
		{From: "interne", To: ZoneHost, Proto: ProtoUDP, Ports: []string{"53"}},
		{From: "publique", To: "donnees", Proto: ProtoICMP},
		{From: "publique", To: "donnees"},
	}
	for _, f := range bons {
		if err := f.Validate(zones); err != nil {
			t.Errorf("flux valide refuse (%+v) : %v", f, err)
		}
	}

	mauvais := []struct {
		nom     string
		flow    Flow
		attendu string
	}{
		{"source absente", Flow{To: "interne"}, "from et to sont obligatoires"},
		{"zone inconnue", Flow{From: "fantome", To: "interne"}, "zone \"fantome\" inconnue"},
		{"zone a elle-meme", Flow{From: "interne", To: "interne"}, "parle librement à elle-même"},
		{"deux zones fictives", Flow{From: ZoneHost, To: ZoneInternet}, "hors des zones gérées"},
		{"entrant depuis internet", Flow{From: ZoneInternet, To: "publique"}, "publication de service"},
		{"protocole inconnu", Flow{From: "publique", To: "interne", Proto: "sctp"}, "protocole \"sctp\" inconnu"},
		{"ports sur icmp", Flow{From: "publique", To: "interne", Proto: ProtoICMP, Ports: []string{"80"}}, "des ports sont indiqués"},
		{"port hors bornes", Flow{From: "publique", To: "interne", Proto: ProtoTCP, Ports: []string{"70000"}}, "hors bornes"},
		{"intervalle inverse", Flow{From: "publique", To: "interne", Proto: ProtoTCP, Ports: []string{"900-100"}}, "borne haute"},
		{"port non numerique", Flow{From: "publique", To: "interne", Proto: ProtoTCP, Ports: []string{"http"}}, "invalide"},
	}
	for _, tc := range mauvais {
		t.Run(tc.nom, func(t *testing.T) {
			err := tc.flow.Validate(zones)
			if err == nil {
				t.Fatal("flux invalide accepte")
			}
			if !strings.Contains(err.Error(), tc.attendu) {
				t.Fatalf("message attendu contenant %q, obtenu %q", tc.attendu, err)
			}
		})
	}
}

func TestValidateFlowsRefuseDoublon(t *testing.T) {
	zones := testPolicy().Zones
	flows := []Flow{
		{From: "publique", To: "interne", Proto: ProtoTCP, Ports: []string{"80"}},
		{From: "publique", To: "interne", Proto: ProtoTCP, Ports: []string{"80"}},
	}
	err := ValidateFlows(flows, zones)
	if err == nil || !strings.Contains(err.Error(), "deux fois") {
		t.Fatalf("un doublon devrait etre signale, obtenu %v", err)
	}
}

func TestExampleFlowsCoherents(t *testing.T) {
	zones := ExampleZones()
	// Les zones d exemple n ont pas d IPv6 avant attribution : on complete.
	a, err := NewAllocation()
	if err != nil {
		t.Fatalf("NewAllocation : %v", err)
	}
	for i := range zones {
		if err := a.ResolveZoneIPv6(&zones[i]); err != nil {
			t.Fatalf("ResolveZoneIPv6 : %v", err)
		}
	}
	if err := ValidateFlows(ExampleFlows(), zones); err != nil {
		t.Fatalf("les flux d exemple devraient etre valides : %v", err)
	}
}

// TestExampleFlowsAucuneSortieDesZonesSensibles tient la promesse du projet :
// aucune donnee ne sort. Elle doit se verifier en lisant la configuration.
func TestExampleFlowsAucuneSortieDesZonesSensibles(t *testing.T) {
	for _, f := range ExampleFlows() {
		if f.To == ZoneInternet && (f.From == "donnees" || f.From == "ia") {
			t.Errorf("la zone %s ne doit avoir aucun flux vers Internet : %+v", f.From, f)
		}
	}
}

func TestRulesetContenu(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	rs, err := fw.Ruleset(testPolicy())
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}

	attendus := []struct {
		motif  string
		raison string
	}{
		{"table inet tinyvmos", "une table de famille inet couvre les deux familles"},
		{"delete table inet tinyvmos", "la table est remise a zero avant reecriture"},
		{"ct state established,related accept", "les retours de connexion doivent passer"},
		{"nd-neighbor-solicit", "la decouverte de voisins IPv6 ne doit jamais etre jetee"},
		{"nd-neighbor-advert", "la decouverte de voisins IPv6 ne doit jamais etre jetee"},
		{"iifname \"br-publique\" oifname \"br-interne\" meta l4proto udp udp dport { 53 } accept", "le flux declare doit produire sa regle"},
		{"iifname \"br-interne\" oifname \"eth0\"", "la sortie Internet passe par l interface de sortie"},
		{"ip saddr 10.42.20.0/24 oifname \"eth0\" masquerade", "la zone avec sortie Internet est traduite en IPv4"},
		{"ip6 saddr fd42:2::/64 oifname \"eth0\" masquerade", "et aussi en IPv6"},
		{"iifname @zones counter drop", "ce qui sort d une zone sans regle est jete"},
		{"oifname @zones counter drop", "ce qui entre dans une zone sans regle est jete"},
		{"limit rate 10/minute log", "les refus sont journalises a debit limite"},
	}
	for _, a := range attendus {
		if !strings.Contains(rs, a.motif) {
			t.Errorf("jeu de regles sans %q : %s\n--- obtenu ---\n%s", a.motif, a.raison, rs)
		}
	}
}

// TestRulesetPolitiquesAccept protege contre une regression grave : une
// politique de chaine a drop couperait tout le trafic route de la machine et,
// sur input, l acces d administration.
func TestRulesetPolitiquesAccept(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	rs, err := fw.Ruleset(testPolicy())
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}
	if strings.Contains(rs, "policy drop") {
		t.Fatal("aucune chaine ne doit avoir policy drop : le refus se fait par regles nommant nos interfaces, " +
			"sinon la table casserait le reste de la machine et couperait l acces d administration")
	}
	for _, chaine := range []string{"chain forward", "chain input", "chain postrouting"} {
		if !strings.Contains(rs, chaine) {
			t.Errorf("chaine manquante : %s", chaine)
		}
	}
}

// TestRulesetZoneSensibleSansSortie verifie qu une zone sans flux declare
// n apparait dans aucune regle d autorisation ni de traduction d adresses.
func TestRulesetZoneSensibleSansSortie(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	rs, err := fw.Ruleset(testPolicy())
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}

	for _, ligne := range strings.Split(rs, "\n") {
		l := strings.TrimSpace(ligne)
		if strings.HasPrefix(l, "#") || l == "" {
			continue
		}
		if !strings.Contains(l, "br-donnees") && !strings.Contains(l, "10.42.30.0/24") {
			continue
		}
		// La zone donnees ne doit apparaitre que dans l ensemble des zones.
		if strings.Contains(l, "elements =") {
			continue
		}
		t.Errorf("la zone donnees ne devrait figurer dans aucune regle, trouvee dans : %s", l)
	}
}

func TestRulesetSansInterfaceDeSortie(t *testing.T) {
	fw := NewFirewall(quiet(), "", "")
	p := Policy{
		Zones: []Zone{{Name: "interne", IPv4: "10.42.20.0/24"}},
		Flows: []Flow{{From: "interne", To: ZoneInternet, Proto: ProtoTCP, Ports: []string{"443"}}},
	}
	// detectUplink peut renvoyer une interface reelle sur la machine de test :
	// on verifie seulement que la generation n echoue pas et reste coherente.
	rs, err := fw.Ruleset(p)
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}
	if !strings.Contains(rs, "table inet tinyvmos") {
		t.Fatal("jeu de regles incomplet")
	}
}

func TestRulesetRefusePolitiqueIncoherente(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	p := Policy{
		Zones: []Zone{
			{Name: "a", IPv4: "10.42.10.0/24"},
			{Name: "b", IPv4: "10.42.10.0/24"},
		},
	}
	if _, err := fw.Ruleset(p); err == nil {
		t.Fatal("des zones qui se recouvrent devraient faire echouer la generation")
	}

	p2 := Policy{
		Zones: []Zone{{Name: "a", IPv4: "10.42.10.0/24"}},
		Flows: []Flow{{From: "a", To: "inexistante"}},
	}
	if _, err := fw.Ruleset(p2); err == nil {
		t.Fatal("un flux vers une zone inconnue devrait faire echouer la generation")
	}
}

func TestMatchExpr(t *testing.T) {
	cases := []struct {
		flow    Flow
		attendu string
	}{
		{Flow{Proto: ProtoTCP, Ports: []string{"80"}}, "meta l4proto tcp tcp dport { 80 }"},
		{Flow{Proto: ProtoTCP, Ports: []string{"80", "443"}}, "meta l4proto tcp tcp dport { 80, 443 }"},
		{Flow{Proto: ProtoUDP, Ports: []string{"8000-8100"}}, "meta l4proto udp udp dport { 8000-8100 }"},
		{Flow{Proto: ProtoTCP}, "meta l4proto tcp"},
		{Flow{Proto: ProtoICMP}, "meta l4proto { icmp, icmpv6 }"},
		{Flow{}, ""},
	}
	for _, c := range cases {
		if got := matchExpr(c.flow); got != c.attendu {
			t.Errorf("matchExpr(%+v) = %q, attendu %q", c.flow, got, c.attendu)
		}
	}
}
