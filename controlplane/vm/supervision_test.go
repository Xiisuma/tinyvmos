package vm

import (
	"context"
	"errors"
	"testing"
	"time"
)

// managerRapide renvoie un gestionnaire dont la supervision est accélérée.
// Vérifier un abandon au bout de dix relances prendrait cinq minutes aux
// valeurs réelles, et un test de cinq minutes ne sera jamais exécuté.
func managerRapide(t *testing.T, fake *FakeVMM, maxFailures int) *Manager {
	t.Helper()
	mgr, err := NewManager(fake, &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}
	mgr.backoffMin = time.Millisecond
	mgr.backoffMax = 2 * time.Millisecond
	mgr.stableWindow = time.Hour // jamais atteinte dans ces tests
	mgr.maxFailures = maxFailures
	return mgr
}

// attendre appelle cond toutes les millisecondes jusqu'à ce qu'elle soit vraie.
func attendre(t *testing.T, delai time.Duration, quoi string, cond func() bool) {
	t.Helper()
	limite := time.Now().Add(delai)
	for time.Now().Before(limite) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("délai dépassé en attendant : %s", quoi)
}

// Le cas de la faille : une VM qui ne démarrera jamais était relancée sans
// fin, occupant le processeur et noyant la panne dans le bruit.
func TestManagerAbandonneApresTropDeRelances(t *testing.T) {
	fake := NewFakeVMM()
	mgr := managerRapide(t, fake, 4)
	ctx := context.Background()

	spec := testSpec(t, "boucle")
	spec.Restart = true
	if err := mgr.Create(ctx, spec); err != nil {
		t.Fatalf("Create : %v", err)
	}
	if err := mgr.Start(ctx, "boucle"); err != nil {
		t.Fatalf("Start : %v", err)
	}

	// À partir de maintenant, tout démarrage échoue : c'est une image cassée,
	// un service qui meurt au premier appel, une configuration fausse.
	fake.SetStartErr(errors.New("image cassée"))
	fake.Crash("boucle")

	var gaveUp time.Time
	var echecs int
	attendre(t, 5*time.Second, "l'abandon du superviseur", func() bool {
		mgr.ReadState(func(snap *Snapshot) {
			gaveUp = snap.VMs["boucle"].GaveUpAt
			echecs = snap.VMs["boucle"].ConsecutiveFailures
		})
		return !gaveUp.IsZero()
	})

	if echecs < 4 {
		t.Fatalf("échecs consécutifs = %d, attendu au moins 4", echecs)
	}

	// Et la boucle s'arrête vraiment : sans cette vérification, l'abandon
	// pourrait n'être qu'une ligne de journal pendant que le superviseur
	// continue.
	avant := fake.StartCount("boucle")
	compteurAvant := echecs
	time.Sleep(50 * time.Millisecond)

	var compteurApres int
	mgr.ReadState(func(snap *Snapshot) {
		compteurApres = snap.VMs["boucle"].ConsecutiveFailures
	})
	if compteurApres != compteurAvant {
		t.Fatalf("le superviseur relance encore : %d échecs, puis %d", compteurAvant, compteurApres)
	}
	if après := fake.StartCount("boucle"); après != avant {
		t.Fatalf("démarrages après abandon : %d, attendu %d", après, avant)
	}

	var statut Status
	var message string
	mgr.ReadState(func(snap *Snapshot) {
		statut = snap.VMs["boucle"].Status
		message = snap.VMs["boucle"].LastError
	})
	if statut != StatusFailed {
		t.Fatalf("statut = %s, attendu %s", statut, StatusFailed)
	}
	if message == "" {
		t.Fatal("l'erreur enregistrée est vide : l'opérateur ne saura pas pourquoi")
	}
}

