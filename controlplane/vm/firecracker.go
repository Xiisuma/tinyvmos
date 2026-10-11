package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// JailUser est l'utilisateur système sous lequel tourne chaque VM. Il n'a ni
// shell ni dossier personnel : son seul rôle est de ne pas être root.
const JailUser = "tinyvmos"

// stopGrace est le temps laissé à une VM pour s'arrêter proprement avant que
// le processus soit tué.
const stopGrace = 5 * time.Second

// Firecracker implémente VMM.
//
// Toute VM est lancée via jailer, sans exception : namespaces, chroot, cgroup,
// uid et gid dédiés, filtre seccomp. Le mode direct existe dans les scripts de
// diagnostic, pas dans le démon.
type Firecracker struct {
	// RunDir contient les sockets, journaux et fichiers de travail.
	RunDir string
	// ChrootBaseDir est la racine sous laquelle jailer construit les jails.
	ChrootBaseDir string
	// FirecrackerBin et JailerBin sont résolus dans le PATH si vides.
	FirecrackerBin string
	JailerBin      string

	Log *slog.Logger

	mu       sync.Mutex
	machines map[string]*instance
	uid      int
	gid      int
}

type instance struct {
	spec    Spec
	machine *firecracker.Machine
	// cancel coupe le contexte confié au SDK, qui tient le processus.
	cancel context.CancelFunc
	// done est fermé quand le processus a rendu la main.
	done chan struct{}
	// waitErr retient la cause de l'arrêt, lisible après fermeture de done.
	waitErr error
	started time.Time
}

// NewFirecracker prépare l'hyperviseur et vérifie ses prérequis.
func NewFirecracker(runDir, chrootBaseDir string, log *slog.Logger) (*Firecracker, error) {
	if log == nil {
		log = slog.Default()
	}
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return nil, fmt.Errorf("/dev/kvm inaccessible : Firecracker ne peut pas démarrer (%w)", err)
	}

	f := &Firecracker{
		RunDir:        runDir,
		ChrootBaseDir: chrootBaseDir,
		Log:           log,
		machines:      map[string]*instance{},
	}

	var err error
	if f.FirecrackerBin, err = exec.LookPath("firecracker"); err != nil {
		return nil, fmt.Errorf("binaire firecracker introuvable dans le PATH : %w", err)
	}
	if f.JailerBin, err = exec.LookPath("jailer"); err != nil {
		return nil, fmt.Errorf("binaire jailer introuvable dans le PATH : il est livré avec firecracker et c'est lui qui confine les VM (%w)", err)
	}

	if f.uid, f.gid, err = lookupJailUser(); err != nil {
		return nil, err
	}

	for _, d := range []string{runDir, chrootBaseDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("création de %s : %w", d, err)
		}
	}
	return f, nil
}

// JailUserIDs renvoie l'uid et le gid de l'utilisateur confine sous lequel
// tournent les VM.
//
// Expose parce que le gestionnaire de reseau en a besoin : l'interface tap doit
// appartenir a cet utilisateur, sinon Firecracker confine ne peut pas l'ouvrir.
// Le paquet network ne peut pas importer vm, qui l'importe deja.
func JailUserIDs() (int, int, error) {
	return lookupJailUser()
}

func lookupJailUser() (int, int, error) {
	u, err := user.Lookup(JailUser)
	if err != nil {
		return 0, 0, fmt.Errorf("utilisateur système %q absent : le créer avec useradd --system --no-create-home --shell /usr/sbin/nologin %s (%w)",
			JailUser, JailUser, err)
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, 0, fmt.Errorf("uid de %s illisible : %w", JailUser, err)
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return 0, 0, fmt.Errorf("gid de %s illisible : %w", JailUser, err)
	}
	return uid, gid, nil
}

func (f *Firecracker) vmRunDir(name string) string { return filepath.Join(f.RunDir, name) }

// ConsoleLogPath renvoie le chemin du journal de console série.
func (f *Firecracker) ConsoleLogPath(name string) string {
	return filepath.Join(f.vmRunDir(name), "console.log")
}

