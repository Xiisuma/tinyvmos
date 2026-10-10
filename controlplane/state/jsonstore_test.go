package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Xiisuma/tinyvmos/controlplane/vm"
)

func newStore(t *testing.T) *JSONStore {
	t.Helper()
	s, err := NewJSONStore(filepath.Join(t.TempDir(), "etat", "state.json"))
	if err != nil {
		t.Fatalf("NewJSONStore : %v", err)
	}
	return s
}

// TestLoadMagasinAbsent vérifie qu'un premier démarrage n'est pas une erreur.
func TestLoadMagasinAbsent(t *testing.T) {
	s := newStore(t)
	snap, err := s.Load()
	if err != nil {
		t.Fatalf("un magasin absent ne devrait pas être une erreur : %v", err)
	}
	if snap.Version != vm.SchemaVersion {
		t.Fatalf("version attendue %d, obtenue %d", vm.SchemaVersion, snap.Version)
	}
	if len(snap.VMs) != 0 {
		t.Fatalf("instantané vide attendu, obtenu %d vm", len(snap.VMs))
	}
}

func TestSaveEtLoad(t *testing.T) {
	s := newStore(t)

	now := time.Now().UTC().Truncate(time.Second)
	snap := vm.NewSnapshot()
	snap.VMs["web1"] = &vm.Record{
		Spec: vm.Spec{
			Name: "web1", Service: "web-apache", Zone: "publique",
			VCPU: 1, MemMiB: 256, TapDevice: "tap0",
			IP: "10.42.10.2/24", Gateway: "10.42.10.1", Restart: true,
		},
		Status:    vm.StatusRunning,
		PID:       4242,
		Restarts:  2,
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.Save(snap); err != nil {
		t.Fatalf("Save : %v", err)
	}

	relu, err := s.Load()
	if err != nil {
		t.Fatalf("Load : %v", err)
	}
	rec, ok := relu.VMs["web1"]
	if !ok {
		t.Fatal("web1 absente de l'état relu")
	}
	if rec.Spec.Service != "web-apache" || rec.Status != vm.StatusRunning ||
		rec.PID != 4242 || rec.Restarts != 2 || !rec.Spec.Restart {
		t.Fatalf("enregistrement relu incorrect : %+v", rec)
	}
	if !rec.CreatedAt.Equal(now) {
		t.Fatalf("horodatage attendu %s, obtenu %s", now, rec.CreatedAt)
	}
}

// TestSaveEstAtomique vérifie qu'aucun fichier temporaire ne subsiste et que
// le fichier final est seul dans le dossier.
func TestSaveEstAtomique(t *testing.T) {
	s := newStore(t)
	snap := vm.NewSnapshot()
	snap.VMs["web1"] = &vm.Record{Spec: vm.Spec{Name: "web1"}, Status: vm.StatusCreated}

	for i := 0; i < 5; i++ {
		if err := s.Save(snap); err != nil {
			t.Fatalf("Save %d : %v", i, err)
		}
	}

	entries, err := os.ReadDir(filepath.Dir(s.Path()))
	if err != nil {
		t.Fatalf("lecture du dossier : %v", err)
	}
	if len(entries) != 1 {
		noms := make([]string, len(entries))
		for i, e := range entries {
			noms[i] = e.Name()
		}
		t.Fatalf("un seul fichier attendu, obtenu %d : %v", len(entries), noms)
	}
	if entries[0].Name() != filepath.Base(s.Path()) {
		t.Fatalf("fichier attendu %s, obtenu %s", filepath.Base(s.Path()), entries[0].Name())
	}
}

// TestSauvegardeLisible tient à la raison même d'avoir choisi du JSON : un
// opérateur doit pouvoir lire l'état avec cat et jq sur une machine déployée.
func TestSauvegardeLisible(t *testing.T) {
	s := newStore(t)
	snap := vm.NewSnapshot()
	snap.VMs["web1"] = &vm.Record{Spec: vm.Spec{Name: "web1"}, Status: vm.StatusRunning}
	if err := s.Save(snap); err != nil {
		t.Fatalf("Save : %v", err)
	}

	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("lecture : %v", err)
	}
	texte := string(data)
	if !strings.Contains(texte, "\n  ") {
		t.Fatal("la sortie devrait être indentée pour rester lisible à la main")
	}
	if !strings.HasSuffix(texte, "\n") {
		t.Fatal("le fichier devrait finir par un retour à la ligne")
	}
	var verif map[string]any
	if err := json.Unmarshal(data, &verif); err != nil {
		t.Fatalf("le fichier écrit n'est pas du JSON valide : %v", err)
	}
}

func TestLoadFichierVide(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o750); err != nil {
		t.Fatalf("mkdir : %v", err)
	}
	if err := os.WriteFile(s.Path(), nil, 0o640); err != nil {
		t.Fatalf("écriture : %v", err)
	}
	snap, err := s.Load()
	if err != nil {
		t.Fatalf("un fichier vide devrait donner un instantané vide : %v", err)
	}
	if len(snap.VMs) != 0 {
		t.Fatalf("instantané vide attendu, obtenu %d vm", len(snap.VMs))
	}
}

func TestLoadJSONCorrompu(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o750); err != nil {
		t.Fatalf("mkdir : %v", err)
	}
	if err := os.WriteFile(s.Path(), []byte("{ceci n'est pas du json"), 0o640); err != nil {
		t.Fatalf("écriture : %v", err)
	}
	if _, err := s.Load(); err == nil {
		t.Fatal("un état corrompu devrait être signalé, pas ignoré en silence")
	}
}

// TestLoadRefuseVersionPlusRecente protège contre l'écrasement de champs
// écrits par une version ultérieure du démon.
func TestLoadRefuseVersionPlusRecente(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o750); err != nil {
		t.Fatalf("mkdir : %v", err)
	}
	contenu := []byte(`{"version": 9999, "vms": {}}`)
	if err := os.WriteFile(s.Path(), contenu, 0o640); err != nil {
		t.Fatalf("écriture : %v", err)
	}
	_, err := s.Load()
	if err == nil {
		t.Fatal("un état plus récent que le démon devrait être refusé")
	}
	if !strings.Contains(err.Error(), "9999") {
		t.Fatalf("le message devrait citer la version lue, obtenu %q", err.Error())
	}
}

func TestLoadVersionZeroTraiteeCommeCourante(t *testing.T) {
	s := newStore(t)
	if err := os.MkdirAll(filepath.Dir(s.Path()), 0o750); err != nil {
		t.Fatalf("mkdir : %v", err)
	}
	if err := os.WriteFile(s.Path(), []byte(`{"vms": {}}`), 0o640); err != nil {
		t.Fatalf("écriture : %v", err)
	}
	snap, err := s.Load()
	if err != nil {
		t.Fatalf("Load : %v", err)
	}
	if snap.Version != vm.SchemaVersion {
		t.Fatalf("version attendue %d, obtenue %d", vm.SchemaVersion, snap.Version)
	}
}

func TestSaveRefuseEtatNul(t *testing.T) {
	if err := newStore(t).Save(nil); err == nil {
		t.Fatal("Save(nil) devrait échouer")
	}
}

// TestDroitsDuFichier : l'état contient la topologie des VM. Il n'a pas à être
// lisible par tout le monde.
func TestDroitsDuFichier(t *testing.T) {
	s := newStore(t)
	if err := s.Save(vm.NewSnapshot()); err != nil {
		t.Fatalf("Save : %v", err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatalf("Stat : %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o640 {
		t.Fatalf("droits attendus 0640, obtenus %#o", mode)
	}
}
