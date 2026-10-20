package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Test à l'échelle du gestionnaire.
//
// Le cahier des charges parle de vingt à trente micro-VM sur une machine
// modeste. Tout avait été éprouvé à deux ou trois, et plusieurs décisions de
// conception ne se jugent qu'au nombre : la plage d'identités, le magasin
// d'état en JSON réécrit en entier à chaque opération, le nombre d'écritures.
//
// Aucune micro-VM n'est réellement démarrée ici : l'hyperviseur factice suffit
// à éprouver ce qui change avec le nombre. Le démarrage réel de trente VM
// demande KVM et vit dans tests/.
const echelleVMs = 30

// specEchelle fabrique une spécification complète sans créer trente dossiers
// temporaires : les artefacts sont partagés, ce que le gestionnaire accepte.
func specEchelle(t *testing.T, dir, nom string, memMiB int64) Spec {
	t.Helper()
	noyau := filepath.Join(dir, "vmlinux")
	image := filepath.Join(dir, "rootfs.ext4")
	for _, p := range []string{noyau, image} {
		if _, err := os.Stat(p); err != nil {
			if err := os.WriteFile(p, []byte("factice"), 0o600); err != nil {
				t.Fatalf("création de %s : %v", p, err)
			}
		}
	}
	return Spec{
		Name:       nom,
		Service:    "web-apache",
		Zone:       "publique",
		VCPU:       1,
		MemMiB:     memMiB,
		KernelPath: noyau,
		ImagePath:  image,
		TapDevice:  "tap-" + nom,
		MAC:        DeterministicMAC(nom),
		IP:         "10.42.10.2/24",
		Gateway:    "10.42.10.1",
		UID:        IdentityMin,
		GID:        IdentityMin,
	}
}

// Trente VM doivent recevoir trente identités distinctes, toutes dans la plage
// réservée. Une collision donnerait à une VM les droits d'une autre, ce que
// l'isolation par identité est précisément censée empêcher.
func TestEchelleIdentitesDistinctes(t *testing.T) {
	mgr, err := NewManager(NewFakeVMM(), &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}

	vues := map[int]string{}
	for i := 0; i < echelleVMs; i++ {
		nom := fmt.Sprintf("vm%02d", i)
		identite, err := mgr.IdentityFor(nom)
		if err != nil {
			t.Fatalf("identité pour %s : %v", nom, err)
		}
		if identite < IdentityMin || identite > IdentityMax {
			t.Fatalf("%s reçoit %d, hors de la plage réservée %d-%d", nom, identite, IdentityMin, IdentityMax)
		}
		if precedent, dejaVu := vues[identite]; dejaVu {
			t.Fatalf("collision d'identité %d entre %s et %s", identite, precedent, nom)
		}
		vues[identite] = nom
	}

	if len(vues) != echelleVMs {
		t.Fatalf("%d identités distinctes pour %d vm", len(vues), echelleVMs)
	}
}

// Le magasin d'état est un fichier JSON réécrit en entier à chaque opération.
// Ce choix n'est tenable que si l'état reste petit et si le nombre d'écritures
// reste proportionnel aux opérations, pas à leur carré.
func TestEchelleMagasinDEtatTenable(t *testing.T) {
	dir := t.TempDir()
	store := &memStore{}
	mgr, err := NewManager(NewFakeVMM(), store, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}
	ctx := context.Background()

	// Budget modeste, pour que trente vm tiennent sur la machine de test.
	const memParVM = 64

	debut := time.Now()
	for i := 0; i < echelleVMs; i++ {
		nom := fmt.Sprintf("vm%02d", i)
		spec := specEchelle(t, dir, nom, memParVM)
		identite, err := mgr.IdentityFor(nom)
		if err != nil {
			t.Fatalf("identité pour %s : %v", nom, err)
		}
		spec.UID, spec.GID = identite, identite
		if err := mgr.Create(ctx, spec); err != nil {
			t.Fatalf("création de %s : %v", nom, err)
		}
		if err := mgr.Start(ctx, nom); err != nil {
			t.Fatalf("démarrage de %s : %v", nom, err)
		}
	}
	duree := time.Since(debut)

	// Deux opérations par VM, plus une écriture par attribution d'identité :
	// linéaire. Une borne large, qui attrape un comportement quadratique sans
	// se briser sur un détail d'implémentation.
	if max := 6 * echelleVMs; store.saves > max {
		t.Errorf("%d écritures de l'état pour %d vm : le coût n'est plus linéaire (borne %d)",
			store.saves, echelleVMs, max)
	}

	if duree > 5*time.Second {
		t.Errorf("mise en route de %d vm en %s avec un hyperviseur factice : trop long", echelleVMs, duree)
	}

	// La documentation du magasin annonce une cinquantaine de kilo-octets pour
	// une trentaine de VM. Une annonce non vérifiée finit fausse.
	var taille int
	mgr.ReadState(func(snap *Snapshot) {
		data, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			t.Fatalf("sérialisation : %v", err)
		}
		taille = len(data)
	})
	if taille > 100*1024 {
		t.Errorf("état de %d octets pour %d vm : le fichier JSON réécrit en entier n'est plus le bon support",
			taille, echelleVMs)
	}
	t.Logf("état de %d octets pour %d vm, %d écritures", taille, echelleVMs, store.saves)

	if err := mgr.StopAll(ctx); err != nil {
		t.Fatalf("StopAll sur %d vm : %v", echelleVMs, err)
	}
}

