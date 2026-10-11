package network

import (
	"bytes"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// TableName est la table nftables que TinyVMOS possède.
//
// Famille inet : une seule table couvre IPv4 et IPv6. C'est ce qui évite
// d'écrire deux jeux de règles et, surtout, d'en oublier un — une règle IPv4
// sans son équivalent IPv6 est un trou qui ne se voit pas.
const TableName = "tinyvmos"

// zoneSetName contient les noms d'interface des bridges de zone.
const zoneSetName = "zones"

// Firewall produit et applique les règles de pare-feu.
type Firewall struct {
	Log *slog.Logger

	// Uplink est l'interface de sortie vers l'extérieur. Vide, elle est
	// déduite de la route par défaut.
	Uplink string

	// RulesetPath est l'endroit où le jeu de règles produit est écrit, pour
	// qu'un opérateur puisse lire exactement ce qui a été installé.
	RulesetPath string

	// applied retient ce qui a réellement été installé, pour que Verify
	// puisse constater une dérive.
	applied applied

	// pendingFingerprint est l'empreinte insérée dans la règle témoin pendant
	// la seconde passe de génération.
	pendingFingerprint string

	// lastEmbedded est la dernière empreinte réellement insérée dans la règle
	// témoin.
	//
	// Elle doit être retenue séparément : l'empreinte porte sur le jeu de
	// règles SANS la règle témoin, puisqu'elle y est ensuite insérée. La
	// recalculer sur le texte final donnerait une autre valeur, et la
	// comparaison échouerait à chaque passe — le pare-feu serait réécrit en
	// boucle et tous ses compteurs remis à zéro.
	lastEmbedded string
}

// NewFirewall renvoie un pare-feu.
func NewFirewall(log *slog.Logger, uplink, rulesetPath string) *Firewall {
	if log == nil {
		log = slog.Default()
	}
	return &Firewall{Log: log, Uplink: uplink, RulesetPath: rulesetPath}
}

// Policy rassemble ce qui détermine les règles.
type Policy struct {
	Zones []Zone
	Flows []Flow

	// Publications expose des ports de VM vers l'extérieur.
	Publications []Publication

	// Leases donne l'adressage de chaque VM, indexé par son nom. Nécessaire
	// aux publications, qui redirigent vers une adresse précise.
	Leases map[string]Lease
}

// Validate vérifie la cohérence de l'ensemble avant de toucher au pare-feu.
func (p Policy) Validate() error {
	if err := ValidateZones(p.Zones); err != nil {
		return err
	}
	if err := ValidateFlows(p.Flows, p.Zones); err != nil {
		return err
	}
	return ValidatePublications(p.Publications, p.Leases, p.Zones)
}

func (p Policy) zoneByName(name string) (Zone, bool) {
	for _, z := range p.Zones {
		if z.Name == name {
			return z, true
		}
	}
	return Zone{}, false
}

// egressZones renvoie les zones ayant un flux déclaré vers Internet, triées.
func (p Policy) egressZones() []Zone {
	seen := map[string]bool{}
	var out []Zone
	for _, f := range p.Flows {
		if f.To != ZoneInternet {
			continue
		}
		if seen[f.From] {
			continue
		}
		if z, ok := p.zoneByName(f.From); ok {
			seen[f.From] = true
			out = append(out, z)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Ruleset produit le jeu de règles nftables.
//
// Le choix d'engendrer du texte appliqué par « nft -f », plutôt que de
// construire les règles par netlink, est délibéré et s'écarte de ce que
// prévoyait le plan. Trois raisons :
//
//   - « nft -f » est atomique : le fichier passe en entier ou pas du tout. Un
//     pare-feu à moitié appliqué serait pire que pas de pare-feu.
//   - le jeu de règles est lisible. Sur une machine déployée, un opérateur doit
//     pouvoir constater ce qui a été installé, sans outil et sans confiance
//     aveugle. C'est le même raisonnement que pour l'état en JSON.
//   - construire à la main le bytecode des expressions nftables pour une
//     politique à deux familles est long et facile à rater d'un rien. Sur un
//     composant de sécurité, la lisibilité du résultat vaut plus que l'élégance
//     du moyen.
func (f *Firewall) Ruleset(p Policy) (string, error) {
	// L'empreinte porte sur la politique, pas sur le texte engendré : sinon
	// elle dépendrait d'elle-même. Elle est donc calculée sur un premier jet
	// sans règle témoin, puis insérée dans le jet définitif.
	f.pendingFingerprint = ""
	plain, err := f.ruleset(p)
	if err != nil {
		return "", err
	}
	f.pendingFingerprint = fingerprintOf(plain)
	f.lastEmbedded = f.pendingFingerprint
	defer func() { f.pendingFingerprint = "" }()
	return f.ruleset(p)
}

func (f *Firewall) ruleset(p Policy) (string, error) {
	if err := p.Validate(); err != nil {
		return "", err
	}

	uplink := f.Uplink
	if uplink == "" {
		uplink = detectUplink()
	}

	var b strings.Builder
	w := func(format string, args ...any) {
		fmt.Fprintf(&b, format+"\n", args...)
	}

	w("# Jeu de regles TinyVMOS, engendre par le demon. Ne pas modifier a la main :")
	w("# toute retouche est ecrasee a la prochaine reconciliation.")
	w("#")
	w("# Politique : tout trafic qui entre ou sort d une zone est refuse, sauf")
	w("# les flux declares ci-dessous.")
	w("")

	// Remettre la table à zéro avant de la réécrire. « delete table » suivi de
	// la nouvelle définition, dans le même fichier, fait un remplacement
	// atomique. Seule notre table est touchée : les règles d un autre outil sur
	// la machine ne sont jamais effacees.
	w("table inet %s", TableName)
	w("delete table inet %s", TableName)
	w("")

	w("table inet %s {", TableName)

	// Ensemble des interfaces de zone, pour que les règles de refus tiennent
	// en deux lignes au lieu d une par zone.
	w("\tset %s {", zoneSetName)
	w("\t\ttype ifname")
	if len(p.Zones) > 0 {
		names := make([]string, 0, len(p.Zones))
		for _, z := range p.Zones {
			names = append(names, `"`+z.BridgeName()+`"`)
		}
		sort.Strings(names)
		w("\t\telements = { %s }", strings.Join(names, ", "))
	}
	w("\t}")
	w("")

	f.writeForwardChain(w, p, uplink)
	f.writeInputChain(w, p)
	f.writePreroutingChain(w, p, uplink)
	f.writeNATChain(w, p, uplink)

	w("}")

	return b.String(), nil
}

// writeForwardChain écrit la chaîne qui filtre le trafic entre zones.
//
// Point délicat : la politique de la chaîne reste « accept », et le refus est
// obtenu par des règles explicites portant sur nos interfaces.
//
// Une politique « drop » s appliquerait à TOUT le trafic routé de la machine,
// y compris celui d autres outils — un conteneur, un tunnel. Dans nftables une
// politique de chaîne ne se laisse pas contredire par une autre table : notre
// table casserait le reste de la machine. Refuser nommément ce qui touche nos
// zones donne la même garantie, sans effet de bord : un paquet qui entre ou
// sort d une zone sans règle l autorisant est jeté, et un paquet qui ne
// concerne aucune zone traverse la chaîne sans être regardé.
func (f *Firewall) writeForwardChain(w func(string, ...any), p Policy, uplink string) {
	w("\tchain forward {")
	w("\t\ttype filter hook forward priority filter; policy accept;")
	w("")
	w("\t\t# Retours des connexions deja autorisees.")
	w("\t\tct state established,related accept")
	w("")
	w("\t\t# Regle temoin. Elle ne rend aucun verdict : elle sert a reconnaitre")
	w("\t\t# cette table et la version de politique qui y est posee. Si le")
	w("\t\t# pare-feu est vide par autre chose, elle disparait et la")
	w("\t\t# reconciliation s en apercoit.")
	w("\t\tcounter comment \"%s%s\"", fingerprintPrefix, f.pendingFingerprint)
	w("")
	w("\t\t# ICMPv6 de decouverte de voisins. A NE JAMAIS jeter en bloc : sans")
	w("\t\t# ces messages, IPv6 ne fonctionne pas du tout.")
	w("\t\ticmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit, packet-too-big, time-exceeded, parameter-problem, destination-unreachable } accept")
	w("")
	w("\t\t# Decouverte du MTU. La jeter produit des connexions qui s etablissent")
	w("\t\t# puis se figent, symptome tres couteux a diagnostiquer.")
	w("\t\ticmp type { destination-unreachable, time-exceeded, parameter-problem } accept")
	w("")

	if len(p.Flows) == 0 {
		w("\t\t# Aucun flux declare.")
		w("")
	}

	for _, fl := range p.Flows {
		if fl.To == ZoneHost {
			continue // traité dans la chaîne input
		}
		src, ok := p.zoneByName(fl.From)
		if !ok {
			continue
		}

		var oif string
		switch fl.To {
		case ZoneInternet:
			if uplink == "" {
				w("\t\t# Flux %s vers internet ignore : aucune interface de sortie detectee.", fl.From)
				continue
			}
			oif = uplink
		default:
			dst, ok := p.zoneByName(fl.To)
			if !ok {
				continue
			}
			oif = dst.BridgeName()
		}

		if fl.Comment != "" {
			w("\t\t# %s", fl.Comment)
		}
		w("\t\t%s", rule(
			fmt.Sprintf("iifname %q", src.BridgeName()),
			fmt.Sprintf("oifname %q", oif),
			matchExpr(fl),
			"accept",
		))
	}

	// Le trafic redirigé par une publication doit être autorisé à traverser
	// vers la VM. La redirection d'adresse a lieu avant ce filtrage : à ce
	// stade, la destination est déjà l'adresse de la VM.
	for _, pub := range sortedPublications(p.Publications) {
		lease, ok := p.Leases[pub.VM]
		if !ok || uplink == "" {
			continue
		}
		z, ok := p.zoneByName(lease.Zone)
		if !ok {
			continue
		}
		addr := lease.IPv4
		if i := strings.IndexByte(addr, '/'); i >= 0 {
			addr = addr[:i]
		}
		if pub.Comment != "" {
			w("\t\t# publication : %s", pub.Comment)
		}
		// ct status dnat est indispensable, pas décoratif. Sans lui, la règle
		// accepte aussi un paquet venu de l'extérieur visant directement
		// l'adresse privée de la VM, sans passer par la redirection. Sur un
		// réseau plat où quelqu'un ajoute une route vers la plage privée, le
		// service entier serait joignable en contournant le port publié.
		w("\t\t%s", rule(
			fmt.Sprintf("iifname %q", uplink),
			fmt.Sprintf("oifname %q", z.BridgeName()),
			"ct status dnat",
			fmt.Sprintf("ip daddr %s", addr),
			fmt.Sprintf("%s dport %d", pub.ProtoOrTCP(), pub.Port),
			"accept",
		))
	}

	w("")
	w("\t\t# Tout le reste de ce qui touche une zone est refuse. Journalise a")
	w("\t\t# debit limite : un refus doit se voir sans pouvoir remplir le disque.")
	w("\t\tiifname @%s limit rate 10/minute log prefix \"tinyvmos refus sortie zone \" level info", zoneSetName)
	w("\t\toifname @%s limit rate 10/minute log prefix \"tinyvmos refus entree zone \" level info", zoneSetName)
	w("\t\tiifname @%s counter drop", zoneSetName)
	w("\t\toifname @%s counter drop", zoneSetName)
	w("\t}")
	w("")
}

// writeInputChain écrit la chaîne qui filtre ce que les VM adressent à l hôte.
//
// Même raisonnement que pour forward, et l enjeu est encore plus net ici : une
// politique « drop » sur input couperait l accès d administration à la machine,
// SSH compris. On se verrouillerait dehors au premier démarrage.
func (f *Firewall) writeInputChain(w func(string, ...any), p Policy) {
	w("\tchain input {")
	w("\t\ttype filter hook input priority filter; policy accept;")
	w("")
	w("\t\tct state established,related accept")
	w("")
	w("\t\t# Decouverte de voisins depuis les zones : indispensable a IPv6.")
	w("\t\ticmpv6 type { nd-neighbor-solicit, nd-neighbor-advert, nd-router-solicit, destination-unreachable, packet-too-big, time-exceeded, parameter-problem } accept")
	w("")

	hostFlows := 0
	for _, fl := range p.Flows {
		if fl.To != ZoneHost {
			continue
		}
		src, ok := p.zoneByName(fl.From)
		if !ok {
			continue
		}
		if fl.Comment != "" {
			w("\t\t# %s", fl.Comment)
		}
		w("\t\t%s", rule(
			fmt.Sprintf("iifname %q", src.BridgeName()),
			matchExpr(fl),
			"accept",
		))
		hostFlows++
	}
	if hostFlows == 0 {
		w("\t\t# Aucun service de l hote n est ouvert aux zones.")
	}

	w("")
	w("\t\t# Le reste de ce qui vient d une zone est refuse. La politique de la")
	w("\t\t# chaine reste accept : la mettre a drop couperait l acces")
	w("\t\t# d administration de la machine.")
	w("\t\tiifname @%s limit rate 10/minute log prefix \"tinyvmos refus vers hote \" level info", zoneSetName)
	w("\t\tiifname @%s counter drop", zoneSetName)
	w("\t}")
	w("")
}

// writePreroutingChain écrit les redirections de port des services publiés.
//
// Seule la famille IPv4 est traitée, et c'est un choix. Les adresses IPv6 des
// zones sont des adresses locales uniques : les publier par redirection
// supposerait du NAT66, qui est une mauvaise pratique reconnue et casse la
// traçabilité des connexions. Publier correctement un service en IPv6 demande
// un préfixe routé depuis l'extérieur, ce que la machine n'a pas aujourd'hui.
// Mieux vaut ne rien faire que faire quelque chose de bancal sur ce terrain.
func (f *Firewall) writePreroutingChain(w func(string, ...any), p Policy, uplink string) {
	pubs := sortedPublications(p.Publications)

	w("\tchain prerouting {")
	w("\t\ttype nat hook prerouting priority dstnat; policy accept;")

	if len(pubs) == 0 {
		w("\t\t# Aucun service publie vers l exterieur.")
		w("\t}")
		w("")
		return
	}
	if uplink == "" {
		w("\t\t# Services publies ignores : aucune interface de sortie detectee.")
		w("\t}")
		w("")
		return
	}

	for _, pub := range pubs {
		lease, ok := p.Leases[pub.VM]
		if !ok {
			continue
		}
		addr := lease.IPv4
		if i := strings.IndexByte(addr, '/'); i >= 0 {
			addr = addr[:i]
		}
		if pub.Comment != "" {
			w("\t\t# %s", pub.Comment)
		}
		w("\t\tiifname %q %s dport %d dnat ip to %s:%d",
			uplink, pub.ProtoOrTCP(), pub.HostPortOrPort(), addr, pub.Port)
	}

	w("\t}")
	w("")
}

// writeNATChain écrit la traduction d adresses pour les zones qui ont une
// sortie Internet déclarée.
//
// Seules celles-là. Une zone sans flux vers Internet n a pas d entrée ici :
// même si une route existait, les paquets sortiraient avec une adresse privée
// et ne reviendraient jamais. Deux barrières valent mieux qu une pour une
// promesse du genre « aucune donnée ne sort ».
func (f *Firewall) writeNATChain(w func(string, ...any), p Policy, uplink string) {
	egress := p.egressZones()

	w("\tchain postrouting {")
	w("\t\ttype nat hook postrouting priority srcnat; policy accept;")
	if len(egress) == 0 || uplink == "" {
		w("\t\t# Aucune zone n a de sortie Internet declaree.")
	}
	for _, z := range egress {
		if uplink == "" {
			break
		}
		w("\t\tip saddr %s oifname \"%s\" masquerade", z.IPv4, uplink)
		if z.IPv6 != "" {
			w("\t\tip6 saddr %s oifname \"%s\" masquerade", z.IPv6, uplink)
		}
	}
	w("\t}")
}

// rule assemble une règle en ignorant les morceaux vides.
//
// Sans ça, un flux qui ne précise aucun protocole produit un double espace
// dans le fichier engendré. nft l'accepte, mais ce fichier est fait pour être
// relu par un opérateur : autant qu'il soit propre.
func rule(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			kept = append(kept, trimmed)
		}
	}
	return strings.Join(kept, " ")
}

// matchExpr rend la partie protocole et ports d une règle.
func matchExpr(f Flow) string {
	switch f.ProtoOrAny() {
	case ProtoTCP, ProtoUDP:
		proto := f.ProtoOrAny()
		if len(f.Ports) == 0 {
			return fmt.Sprintf("meta l4proto %s", proto)
		}
		ports := make([]string, 0, len(f.Ports))
		for _, p := range f.Ports {
			ports = append(ports, strings.Replace(strings.TrimSpace(p), "-", "-", 1))
		}
		return fmt.Sprintf("meta l4proto %s %s dport { %s }", proto, proto, strings.Join(ports, ", "))
	case ProtoICMP:
		// Les deux familles : icmp en IPv4, icmpv6 en IPv6. Ne couvrir que la
		// première laisserait le ping IPv6 au tapis sans raison lisible.
		return "meta l4proto { icmp, icmpv6 }"
	default:
		return "" // tous protocoles
	}
}

// Apply installe le jeu de règles. L opération est atomique : « nft -f »
// applique le fichier en entier ou le refuse en entier.
func (f *Firewall) Apply(p Policy) error {
	ruleset, err := f.Ruleset(p)
	if err != nil {
		return err
	}

	if err := f.Check(ruleset); err != nil {
		return err
	}

	if f.RulesetPath != "" {
		if err := os.MkdirAll(filepath.Dir(f.RulesetPath), 0o750); err == nil {
			if err := os.WriteFile(f.RulesetPath, []byte(ruleset), 0o640); err != nil {
				f.Log.Warn("jeu de regles non ecrit sur disque", "chemin", f.RulesetPath, "erreur", err)
			}
		}
	}

	if err := runNft(ruleset, false); err != nil {
		return fmt.Errorf("application du pare-feu : %w", err)
	}

	f.rememberApplied(ruleset)

	f.Log.Info("pare-feu applique",
		"zones", len(p.Zones), "flux", len(p.Flows),
		"publications", len(p.Publications), "regles", f.RulesetPath)
	return nil
}

// Check demande à nft de valider le jeu de règles sans l installer.
//
// Un jeu de règles refusé doit l être avant que la table en place ne soit
// supprimée, pas après.
func (f *Firewall) Check(ruleset string) error {
	if err := runNft(ruleset, true); err != nil {
		return fmt.Errorf("jeu de regles refuse par nft : %w", err)
	}
	return nil
}

// Flush retire la table de TinyVMOS, et rien d autre.
func (f *Firewall) Flush() error {
	ruleset := fmt.Sprintf("table inet %s\ndelete table inet %s\n", TableName, TableName)
	if err := runNft(ruleset, false); err != nil {
		return fmt.Errorf("retrait du pare-feu : %w", err)
	}
	f.forgetApplied()
	f.Log.Info("pare-feu retire", "table", TableName)
	return nil
}

func runNft(ruleset string, checkOnly bool) error {
	nft, err := exec.LookPath("nft")
	if err != nil {
		return fmt.Errorf("binaire nft introuvable : %w", err)
	}

	args := []string{"-f", "-"}
	if checkOnly {
		args = append([]string{"--check"}, args...)
	}

	cmd := exec.Command(nft, args...)
	cmd.Stdin = strings.NewReader(ruleset)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// detectUplink renvoie l interface portant la route par défaut.
func detectUplink() string {
	out, err := exec.Command("ip", "-4", "route", "show", "default").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}