// kernelArgs construit la ligne de commande du noyau.
//
// ip= est interprété par le noyau avant tout espace utilisateur ;
// tinyvmos.ip est relu par le service tinyvmos-net de l'image. Les deux disent
// la même chose, l'un pour le noyau, l'autre pour OpenRC : c'est ce qui permet
// à une même image de servir plusieurs VM sans être reconstruite.
func kernelArgs(spec Spec) string {
	args := "console=ttyS0 reboot=k panic=1 pci=off" +
		" i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd" +
		" root=/dev/vda rw"

	if spec.IP != "" {
		ip, netw, err := parseCIDR(spec.IP)
		if err == nil {
			args += fmt.Sprintf(" ip=%s::%s:%s::eth0:off", ip, spec.Gateway, netw)
			args += fmt.Sprintf(" tinyvmos.ip=%s", spec.IP)
		}
	}
	if spec.Gateway != "" {
		args += fmt.Sprintf(" tinyvmos.gw=%s", spec.Gateway)
	}

	// Le paramètre ip= du noyau ne sait traiter que l'IPv4. L'adressage IPv6
	// est donc passé par tinyvmos.ip6, relu par le service tinyvmos-net de
	// l'image, qui le pose avec ifupdown.
	if spec.IPv6 != "" {
		args += fmt.Sprintf(" tinyvmos.ip6=%s", spec.IPv6)
	}
	if spec.GatewayV6 != "" {
		args += fmt.Sprintf(" tinyvmos.gw6=%s", spec.GatewayV6)
	}
	return args
}

// Create prépare la VM sans la démarrer.
func (f *Firecracker) Create(ctx context.Context, spec Spec) error {
	if err := spec.Validate(); err != nil {
		return err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	// Une VM déjà préparée peut l'être de nouveau tant qu'elle ne tourne pas :
	// c'est ce qui permet au démon de reprendre, après son propre redémarrage,
	// une VM que l'état connaît mais que ce processus n'a jamais vue.
	if existing, exists := f.machines[spec.Name]; exists {
		if existing.machine != nil {
			return fmt.Errorf("vm %s déjà démarrée : l'arrêter avant de la préparer de nouveau", spec.Name)
		}
	}

	runDir := f.vmRunDir(spec.Name)
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return fmt.Errorf("vm %s : création de %s : %w", spec.Name, runDir, err)
	}
	// jailer tourne sous l'utilisateur dédié : il doit pouvoir écrire ici.
	if err := os.Chown(runDir, f.uid, f.gid); err != nil {
		return fmt.Errorf("vm %s : attribution de %s à %s : %w", spec.Name, runDir, JailUser, err)
	}

	// jailer installe les artefacts dans la jail par liens durs. Un lien dur
	// vers un lien symbolique reste un lien symbolique, dont la cible n'existe
	// pas dans le chroot : Firecracker répondrait « The kernel file cannot be
	// opened ». On résout donc les chemins avant de les lui confier.
	//
	// La spécification d'origine reste celle enregistrée dans l'état : elle dit
	// ce qui a été demandé, pas ce qui a été résolu.
	resolved := spec
	var err error
	if resolved.KernelPath, err = filepath.EvalSymlinks(spec.KernelPath); err != nil {
		return fmt.Errorf("vm %s : résolution du chemin du noyau %s : %w", spec.Name, spec.KernelPath, err)
	}
	srcImage, err := filepath.EvalSymlinks(spec.ImagePath)
	if err != nil {
		return fmt.Errorf("vm %s : résolution du chemin de l'image %s : %w", spec.Name, spec.ImagePath, err)
	}

	// Chaque VM reçoit sa propre copie de l'image, pour deux raisons.
	//
	// D'abord la correction : le disque racine est monté en écriture, et
	// jailer installe les artefacts par liens durs. Deux VM issues de la même
	// image partageraient donc le même inode et s'écraseraient mutuellement.
	//
	// Ensuite les droits : un lien dur conserve le propriétaire de l'original,
	// qui n'est pas l'utilisateur confiné. Firecracker répondrait « Permission
	// denied » à l'attachement du disque.
	//
	// C'est aussi ce qui rend les images réellement jetables : l'image du
	// catalogue n'est jamais modifiée, seule la copie de travail l'est.
	rootfs := filepath.Join(runDir, "rootfs.ext4")
	if err := copyImage(srcImage, rootfs); err != nil {
		return fmt.Errorf("vm %s : copie de l'image : %w", spec.Name, err)
	}
	// jailer installe les artefacts dans la jail par liens durs, qui exigent
	// le même système de fichiers. Sans cette vérification, l'échec arrive
	// bien plus loin, sous la forme d'un « Invalid argument » de jailer qui
	// ne dit pas du tout ce qui se passe.
	if err := sameFilesystem(rootfs, f.ChrootBaseDir); err != nil {
		return fmt.Errorf("vm %s : %w", spec.Name, err)
	}
	if err := os.Chown(rootfs, f.uid, f.gid); err != nil {
		return fmt.Errorf("vm %s : attribution de la copie de l'image à %s : %w", spec.Name, JailUser, err)
	}
	if err := os.Chmod(rootfs, 0o600); err != nil {
		return fmt.Errorf("vm %s : droits de la copie de l'image : %w", spec.Name, err)
	}
	resolved.ImagePath = rootfs

	f.machines[spec.Name] = &instance{spec: resolved}
	f.Log.Info("vm préparée",
		"vm", spec.Name, "service", spec.Service, "zone", spec.Zone,
		"noyau", resolved.KernelPath, "image", resolved.ImagePath)
	return nil
}

