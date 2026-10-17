package vm

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tailleFichier(t *testing.T, chemin string) int64 {
	t.Helper()
	info, err := os.Stat(chemin)
	if err != nil {
		t.Fatalf("stat de %s : %v", chemin, err)
	}
	return info.Size()
}

// Le cas de la faille : sans borne, le journal grossit jusqu'à remplir la
// partition, et la panne d'une seule vm devient la panne de la machine.
func TestConsoleWriterBorneLaTailleTotale(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "console.log")
	const maxOctets = 1024
	const conserves = 1

	w, err := NewConsoleWriter(chemin, maxOctets, conserves)
	if err != nil {
		t.Fatalf("NewConsoleWriter : %v", err)
	}
	defer w.Close()

	ligne := strings.Repeat("x", 100) + "\n"
	for i := 0; i < 500; i++ { // 50 500 octets, cinquante fois le plafond
		if _, err := w.Write([]byte(ligne)); err != nil {
			t.Fatalf("écriture %d : %v", i, err)
		}
	}

	fichiers := ConsoleFiles(chemin, conserves)
	if len(fichiers) != 2 {
		t.Fatalf("fichiers présents = %v, attendu le courant et un conservé", fichiers)
	}

	var total int64
	for _, f := range fichiers {
		total += tailleFichier(t, f)
	}
	// Le plafond est dépassé au plus d'une écriture par fichier.
	plafond := int64(maxOctets*(conserves+1)) + int64(len(ligne))*2
	if total > plafond {
		t.Fatalf("total écrit sur disque = %d octets, plafond attendu %d", total, plafond)
	}

	// Et aucun fichier ne traîne au-delà de ce qui est conservé : sans
	// suppression du plus ancien, la rotation ne bornerait rien.
	if _, err := os.Stat(chemin + ".2"); !os.IsNotExist(err) {
		t.Fatal("un fichier au-delà du nombre conservé existe encore")
	}
}

// Ce qui vient d'être écrit doit être dans le fichier courant : un journal
// dont les dernières lignes sont dans l'archive serait inutilisable au moment
// où on en a besoin.
func TestConsoleWriterGardeLePlusRecentDansLeCourant(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "console.log")
	w, err := NewConsoleWriter(chemin, 256, 1)
	if err != nil {
		t.Fatalf("NewConsoleWriter : %v", err)
	}
	defer w.Close()

	for i := 0; i < 20; i++ {
		if _, err := w.Write([]byte(strings.Repeat("a", 50) + "\n")); err != nil {
			t.Fatalf("écriture : %v", err)
		}
	}
	if _, err := w.Write([]byte("DERNIERE LIGNE\n")); err != nil {
		t.Fatalf("écriture finale : %v", err)
	}

	contenu, err := os.ReadFile(chemin)
	if err != nil {
		t.Fatalf("lecture : %v", err)
	}
	if !strings.Contains(string(contenu), "DERNIERE LIGNE") {
		t.Fatal("la dernière ligne écrite n'est pas dans le journal courant")
	}
}

// Une écriture plus grosse que le plafond passe quand même : tronquer un
// message perdrait précisément la ligne qu'on cherche.
func TestConsoleWriterNeTronquePasUneGrosseEcriture(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "console.log")
	w, err := NewConsoleWriter(chemin, 64, 1)
	if err != nil {
		t.Fatalf("NewConsoleWriter : %v", err)
	}
	defer w.Close()

	gros := []byte(strings.Repeat("z", 500))
	n, err := w.Write(gros)
	if err != nil {
		t.Fatalf("écriture : %v", err)
	}
	if n != len(gros) {
		t.Fatalf("octets écrits = %d, attendu %d", n, len(gros))
	}
}

// Reprendre un journal existant ne doit pas le repartir de zéro, sinon un
// redémarrage du démon effacerait la trace du démarrage précédent.
func TestConsoleWriterReprendUnFichierExistant(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "console.log")
	if err := os.WriteFile(chemin, []byte("avant le redémarrage\n"), 0o600); err != nil {
		t.Fatalf("préparation : %v", err)
	}

	w, err := NewConsoleWriter(chemin, 1<<20, 1)
	if err != nil {
		t.Fatalf("NewConsoleWriter : %v", err)
	}
	if _, err := w.Write([]byte("après le redémarrage\n")); err != nil {
		t.Fatalf("écriture : %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close : %v", err)
	}

	contenu, err := os.ReadFile(chemin)
	if err != nil {
		t.Fatalf("lecture : %v", err)
	}
	for _, attendu := range []string{"avant le redémarrage", "après le redémarrage"} {
		if !strings.Contains(string(contenu), attendu) {
			t.Errorf("le journal a perdu %q", attendu)
		}
	}
}

// Le journal appartient à root et n'est lisible que par lui : une vm ne doit
// pas pouvoir relire ni réécrire la trace de ce qu'elle a fait.
func TestConsoleWriterDroitsRestreints(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "console.log")
	w, err := NewConsoleWriter(chemin, 1<<20, 1)
	if err != nil {
		t.Fatalf("NewConsoleWriter : %v", err)
	}
	defer w.Close()

	info, err := os.Stat(chemin)
	if err != nil {
		t.Fatalf("stat : %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("droits du journal = %o, attendu 600", mode)
	}
}

// Fermer deux fois arrive : la goroutine d'attente et les chemins d'erreur de
// Start peuvent tous deux y passer.
func TestConsoleWriterFermetureIdempotente(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "console.log")
	w, err := NewConsoleWriter(chemin, 1<<20, 1)
	if err != nil {
		t.Fatalf("NewConsoleWriter : %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("première fermeture : %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("seconde fermeture : %v", err)
	}
	if _, err := w.Write([]byte("après fermeture")); err == nil {
		t.Fatal("écrire après fermeture doit échouer plutôt que de perdre la ligne en silence")
	}
}
