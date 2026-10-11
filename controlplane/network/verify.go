package network

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync"
)

// fingerprintPrefix marque la règle témoin que TinyVMOS pose dans sa table.
//
// Elle sert à reconnaître « notre » table : si quelqu'un vide le pare-feu, ou
// le remplace par autre chose, la règle témoin disparaît et la dérive est
// détectée. Son empreinte est celle du jeu de règles appliqué, donc un
// changement de politique se voit aussi.
const fingerprintPrefix = "tinyvmos-empreinte-"

// applied retient ce qui a été réellement installé, pour pouvoir constater
// plus tard que rien n'a bougé.
//
// Le nombre de règles est relu depuis nftables après l'application, et non
// compté sur le texte engendré : c'est l'état réel de la machine qui compte,
// pas ce qu'on croit avoir écrit.
type applied struct {
	mu          sync.Mutex
	fingerprint string
	ruleCount   int
	known       bool
}

func fingerprintOf(ruleset string) string {
	sum := sha256.Sum256([]byte(ruleset))
	return fmt.Sprintf("%x", sum[:6])
}

// Drift décrit un écart entre ce qui devrait être en place et ce qui l'est.
type Drift struct {
	// Detected vaut faux quand tout est conforme.
	Detected bool

	// Reason est lisible par un opérateur : elle part dans les journaux et
	// c'est elle qu'on lira à trois heures du matin.
	Reason string
}

// Verify constate si le pare-feu installé est bien celui qui a été appliqué.
//
// Trois écarts sont détectés : la table a disparu, la règle témoin a disparu
// ou porte une autre empreinte, et le nombre de règles a changé. Le premier
// couvre le cas qui compte le plus — un « nft flush ruleset » lancé par
// n'importe quoi d'autre détruit l'isolation sans rien dire.
//
// La vérification ne réécrit rien : réappliquer à chaque passe remettrait tous
// les compteurs à zéro, et on perdrait la trace des refus.
func (f *Firewall) Verify() (Drift, error) {
	f.applied.mu.Lock()
	wantFingerprint := f.applied.fingerprint
	wantCount := f.applied.ruleCount
	known := f.applied.known
	f.applied.mu.Unlock()

	if !known {
		return Drift{}, nil // rien n'a encore été appliqué
	}

	rules, err := nftRules()
	if err != nil {
		if strings.Contains(err.Error(), "No such file or directory") ||
			strings.Contains(err.Error(), "does not exist") {
			return Drift{Detected: true, Reason: "la table a disparu du pare-feu"}, nil
		}
		return Drift{}, err
	}

	if len(rules) == 0 {
		return Drift{Detected: true, Reason: "la table du pare-feu est vide"}, nil
	}

	gotFingerprint := ""
	for _, r := range rules {
		if c := r.comment(); strings.HasPrefix(c, fingerprintPrefix) {
			gotFingerprint = strings.TrimPrefix(c, fingerprintPrefix)
			break
		}
	}
	if gotFingerprint == "" {
		return Drift{Detected: true, Reason: "la règle témoin de TinyVMOS a disparu"}, nil
	}
	if gotFingerprint != wantFingerprint {
		return Drift{
			Detected: true,
			Reason: fmt.Sprintf("empreinte du pare-feu différente : %s en place, %s attendue",
				gotFingerprint, wantFingerprint),
		}, nil
	}
	if len(rules) != wantCount {
		return Drift{
			Detected: true,
			Reason: fmt.Sprintf("le nombre de règles a changé : %d en place, %d attendues",
				len(rules), wantCount),
		}, nil
	}

	return Drift{}, nil
}

// nftRule est une règle telle que nft la rend en JSON. Seul le commentaire
// nous intéresse ; le reste est ignoré volontairement, pour ne pas dépendre
// du détail d'un format qui peut évoluer.
type nftRule struct {
	Chain   string `json:"chain"`
	Comment string `json:"comment"`
}

func (r nftRule) comment() string { return r.Comment }

// nftRules lit les règles de la table TinyVMOS.
//
// La sortie JSON de nft est utilisée plutôt que son affichage textuel : le
// texte est fait pour être lu par un humain et sa mise en forme change d'une
// version à l'autre, ce qui ferait de la vérification un piège.
func nftRules() ([]nftRule, error) {
	nft, err := exec.LookPath("nft")
	if err != nil {
		return nil, fmt.Errorf("binaire nft introuvable : %w", err)
	}

	out, err := exec.Command(nft, "-j", "list", "table", "inet", TableName).Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = strings.TrimSpace(string(ee.Stderr))
		}
		if stderr == "" {
			stderr = err.Error()
		}
		return nil, fmt.Errorf("%s", stderr)
	}

	var doc struct {
		Nftables []map[string]json.RawMessage `json:"nftables"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("sortie JSON de nft illisible : %w", err)
	}

	var rules []nftRule
	for _, obj := range doc.Nftables {
		raw, ok := obj["rule"]
		if !ok {
			continue
		}
		var r nftRule
		if err := json.Unmarshal(raw, &r); err != nil {
			continue
		}
		rules = append(rules, r)
	}
	return rules, nil
}

// rememberApplied relit la table après application et retient ce qui s'y
// trouve, pour pouvoir constater une dérive ensuite.
func (f *Firewall) rememberApplied(ruleset string) {
	// L'empreinte retenue est celle qui a été INSÉRÉE dans la règle témoin,
	// pas celle du texte final — qui contient déjà la règle et donnerait donc
	// une autre valeur. Les confondre faisait échouer la comparaison à chaque
	// passe, et le pare-feu était réécrit toutes les quelques secondes.
	fp := f.lastEmbedded
	if fp == "" {
		fp = fingerprintOf(ruleset)
	}

	count := 0
	if rules, err := nftRules(); err == nil {
		count = len(rules)
	}

	f.applied.mu.Lock()
	f.applied.fingerprint = fp
	f.applied.ruleCount = count
	f.applied.known = count > 0
	f.applied.mu.Unlock()
}

// forgetApplied oublie l'état retenu, après un retrait volontaire de la table.
func (f *Firewall) forgetApplied() {
	f.applied.mu.Lock()
	f.applied.known = false
	f.applied.fingerprint = ""
	f.applied.ruleCount = 0
	f.applied.mu.Unlock()
}
