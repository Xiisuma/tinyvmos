package vm

import (
	"context"
	"strings"
	"testing"
)

func TestAllocateIdentity(t *testing.T) {
	id, err := AllocateIdentity(nil)
	if err != nil {
		t.Fatalf("AllocateIdentity : %v", err)
	}
	if id != IdentityMin {
		t.Fatalf("%d attendu pour la premiere attribution, obtenu %d", IdentityMin, id)
	}

	pris := map[int]bool{IdentityMin: true, IdentityMin + 1: true}
	id, err = AllocateIdentity(pris)
	if err != nil {
		t.Fatalf("AllocateIdentity : %v", err)
	}
	if id != IdentityMin+2 {
		t.Fatalf("%d attendu, obtenu %d", IdentityMin+2, id)
	}
}

// TestAllocateIdentityReutiliseLesTrous : sans reutilisation, la plage
// s'epuiserait au fil des creations et suppressions.
func TestAllocateIdentityReutiliseLesTrous(t *testing.T) {
	pris := map[int]bool{IdentityMin: true, IdentityMin + 2: true}
	id, err := AllocateIdentity(pris)
	if err != nil {
		t.Fatalf("AllocateIdentity : %v", err)
	}
	if id != IdentityMin+1 {
		t.Fatalf("le trou %d devrait etre repris, obtenu %d", IdentityMin+1, id)
	}
}

func TestAllocateIdentityEpuisement(t *testing.T) {
	pris := map[int]bool{}
	for id := IdentityMin; id <= IdentityMax; id++ {
		pris[id] = true
	}
	if _, err := AllocateIdentity(pris); err == nil {
		t.Fatal("une plage epuisee devrait etre signalee")
	}
}

// TestValidateIdentity : un identifiant hors plage pourrait correspondre a un
// vrai compte du systeme, et zero serait root.
func TestValidateIdentity(t *testing.T) {
	if err := ValidateIdentity(IdentityMin); err != nil {
		t.Errorf("identifiant valide refuse : %v", err)
	}
	if err := ValidateIdentity(IdentityMax); err != nil {
		t.Errorf("identifiant valide refuse : %v", err)
	}

	cas := []struct {
		id      int
		attendu string
	}{
		{0, "jamais tourner en root"},
		{1000, "hors de la plage"},
		{IdentityMin - 1, "hors de la plage"},
		{IdentityMax + 1, "hors de la plage"},
		{65534, "hors de la plage"},
	}
	for _, c := range cas {
		err := ValidateIdentity(c.id)
		if err == nil {
			t.Errorf("identifiant %d devrait etre refuse", c.id)
			continue
		}
		if !strings.Contains(err.Error(), c.attendu) {
			t.Errorf("identifiant %d : message attendu contenant %q, obtenu %q", c.id, c.attendu, err)
		}
	}
}

// TestSpecValidateRefuseIdentiteManquante : une Spec sans identite ferait
// tourner la VM en root, ce qui annule tout le confinement.
func TestSpecValidateRefuseIdentiteManquante(t *testing.T) {
	spec := testSpec(t, "web1")
	spec.UID = 0
	spec.GID = 0
	err := spec.Validate()
	if err == nil {
		t.Fatal("une spec sans identite devrait etre refusee")
	}
	if !strings.Contains(err.Error(), "jamais tourner en root") {
		t.Fatalf("message inattendu : %v", err)
	}
}

// TestIdentityForDistinctesEtStables : deux VM ne doivent jamais partager une
// identite, et celle d'une VM ne doit pas changer d'un appel a l'autre.
func TestIdentityForDistinctesEtStables(t *testing.T) {
	mgr, err := NewManager(NewFakeVMM(), &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}

	vues := map[int]string{}
	for _, nom := range []string{"web1", "web2", "dns", "ssh"} {
		id, err := mgr.IdentityFor(nom)
		if err != nil {
			t.Fatalf("IdentityFor %s : %v", nom, err)
		}
		if err := ValidateIdentity(id); err != nil {
			t.Fatalf("identite attribuee invalide pour %s : %v", nom, err)
		}
		if autre, dup := vues[id]; dup {
			t.Fatalf("vm %s et %s partagent l identifiant %d", autre, nom, id)
		}
		vues[id] = nom

		encore, err := mgr.IdentityFor(nom)
		if err != nil {
			t.Fatalf("IdentityFor %s : %v", nom, err)
		}
		if encore != id {
			t.Fatalf("l identite de %s doit rester stable : %d puis %d", nom, id, encore)
		}
	}
}

// TestIdentityLibereeALaSuppression : sinon la plage s'epuise.
func TestIdentityLibereeALaSuppression(t *testing.T) {
	ctx := context.Background()
	mgr, err := NewManager(NewFakeVMM(), &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}

	spec := testSpec(t, "web1")
	id, err := mgr.IdentityFor("web1")
	if err != nil {
		t.Fatalf("IdentityFor : %v", err)
	}
	spec.UID, spec.GID = id, id

	if err := mgr.Create(ctx, spec); err != nil {
		t.Fatalf("Create : %v", err)
	}
	if err := mgr.Delete(ctx, "web1"); err != nil {
		t.Fatalf("Delete : %v", err)
	}

	if _, present := mgr.Identities()["web1"]; present {
		t.Fatal("l identite devrait etre rendue a la suppression")
	}

	// Une nouvelle vm doit pouvoir reprendre l identifiant libere.
	repris, err := mgr.IdentityFor("web2")
	if err != nil {
		t.Fatalf("IdentityFor : %v", err)
	}
	if repris != id {
		t.Fatalf("l identifiant libere %d devrait etre repris, obtenu %d", id, repris)
	}
}

// TestIdentityPersistee : apres redemarrage du demon, une vm doit retrouver
// son identite, sinon ses fichiers de travail lui deviendraient illisibles.
func TestIdentityPersistee(t *testing.T) {
	store := &memStore{}

	mgr1, err := NewManager(NewFakeVMM(), store, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}
	id, err := mgr1.IdentityFor("web1")
	if err != nil {
		t.Fatalf("IdentityFor : %v", err)
	}

	// Nouveau gestionnaire sur le meme magasin : c'est ce que fait un
	// redemarrage du demon.
	mgr2, err := NewManager(NewFakeVMM(), store, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}
	encore, err := mgr2.IdentityFor("web1")
	if err != nil {
		t.Fatalf("IdentityFor : %v", err)
	}
	if encore != id {
		t.Fatalf("identite non persistee : %d puis %d", id, encore)
	}
}
