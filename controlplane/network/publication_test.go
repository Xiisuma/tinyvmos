package network

import (
	"strings"
	"testing"
)

func testLeases() map[string]Lease {
	return map[string]Lease{
		"web1": {
			Zone: "publique", VM: "web1",
			IPv4: "10.42.10.2/24", GatewayV4: "10.42.10.1",
			IPv6: "fd42:1::2/64", GatewayV6: "fd42:1::1",
		},
		"coffre1": {
			Zone: "donnees", VM: "coffre1",
			IPv4: "10.42.30.2/24", GatewayV4: "10.42.30.1",
		},
	}
}

func TestPublicationValidate(t *testing.T) {
	zones := testPolicy().Zones
	leases := testLeases()

	bonnes := []Publication{
		{VM: "web1", Port: 80},
		{VM: "web1", Port: 80, HostPort: 8080},
		{VM: "web1", Port: 53, Proto: ProtoUDP},
		{VM: "coffre1", Port: 443, Proto: ProtoTCP},
	}
	for _, p := range bonnes {
		if err := p.Validate(leases, zones); err != nil {
			t.Errorf("publication valide refusee (%+v) : %v", p, err)
		}
	}

	mauvaises := []struct {
		nom     string
		pub     Publication
		attendu string
	}{
		{"vm absente", Publication{Port: 80}, "vm est obligatoire"},
		{"vm inconnue", Publication{VM: "fantome", Port: 80}, "pas d'adresse attribuée"},
		{"port nul", Publication{VM: "web1", Port: 0}, "hors bornes"},
		{"port trop grand", Publication{VM: "web1", Port: 70000}, "hors bornes"},
		{"port expose invalide", Publication{VM: "web1", Port: 80, HostPort: 99999}, "hors bornes"},
		{"protocole invalide", Publication{VM: "web1", Port: 80, Proto: "icmp"}, "protocole"},
	}
	for _, tc := range mauvaises {
		t.Run(tc.nom, func(t *testing.T) {
			err := tc.pub.Validate(leases, zones)
			if err == nil {
				t.Fatal("publication invalide acceptee")
			}
			if !strings.Contains(err.Error(), tc.attendu) {
				t.Fatalf("message attendu contenant %q, obtenu %q", tc.attendu, err)
			}
		})
	}
}

// TestPublicationRefuseDoublonDePort : deux redirections sur le meme port
// donneraient un resultat dependant de l'ordre des regles.
func TestPublicationRefuseDoublonDePort(t *testing.T) {
	zones := testPolicy().Zones
	leases := testLeases()

	pubs := []Publication{
		{VM: "web1", Port: 80, HostPort: 8080},
		{VM: "coffre1", Port: 443, HostPort: 8080},
	}
	err := ValidatePublications(pubs, leases, zones)
	if err == nil {
		t.Fatal("deux publications sur le meme port expose devraient etre refusees")
	}
	if !strings.Contains(err.Error(), "déjà pris") {
		t.Fatalf("message inattendu : %v", err)
	}

	// Le meme port en tcp et en udp ne se marchent pas sur les pieds.
	ok := []Publication{
		{VM: "web1", Port: 53, HostPort: 53, Proto: ProtoTCP},
		{VM: "coffre1", Port: 53, HostPort: 53, Proto: ProtoUDP},
	}
	if err := ValidatePublications(ok, leases, zones); err != nil {
		t.Fatalf("tcp et udp sur le meme numero de port devraient coexister : %v", err)
	}
}

func TestRulesetPublication(t *testing.T) {
	p := testPolicy()
	p.Leases = testLeases()
	p.Publications = []Publication{
		{VM: "web1", Port: 80, HostPort: 8080, Comment: "serveur web de l evenement"},
	}

	fw := NewFirewall(quiet(), "eth0", "")
	rs, err := fw.Ruleset(p)
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}

	attendus := []struct{ motif, raison string }{
		{"chain prerouting", "la redirection de port vit dans une chaine prerouting"},
		{`iifname "eth0" tcp dport 8080 dnat ip to 10.42.10.2:80`, "la redirection doit viser l adresse de la vm"},
		{`iifname "eth0" oifname "br-publique" ct status dnat ip daddr 10.42.10.2 tcp dport 80 accept`, "seul le trafic REELLEMENT redirige est autorise : sans ct status dnat, un acces direct a l adresse privee passerait aussi"},
		{"serveur web de l evenement", "le commentaire est repris pour qu un operateur sache pourquoi la regle existe"},
	}
	for _, a := range attendus {
		if !strings.Contains(rs, a.motif) {
			t.Errorf("jeu de regles sans %q : %s\n--- obtenu ---\n%s", a.motif, a.raison, rs)
		}
	}
}

// TestRulesetSansPublicationAucuneRedirection : sans publication declaree,
// aucune redirection ne doit apparaitre.
func TestRulesetSansPublicationAucuneRedirection(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	rs, err := fw.Ruleset(testPolicy())
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}
	if strings.Contains(rs, "dnat") {
		t.Fatal("aucune redirection ne devrait figurer sans publication declaree")
	}
	if !strings.Contains(rs, "Aucun service publie") {
		t.Fatal("l absence de publication devrait etre dite explicitement dans le fichier")
	}
}

// TestRulesetPublicationIPv4Seulement documente un choix : publier une adresse
// locale unique par redirection supposerait du NAT66, mauvaise pratique.
func TestRulesetPublicationIPv4Seulement(t *testing.T) {
	p := testPolicy()
	p.Leases = testLeases()
	p.Publications = []Publication{{VM: "web1", Port: 80}}

	fw := NewFirewall(quiet(), "eth0", "")
	rs, err := fw.Ruleset(p)
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}
	if strings.Contains(rs, "dnat ip6") {
		t.Fatal("aucune redirection IPv6 ne doit etre produite : il faudrait un prefixe routé, pas du NAT66")
	}
}

// TestRuleSansEspaceSuperflu : le fichier engendre est fait pour etre relu.
func TestRuleSansEspaceSuperflu(t *testing.T) {
	p := Policy{
		Zones: []Zone{
			{Name: "a", IPv4: "10.42.10.0/24"},
			{Name: "b", IPv4: "10.42.20.0/24"},
		},
		// Flux sans protocole : c est le cas qui produisait un double espace.
		Flows: []Flow{{From: "a", To: "b"}},
	}
	fw := NewFirewall(quiet(), "eth0", "")
	rs, err := fw.Ruleset(p)
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}
	for _, ligne := range strings.Split(rs, "\n") {
		corps := strings.TrimSpace(ligne)
		if corps == "" || strings.HasPrefix(corps, "#") {
			continue
		}
		if strings.Contains(corps, "  ") {
			t.Errorf("double espace dans une regle : %q", corps)
		}
	}
	if !strings.Contains(rs, `iifname "br-a" oifname "br-b" accept`) {
		t.Errorf("regle sans protocole mal formee :\n%s", rs)
	}
}

func TestRulesetRefusePublicationIncoherente(t *testing.T) {
	p := testPolicy()
	p.Leases = testLeases()
	p.Publications = []Publication{{VM: "inconnue", Port: 80}}

	fw := NewFirewall(quiet(), "eth0", "")
	if _, err := fw.Ruleset(p); err == nil {
		t.Fatal("une publication vers une vm inconnue devrait faire echouer la generation")
	}
}
