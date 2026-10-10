package vm

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// memStore est un magasin d'état en mémoire, pour que les tests du
// gestionnaire n'aient pas à toucher le disque.
type memStore struct {
	snap  *Snapshot
	saves int
}

func (s *memStore) Load() (*Snapshot, error) {
	if s.snap == nil {
		s.snap = NewSnapshot()
	}
	return s.snap, nil
}

func (s *memStore) Save(snap *Snapshot) error {
	s.snap = snap
	s.saves++
	return nil
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func testSpec(t *testing.T, name string) Spec {
	t.Helper()
	dir := t.TempDir()
	kernel := filepath.Join(dir, "vmlinux")
	image := filepath.Join(dir, "rootfs.ext4")
	for _, p := range []string{kernel, image} {
		if err := os.WriteFile(p, []byte("factice"), 0o600); err != nil {
			t.Fatalf("création du fichier factice %s : %v", p, err)
		}
	}
	return Spec{
		Name:       name,
		Service:    "web-apache",
		Zone:       "publique",
		VCPU:       1,
		MemMiB:     128,
		KernelPath: kernel,
		ImagePath:  image,
		TapDevice:  "tap0",
		MAC:        "02:fc:00:00:00:01",
		IP:         "10.42.10.2/24",
		Gateway:    "10.42.10.1",
	}
}

func TestSpecValidate(t *testing.T) {
	base := testSpec(t, "web1")

	if err := base.Validate(); err != nil {
		t.Fatalf("une spécification valide a été refusée : %v", err)
	}

	cases := []struct {
		nom     string
		modifie func(*Spec)
		attendu string
	}{
		{"nom vide", func(s *Spec) { s.Name = "" }, "nom de VM invalide"},
		{"nom en majuscules", func(s *Spec) { s.Name = "Web1" }, "nom de VM invalide"},
		{"nom avec tiret final", func(s *Spec) { s.Name = "web1-" }, "nom de VM invalide"},
		{"service absent", func(s *Spec) { s.Service = "" }, "service non renseigné"},
		{"vcpu nul", func(s *Spec) { s.VCPU = 0 }, "vcpu doit valoir au moins 1"},
		{"mémoire trop basse", func(s *Spec) { s.MemMiB = 16 }, "mem_mib doit valoir au moins 32"},
		{"noyau introuvable", func(s *Spec) { s.KernelPath = "/n/existe/pas" }, "noyau introuvable"},
		{"image introuvable", func(s *Spec) { s.ImagePath = "/n/existe/pas" }, "image introuvable"},
		{"tap absent", func(s *Spec) { s.TapDevice = "" }, "interface tap non renseignée"},
		{"mac invalide", func(s *Spec) { s.MAC = "pas-une-mac" }, "adresse MAC invalide"},
		{"ip sans préfixe", func(s *Spec) { s.IP = "10.42.10.2" }, "adresse IP invalide"},
		{"passerelle invalide", func(s *Spec) { s.Gateway = "10.42.10" }, "passerelle invalide"},
	}

	for _, tc := range cases {
		t.Run(tc.nom, func(t *testing.T) {
			s := base
			tc.modifie(&s)
			err := s.Validate()
			if err == nil {
				t.Fatalf("spécification invalide acceptée")
			}
			if !strings.Contains(err.Error(), tc.attendu) {
				t.Fatalf("message attendu contenant %q, obtenu %q", tc.attendu, err.Error())
			}
		})
	}
}

func TestStatusTransitions(t *testing.T) {
	autorisees := []struct{ de, vers Status }{
		{StatusCreated, StatusStarting},
		{StatusStarting, StatusRunning},
		{StatusRunning, StatusStopping},
		{StatusStopping, StatusStopped},
		{StatusStopped, StatusStarting},
		{StatusFailed, StatusStarting},
		{StatusRunning, StatusRunning},
	}
	for _, tc := range autorisees {
		if !tc.de.CanTransitionTo(tc.vers) {
			t.Errorf("transition %s vers %s devrait être autorisée", tc.de, tc.vers)
		}
	}

	interdites := []struct{ de, vers Status }{
		{StatusCreated, StatusRunning},
		{StatusStopping, StatusRunning},
		{StatusStarting, StatusStopping},
	}
	for _, tc := range interdites {
		if tc.de.CanTransitionTo(tc.vers) {
			t.Errorf("transition %s vers %s devrait être interdite", tc.de, tc.vers)
		}
	}
}

func TestManagerCycleDeVie(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeVMM()
	store := &memStore{}
	mgr, err := NewManager(fake, store, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}

	spec := testSpec(t, "web1")

	if err := mgr.Create(ctx, spec); err != nil {
		t.Fatalf("Create : %v", err)
	}
	rec, err := mgr.Get("web1")
	if err != nil {
		t.Fatalf("Get : %v", err)
	}
	if rec.Status != StatusCreated {
		t.Fatalf("statut attendu %s, obtenu %s", StatusCreated, rec.Status)
	}

	if err := mgr.Create(ctx, spec); err == nil {
		t.Fatal("une seconde création du même nom devrait échouer")
	}

	if err := mgr.Start(ctx, "web1"); err != nil {
		t.Fatalf("Start : %v", err)
	}
	rec, _ = mgr.Get("web1")
	if rec.Status != StatusRunning {
		t.Fatalf("statut attendu %s, obtenu %s", StatusRunning, rec.Status)
	}
	if rec.PID == 0 {
		t.Fatal("le PID devrait être renseigné après démarrage")
	}
	if !fake.IsRunning("web1") {
		t.Fatal("la vm simulée devrait tourner")
	}

	if err := mgr.Start(ctx, "web1"); err == nil {
		t.Fatal("démarrer une vm déjà démarrée devrait échouer")
	}

	if err := mgr.Stop(ctx, "web1"); err != nil {
		t.Fatalf("Stop : %v", err)
	}
	rec, _ = mgr.Get("web1")
	if rec.Status != StatusStopped {
		t.Fatalf("statut attendu %s, obtenu %s", StatusStopped, rec.Status)
	}
	if rec.PID != 0 {
		t.Fatal("le PID devrait être remis à zéro après arrêt")
	}

	if err := mgr.Delete(ctx, "web1"); err != nil {
		t.Fatalf("Delete : %v", err)
	}
	if _, err := mgr.Get("web1"); err == nil {
		t.Fatal("la vm devrait avoir disparu de l'état")
	}
}

func TestManagerVMInconnue(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(NewFakeVMM(), &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}
	for nom, action := range map[string]func() error{
		"Start":  func() error { return mgr.Start(ctx, "absente") },
		"Stop":   func() error { return mgr.Stop(ctx, "absente") },
		"Delete": func() error { return mgr.Delete(ctx, "absente") },
	} {
		if err := action(); err == nil {
			t.Errorf("%s sur une vm inconnue devrait échouer", nom)
		}
	}
}

// TestManagerRelanceApresCrash vérifie que le superviseur relance une VM qui
// s'arrête sans qu'on le lui ait demandé, et qu'il compte la relance.
func TestManagerRelanceApresCrash(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeVMM()
	mgr, err := NewManager(fake, &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}

	spec := testSpec(t, "web1")
	spec.Restart = true

	if err := mgr.Create(ctx, spec); err != nil {
		t.Fatalf("Create : %v", err)
	}
	if err := mgr.Start(ctx, "web1"); err != nil {
		t.Fatalf("Start : %v", err)
	}

	fake.Crash("web1")

	// La première temporisation est d'une seconde ; on laisse de la marge.
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec, err := mgr.Get("web1")
		if err != nil {
			t.Fatalf("Get : %v", err)
		}
		if rec.Status == StatusRunning && rec.Restarts >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("la vm n'a pas été relancée : statut %s, relances %d, dernière erreur %q",
				rec.Status, rec.Restarts, rec.LastError)
		}
		time.Sleep(25 * time.Millisecond)
	}

	if got := fake.StartCount("web1"); got < 2 {
		t.Fatalf("démarrages attendus au moins 2, obtenu %d", got)
	}

	if err := mgr.Stop(ctx, "web1"); err != nil {
		t.Fatalf("Stop : %v", err)
	}
}

