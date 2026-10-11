package vm

import (
	"time"

	"github.com/Xiisuma/tinyvmos/controlplane/network"
)

// SchemaVersion est la version du schéma du magasin d'état. Elle est écrite
// dans le fichier pour qu'une version ultérieure du démon sache migrer un état
// écrit par une version antérieure.
const SchemaVersion = 1

// Record est ce que le démon doit se rappeler d'une VM entre deux démarrages.
//
// Seuls des faits durables y figurent. Le PID est conservé parce qu'il permet
// de retrouver un processus survivant à un redémarrage du démon ; le relevé de
// mémoire ou de CPU, lui, n'a rien à faire ici.
type Record struct {
	Spec   Spec   `json:"spec"`
	Status Status `json:"status"`
	PID    int    `json:"pid,omitempty"`

	// Restarts compte les relances automatiques depuis la création.
	Restarts int `json:"restarts"`

	// LastError retient la dernière erreur, pour que l'opérateur n'ait pas à
	// fouiller les journaux pour savoir pourquoi une VM est en échec.
	LastError string `json:"last_error,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Snapshot est l'état constaté complet du démon, tel qu'il est écrit sur disque.
type Snapshot struct {
	Version int                `json:"version"`
	VMs     map[string]*Record `json:"vms"`

	// Zones est la liste des zones réseau en place.
	Zones []network.Zone `json:"zones,omitempty"`

	// Alloc retient le préfixe IPv6 de l'installation, les indices de zone et
	// les adresses attribuées.
	//
	// Ces attributions sont enregistrées, pas recalculées : une adresse
	// dérivée d'un hachage du nom entrerait en collision tôt ou tard, et sans
	// bruit. Une VM doit aussi retrouver la même adresse après un redémarrage
	// du démon, sinon les règles de pare-feu ne voudraient plus rien dire.
	Alloc *network.Allocation `json:"alloc,omitempty"`
}

// NewSnapshot renvoie un état vide à la version courante du schéma.
func NewSnapshot() *Snapshot {
	return &Snapshot{Version: SchemaVersion, VMs: map[string]*Record{}}
}

// Store persiste l'état constaté.
//
// L'interface est déclarée ici, du côté qui la consomme, et non du côté de
// l'implémentation. Celle qui est retenue écrit un fichier JSON de façon
// atomique ; une autre pourrait la remplacer sans toucher au superviseur.
type Store interface {
	// Load lit l'état. Un magasin vide doit renvoyer un instantané vide, pas
	// une erreur : le premier démarrage est un cas normal.
	Load() (*Snapshot, error)

	// Save écrit l'état de façon atomique : soit l'ancien état complet, soit le
	// nouveau, jamais un mélange.
	Save(*Snapshot) error
}
