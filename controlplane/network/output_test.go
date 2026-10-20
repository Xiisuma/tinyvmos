package network

import (
	"strings"
	"testing"
)

// extraireChaine renvoie le corps d'une chaîne du jeu de règles.
func extraireChaine(t *testing.T, ruleset, nom string) string {
	t.Helper()
	debut := strings.Index(ruleset, "chain "+nom+" {")
	if debut < 0 {
		t.Fatalf("chaîne %s absente du jeu de règles", nom)
	}
	reste := ruleset[debut:]
	fin := strings.Index(reste, "\n\t}")
	if fin < 0 {
		t.Fatalf("chaîne %s non terminée", nom)
	}
	return reste[:fin]
}

// Le cas de la faille : rien ne filtrait ce que l'hôte envoyait aux zones.
func TestOutputChainFiltreVersLesZones(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	ruleset, err := fw.Ruleset(testPolicy())
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}

	out := extraireChaine(t, ruleset, "output")

	for _, attendu := range []struct{ motif, pourquoi string }{
		{"oifname @zones counter drop", "le trafic vers une zone doit être refusé par défaut"},
		{"oifname @zones meta skuid 0 counter accept", "les processus de root doivent passer : opérateur et démon"},
		{"ct state established,related accept", "les retours des connexions déjà autorisées doivent passer"},
		{"oifname @zones tcp flags rst accept", "un RST sans socket doit passer, sinon la vm attend sans fin"},
		{"nd-neighbor-solicit", "la découverte de voisins est indispensable à IPv6"},
		{"log prefix \"tinyvmos refus hote vers zone \"", "un refus doit se voir"},
	} {
		if !strings.Contains(out, attendu.motif) {
			t.Errorf("règle absente (%s) : %s", attendu.pourquoi, attendu.motif)
		}
	}

	// La politique de chaîne reste accept : la mettre à drop couperait tout le
	// trafic sortant de la machine, y compris celui qui ne nous concerne pas.
	if !strings.Contains(out, "policy accept;") {
		t.Error("la politique de la chaîne output doit rester accept")
	}
}

// Un flux déclaré de l'hôte vers une zone ne produisait aucune règle : il était
// silencieusement ignoré, ce qui est pire que de ne pas exister.
func TestOutputChainFluxDeclareDepuisLHote(t *testing.T) {
	p := testPolicy()
	p.Flows = append(p.Flows, Flow{
		From: ZoneHost, To: "donnees", Proto: ProtoTCP, Ports: []string{"5432"},
		Comment: "sauvegarde depuis l hôte",
	})

	fw := NewFirewall(quiet(), "eth0", "")
	ruleset, err := fw.Ruleset(p)
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}
	out := extraireChaine(t, ruleset, "output")

	// nftables agrège les ports en ensemble : « tcp dport { 5432 } ».
	if !strings.Contains(out, `oifname "br-donnees"`) || !strings.Contains(out, "5432") {
		t.Fatalf("le flux déclaré depuis l'hôte ne produit pas de règle :\n%s", out)
	}
	if !strings.Contains(out, "sauvegarde depuis l hôte") {
		t.Error("le commentaire du flux n'est pas repris dans le jeu de règles")
	}

	// Le flux vise une zone précise, pas l'ensemble : une règle sur @zones
	// ouvrirait ce port sur toutes les zones d'un coup.
	for _, ligne := range strings.Split(out, "\n") {
		if strings.Contains(ligne, "5432") && strings.Contains(ligne, "@zones") {
			t.Errorf("le flux déclaré porte sur toutes les zones : %s", ligne)
		}
	}
}

// L'ordre compte : la règle de refus doit être la dernière, sinon elle jetterait
// ce que les règles suivantes autorisent.
func TestOutputChainLeRefusEstEnDernier(t *testing.T) {
	p := testPolicy()
	p.Flows = append(p.Flows, Flow{From: ZoneHost, To: "donnees", Proto: ProtoTCP, Ports: []string{"5432"}})

	fw := NewFirewall(quiet(), "eth0", "")
	ruleset, err := fw.Ruleset(p)
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}
	out := extraireChaine(t, ruleset, "output")

	refus := strings.Index(out, "oifname @zones counter drop")
	for _, avant := range []string{
		"oifname @zones meta skuid 0 counter accept",
		"5432",
		"ct state established,related accept",
	} {
		i := strings.Index(out, avant)
		if i < 0 {
			t.Fatalf("règle attendue absente : %s", avant)
		}
		if i > refus {
			t.Errorf("la règle %q est placée après le refus : elle ne servirait à rien", avant)
		}
	}
}

// Sans zone déclarée, l'ensemble est vide et la chaîne ne doit rien jeter de ce
// qui ne la concerne pas.
func TestOutputChainSansZone(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	ruleset, err := fw.Ruleset(Policy{})
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}
	out := extraireChaine(t, ruleset, "output")
	if !strings.Contains(out, "Aucun flux declare de l hote vers une zone") {
		t.Errorf("l'absence de flux déclaré doit être dite :\n%s", out)
	}
}
