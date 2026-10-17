package vm

import (
	"fmt"
	"os"
	"sync"
)

// Rotation des journaux de console.
//
// Le journal de console série est la seule fenêtre sur ce qui se passe dans
// une micro-VM, et la seule façon de diagnostiquer un démarrage qui échoue. Il
// était écrit en ajout, sans aucune limite.
//
// Une VM bavarde, un noyau qui répète la même erreur, un service qui boucle :
// le fichier grossit jusqu'à remplir la partition. Sur une machine modeste
// c'est tout le système qui tombe — le démon ne peut plus écrire son état,
// nftables ne peut plus recharger, et la panne d'une seule VM devient la panne
// de la machine.
//
// La rotation est faite ici plutôt que confiée à logrotate, pour deux raisons.
// D'abord parce qu'un outil externe ne doit pas être nécessaire au bon
// fonctionnement du démon sur une machine qu'on veut minimale. Ensuite parce
// que le fichier est tenu ouvert par le processus qui écrit : logrotate devrait
// passer par copytruncate, qui perd les lignes écrites entre la copie et la
// troncature.
//
// Les valeurs par défaut sont volontairement basses. Trente VM à 8 Mio font
// déjà 240 Mio, et le disque d'une machine modeste n'est pas extensible.

const (
	// ConsoleMaxBytes est la taille au-delà de laquelle le journal courant est
	// mis de côté.
	ConsoleMaxBytes = 4 << 20 // 4 Mio

	// ConsoleKeep est le nombre de journaux conservés en plus du courant.
	// Un seul : le fichier précédent suffit presque toujours à retrouver la
	// cause d'un arrêt, et chaque fichier supplémentaire se paie par VM.
	ConsoleKeep = 1
)

// ConsoleWriter écrit le journal de console d'une VM en le faisant tourner.
//
// Il est passé au SDK comme io.Writer et non comme *os.File, délibérément :
// os/exec interpose alors un tube et une goroutine de recopie, de sorte que le
// processus confiné n'hérite jamais d'un descripteur vers le fichier. La
// rotation devient sûre — personne d'autre ne tient le fichier ouvert — et le
// journal reste la propriété de root, illisible par la VM qui le remplit.
type ConsoleWriter struct {
	path     string
	maxBytes int64
	keep     int

	mu   sync.Mutex
	f    *os.File
	size int64
}

// NewConsoleWriter ouvre le journal de console, en reprenant un fichier
// existant là où il s'était arrêté.
//
// maxBytes ou keep à zéro ou moins prennent les valeurs par défaut : un appelant
// qui oublie de les renseigner doit obtenir une rotation, pas son absence.
func NewConsoleWriter(path string, maxBytes int64, keep int) (*ConsoleWriter, error) {
	if maxBytes <= 0 {
		maxBytes = ConsoleMaxBytes
	}
	if keep < 0 {
		keep = ConsoleKeep
	}

	w := &ConsoleWriter{path: path, maxBytes: maxBytes, keep: keep}
	if err := w.open(); err != nil {
		return nil, err
	}
	return w, nil
}

// open ouvre le fichier courant et relève sa taille. Le verrou doit être tenu,
// sauf à la construction où personne d'autre ne voit encore l'objet.
func (w *ConsoleWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("ouverture du journal de console %s : %w", w.path, err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return fmt.Errorf("taille du journal de console %s : %w", w.path, err)
	}
	w.f = f
	w.size = info.Size()
	return nil
}

func (w *ConsoleWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.f == nil {
		return 0, fmt.Errorf("journal de console %s fermé", w.path)
	}

	// Une écriture plus grosse que le plafond à elle seule est écrite quand
	// même : tronquer un message perdrait précisément la ligne qu'on cherche.
	// Elle sera mise de côté au tour suivant.
	if w.size > 0 && w.size+int64(len(p)) > w.maxBytes {
		if err := w.rotateLocked(); err != nil {
			return 0, err
		}
	}

	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

// rotateLocked met le journal courant de côté et en ouvre un neuf.
//
// Les fichiers conservés sont décalés du plus ancien vers le plus récent, sans
// quoi un renommage écraserait le suivant avant qu'il n'ait été déplacé.
func (w *ConsoleWriter) rotateLocked() error {
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("fermeture du journal de console %s : %w", w.path, err)
	}
	w.f = nil

	// Le plus ancien disparaît. Sans cette suppression, les fichiers
	// s'accumuleraient sous un autre nom et la rotation ne bornerait rien.
	plusAncien := fmt.Sprintf("%s.%d", w.path, w.keep)
	if w.keep > 0 {
		if err := os.Remove(plusAncien); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("suppression de %s : %w", plusAncien, err)
		}
		for i := w.keep - 1; i >= 1; i-- {
			de := fmt.Sprintf("%s.%d", w.path, i)
			vers := fmt.Sprintf("%s.%d", w.path, i+1)
			if err := os.Rename(de, vers); err != nil && !os.IsNotExist(err) {
				return fmt.Errorf("rotation de %s vers %s : %w", de, vers, err)
			}
		}
		if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("rotation de %s : %w", w.path, err)
		}
	} else {
		// Aucun fichier conservé : le journal courant est simplement vidé.
		if err := os.Remove(w.path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("suppression de %s : %w", w.path, err)
		}
	}

	return w.open()
}

// Close ferme le journal. Fermer deux fois n'est pas une erreur : la goroutine
// d'attente et les chemins d'erreur de Start peuvent tous deux y passer.
func (w *ConsoleWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

// ConsoleFiles renvoie les chemins du journal courant et de ceux qui sont
// conservés, du plus récent au plus ancien, en ne gardant que ceux qui
// existent. Utile pour dire à un opérateur où regarder.
func ConsoleFiles(path string, keep int) []string {
	out := []string{}
	if _, err := os.Stat(path); err == nil {
		out = append(out, path)
	}
	for i := 1; i <= keep; i++ {
		p := fmt.Sprintf("%s.%d", path, i)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
