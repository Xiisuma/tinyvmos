// Commande tinyvmosd : le démon TinyVMOS.
//
// À ce stade (Phase 2, lots A et B1), le démon ne sert pas encore d'API : il
// prend ses zones et ses VM sur la ligne de commande, pose le réseau, démarre
// les VM, les supervise, et nettoie tout à l'extinction. L'API REST et
// WebSocket, la configuration déclarative tinyvmos.yaml et le pare-feu arrivent
// dans les lots suivants.
//
// Ce qui est déjà vrai et ne changera pas : toute VM est lancée via jailer,
// l'état constaté est écrit dans un fichier JSON de façon atomique, une VM qui
// s'arrête sans qu'on le lui ait demandé est relancée avec une temporisation
// croissante, et chaque VM reçoit son adressage des deux familles depuis
// l'attribution persistée de sa zone.
package main

import (
	"context"
	"encoding/json"
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

	"github.com/Xiisuma/tinyvmos/controlplane/network"
	"github.com/Xiisuma/tinyvmos/controlplane/state"
	"github.com/Xiisuma/tinyvmos/controlplane/vm"
)

// version est renseignée à la compilation par -ldflags.
var version = "dev"

type repeatedFlag []string

func (v *repeatedFlag) String() string     { return strings.Join(*v, " ") }
func (v *repeatedFlag) Set(s string) error { *v = append(*v, s); return nil }

// vmRequest est une VM demandée sur la ligne de commande, avant que son
// adressage ne lui soit attribué.
type vmRequest struct {
	Name    string
	Service string
	Zone    string
	VCPU    int64
	MemMiB  int64
	Restart bool
	Kernel  string
	Image   string
}

