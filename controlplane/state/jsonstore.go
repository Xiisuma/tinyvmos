// Package state implémente le magasin d'état du démon.
//
// Le support retenu est un fichier JSON réécrit de façon atomique. L'état du
// démon est petit par construction — l'architecture est déclarative, l'état
// constaté n'est qu'une projection de tinyvmos.yaml — et une trentaine de VM
// représentent une cinquantaine de kilo-octets.
//
// Ce choix garde le binaire statique et la compilation croisée ARM64 gratuites,
// ce qu'un SQLite avec CGO interdirait, et laisse un état lisible avec cat et
// jq sur une machine déployée en clientèle.
//
// Deux choses restent dehors, délibérément :
//   - le journal d'audit, en ajout seul et chaîné par hachage : le réécrire
//     entièrement à chaque entrée détruirait la propriété recherchée ;
//   - les secrets, qui sont chiffrés dans leur propre magasin.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/Xiisuma/tinyvmos/controlplane/vm"
)

// JSONStore écrit l'état dans un fichier JSON unique.
//
// Le démon est seul écrivain. Le mutex protège contre deux goroutines du même
// démon, pas contre deux processus : ce cas n'existe pas dans l'architecture.
type JSONStore struct {
	path string
	mu   sync.Mutex
}

// NewJSONStore renvoie un magasin adossé au fichier indiqué. Le dossier parent
// est créé s'il manque.
func NewJSONStore(path string) (*JSONStore, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("création du dossier d'état %s : %w", dir, err)
	}
	return &JSONStore{path: path}, nil
}

// Path renvoie le chemin du fichier d'état.
func (s *JSONStore) Path() string { return s.path }

// Load lit l'état. Un fichier absent renvoie un instantané vide : le premier
// démarrage est un cas normal, pas une erreur.
func (s *JSONStore) Load() (*vm.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return vm.NewSnapshot(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("lecture de l'état %s : %w", s.path, err)
	}
	if len(data) == 0 {
		return vm.NewSnapshot(), nil
	}

	var snap vm.Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, fmt.Errorf("état %s illisible : %w", s.path, err)
	}
	if snap.VMs == nil {
		snap.VMs = map[string]*vm.Record{}
	}

	if err := migrate(&snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// migrate amène un état écrit par une version antérieure au schéma courant.
// Un état plus récent que le démon est refusé : une version ultérieure a pu y
// écrire des champs que celle-ci effacerait en réécrivant.
func migrate(snap *vm.Snapshot) error {
	switch {
	case snap.Version == 0:
		// Aucun état publié n'a jamais porté la version 0 ; on la traite comme
		// la version 1 plutôt que de refuser le démarrage.
		snap.Version = vm.SchemaVersion
		return nil
	case snap.Version > vm.SchemaVersion:
		return fmt.Errorf("état en version %d, ce démon ne connaît que la version %d : mettre le démon à jour plutôt que de risquer d'écraser des champs inconnus",
			snap.Version, vm.SchemaVersion)
	default:
		return nil
	}
}

// Save écrit l'état de façon atomique.
//
// Le fichier temporaire est créé dans le même dossier que la cible, pour que
// le rename reste dans le même système de fichiers et donc atomique. Le fsync
// du fichier garantit que les données sont sur le disque avant le rename ; le
// fsync du dossier garantit que le rename lui-même survit à une coupure.
func (s *JSONStore) Save(snap *vm.Snapshot) error {
	if snap == nil {
		return errors.New("état nul")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if snap.Version == 0 {
		snap.Version = vm.SchemaVersion
	}
	if snap.VMs == nil {
		snap.VMs = map[string]*vm.Record{}
	}

	// Indenté : l'intérêt de ce support est qu'un opérateur puisse le lire.
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("sérialisation de l'état : %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".tinyvmos-state-*.tmp")
	if err != nil {
		return fmt.Errorf("création du fichier temporaire d'état : %w", err)
	}
	tmpName := tmp.Name()

	// En cas d'échec en cours de route, ne pas laisser de temporaire derrière.
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("écriture de l'état : %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("fsync de l'état : %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("fermeture du fichier temporaire d'état : %w", err)
	}
	if err := os.Chmod(tmpName, 0o640); err != nil {
		return fmt.Errorf("droits du fichier d'état : %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("remplacement atomique de l'état : %w", err)
	}
	tmpName = "" // le rename a réussi : plus rien à nettoyer

	return syncDir(dir)
}

// syncDir force l'écriture de l'entrée de répertoire créée par le rename.
// Sans cela, une coupure d'alimentation juste après peut laisser l'ancien nom.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("ouverture du dossier d'état %s : %w", dir, err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("fsync du dossier d'état %s : %w", dir, err)
	}
	return nil
}