// L'abandon n'est pas définitif : un démarrage explicite est l'intervention
// qu'il réclamait.
func TestManagerDemarrageExpliciteLeveLaQuarantaine(t *testing.T) {
	fake := NewFakeVMM()
	mgr := managerRapide(t, fake, 3)
	ctx := context.Background()

	spec := testSpec(t, "boucle")
	spec.Restart = true
	if err := mgr.Create(ctx, spec); err != nil {
		t.Fatalf("Create : %v", err)
	}
	if err := mgr.Start(ctx, "boucle"); err != nil {
		t.Fatalf("Start : %v", err)
	}
	fake.SetStartErr(errors.New("image cassée"))
	fake.Crash("boucle")

	attendre(t, 5*time.Second, "l'abandon du superviseur", func() bool {
		var gaveUp time.Time
		mgr.ReadState(func(snap *Snapshot) { gaveUp = snap.VMs["boucle"].GaveUpAt })
		return !gaveUp.IsZero()
	})

	// La cause est corrigée, l'opérateur redémarre.
	fake.SetStartErr(nil)
	if err := mgr.Start(ctx, "boucle"); err != nil {
		t.Fatalf("démarrage explicite après abandon : %v", err)
	}

	mgr.ReadState(func(snap *Snapshot) {
		rec := snap.VMs["boucle"]
		if !rec.GaveUpAt.IsZero() {
			t.Error("la quarantaine n'a pas été levée")
		}
		if rec.ConsecutiveFailures != 0 {
			t.Errorf("compteur d'échecs = %d, attendu 0 : sinon le premier incident suivant rabandonnerait aussitôt", rec.ConsecutiveFailures)
		}
		if rec.Status != StatusRunning {
			t.Errorf("statut = %s, attendu %s", rec.Status, StatusRunning)
		}
	})

	if err := mgr.Stop(ctx, "boucle"); err != nil {
		t.Fatalf("Stop : %v", err)
	}
}

// Le compteur se remet à zéro sur la durée de fonctionnement, et non sur un
// démarrage réussi. C'est la distinction qui compte : une VM qui s'écroule
// aussitôt relancée réussit son démarrage à chaque tour, et remettre le
// compteur à zéro là rendrait l'abandon inatteignable.
func TestManagerLeCompteurSeRemetAZeroSurLaStabilite(t *testing.T) {
	fake := NewFakeVMM()
	mgr := managerRapide(t, fake, 50) // assez haut pour ne pas abandonner ici
	ctx := context.Background()

	spec := testSpec(t, "instable")
	spec.Restart = true
	if err := mgr.Create(ctx, spec); err != nil {
		t.Fatalf("Create : %v", err)
	}
	if err := mgr.Start(ctx, "instable"); err != nil {
		t.Fatalf("Start : %v", err)
	}

	// Deux pannes suivies de relances réussies : le compteur doit monter,
	// puisque la VM n'a jamais tenu la fenêtre de stabilité.
	for tour := 1; tour <= 2; tour++ {
		fake.Crash("instable")
		attendre(t, 5*time.Second, "la relance", func() bool {
			return fake.StartCount("instable") > tour
		})
	}

	var echecs int
	attendre(t, 5*time.Second, "le comptage des deux pannes", func() bool {
		mgr.ReadState(func(snap *Snapshot) { echecs = snap.VMs["instable"].ConsecutiveFailures })
		return echecs >= 2
	})

	// Maintenant la VM tient : la fenêtre est ramenée à zéro, la panne
	// suivante repart d'un compteur neuf.
	mgr.mu.Lock()
	mgr.stableWindow = 0
	mgr.mu.Unlock()

	fake.Crash("instable")
	attendre(t, 5*time.Second, "la remise à zéro du compteur", func() bool {
		mgr.ReadState(func(snap *Snapshot) { echecs = snap.VMs["instable"].ConsecutiveFailures })
		return echecs == 1
	})

	if err := mgr.Stop(ctx, "instable"); err != nil {
		t.Fatalf("Stop : %v", err)
	}
}

// Une VM sans relance demandée n'est pas mise en quarantaine : il n'y a rien
// à abandonner, elle s'arrête et c'est tout.
func TestManagerPasDeQuarantaineSansRelance(t *testing.T) {
	fake := NewFakeVMM()
	mgr := managerRapide(t, fake, 2)
	ctx := context.Background()

	spec := testSpec(t, "unique")
	spec.Restart = false
	if err := mgr.Create(ctx, spec); err != nil {
		t.Fatalf("Create : %v", err)
	}
	if err := mgr.Start(ctx, "unique"); err != nil {
		t.Fatalf("Start : %v", err)
	}
	fake.Crash("unique")

	attendre(t, 5*time.Second, "la prise en compte de l'arrêt", func() bool {
		var statut Status
		mgr.ReadState(func(snap *Snapshot) { statut = snap.VMs["unique"].Status })
		return statut == StatusFailed
	})

	mgr.ReadState(func(snap *Snapshot) {
		if !snap.VMs["unique"].GaveUpAt.IsZero() {
			t.Error("une vm sans relance ne doit pas être mise en quarantaine")
		}
	})
}
