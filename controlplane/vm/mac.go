package vm

import (
	"crypto/sha256"
	"fmt"
)

// DeterministicMAC dérive une adresse MAC du nom de la VM.
//
// Une VM garde ainsi la même adresse d'un démarrage à l'autre, ce qui rend les
// baux DHCP et les règles de pare-feu reproductibles — propriété nécessaire
// dès que les zones arrivent.
//
// Le premier octet vaut 0x02 : bit « localement administré » à 1, bit
// « multicast » à 0, comme l'exige une adresse d'unité valide qui n'empiète pas
// sur une plage attribuée à un constructeur.
func DeterministicMAC(name string) string {
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("02:fc:%02x:%02x:%02x:%02x", sum[0], sum[1], sum[2], sum[3])
}