func main() {
	var (
		vms       repeatedFlag
		zonesPath = flag.String("zones", "", "fichier JSON décrivant les zones. Vide, les cinq zones d'exemple sont utilisées")
		flowsPath = flag.String("flows", "", "fichier JSON décrivant les flux autorisés. Vide avec des zones explicites, aucun flux n'est autorisé")
		uplink    = flag.String("uplink", "", "interface de sortie vers l'extérieur. Vide, elle est déduite de la route par défaut")
		runDir    = flag.String("run-dir", "/run/tinyvmos", "dossier des sockets et journaux de console")
		jailDir   = flag.String("jail-dir", "", "racine des jails. Vide, un dossier jail/ à côté du fichier d'état, pour que les liens durs de jailer restent possibles")
		statePth  = flag.String("state", "/var/lib/tinyvmos/state.json", "fichier d'état du démon")
		logLevel  = flag.String("log-level", "info", "niveau de journalisation : debug, info, warn, error")
		showVer   = flag.Bool("version", false, "affiche la version et quitte")
		listOnly  = flag.Bool("list", false, "affiche l'état enregistré et quitte")
		dumpZones = flag.Bool("print-zones", false, "affiche les zones d'exemple en JSON et quitte")
		dumpFlows = flag.Bool("print-flows", false, "affiche les flux d'exemple en JSON et quitte")
		dumpRules = flag.Bool("print-ruleset", false, "affiche le jeu de règles nftables qui serait appliqué et quitte")
		keepNet   = flag.Bool("keep-network", false, "conserve bridges et interfaces tap à l'extinction")
	)
	flag.Var(&vms, "vm", "vm à faire tourner, répétable. Forme :\n"+
		"  name=web1,service=web-apache,zone=publique[,vcpu=1][,mem=256][,restart=true]\n"+
		"  l'adressage est attribué par la zone, il n'est pas à indiquer")

	flag.Parse()

	if *showVer {
		fmt.Printf("tinyvmosd %s\n", version)
		return
	}
	if *dumpZones {
		if err := printJSON(network.ExampleZones()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if *dumpFlows {
		if err := printJSON(network.ExampleFlows()); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	log := newLogger(*logLevel)

	opts := runOptions{
		statePath: *statePth,
		runDir:    *runDir,
		jailDir:   *jailDir,
		zonesPath: *zonesPath,
		flowsPath: *flowsPath,
		uplink:    *uplink,
		vms:       vms,
		listOnly:  *listOnly,
		keepNet:   *keepNet,
		dumpRules: *dumpRules,
	}
	if err := run(log, opts); err != nil {
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

type runOptions struct {
	statePath string
	runDir    string
	jailDir   string
	zonesPath string
	flowsPath string
	uplink    string
	vms       []string
	listOnly  bool
	keepNet   bool
	dumpRules bool
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func run(log *slog.Logger, o runOptions) error {
	// Avant toute chose : afficher le jeu de regles ne doit rien creer ni
	// exiger de droits. Sur un pare-feu, pouvoir lire ce qui va etre pose
	// avant de le poser est la premiere des commodites.
	if o.dumpRules {
		return printRuleset(log, o)
	}

	store, err := state.NewJSONStore(o.statePath)
	if err != nil {
		return err
	}

	if o.listOnly {
		return printState(store)
	}

	if os.Geteuid() != 0 {
		return fmt.Errorf("tinyvmosd doit tourner en root : il crée des interfaces réseau, des jails et des cgroups")
	}

	// jailer installe les artefacts dans la jail par liens durs, qui exigent
	// que la jail et les artefacts soient sur le même système de fichiers. La
	// placer à côté du fichier d'état est le défaut le plus sûr ; un chemin
	// explicite reste possible pour qui sait ce qu'il fait.
	jailDir := o.jailDir
	if jailDir == "" {
		jailDir = filepath.Join(filepath.Dir(o.statePath), "jail")
	}

	requests, err := parseVMRequests(o.vms)
	if err != nil {
		return err
	}

	zones, err := loadZones(o.zonesPath)
	if err != nil {
		return err
	}
	if err := network.ValidateZones(zones); err != nil {
		return err
	}

	flows, err := loadFlows(o.flowsPath, o.zonesPath)
	if err != nil {
		return err
	}

	hypervisor, err := vm.NewFirecracker(o.runDir, jailDir, log)
	if err != nil {
		return err
	}
	mgr, err := vm.NewManager(hypervisor, store, log)
	if err != nil {
		return err
	}
	// L'interface tap doit appartenir à l'utilisateur confiné sous lequel
	// tourne Firecracker, sinon il ne peut pas l'ouvrir.
	jailUID, jailGID, err := vm.JailUserIDs()
	if err != nil {
		return err
	}
	netMgr := network.NewManager(log, uint32(jailUID), uint32(jailGID))
	firewall := network.NewFirewall(log, o.uplink, filepath.Join(o.runDir, "ruleset.nft"))

	log.Info("démon démarré",
		"version", version, "état", o.statePath, "run", o.runDir, "jails", jailDir)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Le préfixe IPv6 de l'installation est tiré au premier démarrage puis
	// conservé : s'il changeait, toutes les adresses déjà attribuées
	// deviendraient fausses.
	if err := mgr.WithState(func(snap *vm.Snapshot) error {
		if snap.Alloc == nil {
			alloc, err := network.NewAllocation()
			if err != nil {
				return err
			}
			snap.Alloc = alloc
			log.Info("préfixe IPv6 de l'installation tiré", "préfixe", alloc.ULAPrefix)
		}
		for i := range zones {
			if err := snap.Alloc.ResolveZoneIPv6(&zones[i]); err != nil {
				return err
			}
		}
		snap.Zones = zones
		return nil
	}); err != nil {
		return err
	}

	if err := network.ValidateZones(zones); err != nil {
		return fmt.Errorf("zones invalides après attribution IPv6 : %w", err)
	}

	if err := netMgr.EnableForwarding(); err != nil {
		return err
	}

	// Le pare-feu est posé avant la première VM. Dans l'autre ordre, une VM
	// aurait un réseau ouvert pendant quelques instants, ce qui suffirait à
	// invalider la promesse d'isolation.
	policy := network.Policy{Zones: zones, Flows: flows}
	if err := firewall.Apply(policy); err != nil {
		return err
	}
	log.Info("pare-feu en place",
		"zones", len(zones), "flux", len(flows),
		"règles", filepath.Join(o.runDir, "ruleset.nft"))

	// Toutes les zones déclarées sont posées, pas seulement celles qui portent
	// une VM. C'est la cohérence d'un système déclaratif : ce qui figure dans la
	// configuration existe. Et ça rend le pare-feu vrai — ses règles de refus
	// nomment les interfaces de zone, qui doivent donc exister pour que le refus
	// ait un sens.
	used := map[string]network.Zone{}
	for _, z := range zones {
		used[z.Name] = z
	}
	for _, r := range requests {
		if _, ok := findZone(zones, r.Zone); !ok {
			return fmt.Errorf("vm %s : zone %q inconnue. Zones déclarées : %s",
				r.Name, r.Zone, strings.Join(zoneNames(zones), ", "))
		}
	}
	for _, name := range zoneNames(zones) {
		if err := netMgr.EnsureZone(used[name]); err != nil {
			return err
		}
	}

	var createdTaps []string
	cleanupNetwork := func() {
		if o.keepNet {
			log.Info("réseau conservé à la demande", "taps", len(createdTaps))
			return
		}
		if err := firewall.Flush(); err != nil {
			log.Warn("retrait du pare-feu en échec", "erreur", err)
		}
		for _, name := range createdTaps {
			if err := netMgr.RemoveTap(name); err != nil {
				log.Warn("suppression d'interface tap en échec", "vm", name, "erreur", err)
			}
		}
		for _, z := range used {
			if err := netMgr.RemoveZone(z); err != nil {
				log.Warn("suppression de zone en échec", "zone", z.Name, "erreur", err)
			}
		}
	}

	for _, r := range requests {
		z := used[r.Zone]

		var lease *network.Lease
		if err := mgr.WithState(func(snap *vm.Snapshot) error {
			l, err := snap.Alloc.Allocate(z, r.Name)
			if err != nil {
				return err
			}
			lease = l
			return nil
		}); err != nil {
			cleanupNetwork()
			return err
		}

		tap, err := netMgr.EnsureTap(z, r.Name)
		if err != nil {
			cleanupNetwork()
			return err
		}
		createdTaps = append(createdTaps, r.Name)

		spec := vm.Spec{
			Name:       r.Name,
			Service:    r.Service,
			Zone:       z.Name,
			VCPU:       r.VCPU,
			MemMiB:     r.MemMiB,
			KernelPath: r.Kernel,
			ImagePath:  r.Image,
			TapDevice:  tap,
			MAC:        vm.DeterministicMAC(r.Name),
			IP:         lease.IPv4,
			Gateway:    lease.GatewayV4,
			IPv6:       lease.IPv6,
			GatewayV6:  lease.GatewayV6,
			Restart:    r.Restart,
		}

		log.Info("vm adressée",
			"vm", r.Name, "zone", z.Name, "tap", tap,
			"ipv4", lease.IPv4, "ipv6", lease.IPv6)

		if err := mgr.Ensure(ctx, spec); err != nil {
			cleanupNetwork()
			return err
		}
		if err := mgr.Start(ctx, r.Name); err != nil {
			cleanupNetwork()
			return err
		}
	}

	if len(requests) == 0 {
		log.Warn("aucune vm demandée : le démon tourne à vide. Utiliser -vm pour en déclarer une.")
	}

	<-ctx.Done()
	log.Info("signal reçu, arrêt des vm")

	// Contexte neuf : celui du démon est déjà annulé, et l'arrêt propre des
	// invités a besoin de quelques secondes.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	stopErr := mgr.StopAll(shutdownCtx)
	cleanupNetwork()

	if stopErr != nil {
		return fmt.Errorf("arrêt des vm incomplet : %w", stopErr)
	}
	log.Info("démon arrêté")
	return nil
}

func findZone(zones []network.Zone, name string) (network.Zone, bool) {
	for _, z := range zones {
		if z.Name == name {
			return z, true
		}
	}
	return network.Zone{}, false
}

func zoneNames(zones []network.Zone) []string {
	out := make([]string, 0, len(zones))
	for _, z := range zones {
		out = append(out, z.Name)
	}
	return out
}

// printRuleset affiche le jeu de règles qui serait appliqué, sans rien
// installer et sans exiger les droits root.
//
// C'est le pendant du --dry-run : sur un pare-feu, pouvoir lire ce qui va être
// posé avant de le poser n'est pas un luxe.
func printRuleset(log *slog.Logger, o runOptions) error {
	zones, err := loadZones(o.zonesPath)
	if err != nil {
		return err
	}
	flows, err := loadFlows(o.flowsPath, o.zonesPath)
	if err != nil {
		return err
	}

	// Les zones d'exemple n'ont pas d'IPv6 avant attribution ; on en tire une
	// jetable pour que l'affichage soit représentatif.
	alloc, err := network.NewAllocation()
	if err != nil {
		return err
	}
	for i := range zones {
		if err := alloc.ResolveZoneIPv6(&zones[i]); err != nil {
			return err
		}
	}

	fw := network.NewFirewall(log, o.uplink, "")
	ruleset, err := fw.Ruleset(network.Policy{Zones: zones, Flows: flows})
	if err != nil {
		return err
	}
	fmt.Print(ruleset)
	return nil
}

// loadFlows lit les flux depuis un fichier JSON.
//
// Sans fichier de flux, le comportement dépend des zones. Avec les zones
// d'exemple, les flux d'exemple vont avec. Avec des zones explicites, aucun
// flux n'est autorisé : pour un pare-feu, le défaut doit toujours être le
// refus, jamais une liste héritée qui ne correspond à rien.
func loadFlows(path, zonesPath string) ([]network.Flow, error) {
	if path == "" {
		if zonesPath == "" {
			return network.ExampleFlows(), nil
		}
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lecture des flux %s : %w", path, err)
	}
	var flows []network.Flow
	if err := json.Unmarshal(data, &flows); err != nil {
		return nil, fmt.Errorf("flux %s illisibles : %w", path, err)
	}
	return flows, nil
}

// loadZones lit les zones depuis un fichier JSON, ou renvoie celles d'exemple.
func loadZones(path string) ([]network.Zone, error) {
	if path == "" {
		return network.ExampleZones(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("lecture des zones %s : %w", path, err)
	}
	var zones []network.Zone
	if err := json.Unmarshal(data, &zones); err != nil {
		return nil, fmt.Errorf("zones %s illisibles : %w", path, err)
	}
	if len(zones) == 0 {
		return nil, fmt.Errorf("zones %s : aucune zone déclarée", path)
	}
	return zones, nil
}

func printState(store *state.JSONStore) error {
	snap, err := store.Load()
	if err != nil {
		return err
	}

	if snap.Alloc != nil && snap.Alloc.ULAPrefix != "" {
		fmt.Printf("préfixe IPv6 de l'installation : %s\n\n", snap.Alloc.ULAPrefix)
	}

	if len(snap.Zones) > 0 {
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ZONE\tBRIDGE\tIPV4\tIPV6")
		for _, z := range snap.Zones {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", z.Name, z.BridgeName(), z.IPv4, z.IPv6)
		}
		if err := w.Flush(); err != nil {
			return err
		}
		fmt.Println()
	}

	if len(snap.VMs) == 0 {
		fmt.Println("aucune vm enregistrée")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "NOM\tSERVICE\tZONE\tSTATUT\tPID\tRELANCES\tIPV4\tIPV6\tERREUR")
	for _, rec := range snap.VMs {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\n",
			rec.Spec.Name, rec.Spec.Service, rec.Spec.Zone,
			rec.Status, rec.PID, rec.Restarts,
			rec.Spec.IP, rec.Spec.IPv6, rec.LastError)
	}
	return w.Flush()
}

// parseVMRequests lit les drapeaux -vm.
//
// Cette forme est volontairement rudimentaire : elle disparaîtra au profit de
// tinyvmos.yaml dès le lot C. Elle n'existe que pour pouvoir exercer le démon
// de bout en bout dès maintenant.
func parseVMRequests(raws []string) ([]vmRequest, error) {
	out := make([]vmRequest, 0, len(raws))
	for _, raw := range raws {
		r, err := parseVMRequest(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func parseVMRequest(raw string) (vmRequest, error) {
	r := vmRequest{VCPU: 1, MemMiB: 256, Zone: "publique", Restart: true}

	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, found := strings.Cut(part, "=")
		if !found {
			return r, fmt.Errorf("-vm %q : %q n'est pas de la forme clé=valeur", raw, part)
		}

		switch key {
		case "name":
			r.Name = value
		case "service":
			r.Service = value
		case "zone":
			r.Zone = value
		case "kernel":
			r.Kernel = value
		case "image":
			r.Image = value
		case "vcpu":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return r, fmt.Errorf("-vm %q : vcpu=%q n'est pas un entier", raw, value)
			}
			r.VCPU = n
		case "mem":
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil {
				return r, fmt.Errorf("-vm %q : mem=%q n'est pas un entier", raw, value)
			}
			r.MemMiB = n
		case "restart":
			b, err := strconv.ParseBool(value)
			if err != nil {
				return r, fmt.Errorf("-vm %q : restart=%q n'est pas un booléen", raw, value)
			}
			r.Restart = b
		case "tap", "ip", "gw", "mac":
			return r, fmt.Errorf("-vm %q : %q n'est plus accepté. L'adressage et l'interface tap sont attribués par la zone", raw, key)
		default:
			return r, fmt.Errorf("-vm %q : clé inconnue %q", raw, key)
		}
	}

	if r.Name == "" {
		return r, fmt.Errorf("-vm %q : name est obligatoire", raw)
	}
	if r.Service == "" {
		return r, fmt.Errorf("-vm %q : service est obligatoire", raw)
	}

	// Chemins par défaut : ceux produits par les scripts de build.
	if r.Kernel == "" {
		r.Kernel = defaultKernelPath()
	}
	if r.Image == "" {
		r.Image = defaultImagePath(r.Service)
	}
	return r, nil
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
