package vm

import (
	"fmt"
	"sort"
)

// Plage d'identifiants réservée aux micro-VM.
//
// Chaque VM reçoit son propre uid et son propre gid, au lieu de les partager.
// Sans cela, une VM qui s'échapperait de Firecracker aurait les droits de
// toutes les autres : leurs images de travail, leurs jails, leurs sockets.
// L'isolation réseau tiendrait, celle du système de fichiers tomberait d'un
// coup. La documentation Firecracker recommande explicitement un identifiant
// par VM.
//
// La plage 60000-60999 est au-dessus des comptes ordinaires et en dessous de
// nobody (65534), et laisse mille VM — bien au-delà de ce qu'une machine peut
// faire tourner.
const (
	IdentityMin = 60000
	IdentityMax = 60999
)

// Aucun utilisateur système n'est créé pour ces identifiants, et c'est
// volontaire.
//
// jailer appelle setuid avec une valeur numérique, et chown aussi : ni l'un ni
// l'autre n'a besoin d'une entrée dans /etc/passwd. Firecracker confiné ne lit
// jamais la base des utilisateurs. Créer mille comptes pour mille VM
// polluerait le système pour rien, et chaque création serait une écriture de
// plus à faire échouer.

// AllocateIdentity renvoie le plus petit identifiant libre de la plage.
//
// Le plus petit libre, et non le suivant : un identifiant rendu doit pouvoir
// être réutilisé, sinon la plage s'épuiserait au fil des créations et
// suppressions.
func AllocateIdentity(taken map[int]bool) (int, error) {
	for id := IdentityMin; id <= IdentityMax; id++ {
		if !taken[id] {
			return id, nil
		}
	}
	return 0, fmt.Errorf("plus d'identifiant disponible dans la plage %d-%d : %d vm déjà déclarées",
		IdentityMin, IdentityMax, len(taken))
}

// ValidateIdentity refuse un identifiant hors de la plage réservée.
//
// Un zéro serait root, et un identifiant pris ailleurs sur le système
// donnerait à une VM les droits d'un vrai compte.
func ValidateIdentity(id int) error {
	if id == 0 {
		return fmt.Errorf("identifiant 0 : une vm ne doit jamais tourner en root")
	}
	if id < IdentityMin || id > IdentityMax {
		return fmt.Errorf("identifiant %d hors de la plage réservée %d-%d : il pourrait correspondre à un compte réel du système",
			id, IdentityMin, IdentityMax)
	}
	return nil
}

// identitiesTaken renvoie les identifiants déjà attribués.
func identitiesTaken(identities map[string]int, except string) map[int]bool {
	taken := make(map[int]bool, len(identities))
	for name, id := range identities {
		if name == except {
			continue
		}
		taken[id] = true
	}
	return taken
}

// SortedIdentities renvoie les attributions triées par nom de VM, pour que les
// affichages soient stables d'un appel à l'autre.
func SortedIdentities(identities map[string]int) []struct {
	VM string
	ID int
} {
	out := make([]struct {
		VM string
		ID int
	}, 0, len(identities))
	for name, id := range identities {
		out = append(out, struct {
			VM string
			ID int
		}{name, id})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VM < out[j].VM })
	return out
}