// TestManagerPasDeRelanceSiArretDemande vérifie qu'un arrêt volontaire ne
// déclenche pas de relance. Sans cela, impossible d'arrêter une VM.
func TestManagerPasDeRelanceSiArretDemande(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeVMM()
	mgr, err := NewManager(fake, &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}

	spec := testSpec(t, "web1")
	spec.Restart = true

	if err := mgr.Create(ctx, spec); err != nil {
		t.Fatalf("Create : %v", err)
	}
	if err := mgr.Start(ctx, "web1"); err != nil {
		t.Fatalf("Start : %v", err)
	}
	if err := mgr.Stop(ctx, "web1"); err != nil {
		t.Fatalf("Stop : %v", err)
	}

	// Largement plus que la temporisation minimale de relance.
	time.Sleep(2 * time.Second)

	rec, _ := mgr.Get("web1")
	if rec.Status != StatusStopped {
		t.Fatalf("statut attendu %s, obtenu %s", StatusStopped, rec.Status)
	}
	if rec.Restarts != 0 {
		t.Fatalf("aucune relance attendue, obtenu %d", rec.Restarts)
	}
	if fake.IsRunning("web1") {
		t.Fatal("la vm simulée ne devrait pas tourner")
	}
}

// TestManagerSansRelanceSiDesactivee vérifie qu'une VM dont Restart est faux
// reste en échec après un crash.
func TestManagerSansRelanceSiDesactivee(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeVMM()
	mgr, err := NewManager(fake, &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}

	spec := testSpec(t, "web1") // Restart reste à faux
	if err := mgr.Create(ctx, spec); err != nil {
		t.Fatalf("Create : %v", err)
	}
	if err := mgr.Start(ctx, "web1"); err != nil {
		t.Fatalf("Start : %v", err)
	}

	fake.Crash("web1")

	deadline := time.Now().Add(5 * time.Second)
	for {
		rec, _ := mgr.Get("web1")
		if rec.Status == StatusFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("statut attendu %s, obtenu %s", StatusFailed, rec.Status)
		}
		time.Sleep(25 * time.Millisecond)
	}

	time.Sleep(2 * time.Second)
	if got := fake.StartCount("web1"); got != 1 {
		t.Fatalf("un seul démarrage attendu, obtenu %d", got)
	}
}

