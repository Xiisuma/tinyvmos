package vm

import (
	"time"

	"github.com/Xiisuma/tinyvmos/controlplane/network"
)

// SchemaVersion est la version du schéma du magasin d'état. Elle est écrite
// dans le fichier pour qu'une version ultérieure du démon sache migrer un état
// écrit par une version antérieure.
//
// Version 2 : les empreintes des artefacts démarrables. Le numéro est monté
// plutôt que le champ ajouté en silence, parce qu'un démon antérieur lirait
// cet état sans vérifier aucune empreinte : le refus explicite vaut mieux
// qu'un retour en arrière qui désactive la vérification sans le dire.
const SchemaVersion = 2

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

// Pin est l'empreinte retenue d'un artefact démarrable — image de micro-VM ou
// noyau invité.
//
// Elle est enregistrée à la première utilisation de l'artefact et comparée à
// chaque démarrage suivant : c'est ce qui fait qu'une image modifiée dans le
// catalogue ne démarre pas. Voir integrity.go pour ce que cette garantie vaut
// exactement, et ce qu'elle ne couvre pas.
type Pin struct {
	// SHA256 est l'empreinte attendue, en hexadécimal minuscule.
	SHA256 string `json:"sha256"`

	// Source dit d'où vient cette empreinte : métadonnées de construction,
	// première utilisation, ou acceptation explicite par un opérateur. Sans
	// cette information, impossible de savoir ce que la vérification vaut.
	Source string `json:"source"`

	PinnedAt time.Time `json:"pinned_at"`
}

// Snapshot est l'état constaté complet du démon, tel qu'il est écrit sur disque.
type Snapshot struct {
	Version int                `json:"version"`
	VMs     map[string]*Record `json:"vms"`

	// Zones est la liste des zones réseau en place.
	Zones []network.Zone `json:"zones,omitempty"`

	// Identities associe un nom de VM à son identifiant confiné, qui sert à la
	// fois d'uid et de gid.
	//
	// Persisté pour que l'identité d'une VM reste la même d'un démarrage à
	// l'autre : sinon les fichiers de travail de la passe précédente
	// deviendraient illisibles pour elle.
	Identities map[string]int `json:"identities,omitempty"`

	// Artifacts associe le chemin absolu d'un artefact démarrable à son
	// empreinte retenue.
	//
	// La clé est le chemin, et non le nom du service : un artefact est un
	// fichier, et c'est son contenu à cet endroit qui est vérifié. Pointer un
	// service vers une autre image est un changement de configuration du
	// démon, pas une altération d'image — qui peut changer la ligne de
	// commande peut aussi passer -trust.
	Artifacts map[string]Pin `json:"artifacts,omitempty"`

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
