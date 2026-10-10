// Commande tinyvmosd : le démon TinyVMOS.
//
// À ce stade (Phase 2, lot A), le démon ne sert pas encore d'API : il prend les
// VM à faire tourner sur sa ligne de commande, les supervise, et les arrête
// proprement à l'extinction. L'API REST et WebSocket, la configuration
// déclarative tinyvmos.yaml et les zones réseau arrivent dans les lots suivants.
//
// Ce qui est déjà vrai et ne changera pas : toute VM est lancée via jailer,
// l'état constaté est écrit dans un fichier JSON de façon atomique, et une VM
// qui s'arrête sans qu'on le lui ait demandé est relancée avec une
// temporisation croissante.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/Xiisuma/tinyvmos/controlplane/state"
	"github.com/Xiisuma/tinyvmos/controlplane/vm"
)

// version est renseignée à la compilation par -ldflags.
var version = "dev"

type vmFlags []string

func (v *vmFlags) String() string     { return strings.Join(*v, " ") }
func (v *vmFlags) Set(s string) error { *v = append(*v, s); return nil }

func main() {
	var (
		specs    vmFlags
		runDir   = flag.String("run-dir", "/run/tinyvmos", "dossier des sockets et journaux de console")
		jailDir  = flag.String("jail-dir", "/srv/jailer", "racine sous laquelle jailer construit les jails")
		statePth = flag.String("state", "/var/lib/tinyvmos/state.json", "fichier d'état du démon")
		logLevel = flag.String("log-level", "info", "niveau de journalisation : debug, info, warn, error")
		showVer  = flag.Bool("version", false, "affiche la version et quitte")
		listOnly = flag.Bool("list", false, "affiche l'état enregistré et quitte")
	)
	flag.Var(&specs, "vm", "vm à faire tourner, répétable. Forme :\n"+
		"  name=web1,service=web-apache,tap=tap0,ip=10.42.10.2/24,gw=10.42.10.1[,vcpu=1][,mem=256][,zone=publique][,restart=true]")

	flag.Parse()

	if *showVer {
		fmt.Printf("tinyvmosd %s\n", version)
		return
	}

	log := newLogger(*logLevel)

	if err := run(log, *statePth, *runDir, *jailDir, specs, *listOnly); err != nil {
		log.Error("arrêt sur erreur", "erreur", err)
		os.Exit(1)
	}
}

func newLogger(level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}

func run(log *slog.Logger, statePath, runDir, jailDir string, specs vmFlags, listOnly bool) error {
	store, err := state.NewJSONStore(statePath)
	if err != nil {
		return err
	}

	if listOnly {
		return printState(store)
	}

	if os.Geteuid() != 0 {
		return fmt.Errorf("tinyvmosd doit tourner en root : il crée des interfaces réseau, des jails et des cgroups")
	}

	hypervisor, err := vm.NewFirecracker(runDir, jailDir, log)
	if err != nil {
		return err
	}

	mgr, err := vm.NewManager(hypervisor, store, log)
	if err != nil {
		return err
	}

	log.Info("démon démarré",
		"version", version, "état", statePath, "run", runDir, "jails", jailDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	parsed := make([]vm.Spec, 0, len(specs))
	for _, raw := range specs {
		spec, err := parseVMFlag(raw)
		if err != nil {
			return err
		}
		parsed = append(parsed, spec)
	}

	for _, spec := range parsed {
		// Une VM déjà connue de l'état précédent est reprise, pas recréée.
		if _, err := mgr.Get(spec.Name); err != nil {
			if err := mgr.Create(ctx, spec); err != nil {
				return err
			}
		}
		if err := mgr.Start(ctx, spec.Name); err != nil {
			return err
		}
	}

	if len(parsed) == 0 {
		log.Warn("aucune vm demandée : le démon tourne à vide. Utiliser -vm pour en déclarer une.")
	}

	<-ctx.Done()
	log.Info("signal reçu, arrêt des vm")

	// Contexte neuf : celui du démon est déjà annulé, et l'arrêt propre des
	// invités a besoin de quelques secondes.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := mgr.StopAll(shutdownCtx); err != nil {
		return fmt.Errorf("arrêt des vm incomplet : %w", err)
	}
	log.Info("démon arrêté")
	return nil
}

func printState(store *state.JSONStore) error {
	snap, err := store.Load()
	if err != nil {
		return err
	}
	if len(snap.VMs) == 0 {
		fmt.Println("aucune vm enregistrée")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NOM\tSERVICE\tZONE\tSTATUT\tPID\tRELANCES\tIP\tERREUR")
	for _, rec := range snap.VMs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
			rec.Spec.Name, rec.Spec.Service, rec.Spec.Zone,
			rec.Status, rec.PID, rec.Restarts, rec.Spec.IP, rec.LastError)
	}
	return w.Flush()
}

// parseVMFlag lit la forme clé=valeur séparée par des virgules de -vm.
//
// Cette forme est volontairement rudimentaire : elle disparaîtra au profit de
// tinyvmos.yaml dès le lot C. Elle n'existe que pour pouvoir exercer le démon
// de bout en bout dès maintenant.
func parseVMFlag(raw string) (vm.Spec, error) {
	spec := vm.Spec{VCPU: 1, MemMiB: 256, Zone: "defaut", Restart: true}

	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, found := strings.Cut(part, "=")
		if !found {
			return spec, fmt.Errorf("-vm %q : %q n'est pas de la forme clé=valeur", raw, part)
		}

		switch key {
		case "name":
			spec.Name = value
		case "service":
			spec.Service = value
		case "zone":
			spec.Zone = value
		case "tap":
			spec.TapDevice = value
		case "ip":
			spec.IP = value
		case "gw":
			spec.Gateway = value
		case "mac":
			spec.MAC = value
		case "kernel":
			spec.KernelPath = value
		case "image":
			spec.ImagePath = value
		case "vcpu":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return spec, fmt.Errorf("-vm %q : vcpu=%q n'est pas un entier", raw, value)
			}
			spec.VCPU = n
		case "mem":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return spec, fmt.Errorf("-vm %q : mem=%q n'est pas un entier", raw, value)
			}
			spec.MemMiB = n
		case "restart":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return spec, fmt.Errorf("-vm %q : restart=%q n'est pas un booléen", raw, value)
			}
			spec.Restart = b
		default:
			return spec, fmt.Errorf("-vm %q : clé inconnue %q", raw, key)
		}
	}

	// Chemins par défaut : ceux produits par les scripts de la Phase 1.
	if spec.KernelPath == "" {
		spec.KernelPath = defaultKernelPath()
	}
	if spec.ImagePath == "" && spec.Service != "" {
		spec.ImagePath = defaultImagePath(spec.Service)
	}
	// MAC déterministe dérivée du nom : une VM garde la même d'un démarrage à
	// l'autre, ce qui rend baux et règles reproductibles.
	if spec.MAC == "" && spec.Name != "" {
		spec.MAC = vm.DeterministicMAC(spec.Name)
	}

	if err := spec.Validate(); err != nil {
		return spec, err
	}
	return spec, nil
}

// artifactRoot localise les artefacts produits par les scripts de build.
func artifactRoot() string {
	if root := os.Getenv("TINYVMOS_OUTPUT"); root != "" {
		return root
	}
	if wd, err := os.Getwd(); err == nil {
		return filepath.Join(wd, "output")
	}
	return "output"
}

func defaultKernelPath() string {
	return filepath.Join(artifactRoot(), "kernel", "vmlinux")
}

func defaultImagePath(service string) string {
	return filepath.Join(artifactRoot(), "images", service+".ext4")
}
