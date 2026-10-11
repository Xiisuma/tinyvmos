package network

import (
	"regexp"
	"strings"
	"testing"
)

var empreinteRe = regexp.MustCompile(`counter comment "tinyvmos-empreinte-([0-9a-f]{12})"`)

// TestRulesetPorteRegleTemoin : sans elle, la reconciliation ne saurait pas
// reconnaitre sa propre table et un pare-feu vide passerait inapercu.
func TestRulesetPorteRegleTemoin(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	rs, err := fw.Ruleset(testPolicy())
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}

	m := empreinteRe.FindStringSubmatch(rs)
	if m == nil {
		t.Fatalf("aucune regle temoin dans le jeu de regles :\n%s", rs)
	}
	if len(m[1]) != 12 {
		t.Fatalf("empreinte de longueur inattendue : %q", m[1])
	}
}

// TestRulesetDeterministe : deux generations de la meme politique doivent
// donner exactement le meme texte. Sinon la reconciliation croirait a une
// derive a chaque passe et reecrirait le pare-feu sans arret, en remettant
// les compteurs a zero.
func TestRulesetDeterministe(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	p := testPolicy()

	premier, err := fw.Ruleset(p)
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}
	for i := 0; i < 5; i++ {
		suivant, err := fw.Ruleset(p)
		if err != nil {
			t.Fatalf("Ruleset : %v", err)
		}
		if suivant != premier {
			t.Fatalf("generation %d differente de la premiere", i+1)
		}
	}
}

// TestEmpreinteChangeAvecLaPolitique : un changement de politique doit se
// voir, sinon une modification de flux ne serait jamais appliquee par la
// reconciliation.
func TestEmpreinteChangeAvecLaPolitique(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")

	p1 := testPolicy()
	rs1, err := fw.Ruleset(p1)
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}

	p2 := testPolicy()
	p2.Flows = append(p2.Flows, Flow{
		From: "publique", To: "donnees", Proto: ProtoTCP, Ports: []string{"8080"},
	})
	rs2, err := fw.Ruleset(p2)
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}

	e1 := empreinteRe.FindStringSubmatch(rs1)
	e2 := empreinteRe.FindStringSubmatch(rs2)
	if e1 == nil || e2 == nil {
		t.Fatal("regle temoin absente")
	}
	if e1[1] == e2[1] {
		t.Fatal("l empreinte devrait changer quand la politique change")
	}
}

// TestRegleTemoinSansVerdict : la regle temoin ne doit ni accepter ni jeter
// quoi que ce soit, sinon elle modifierait la politique qu elle sert a
// identifier.
func TestRegleTemoinSansVerdict(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	rs, err := fw.Ruleset(testPolicy())
	if err != nil {
		t.Fatalf("Ruleset : %v", err)
	}
	for _, ligne := range strings.Split(rs, "\n") {
		if !strings.Contains(ligne, "tinyvmos-empreinte-") {
			continue
		}
		for _, verdict := range []string{"accept", "drop", "reject", "jump", "goto"} {
			if strings.Contains(ligne, verdict) {
				t.Fatalf("la regle temoin porte un verdict %q : %s", verdict, strings.TrimSpace(ligne))
			}
		}
		return
	}
	t.Fatal("regle temoin introuvable")
}

// TestVerifySansApplicationPrealable : tant que rien n a ete applique, il n y
// a pas de derive a signaler. Le contraire ferait reecrire le pare-feu au
// premier demarrage.
func TestVerifySansApplicationPrealable(t *testing.T) {
	fw := NewFirewall(quiet(), "eth0", "")
	drift, err := fw.Verify()
	if err != nil {
		t.Fatalf("Verify : %v", err)
	}
	if drift.Detected {
		t.Fatalf("aucune derive attendue avant application, obtenu : %s", drift.Reason)
	}
}

func TestFingerprintOfStable(t *testing.T) {
	a := fingerprintOf("meme contenu")
	b := fingerprintOf("meme contenu")
	c := fingerprintOf("autre contenu")

	if a != b {
		t.Fatal("la meme entree doit donner la meme empreinte")
	}
	if a == c {
		t.Fatal("deux entrees differentes doivent donner des empreintes differentes")
	}
	if len(a) != 12 {
		t.Fatalf("empreinte de 12 caracteres attendue, obtenu %d", len(a))
	}
}