// Start démarre une VM préparée, sous jailer.
func (f *Firecracker) Start(ctx context.Context, name string) error {
	f.mu.Lock()
	inst, ok := f.machines[name]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("vm %s inconnue", name)
	}
	if inst.machine != nil {
		f.mu.Unlock()
		return fmt.Errorf("vm %s déjà démarrée", name)
	}
	spec := inst.spec
	f.mu.Unlock()

	consolePath := f.ConsoleLogPath(name)

	// jailer refuse de construire une jail dont le dossier existe déjà. Après
	// un arrêt, propre ou non, celle du tour précédent est encore là : sans ce
	// nettoyage, toute relance échouerait.
	//
	// Supprimer la jail est sans danger : elle ne contient que des liens durs
	// vers le noyau et vers la copie de travail de l'image, qui vivent tous
	// deux à l'extérieur. Le disque de la VM survit donc à la relance.
	jail := filepath.Join(f.ChrootBaseDir, filepath.Base(f.FirecrackerBin), name)
	if err := os.RemoveAll(jail); err != nil {
		return fmt.Errorf("vm %s : nettoyage de la jail %s : %w", name, jail, err)
	}

	console, err := os.OpenFile(consolePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return fmt.Errorf("vm %s : ouverture du journal de console : %w", name, err)
	}
	if err := console.Chown(f.uid, f.gid); err != nil {
		console.Close()
		return fmt.Errorf("vm %s : attribution du journal de console : %w", name, err)
	}

	uid, gid := f.uid, f.gid
	// Le SDK déréférence NumaNode sans vérifier : un pointeur nul le fait
	// paniquer dans jail(). Nœud 0 par défaut.
	numaNode := 0
	cfg := firecracker.Config{
		SocketPath:      "run/firecracker.socket",
		KernelImagePath: spec.KernelPath,
		KernelArgs:      kernelArgs(spec),
		Drives: []models.Drive{{
			DriveID:      strPtr("rootfs"),
			PathOnHost:   strPtr(spec.ImagePath),
			IsRootDevice: boolPtr(true),
			IsReadOnly:   boolPtr(false),
		}},
		NetworkInterfaces: firecracker.NetworkInterfaces{{
			StaticConfiguration: &firecracker.StaticNetworkConfiguration{
				MacAddress:  spec.MAC,
				HostDevName: spec.TapDevice,
			},
		}},
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  &spec.VCPU,
			MemSizeMib: &spec.MemMiB,
			Smt:        boolPtr(false),
		},
		JailerCfg: &firecracker.JailerConfig{
			ID:             name,
			UID:            &uid,
			GID:            &gid,
			NumaNode:       &numaNode,
			ExecFile:       f.FirecrackerBin,
			JailerBinary:   f.JailerBin,
			ChrootBaseDir:  f.ChrootBaseDir,
			ChrootStrategy: firecracker.NewNaiveChrootStrategy(spec.KernelPath),
			CgroupVersion:  detectCgroupVersion(),
			Daemonize:      false,
			Stdout:         console,
			Stderr:         console,
		},
		// Le filtre seccomp de jailer est la dernière barrière entre une VM
		// compromise et l'hôte. Il n'est jamais désactivé.
		Seccomp: firecracker.SeccompConfig{Enabled: true},
		VMID:    name,
	}

	// Le contexte confié au SDK tient le processus : il ne doit pas être celui
	// de l'appel Start, qui se termine dès que la VM a démarré.
	vmCtx, cancel := context.WithCancel(context.Background())

	machine, err := firecracker.NewMachine(vmCtx, cfg)
	if err != nil {
		cancel()
		console.Close()
		return fmt.Errorf("vm %s : construction de la machine : %w", name, err)
	}

	if err := machine.Start(ctx); err != nil {
		cancel()
		console.Close()
		return fmt.Errorf("vm %s : démarrage refusé (console : %s) : %w", name, consolePath, err)
	}

	done := make(chan struct{})

	f.mu.Lock()
	inst.machine = machine
	inst.cancel = cancel
	inst.done = done
	inst.waitErr = nil
	inst.started = time.Now()
	f.mu.Unlock()

	// Une seule goroutine attend la fin du processus et publie la cause.
	//
	// Elle remet aussi machine à nil : sans cela, une VM morte resterait
	// marquée comme démarrée et Start refuserait toute relance avec
	// « déjà démarrée ». Cette remise à zéro doit précéder la fermeture de
	// done, qui est le signal attendu par le superviseur.
	go func() {
		err := machine.Wait(vmCtx)
		f.mu.Lock()
		inst.waitErr = err
		inst.machine = nil
		f.mu.Unlock()
		console.Close()
		cancel()
		close(done)
	}()

	pid, _ := machine.PID()
	f.Log.Info("vm démarrée", "vm", name, "pid", pid, "vcpu", spec.VCPU, "mem_mib", spec.MemMiB)
	return nil
}

