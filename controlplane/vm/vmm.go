// Package vm définit le contrat d'un hyperviseur de micro-VM et la supervision
// de leur cycle de vie.
//
// L'implémentation par défaut est Firecracker, mais elle passe derrière
// l'interface VMM pour qu'un autre hyperviseur (Cloud Hypervisor, par exemple,
// si le passthrough GPU devenait nécessaire) puisse être ajouté sans toucher au
// reste du plan de contrôle.
package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"time"
)

// Status est l'état constaté d'une micro-VM.
type Status string

const (
	StatusCreated  Status = "created"
	StatusStarting Status = "starting"
	StatusRunning  Status = "running"
	StatusStopping Status = "stopping"
	StatusStopped  Status = "stopped"
	StatusFailed   Status = "failed"
)

// transitions décrit les changements d'état autorisés. Une transition absente
// de cette table est un bug, pas un cas à traiter : la signaler plutôt que de
// laisser l'état dériver silencieusement.
var transitions = map[Status][]Status{
	StatusCreated:  {StatusStarting, StatusStopped, StatusFailed},
	StatusStarting: {StatusRunning, StatusFailed, StatusStopped},
	StatusRunning:  {StatusStopping, StatusStopped, StatusFailed},
	StatusStopping: {StatusStopped, StatusFailed},
	StatusStopped:  {StatusStarting, StatusCreated},
	StatusFailed:   {StatusStarting, StatusStopped, StatusCreated},
}

// CanTransitionTo indique si le passage de s à next est autorisé.
func (s Status) CanTransitionTo(next Status) bool {
	if s == next {
		return true
	}
	for _, allowed := range transitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// Spec est l'état voulu d'une micro-VM. Il ne contient que des faits durables :
// ni PID, ni statut, ni métrique.
type Spec struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Zone    string `json:"zone"`

	VCPU   int64 `json:"vcpu"`
	MemMiB int64 `json:"mem_mib"`

	KernelPath string `json:"kernel_path"`
	ImagePath  string `json:"image_path"`

	TapDevice string `json:"tap_device"`
	MAC       string `json:"mac"`
	// IP est notée en CIDR, par exemple 10.42.10.2/24.
	IP      string `json:"ip"`
	Gateway string `json:"gateway"`

	// IPv6 et GatewayV6 portent l'adressage de la seconde famille, en CIDR
	// également. Les deux familles sont gérées : un IPv6 laissé sans
	// configuration n'est pas neutre, il contourne les règles IPv4.
	IPv6      string `json:"ipv6,omitempty"`
	GatewayV6 string `json:"gateway_v6,omitempty"`

	// Restart demande au superviseur de relancer la VM si elle s'arrête sans
	// qu'on le lui ait demandé.
	Restart bool `json:"restart"`

	// UID et GID sont l'identité confinée sous laquelle tourne cette VM.
	//
	// Chaque VM a la sienne. Les partager voudrait dire qu'une évasion donne
	// accès aux images de travail, aux jails et aux sockets de toutes les
	// autres. Elles sont attribuées par le plan de contrôle, comme les
	// adresses, et persistées pour rester stables d'un démarrage à l'autre.
	UID int `json:"uid"`
	GID int `json:"gid"`
}

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)