func TestManagerListeTriee(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(NewFakeVMM(), &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}
	for _, name := range []string{"web2", "dns", "ssh"} {
		if err := mgr.Create(ctx, testSpec(t, name)); err != nil {
			t.Fatalf("Create %s : %v", name, err)
		}
	}
	got := mgr.List()
	attendu := []string{"dns", "ssh", "web2"}
	if len(got) != len(attendu) {
		t.Fatalf("%d vm attendues, obtenu %d", len(attendu), len(got))
	}
	for i, name := range attendu {
		if got[i].Spec.Name != name {
			t.Fatalf("position %d : %s attendu, obtenu %s", i, name, got[i].Spec.Name)
		}
	}
}

func TestManagerStopAll(t *testing.T) {
	ctx := context.Background()
	fake := NewFakeVMM()
	mgr, err := NewManager(fake, &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}
	for _, name := range []string{"web1", "web2"} {
		if err := mgr.Create(ctx, testSpec(t, name)); err != nil {
			t.Fatalf("Create %s : %v", name, err)
		}
		if err := mgr.Start(ctx, name); err != nil {
			t.Fatalf("Start %s : %v", name, err)
		}
	}

	if err := mgr.StopAll(ctx); err != nil {
		t.Fatalf("StopAll : %v", err)
	}
	for _, name := range []string{"web1", "web2"} {
		if fake.IsRunning(name) {
			t.Errorf("%s devrait être arrêtée", name)
		}
	}
}

func TestKernelArgs(t *testing.T) {
	spec := Spec{IP: "10.42.10.2/24", Gateway: "10.42.10.1"}
	args := kernelArgs(spec)

	for _, attendu := range []string{
		"console=ttyS0",
		"panic=1",
		"pci=off",
		"root=/dev/vda rw",
		"ip=10.42.10.2::10.42.10.1:255.255.255.0::eth0:off",
		"tinyvmos.ip=10.42.10.2/24",
		"tinyvmos.gw=10.42.10.1",
	} {
		if !strings.Contains(args, attendu) {
			t.Errorf("ligne de commande du noyau sans %q : %s", attendu, args)
		}
	}
}
