// Package reconcile ramène l'état réel de la machine vers l'état voulu, en
// continu.
//
// Sans cette boucle, le démon posait son réseau et son pare-feu une seule fois
// au démarrage puis ne corrigeait plus rien. Un « nft flush ruleset » lancé par
// n'importe quel autre outil détruisait donc toute l'isolation, définitivement
// et sans un mot. C'était la faiblesse la plus grave du plan de contrôle.
//
// La réconciliation est aussi ce qui rend vraie la phrase écrite en tête du jeu
// de règles : « toute retouche est écrasée à la prochaine réconciliation ».
package reconcile

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/Xiisuma/tinyvmos/controlplane/network"
	"github.com/Xiisuma/tinyvmos/controlplane/vm"
)

// DefaultInterval est le délai entre deux passes.
//
// Dix secondes est un compromis : assez court pour qu'une isolation détruite
// soit rétablie avant qu'on ait eu le temps d'en profiter, assez long pour que
// la vérification ne pèse rien. Chaque passe ne fait que des lectures tant
// qu'aucune dérive n'est constatée.
const DefaultInterval = 10 * time.Second

// Reconciler compare l'état réel à l'état voulu et répare les écarts.
type Reconciler struct {
	VMs      *vm.Manager
	Net      *network.Manager
	Firewall *network.Firewall
	Policy   network.Policy
	Log      *slog.Logger
	Interval time.Duration

	passes int
	fixes  int
}

// Report résume une passe.
type Report struct {
	ZonesRepaired    int
	TapsRepaired     int
	FirewallRestored bool

	// Problems liste ce qui a été constaté sans pouvoir être réparé.
	Problems []string
}

// Changed indique si la passe a corrigé quelque chose.
func (r Report) Changed() bool {
	return r.ZonesRepaired > 0 || r.TapsRepaired > 0 || r.FirewallRestored
}

// Run boucle jusqu'à annulation du contexte.
func (r *Reconciler) Run(ctx context.Context) {
	interval := r.Interval
	if interval <= 0 {
		r.Log.Info("réconciliation désactivée : aucune dérive ne sera corrigée")
		return
	}

	r.Log.Info("réconciliation démarrée", "intervalle", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			r.Log.Info("réconciliation arrêtée", "passes", r.passes, "corrections", r.fixes)
			return
		case <-ticker.C:
			report, err := r.Pass(ctx)
			if err != nil {
				r.Log.Error("passe de réconciliation en échec", "erreur", err)
				continue
			}
			if report.Changed() {
				r.fixes++
				r.Log.Warn("dérive corrigée",
					"zones", report.ZonesRepaired,
					"taps", report.TapsRepaired,
					"pare_feu", report.FirewallRestored)
			}
			for _, p := range report.Problems {
				r.Log.Error("écart non réparable", "détail", p)
			}
		}
	}
}

// Pass exécute une passe et renvoie ce qu'elle a trouvé.
//
// L'ordre compte. Le pare-feu passe en premier : si l'isolation est tombée,
// c'est ce qu'il faut rétablir avant tout le reste.
func (r *Reconciler) Pass(ctx context.Context) (Report, error) {
	r.passes++
	var report Report

	if err := r.reconcileFirewall(&report); err != nil {
		return report, err
	}
	if err := r.reconcileZones(&report); err != nil {
		return report, err
	}
	r.reconcileTaps(&report)

	return report, nil
}

// reconcileFirewall rétablit le pare-feu s'il a dérivé.
func (r *Reconciler) reconcileFirewall(report *Report) error {
	drift, err := r.Firewall.Verify()
	if err != nil {
		return fmt.Errorf("vérification du pare-feu : %w", err)
	}
	if !drift.Detected {
		return nil
	}

	r.Log.Warn("pare-feu dérivé, rétablissement", "cause", drift.Reason)
	if err := r.Firewall.Apply(r.Policy); err != nil {
		return fmt.Errorf("rétablissement du pare-feu : %w", err)
	}
	report.FirewallRestored = true
	return nil
}

// reconcileZones repose les bridges de zone manquants.
//
// EnsureZone est idempotent : une zone déjà conforme ne produit aucune
// écriture, et la passe ne coûte alors que quelques lectures netlink.
func (r *Reconciler) reconcileZones(report *Report) error {
	for _, z := range r.Policy.Zones {
		existed, err := r.Net.ZoneExists(z)
		if err != nil {
			return err
		}
		if err := r.Net.EnsureZone(z); err != nil {
			report.Problems = append(report.Problems,
				fmt.Sprintf("zone %s non réparable : %v", z.Name, err))
			continue
		}
		if !existed {
			r.Log.Warn("bridge de zone recréé", "zone", z.Name, "bridge", z.BridgeName())
			report.ZonesRepaired++
		}
	}
	return nil
}

// reconcileTaps repose les interfaces tap manquantes.
//
// Une VM arrêtée voit son tap recréé sans conséquence. Pour une VM qui tourne,
// c'est différent : Firecracker détient un descripteur sur l'ancienne
// interface, et recréer une interface du même nom ne le rebranche pas. La VM
// reste donc sans réseau, et seule une relance la rétablirait.
//
// Ce cas est signalé, pas réparé en silence : relancer une VM de production
// parce qu'une interface a disparu est une décision qui mérite d'être prise
// explicitement, pas déclenchée par une boucle de fond. L'écart est inscrit
// dans l'état de la VM pour qu'un opérateur le voie.
func (r *Reconciler) reconcileTaps(report *Report) {
	for _, rec := range r.VMs.List() {
		zone, ok := r.zoneOf(rec.Spec.Zone)
		if !ok {
			continue
		}

		tap := network.TapName(rec.Spec.Name)
		exists, err := r.Net.LinkExists(tap)
		if err != nil {
			report.Problems = append(report.Problems,
				fmt.Sprintf("lecture de %s : %v", tap, err))
			continue
		}
		if exists {
			continue
		}

		if rec.Status == vm.StatusRunning {
			report.Problems = append(report.Problems, fmt.Sprintf(
				"l'interface %s de la vm %s a disparu alors qu'elle tourne. Firecracker tient encore l'ancienne interface : la vm est sans réseau et seule une relance la rétablira",
				tap, rec.Spec.Name))
			continue
		}

		if _, err := r.Net.EnsureTap(zone, rec.Spec.Name); err != nil {
			report.Problems = append(report.Problems,
				fmt.Sprintf("interface %s non réparable : %v", tap, err))
			continue
		}
		r.Log.Warn("interface tap recréée", "vm", rec.Spec.Name, "tap", tap)
		report.TapsRepaired++
	}
}

func (r *Reconciler) zoneOf(name string) (network.Zone, bool) {
	for _, z := range r.Policy.Zones {
		if z.Name == name {
			return z, true
		}
	}
	return network.Zone{}, false
}