// Une installation qui va et vient — on crée, on supprime, on recrée — ne doit
// pas épuiser la plage d'identités. Sans réutilisation, mille créations
// suffiraient à la vider.
func TestEchelleLesIdentitesRenduesSontReprises(t *testing.T) {
	dir := t.TempDir()
	mgr, err := NewManager(NewFakeVMM(), &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}
	ctx := context.Background()

	// Un socle de vm qui restent en place, pour que la réutilisation porte sur
	// un trou au milieu de la plage et non sur la première valeur libre.
	for i := 0; i < 5; i++ {
		nom := fmt.Sprintf("socle%d", i)
		identite, err := mgr.IdentityFor(nom)
		if err != nil {
			t.Fatalf("identité : %v", err)
		}
		spec := specEchelle(t, dir, nom, 64)
		spec.UID, spec.GID = identite, identite
		if err := mgr.Create(ctx, spec); err != nil {
			t.Fatalf("création : %v", err)
		}
	}

	var premiere int
	for tour := 0; tour < 200; tour++ {
		identite, err := mgr.IdentityFor("ephemere")
		if err != nil {
			t.Fatalf("tour %d : %v", tour, err)
		}
		if tour == 0 {
			premiere = identite
		} else if identite != premiere {
			t.Fatalf("tour %d : identité %d au lieu de %d, la plage se consomme", tour, identite, premiere)
		}

		spec := specEchelle(t, dir, "ephemere", 64)
		spec.UID, spec.GID = identite, identite
		if err := mgr.Create(ctx, spec); err != nil {
			t.Fatalf("tour %d : création : %v", tour, err)
		}
		if err := mgr.Delete(ctx, "ephemere"); err != nil {
			t.Fatalf("tour %d : suppression : %v", tour, err)
		}
	}
}

// Trente VM à budget réaliste doivent passer l'admission sur une machine
// modeste, sinon le plafond serait réglé trop serré pour l'usage prévu.
func TestEchelleTrenteVMTiennentSurUneMachineModeste(t *testing.T) {
	p := DefaultCapacityPolicy()
	p.MaxVMs = echelleVMs

	// 8 Gio, ce que la machine cible a de plausible.
	host := HostResources{MemTotalMiB: 8192, CPUs: 4}

	budgets := make([]Budget, 0, echelleVMs)
	for i := 0; i < echelleVMs; i++ {
		budgets = append(budgets, Budget{Name: fmt.Sprintf("vm%02d", i), VCPU: 1, MemMiB: 192})
	}

	rapport, err := p.Admit(host, budgets)
	if err != nil {
		t.Fatalf("trente vm à 192 Mio refusées sur une machine de 8 Gio : %v", err)
	}
	t.Logf("trente vm à 192 Mio : %d Mio engagés sur %d, %d Mio libres",
		rapport.MemCommitted(), rapport.MemTotal, rapport.MemFree())

	// Et le même déploiement à 512 Mio par vm ne tient pas : sans ce contrôle
	// inverse, le test précédent ne prouverait pas que le plafond fonctionne.
	for i := range budgets {
		budgets[i].MemMiB = 512
	}
	if _, err := p.Admit(host, budgets); err == nil {
		t.Fatal("trente vm à 512 Mio devraient être refusées sur une machine de 8 Gio")
	}
}
