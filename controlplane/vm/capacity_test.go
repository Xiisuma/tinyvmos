package vm

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// hoteDeTest décrit une machine fictive, pour que les tests ne dépendent pas
// de celle sur laquelle ils tournent.
func hoteDeTest(memMiB int64, cpus int) HostResources {
	return HostResources{MemTotalMiB: memMiB, CPUs: cpus}
}

func TestReadHostResources(t *testing.T) {
	host, err := ReadHostResources()
	if err != nil {
		t.Fatalf("ReadHostResources : %v", err)
	}
	if host.MemTotalMiB <= 0 {
		t.Fatalf("mémoire totale = %d Mio, attendu une valeur positive", host.MemTotalMiB)
	}
	if host.CPUs < 1 {
		t.Fatalf("cœurs = %d, attendu au moins 1", host.CPUs)
	}
}

// La réserve doit convenir aux deux extrêmes : un pourcentage seul est trop
// faible sur une petite machine, un montant fixe trop faible sur une grande.
func TestHostReserve(t *testing.T) {
	p := DefaultCapacityPolicy()

	if got := p.HostReserve(hoteDeTest(2048, 2)); got != MinHostReserveMiB {
		t.Fatalf("petite machine : réserve = %d Mio, attendu le plancher %d", got, MinHostReserveMiB)
	}
	if got := p.HostReserve(hoteDeTest(32768, 8)); got != 3276 {
		t.Fatalf("grande machine : réserve = %d Mio, attendu 10 %% soit 3276", got)
	}

	p.HostReserveMiB = 128
	if got := p.HostReserve(hoteDeTest(32768, 8)); got != 128 {
		t.Fatalf("réserve explicite ignorée : %d Mio", got)
	}
}

func TestAdmitAccepteCeQuiTient(t *testing.T) {
	p := DefaultCapacityPolicy()
	host := hoteDeTest(8192, 4)

	budgets := []Budget{
		{Name: "web1", VCPU: 1, MemMiB: 256},
		{Name: "web2", VCPU: 1, MemMiB: 256},
		{Name: "dns", VCPU: 1, MemMiB: 128},
	}

	rapport, err := p.Admit(host, budgets)
	if err != nil {
		t.Fatalf("déploiement raisonnable refusé : %v", err)
	}
	if rapport.MemRequested != 640 {
		t.Fatalf("mémoire demandée = %d Mio, attendu 640", rapport.MemRequested)
	}
	if rapport.MemOverhead != 3*VMOverheadMiB {
		t.Fatalf("surcoût = %d Mio, attendu %d", rapport.MemOverhead, 3*VMOverheadMiB)
	}
	if rapport.MemFree() <= 0 {
		t.Fatalf("libre après déploiement = %d Mio", rapport.MemFree())
	}
	if len(rapport.Warnings) != 0 {
		t.Fatalf("avertissements inattendus : %v", rapport.Warnings)
	}
}

// Le cas que la faille décrivait : la somme dépasse, personne ne disait rien,
// et c'est le tueur de mémoire du noyau qui tranchait.
func TestAdmitRefuseLaSommeQuiDepasse(t *testing.T) {
	p := DefaultCapacityPolicy()
	host := hoteDeTest(8192, 4)

	budgets := []Budget{}
	for _, nom := range []string{"a", "b", "c", "d", "e"} {
		budgets = append(budgets, Budget{Name: nom, VCPU: 1, MemMiB: 2048})
	}

	rapport, err := p.Admit(host, budgets)
	if err == nil {
		t.Fatal("10 Gio de vm sur une machine de 8 Gio doivent être refusés")
	}
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("erreur = %v, attendu une erreur de capacité", err)
	}
	// Le message doit donner les chiffres : un refus sans chiffres n'aide pas
	// à décider quoi réduire.
	for _, attendu := range []string{"10240", "8192", "-host-reserve"} {
		if !strings.Contains(err.Error(), attendu) {
			t.Errorf("le message ne contient pas %q : %v", attendu, err)
		}
	}
	if rapport.MemFree() >= 0 {
		t.Fatalf("le rapport devrait montrer un déficit, libre = %d Mio", rapport.MemFree())
	}
}

// La réserve de l'hôte fait partie du calcul : sans elle, un déploiement qui
// remplit exactement la mémoire ne laisserait rien au noyau ni au démon.
func TestAdmitCompteLaReserveEtLeSurcout(t *testing.T) {
	p := DefaultCapacityPolicy()
	host := hoteDeTest(2048, 2) // réserve de 512 Mio

	// 1536 Mio demandés : la somme brute tient tout juste, mais pas une fois
	// la réserve et le surcoût comptés.
	_, err := p.Admit(host, []Budget{{Name: "gros", VCPU: 1, MemMiB: 1536}})
	if err == nil {
		t.Fatal("un déploiement qui ne laisse rien à la machine doit être refusé")
	}

	// 1024 Mio : 1024 + 16 + 512 = 1552, ça passe.
	if _, err := p.Admit(host, []Budget{{Name: "raisonnable", VCPU: 1, MemMiB: 1024}}); err != nil {
		t.Fatalf("déploiement qui tient refusé : %v", err)
	}
}

func TestAdmitPlafondDuNombreDeVM(t *testing.T) {
	p := DefaultCapacityPolicy()
	p.MaxVMs = 3
	host := hoteDeTest(65536, 16)

	budgets := []Budget{}
	for _, nom := range []string{"a", "b", "c", "d"} {
		budgets = append(budgets, Budget{Name: nom, VCPU: 1, MemMiB: 64})
	}

	_, err := p.Admit(host, budgets)
	if err == nil {
		t.Fatal("4 vm pour un plafond de 3 doivent être refusées")
	}
	if !strings.Contains(err.Error(), "-max-vms") {
		t.Fatalf("le message doit indiquer le drapeau à relever : %v", err)
	}
}