// Validate refuse une spécification incohérente avant toute action. Mieux vaut
// un message clair ici qu'un échec opaque de Firecracker trente lignes plus loin.
func (s Spec) Validate() error {
	if !nameRe.MatchString(s.Name) {
		return fmt.Errorf("nom de VM invalide %q : minuscules, chiffres et tirets, 64 caractères maximum, sans tiret en début ni en fin", s.Name)
	}
	if s.Service == "" {
		return fmt.Errorf("vm %s : service non renseigné", s.Name)
	}
	if s.VCPU < 1 {
		return fmt.Errorf("vm %s : vcpu doit valoir au moins 1, reçu %d", s.Name, s.VCPU)
	}
	if s.MemMiB < 32 {
		return fmt.Errorf("vm %s : mem_mib doit valoir au moins 32, reçu %d", s.Name, s.MemMiB)
	}
	for _, f := range []struct{ label, path string }{
		{"noyau", s.KernelPath},
		{"image", s.ImagePath},
	} {
		if f.path == "" {
			return fmt.Errorf("vm %s : chemin du %s non renseigné", s.Name, f.label)
		}
		if _, err := os.Stat(f.path); err != nil {
			return fmt.Errorf("vm %s : %s introuvable à %s", s.Name, f.label, f.path)
		}
	}
	if s.TapDevice == "" {
		return fmt.Errorf("vm %s : interface tap non renseignée", s.Name)
	}
	if s.MAC != "" {
		if _, err := net.ParseMAC(s.MAC); err != nil {
			return fmt.Errorf("vm %s : adresse MAC invalide %q", s.Name, s.MAC)
		}
	}
	if s.IP != "" {
		if _, _, err := net.ParseCIDR(s.IP); err != nil {
			return fmt.Errorf("vm %s : adresse IP invalide %q, un CIDR est attendu comme 10.42.10.2/24", s.Name, s.IP)
		}
	}
	if s.Gateway != "" && net.ParseIP(s.Gateway) == nil {
		return fmt.Errorf("vm %s : passerelle invalide %q", s.Name, s.Gateway)
	}
	if s.IPv6 != "" {
		addr, _, err := net.ParseCIDR(s.IPv6)
		if err != nil {
			return fmt.Errorf("vm %s : adresse IPv6 invalide %q, un CIDR est attendu comme fd42:1::2/64", s.Name, s.IPv6)
		}
		if addr.To4() != nil {
			return fmt.Errorf("vm %s : %q est une adresse IPv4 dans le champ IPv6", s.Name, s.IPv6)
		}
	}
	if s.GatewayV6 != "" {
		addr := net.ParseIP(s.GatewayV6)
		if addr == nil {
			return fmt.Errorf("vm %s : passerelle IPv6 invalide %q", s.Name, s.GatewayV6)
		}
		if addr.To4() != nil {
			return fmt.Errorf("vm %s : %q est une adresse IPv4 dans le champ GatewayV6", s.Name, s.GatewayV6)
		}
	}
	if err := ValidateIdentity(s.UID); err != nil {
		return fmt.Errorf("vm %s : uid : %w", s.Name, err)
	}
	if err := ValidateIdentity(s.GID); err != nil {
		return fmt.Errorf("vm %s : gid : %w", s.Name, err)
	}
	return nil
}

// Metrics est un relevé instantané. Ces valeurs ne sont jamais écrites dans le
// magasin d'état : elles vivent en mémoire et partent par WebSocket.
type Metrics struct {
	PID    int           `json:"pid"`
	RSSMiB int64         `json:"rss_mib"`
	Uptime time.Duration `json:"uptime"`
}

// VMM est le contrat d'un hyperviseur de micro-VM.
//
// Toute implémentation doit confiner la VM. Pour Firecracker, cela veut dire
// jailer, sans exception : namespaces, chroot, cgroup, uid et gid dédiés,
// filtre seccomp.
type VMM interface {
	// Create prépare la VM sans la démarrer : jail, copie des artefacts,
	// configuration.
	Create(ctx context.Context, spec Spec) error

	// Start démarre une VM préparée.
	Start(ctx context.Context, name string) error

	// Stop demande un arrêt propre, puis force si la VM ne répond pas.
	Stop(ctx context.Context, name string) error

	// Delete libère tout ce que Create a réservé.
	Delete(ctx context.Context, name string) error

	// Metrics relève l'état instantané du processus.
	Metrics(ctx context.Context, name string) (Metrics, error)

	// ConsoleLogPath renvoie le chemin du journal de console série de la VM.
	// C'est la seule console qu'expose Firecracker, et la seule façon de
	// diagnostiquer un démarrage qui échoue.
	ConsoleLogPath(name string) string

	// Wait bloque jusqu'à la fin du processus de la VM. C'est ce qui permet au
	// superviseur de détecter un arrêt qu'il n'a pas demandé.
	Wait(ctx context.Context, name string) error
}
