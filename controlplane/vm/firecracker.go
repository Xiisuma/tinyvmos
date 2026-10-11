package vm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	firecracker "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// Il n'y a volontairement plus d'utilisateur partagé entre les VM.
//
// Chacune tourne sous son propre uid et son propre gid, attribués par le plan
// de contrôle dans la plage IdentityMin-IdentityMax et persistés avec l'état.
// Les partager voudrait dire qu'une évasion de Firecracker donne accès aux
// images de travail, aux jails et aux sockets de toutes les autres VM.
//
// Ces identifiants n'ont pas d'entrée dans /etc/passwd, et n'en ont pas
// besoin : jailer appelle setuid avec une valeur numérique, chown aussi, et
// Firecracker confiné ne lit jamais la base des utilisateurs.

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

	for _, d := range []string{runDir, chrootBaseDir} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			return nil, fmt.Errorf("création de %s : %w", d, err)
		}
	}

	// Le dossier des exécutions est traversable par tous, mais pas listable :
	// chaque VM doit pouvoir atteindre son propre sous-dossier, qui est en
	// 0700, sans pouvoir découvrir quelles autres VM existent.
	//
	// Sans ce droit de traversée, une VM ne pourrait même pas ouvrir ses
	// propres fichiers par leur chemin. Ça fonctionnerait quand même, puisque
	// jailer installe les artefacts dans la jail en tant que root — mais la
	// séparation reposerait alors sur l'inaccessibilité générale plutôt que
	// sur la propriété des fichiers, et on ne saurait pas laquelle des deux
	// protège réellement.
	if err := os.Chmod(runDir, 0o711); err != nil {
		return nil, fmt.Errorf("droits de %s : %w", runDir, err)
	}

	return f, nil
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
	// jailer tourne sous l'identité de cette VM : il doit pouvoir écrire ici.
	// Le mode 0700 est délibéré : seule cette VM doit voir son dossier de
	// travail, pas les autres.
	if err := os.Chown(runDir, spec.UID, spec.GID); err != nil {
		return fmt.Errorf("vm %s : attribution de %s à l'uid %d : %w", spec.Name, runDir, spec.UID, err)
	}
	if err := os.Chmod(runDir, 0o700); err != nil {
		return fmt.Errorf("vm %s : droits de %s : %w", spec.Name, runDir, err)
	}

	// jailer installe les artefacts dans la jail par liens durs. Un lien dur
	// vers un lien symbolique reste un lien symbolique, dont la cible n'existe
	// pas dans le chroot : Firecracker répondrait « The kernel file cannot be
	// opened ». On résout donc les chemins avant de les lui confier.
	//
	// La spécification d'origine reste celle enregistrée dans l'état : elle dit
	// ce qui a été demandé, pas ce qui a été résolu.
	resolved := spec
	srcKernel, err := filepath.EvalSymlinks(spec.KernelPath)
	if err != nil {
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
	// Le noyau est copié lui aussi, et au même endroit.
	//
	// jailer installe TOUS les artefacts dans la jail par liens durs : l'image
	// comme le noyau. Laisser le noyau à sa place d'origine marche tant que le
	// catalogue et la jail sont sur le même système de fichiers, et échoue
	// sinon avec « invalid cross-device link ». En copiant les deux dans le
	// dossier de travail de la VM, la seule contrainte restante est que ce
	// dossier et la jail partagent leur système de fichiers — ce que les
	// chemins par défaut garantissent.
	kernel := filepath.Join(runDir, "vmlinux")
	if err := copyImage(srcKernel, kernel); err != nil {
		return fmt.Errorf("vm %s : copie du noyau : %w", spec.Name, err)
	}
	if err := os.Chown(kernel, spec.UID, spec.GID); err != nil {
		return fmt.Errorf("vm %s : attribution du noyau à l'uid %d : %w", spec.Name, spec.UID, err)
	}
	if err := os.Chmod(kernel, 0o600); err != nil {
		return fmt.Errorf("vm %s : droits du noyau : %w", spec.Name, err)
	}
	resolved.KernelPath = kernel

	// Vérification de ceinture : les deux artefacts et la jail doivent être
	// sur le même système de fichiers. Sans ce contrôle, l'échec arrive bien
	// plus loin, sous la forme d'un message de jailer qui ne dit pas ce qui
	// se passe.
	for _, artefact := range []string{rootfs, kernel} {
		if err := sameFilesystem(artefact, f.ChrootBaseDir); err != nil {
			return fmt.Errorf("vm %s : %w", spec.Name, err)
		}
	}
	if err := os.Chown(rootfs, spec.UID, spec.GID); err != nil {
		return fmt.Errorf("vm %s : attribution de la copie de l'image à l'uid %d : %w", spec.Name, spec.UID, err)
	}
	if err := os.Chmod(rootfs, 0o600); err != nil {
		return fmt.Errorf("vm %s : droits de la copie de l'image : %w", spec.Name, err)
	}
	resolved.ImagePath = rootfs

	f.machines[spec.Name] = &instance{spec: resolved}
	f.Log.Info("vm préparée",
		"vm", spec.Name, "service", spec.Service, "zone", spec.Zone,
		"noyau", resolved.KernelPath, "image", resolved.ImagePath, "uid", spec.UID)
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
	if err := console.Chown(spec.UID, spec.GID); err != nil {
		console.Close()
		return fmt.Errorf("vm %s : attribution du journal de console : %w", name, err)
	}

	uid, gid := spec.UID, spec.GID
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
	for _, artefact := range []string{"rootfs.ext4", "vmlinux"} {
		chemin := filepath.Join(f.vmRunDir(name), artefact)
		if err := os.Remove(chemin); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("vm %s : suppression de %s : %w", name, chemin, err)
		}
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
