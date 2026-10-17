package vm

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Plafonds de ressources : ce qu'une machine peut réellement porter.
//
// Rien n'empêchait de déclarer dix VM à 2 Gio sur une machine qui en a 8. Les
// premières démarraient, les suivantes aussi, et c'est le tueur de mémoire du
// noyau qui tranchait — au hasard, souvent sur autre chose que la VM fautive,
// parfois sur le démon lui-même. Un déploiement impossible doit être refusé à
// la déclaration, avec les chiffres sous les yeux, pas découvert en production.
//
// La mémoire et le processeur ne se traitent pas de la même façon, et c'est le
// point important :
//
//   - La mémoire ne se partage pas. Firecracker ne fait pas de ballon ni de
//     surréservation : les Mio promis à une VM lui sont promis pour de bon. La
//     somme qui dépasse est donc une erreur, et elle est refusée.
//
//   - Le processeur se partage. Trente VM sur quatre cœurs fonctionnent, elles
//     sont simplement plus lentes. Le surengagement est un choix légitime, pas
//     une faute : il est signalé, pas refusé. Une seule chose est refusée, une
//     VM qui demande plus de cœurs virtuels que la machine n'a de cœurs : elle
//     ne gagnerait rien et perdrait du temps en contention.
//
// Ces plafonds sont de l'admission, pas de la contrainte. Ils empêchent de
// déclarer l'impossible ; ils n'empêchent pas un processus Firecracker de
// déborder du budget annoncé. Poser une limite dure par cgroup sur chaque VM
// est la suite logique, et elle demande d'être mesurée sur une vraie charge
// avant d'être posée : un plafond trop serré ferait tuer des VM saines.

// ErrCapacity signale un déploiement que la machine ne peut pas porter.
var ErrCapacity = errors.New("capacité de la machine")

// HostResources est ce que la machine a réellement, relevé une fois.
type HostResources struct {
	// MemTotalMiB est la mémoire physique totale, telle que le noyau la voit.
	MemTotalMiB int64
	// CPUs est le nombre de cœurs utilisables par ce processus.
	CPUs int
}

// ReadHostResources relève les ressources de la machine.
//
// MemTotal de /proc/meminfo, et non la mémoire libre : le budget se raisonne
// sur ce que la machine possède, pas sur ce qu'elle n'utilise pas à l'instant
// où on regarde. La mémoire libre varie à la seconde, et un déploiement validé
// sur un creux échouerait au premier pic.
func ReadHostResources() (HostResources, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return HostResources{}, fmt.Errorf("lecture de /proc/meminfo : %w", err)
	}
	defer f.Close()

	var totalKiB int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		champs := strings.Fields(sc.Text())
		if len(champs) < 2 || champs[0] != "MemTotal:" {
			continue
		}
		if totalKiB, err = strconv.ParseInt(champs[1], 10, 64); err != nil {
			return HostResources{}, fmt.Errorf("MemTotal illisible dans /proc/meminfo : %w", err)
		}
		break
	}
	if err := sc.Err(); err != nil {
		return HostResources{}, fmt.Errorf("lecture de /proc/meminfo : %w", err)
	}
	if totalKiB <= 0 {
		return HostResources{}, errors.New("/proc/meminfo ne donne pas MemTotal : impossible de dimensionner quoi que ce soit")
	}

	return HostResources{
		MemTotalMiB: totalKiB / 1024,
		CPUs:        runtime.NumCPU(),
	}, nil
}

// Budget est ce qu'une VM demande.
type Budget struct {
	Name   string
	VCPU   int64
	MemMiB int64
}

// Valeurs par défaut des plafonds.
const (
	// VMOverheadMiB est le surcoût du processus Firecracker lui-même, au-delà
	// de la mémoire promise à l'invité : tables de pages, pile du VMM, tampons
	// des périphériques virtuels.
	//
	// Volontairement généreux. Se tromper vers le bas fait accepter un
	// déploiement qui ne tient pas, ce qui est exactement la faille qu'on
	// corrige ici. Le test à l'échelle le confrontera à la mesure.
	VMOverheadMiB = 16

	// MinHostReserveMiB est le plancher de mémoire laissée à la machine :
	// noyau, démon, nftables, accès d'administration. En dessous, un hôte
	// sous pression ne peut même plus être dépanné.
	MinHostReserveMiB = 512

	// HostReservePercent complète le plancher sur les machines mieux dotées,
	// où 512 Mio ne suffisent pas au cache et aux tampons.
	HostReservePercent = 10

	// DefaultMaxVMs est un garde-fou, pas un dimensionnement : sur toute
	// machine réelle, la mémoire refusera bien avant. Il existe pour qu'une
	// configuration engendrée en boucle soit arrêtée avant d'avoir épuisé les
	// interfaces et les identités.
	DefaultMaxVMs = 64

	// MaxVCPURatio est le rapport entre cœurs virtuels déclarés et cœurs
	// réels au-delà duquel le surengagement est signalé. Signalé, pas refusé :
	// le temps processeur se partage.
	MaxVCPURatio = 4
)

// CapacityPolicy sont les plafonds appliqués à un déploiement.
type CapacityPolicy struct {
	// HostReserveMiB est la mémoire réservée à la machine. Zéro applique la
	// règle par défaut, qui dépend de la taille de la machine.
	HostReserveMiB int64
	// VMOverheadMiB est le surcoût compté par VM.
	VMOverheadMiB int64
	// MaxVMs plafonne le nombre de VM déclarées.
	MaxVMs int
}

// DefaultCapacityPolicy renvoie les plafonds par défaut.
func DefaultCapacityPolicy() CapacityPolicy {
	return CapacityPolicy{VMOverheadMiB: VMOverheadMiB, MaxVMs: DefaultMaxVMs}
}

