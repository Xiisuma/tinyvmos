package main

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Xiisuma/tinyvmos/controlplane/network"
	"github.com/Xiisuma/tinyvmos/controlplane/state"
	"github.com/Xiisuma/tinyvmos/controlplane/vm"
)

func logSilencieux() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// capturerSortie exécute fn en détournant la sortie standard.
func capturerSortie(t *testing.T, fn func() error) string {
	t.Helper()
	ancien := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe : %v", err)
	}
	os.Stdout = w

	lu := make(chan string, 1)
	go func() {
		data, _ := io.ReadAll(r)
		lu <- string(data)
	}()

	errFn := fn()
	w.Close()
	os.Stdout = ancien
	sortie := <-lu

	if errFn != nil {
		t.Fatalf("exécution : %v", errFn)
	}
	return sortie
}

// Le cas de la faille : -print-ruleset tirait un préfixe local unique neuf à
// chaque appel. Les adresses IPv6 affichées n'étaient celles de personne, et
// surtout pas celles qui allaient être appliquées.
func TestPrintRulesetUtiliseLePrefixePersiste(t *testing.T) {
	dir := t.TempDir()
	chemin := filepath.Join(dir, "state.json")

	const prefixe = "fd12:3456:789a::/48"
	store, err := state.NewJSONStore(chemin)
	if err != nil {
		t.Fatalf("NewJSONStore : %v", err)
	}
	snap := vm.NewSnapshot()
	snap.Alloc = &network.Allocation{
		ULAPrefix: prefixe,
		ZoneIndex: map[string]int{},
		Leases:    map[string]*network.Lease{},
	}
	if err := store.Save(snap); err != nil {
		t.Fatalf("Save : %v", err)
	}

	sortie := capturerSortie(t, func() error {
		return printRuleset(logSilencieux(), runOptions{statePath: chemin})
	})

	if !strings.Contains(sortie, "fd12:3456:789a:") {
		t.Fatalf("le jeu de règles n'utilise pas le préfixe persisté :\n%s", premieresLignes(sortie, 12))
	}
	if !strings.Contains(sortie, "# adressage IPv6 : état "+chemin) {
		t.Fatalf("la provenance de l'adressage n'est pas annoncée :\n%s", premieresLignes(sortie, 3))
	}
	// Et surtout : aucune autre adresse locale unique ne doit apparaître.
	for _, ligne := range strings.Split(sortie, "\n") {
		i := strings.Index(ligne, "fd")
		if i < 0 {
			continue
		}
		if !strings.Contains(ligne, "fd12:3456:789a:") {
			t.Fatalf("une adresse hors du préfixe persisté apparaît : %s", ligne)
		}
	}
}

// Sans état, il faut bien montrer quelque chose — mais il faut dire que ce
// n'est qu'une illustration, sans quoi on retombe dans la faille.
func TestPrintRulesetSansEtatAvertitQueLAdressageEstFictif(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "absent.json")

	sortie := capturerSortie(t, func() error {
		return printRuleset(logSilencieux(), runOptions{statePath: chemin})
	})

	if !strings.Contains(sortie, "PAS celui qui sera appliqué") {
		t.Fatalf("l'avertissement manque :\n%s", premieresLignes(sortie, 3))
	}
}

func premieresLignes(s string, n int) string {
	lignes := strings.Split(s, "\n")
	if len(lignes) > n {
		lignes = lignes[:n]
	}
	return strings.Join(lignes, "\n")
}