// Une VM plus large que la machine ne gagne rien, elle perd du temps en
// contention : c'est le seul cas processeur qui soit refusé.
func TestAdmitRefuseUneVMPlusLargeQueLaMachine(t *testing.T) {
	p := DefaultCapacityPolicy()
	host := hoteDeTest(16384, 4)

	_, err := p.Admit(host, []Budget{{Name: "gourmande", VCPU: 8, MemMiB: 256}})
	if err == nil {
		t.Fatal("8 cœurs virtuels sur une machine de 4 cœurs doivent être refusés")
	}
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("erreur = %v, attendu une erreur de capacité", err)
	}
}

// Le surengagement processeur est signalé, jamais refusé : le temps se
// partage, contrairement à la mémoire.
func TestAdmitSignaleLeSurengagementProcesseurSansRefuser(t *testing.T) {
	p := DefaultCapacityPolicy()
	host := hoteDeTest(65536, 2)

	budgets := []Budget{}
	for i := 0; i < 10; i++ {
		budgets = append(budgets, Budget{Name: string(rune('a' + i)), VCPU: 2, MemMiB: 128})
	}

	rapport, err := p.Admit(host, budgets)
	if err != nil {
		t.Fatalf("le surengagement processeur ne doit pas être refusé : %v", err)
	}
	if len(rapport.Warnings) == 0 {
		t.Fatal("20 cœurs virtuels pour 2 cœurs réels doivent être signalés")
	}
	var trouve bool
	for _, w := range rapport.Warnings {
		if strings.Contains(w, "surengagement processeur") {
			trouve = true
		}
	}
	if !trouve {
		t.Fatalf("avertissement attendu absent : %v", rapport.Warnings)
	}
}

func TestAdmitSignaleUneMargeMince(t *testing.T) {
	p := DefaultCapacityPolicy()
	p.HostReserveMiB = 100
	host := hoteDeTest(2048, 4)

	// 1900 + 16 + 100 = 2016 sur 2048 : 32 Mio libres, moins de 5 %.
	rapport, err := p.Admit(host, []Budget{{Name: "juste", VCPU: 1, MemMiB: 1900}})
	if err != nil {
		t.Fatalf("un déploiement qui tient de justesse ne doit pas être refusé : %v", err)
	}
	var trouve bool
	for _, w := range rapport.Warnings {
		if strings.Contains(w, "marge mémoire mince") {
			trouve = true
		}
	}
	if !trouve {
		t.Fatalf("une marge de %d Mio doit être signalée : %v", rapport.MemFree(), rapport.Warnings)
	}
}

// Le gestionnaire compte les VM déjà déclarées, pas seulement celle qui
// arrive : c'est la somme qui dépasse, jamais une VM isolée.
func TestManagerRefuseQuandLaSommeDepasse(t *testing.T) {
	mgr, err := NewManager(NewFakeVMM(), &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}

	host := mgr.Host()
	// Deux VM qui, ensemble, dépassent la machine, mais dont chacune tient.
	chacune := host.MemTotalMiB*2/3 + 1

	premiere := testSpec(t, "alpha")
	premiere.MemMiB = chacune
	if err := mgr.Create(context.Background(), premiere); err != nil {
		t.Fatalf("la première vm doit être acceptée : %v", err)
	}

	seconde := testSpec(t, "beta")
	seconde.MemMiB = chacune
	err = mgr.Create(context.Background(), seconde)
	if err == nil {
		t.Fatal("la seconde vm doit être refusée : la somme dépasse la machine")
	}
	if !errors.Is(err, ErrCapacity) {
		t.Fatalf("erreur = %v, attendu une erreur de capacité", err)
	}

	// Et rien ne doit avoir été enregistré pour elle.
	mgr.ReadState(func(snap *Snapshot) {
		if _, present := snap.VMs["beta"]; present {
			t.Error("la vm refusée a quand même été enregistrée")
		}
	})
}

// Redéclarer une VM existante ne doit pas la compter deux fois, sans quoi un
// redémarrage du démon refuserait un déploiement qui tournait la veille.
func TestManagerNeComptePasDeuxFoisUneRedeclaration(t *testing.T) {
	mgr, err := NewManager(NewFakeVMM(), &memStore{}, quietLogger())
	if err != nil {
		t.Fatalf("NewManager : %v", err)
	}

	host := mgr.Host()
	spec := testSpec(t, "alpha")
	spec.MemMiB = host.MemTotalMiB * 2 / 3

	if err := mgr.Ensure(context.Background(), spec); err != nil {
		t.Fatalf("première déclaration : %v", err)
	}

	// L'admission est interrogée directement : passer par l'hyperviseur
	// factice ferait échouer le test sur sa propre règle « déjà présente »,
	// qui n'a rien à voir avec les plafonds.
	mgr.mu.Lock()
	err = mgr.admitLocked(spec)
	mgr.mu.Unlock()
	if err != nil {
		t.Fatalf("la redéclaration de la même vm est comptée deux fois : %v", err)
	}

	// Contrôle inverse : une seconde vm du même budget, elle, ne tient pas.
	autre := spec
	autre.Name = "beta"
	mgr.mu.Lock()
	err = mgr.admitLocked(autre)
	mgr.mu.Unlock()
	if err == nil {
		t.Fatal("une seconde vm de ce budget devrait être refusée : sinon le test précédent ne prouve rien")
	}
}