// HostReserve renvoie la mémoire réservée à la machine pour cet hôte.
//
// Un pourcentage seul serait trop faible sur une petite machine ; un montant
// fixe serait trop faible sur une grande. Le plus élevé des deux convient aux
// deux cas.
func (p CapacityPolicy) HostReserve(host HostResources) int64 {
	if p.HostReserveMiB > 0 {
		return p.HostReserveMiB
	}
	proportionnel := host.MemTotalMiB * HostReservePercent / 100
	if proportionnel > MinHostReserveMiB {
		return proportionnel
	}
	return MinHostReserveMiB
}

// CapacityReport est le bilan d'un déploiement par rapport à la machine.
type CapacityReport struct {
	VMs           int
	MemRequested  int64 // somme des budgets déclarés
	MemOverhead   int64 // surcoût des processus Firecracker
	MemReserved   int64 // part laissée à la machine
	MemTotal      int64
	VCPURequested int64
	CPUs          int

	// Warnings sont les points à signaler sans refuser.
	Warnings []string
}

// MemCommitted est ce que le déploiement engage réellement sur la machine.
func (r CapacityReport) MemCommitted() int64 {
	return r.MemRequested + r.MemOverhead + r.MemReserved
}

// MemFree est ce qui resterait après ce déploiement. Peut être négatif.
func (r CapacityReport) MemFree() int64 { return r.MemTotal - r.MemCommitted() }

// Admit vérifie qu'un ensemble de VM tient sur la machine.
//
// L'ensemble est examiné d'un bloc, et non VM par VM : accepter les six
// premières puis refuser la septième laisserait six bridges, six interfaces
// tap et six jails derrière soi, pour un déploiement de toute façon invalide.
func (p CapacityPolicy) Admit(host HostResources, budgets []Budget) (CapacityReport, error) {
	rapport := CapacityReport{
		VMs:         len(budgets),
		MemTotal:    host.MemTotalMiB,
		CPUs:        host.CPUs,
		MemReserved: p.HostReserve(host),
	}

	maxVMs := p.MaxVMs
	if maxVMs <= 0 {
		maxVMs = DefaultMaxVMs
	}
	overhead := p.VMOverheadMiB
	if overhead < 0 {
		overhead = VMOverheadMiB
	}

	for _, b := range budgets {
		rapport.MemRequested += b.MemMiB
		rapport.MemOverhead += overhead
		rapport.VCPURequested += b.VCPU
	}

	if len(budgets) > maxVMs {
		return rapport, fmt.Errorf("%w : %d vm déclarées, le plafond est %d. "+
			"Ce plafond est un garde-fou contre une configuration engendrée en boucle ; "+
			"le relever avec -max-vms si le besoin est réel",
			ErrCapacity, len(budgets), maxVMs)
	}

	// Une VM plus large que la machine ne gagne rien : ses cœurs virtuels se
	// disputent les mêmes cœurs réels, et le surcoût d'ordonnancement est pur
	// gaspillage.
	for _, b := range budgets {
		if host.CPUs > 0 && b.VCPU > int64(host.CPUs) {
			return rapport, fmt.Errorf("%w : la vm %s demande %d cœurs virtuels, la machine en a %d. "+
				"Au-delà du nombre de cœurs réels, les cœurs virtuels se disputent les mêmes cœurs et ralentissent la vm au lieu de l'accélérer",
				ErrCapacity, b.Name, b.VCPU, host.CPUs)
		}
	}

	if rapport.MemCommitted() > rapport.MemTotal {
		return rapport, fmt.Errorf("%w : le déploiement demande %d Mio pour %d vm, plus %d Mio de surcoût des processus et %d Mio réservés à la machine, "+
			"soit %d Mio sur les %d Mio installés. Il manque %d Mio. "+
			"Réduire les budgets, retirer des vm, ou abaisser la réserve avec -host-reserve si cette machine ne fait vraiment rien d'autre",
			ErrCapacity,
			rapport.MemRequested, rapport.VMs, rapport.MemOverhead, rapport.MemReserved,
			rapport.MemCommitted(), rapport.MemTotal, -rapport.MemFree())
	}

	// Le surengagement processeur est signalé, jamais refusé : contrairement à
	// la mémoire, le temps se partage, et une machine chargée est lente, pas
	// cassée.
	if host.CPUs > 0 && rapport.VCPURequested > int64(host.CPUs)*MaxVCPURatio {
		rapport.Warnings = append(rapport.Warnings, fmt.Sprintf(
			"surengagement processeur : %d cœurs virtuels déclarés pour %d cœurs réels, soit plus de %d pour un. "+
				"Les vm fonctionneront, mais se partageront le temps disponible",
			rapport.VCPURequested, host.CPUs, MaxVCPURatio))
	}

	// Une marge très mince n'est pas une erreur, mais elle mérite d'être dite :
	// c'est le moment où le moindre dépassement devient fatal.
	if reste := rapport.MemFree(); reste >= 0 && reste < rapport.MemTotal/20 {
		rapport.Warnings = append(rapport.Warnings, fmt.Sprintf(
			"marge mémoire mince : %d Mio libres après ce déploiement, sur %d Mio",
			reste, rapport.MemTotal))
	}

	return rapport, nil
}

// BudgetsFromSpecs construit la liste des budgets à partir de spécifications,
// triée par nom pour que les messages soient reproductibles.
func BudgetsFromSpecs(specs []Spec) []Budget {
	out := make([]Budget, 0, len(specs))
	for _, s := range specs {
		out = append(out, Budget{Name: s.Name, VCPU: s.VCPU, MemMiB: s.MemMiB})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
