package vm

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// parseCIDR découpe une notation CIDR en adresse et masque décimal pointé,
// forme attendue par le paramètre ip= de la ligne de commande du noyau.
func parseCIDR(cidr string) (addr string, mask string, err error) {
	ip, netw, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", err
	}
	v4 := netw.Mask
	if len(v4) != net.IPv4len {
		return "", "", fmt.Errorf("masque non IPv4 dans %q", cidr)
	}
	return ip.String(), net.IP(v4).String(), nil
}

// detectCgroupVersion renvoie "2" ou "1" selon la hiérarchie montée sur l'hôte.
//
// jailer refuse de démarrer si on lui annonce une version de cgroup absente
// de la machine, avec le message CgroupHierarchyMissing. Le SDK laisse ce
// champ vide par défaut, ce qui fait retomber jailer sur cgroup v1 : erreur
// garantie sur tout hôte en v2 pur, dont WSL2 et la plupart des distributions
// récentes. La détection vaut mieux qu'une valeur en dur : l'OS doit démarrer
// sur un parc hétérogène.
//
// La présence de /sys/fs/cgroup/cgroup.controllers est le marqueur de v2.
func detectCgroupVersion() string {
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		return "2"
	}
	return "1"
}

// readRSSMiB lit la mémoire résidente d'un processus depuis /proc.
//
// C'est la mémoire consommée côté hôte par le processus Firecracker, qui n'est
// pas la mémoire allouée à l'invité : Firecracker ne réserve pas d'emblée la
// totalité de mem_size_mib.
func readRSSMiB(pid int) (int64, error) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0, err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("ligne VmRSS inattendue : %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb / 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("VmRSS absent de /proc/%d/status", pid)
}