// Stop demande un arrêt propre, puis force si la VM ne répond pas.
func (f *Firecracker) Stop(ctx context.Context, name string) error {
	f.mu.Lock()
	inst, ok := f.machines[name]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("vm %s inconnue", name)
	}
	machine, done, cancel := inst.machine, inst.done, inst.cancel
	f.mu.Unlock()

	if machine == nil {
		return nil // déjà arrêtée
	}

	// Ctrl-Alt-Suppr : l'invité s'arrête proprement, les systèmes de fichiers
	// sont démontés. Un kill direct risquerait de laisser l'image incohérente.
	if err := machine.Shutdown(ctx); err != nil {
		f.Log.Warn("arrêt propre refusé, passage en force", "vm", name, "erreur", err)
	}

	select {
	case <-done:
	case <-time.After(stopGrace):
		f.Log.Warn("arrêt propre trop long, processus tué", "vm", name, "délai", stopGrace)
		if err := machine.StopVMM(); err != nil {
			f.Log.Warn("StopVMM en échec", "vm", name, "erreur", err)
		}
		select {
		case <-done:
		case <-time.After(stopGrace):
			if cancel != nil {
				cancel()
			}
		}
	}

	f.mu.Lock()
	inst.machine = nil
	if inst.cancel != nil {
		inst.cancel()
		inst.cancel = nil
	}
	f.mu.Unlock()

	f.Log.Info("vm arrêtée", "vm", name)
	return nil
}

// Delete libère tout ce que Create a réservé.
func (f *Firecracker) Delete(ctx context.Context, name string) error {
	if err := f.Stop(ctx, name); err != nil && !errors.Is(err, os.ErrNotExist) {
		f.Log.Warn("arrêt avant suppression en échec", "vm", name, "erreur", err)
	}

	f.mu.Lock()
	delete(f.machines, name)
	f.mu.Unlock()

	// Le journal de console est conservé : après une suppression, c'est la
	// seule trace de ce qui s'est passé dans la VM.
	jail := filepath.Join(f.ChrootBaseDir, filepath.Base(f.FirecrackerBin), name)
	if err := os.RemoveAll(jail); err != nil {
		return fmt.Errorf("vm %s : suppression de la jail %s : %w", name, jail, err)
	}
	// La copie de travail de l'image disparaît avec la VM : les images du
	// catalogue sont jetables, les données durables vivent sur des volumes
	// persistants séparés.
	rootfs := filepath.Join(f.vmRunDir(name), "rootfs.ext4")
	if err := os.Remove(rootfs); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("vm %s : suppression de la copie de l'image %s : %w", name, rootfs, err)
	}

	f.Log.Info("vm supprimée", "vm", name)
	return nil
}

// Metrics relève l'état instantané du processus depuis /proc.
func (f *Firecracker) Metrics(ctx context.Context, name string) (Metrics, error) {
	f.mu.Lock()
	inst, ok := f.machines[name]
	if !ok {
		f.mu.Unlock()
		return Metrics{}, fmt.Errorf("vm %s inconnue", name)
	}
	machine, started := inst.machine, inst.started
	f.mu.Unlock()

	if machine == nil {
		return Metrics{}, fmt.Errorf("vm %s arrêtée", name)
	}

	pid, err := machine.PID()
	if err != nil {
		return Metrics{}, fmt.Errorf("vm %s : pid indisponible : %w", name, err)
	}

	m := Metrics{PID: pid, Uptime: time.Since(started)}
	if rss, err := readRSSMiB(pid); err == nil {
		m.RSSMiB = rss
	}
	return m, nil
}

// Wait bloque jusqu'à la fin du processus de la VM.
func (f *Firecracker) Wait(ctx context.Context, name string) error {
	f.mu.Lock()
	inst, ok := f.machines[name]
	if !ok {
		f.mu.Unlock()
		return fmt.Errorf("vm %s inconnue", name)
	}
	done := inst.done
	f.mu.Unlock()

	if done == nil {
		return fmt.Errorf("vm %s jamais démarrée", name)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		f.mu.Lock()
		err := inst.waitErr
		f.mu.Unlock()
		return err
	}
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
