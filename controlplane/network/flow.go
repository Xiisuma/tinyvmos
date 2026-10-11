package network

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Zones fictives utilisables comme source ou destination d'un flux.
//
// Elles ne portent ni bridge ni adressage : ce sont des points de rattachement
// qui rendent le modèle uniforme. « Cette zone a une sortie Internet » n'est
// donc pas un drapeau sur la zone, c'est un flux déclaré vers ZoneInternet,
// exactement comme n'importe quel autre flux. Un seul concept à comprendre,
// et la configuration se lit d'un bloc.
const (
	// ZoneInternet est tout ce qui se trouve derrière l'interface de sortie.
	ZoneInternet = "internet"

	// ZoneHost est la machine elle-même : les services que l'hôte rend aux VM,
	// le DNS interne par exemple.
	ZoneHost = "host"
)

// Protocoles acceptés dans un flux.
const (
	ProtoTCP  = "tcp"
	ProtoUDP  = "udp"
	ProtoICMP = "icmp"
	ProtoAny  = "any"
)

// Flow autorise un trafic entre deux zones. Tout ce qui n'est pas déclaré est
// refusé.
type Flow struct {
	From string `json:"from"`
	To   string `json:"to"`

	// Proto vaut tcp, udp, icmp ou any. Vide équivaut à any.
	Proto string `json:"proto,omitempty"`

	// Ports liste des ports de destination, en valeur isolée ou en intervalle :
	// "53", "8000-8100". Vide, tous les ports sont concernés.
	Ports []string `json:"ports,omitempty"`

	// Comment est repris en commentaire dans les règles produites, pour qu'un
	// opérateur qui lit nft list ruleset sache pourquoi une règle existe.
	Comment string `json:"comment,omitempty"`
}

var portRe = regexp.MustCompile(`^(\d{1,5})(-(\d{1,5}))?$`)

// ProtoOrAny renvoie le protocole effectif.
func (f Flow) ProtoOrAny() string {
	if f.Proto == "" {
		return ProtoAny
	}
	return f.Proto
}

// Validate refuse un flux incohérent.
func (f Flow) Validate(zones []Zone) error {
	if f.From == "" || f.To == "" {
		return fmt.Errorf("flux incomplet : from et to sont obligatoires (reçu from=%q to=%q)", f.From, f.To)
	}
	if f.From == f.To {
		return fmt.Errorf("flux %s vers %s : une zone parle librement à elle-même, ce flux est inutile", f.From, f.To)
	}

	known := map[string]bool{ZoneInternet: true, ZoneHost: true}
	for _, z := range zones {
		known[z.Name] = true
	}
	for _, name := range []string{f.From, f.To} {
		if !known[name] {
			return fmt.Errorf("flux %s vers %s : zone %q inconnue", f.From, f.To, name)
		}
	}

	// Deux zones fictives mises bout à bout ne décriraient rien que TinyVMOS
	// contrôle : l'hôte sort sur Internet par sa propre configuration réseau.
	pseudo := func(n string) bool { return n == ZoneInternet || n == ZoneHost }
	if pseudo(f.From) && pseudo(f.To) {
		return fmt.Errorf("flux %s vers %s : ces deux extrémités sont hors des zones gérées", f.From, f.To)
	}

	// Un flux entrant depuis Internet n'est pas un flux mais une publication :
	// il demande une redirection de port vers une VM précise, donc de savoir
	// laquelle. Le refuser clairement vaut mieux que de l'accepter sans effet.
	if f.From == ZoneInternet {
		return fmt.Errorf("flux %s vers %s : un accès entrant depuis Internet est une publication de service, pas un flux entre zones. Pas encore pris en charge",
			f.From, f.To)
	}

	switch f.ProtoOrAny() {
	case ProtoTCP, ProtoUDP:
		for _, p := range f.Ports {
			if err := validatePort(p); err != nil {
				return fmt.Errorf("flux %s vers %s : %w", f.From, f.To, err)
			}
		}
	case ProtoICMP, ProtoAny:
		if len(f.Ports) > 0 {
			return fmt.Errorf("flux %s vers %s : des ports sont indiqués alors que le protocole est %s. Préciser tcp ou udp",
				f.From, f.To, f.ProtoOrAny())
		}
	default:
		return fmt.Errorf("flux %s vers %s : protocole %q inconnu. Valeurs acceptées : tcp, udp, icmp, any",
			f.From, f.To, f.Proto)
	}

	return nil
}

func validatePort(p string) error {
	m := portRe.FindStringSubmatch(strings.TrimSpace(p))
	if m == nil {
		return fmt.Errorf("port %q invalide : un nombre ou un intervalle comme 8000-8100 est attendu", p)
	}
	lo, err := strconv.Atoi(m[1])
	if err != nil || lo < 1 || lo > 65535 {
		return fmt.Errorf("port %q hors bornes : 1 à 65535", p)
	}
	if m[3] != "" {
		hi, err := strconv.Atoi(m[3])
		if err != nil || hi < 1 || hi > 65535 {
			return fmt.Errorf("port %q hors bornes : 1 à 65535", p)
		}
		if hi <= lo {
			return fmt.Errorf("intervalle de ports %q : la borne haute doit dépasser la borne basse", p)
		}
	}
	return nil
}

// ValidateFlows vérifie un ensemble de flux et signale les doublons exacts,
// qui sont au mieux inutiles, au pire le signe d'une configuration recopiée
// sans être relue.
func ValidateFlows(flows []Flow, zones []Zone) error {
	seen := map[string]bool{}
	for _, f := range flows {
		if err := f.Validate(zones); err != nil {
			return err
		}
		key := fmt.Sprintf("%s|%s|%s|%s", f.From, f.To, f.ProtoOrAny(), strings.Join(f.Ports, ","))
		if seen[key] {
			return fmt.Errorf("flux %s vers %s en %s déclaré deux fois", f.From, f.To, f.ProtoOrAny())
		}
		seen[key] = true
	}
	return nil
}

// ExampleFlows renvoie un jeu de flux cohérent avec les zones d'exemple.
//
// Ce qui n'y figure pas compte autant que ce qui y figure : aucun flux ne part
// des zones donnees et ia, ni vers Internet ni vers quoi que ce soit d'autre.
// C'est ce qui réalise la promesse « aucune donnée ne sort », et ça se vérifie
// en lisant cette liste, pas en fouillant le code.
func ExampleFlows() []Flow {
	return []Flow{
		{
			From: "publique", To: "interne", Proto: ProtoUDP, Ports: []string{"53"},
			Comment: "resolution de noms par le DNS interne",
		},
		{
			From: "publique", To: "interne", Proto: ProtoTCP, Ports: []string{"53"},
			Comment: "resolution de noms en TCP, pour les reponses longues",
		},
		{
			From: "interne", To: ZoneHost, Proto: ProtoUDP, Ports: []string{"53"},
			Comment: "le DNS interne interroge le resolveur de l hote",
		},
		{
			From: "interne", To: ZoneInternet, Proto: ProtoTCP, Ports: []string{"80", "443"},
			Comment: "mises a jour des services d infrastructure",
		},
		{
			From: "gestion", To: "publique", Proto: ProtoAny,
			Comment: "le plan de controle supervise les services exposes",
		},
		{
			From: "gestion", To: "interne", Proto: ProtoAny,
			Comment: "le plan de controle supervise l infrastructure",
		},
		{
			From: "ia", To: "donnees", Proto: ProtoTCP, Ports: []string{"80", "443"},
			Comment: "les clients d IA lisent et ecrivent dans les coffres",
		},
	}
}
