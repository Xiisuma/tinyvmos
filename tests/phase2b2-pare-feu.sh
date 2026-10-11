#!/usr/bin/env bash
#
# TinyVMOS — test d'acceptation du lot B2 de la Phase 2 : l'isolation.
#
# C'est le test qui porte la promesse du projet. Tout ce qui précède ne faisait
# que préparer le terrain : ici on vérifie qu'un flux déclaré passe, que tout le
# reste est jeté, et que les zones sans flux ne peuvent joindre personne.
#
# Les sondes sont des espaces de noms réseau rattachés aux bridges de zone, et
# non la machine hôte. C'est délibéré : le trafic émis par l'hôte ne traverse pas
# la chaîne forward, donc un test lancé depuis l'hôte ne prouverait rien du
# pare-feu entre zones. Un espace de noms derrière un veth produit exactement le
# même chemin qu'une VM.
#
# Usage : sudo ./tests/phase2b2-pare-feu.sh

set -uo pipefail
export SCRIPT_NAME="test-phase2b2"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

need_root

SERVICE="web-apache"
VM="web2"
TEST_RUN="${OUTPUT_DIR}/test-phase2b2"
STATE_FILE="${TEST_RUN}/state.json"
DAEMON_LOG="${TEST_RUN}/tinyvmosd.log"
DAEMON_BIN="${OUTPUT_DIR}/bin/tinyvmosd"
ZONES_FILE="${TEST_RUN}/zones.json"
FLOWS_FILE="${TEST_RUN}/flows.json"
RULESET="${TEST_RUN}/run/ruleset.nft"

Z_PUB="t2b2-pub"
Z_INT="t2b2-int"
Z_DAT="t2b2-dat"
VM_IP="10.42.94.2"

PASS=0
FAIL=0
DAEMON_PID=""

check() {
    if [ "$1" -eq 0 ]; then
        ok "$2"; PASS=$((PASS + 1))
    else
        printf '  ECHEC : %s\n' "$2" >&2; FAIL=$((FAIL + 1))
    fi
}

stop_daemon() {
    [ -n "$DAEMON_PID" ] || return 0
    kill -TERM "$DAEMON_PID" 2>/dev/null || true
    local i=0
    while kill -0 "$DAEMON_PID" 2>/dev/null && [ "$i" -lt 150 ]; do sleep 0.1; i=$((i + 1)); done
    kill -9 "$DAEMON_PID" 2>/dev/null || true
    DAEMON_PID=""
}

cleanup() {
    stop_daemon
    pkill -x firecracker 2>/dev/null || true
    for ns in probe-pub probe-dat; do ip netns del "$ns" 2>/dev/null || true; done
    for v in vpp vpd; do ip link del "$v" 2>/dev/null || true; done
    ip link del "tap-${VM}" 2>/dev/null || true
    for z in "$Z_PUB" "$Z_INT" "$Z_DAT"; do ip link del "br-${z}" 2>/dev/null || true; done
    nft delete table inet tinyvmos 2>/dev/null || true
    nft delete table inet essai_voisin 2>/dev/null || true
}
trap cleanup EXIT

wait_for() {
    local seconds="$1"; shift
    local deadline=$(( $(date +%s) + seconds ))
    until "$@" >/dev/null 2>&1; do
        [ "$(date +%s)" -ge "$deadline" ] && return 1
        sleep 0.25
    done
    return 0
}

# Crée une sonde : espace de noms réseau relié au bridge d'une zone.
make_probe() {
    local ns="$1" veth="$2" bridge="$3" ip4="$4" gw4="$5" ip6="$6" gw6="$7"

    ip link show "$bridge" >/dev/null 2>&1         || die "sonde $ns : le bridge $bridge n existe pas. Une zone declaree doit etre posee par le demon."

    ip netns add "$ns"
    ip link add "$veth" type veth peer name peer0 netns "$ns"
    ip link set "$veth" master "$bridge" up

    ip netns exec "$ns" ip link set lo up
    ip netns exec "$ns" ip link set peer0 up
    ip netns exec "$ns" ip addr add "$ip4" dev peer0
    ip netns exec "$ns" ip route add default via "$gw4"

    if [ -n "$ip6" ]; then
        ip netns exec "$ns" sysctl -qw net.ipv6.conf.peer0.accept_dad=0
        ip netns exec "$ns" ip -6 addr add "$ip6" dev peer0
        ip netns exec "$ns" ip -6 route add default via "$gw6" 2>/dev/null || true
    fi
}

# Lit le total des paquets jetés par les règles de refus de notre table.
drop_counter() {
    nft -a list table inet tinyvmos 2>/dev/null \
        | grep -oE 'counter packets [0-9]+' | grep -oE '[0-9]+' \
        | awk '{s+=$1} END {print s+0}'
}

# ------------------------------------------------------------ Préparation

log "préparation"
command -v jq >/dev/null || die "jq requis"
command -v nft >/dev/null || die "nft requis"
[ -x "$DAEMON_BIN" ] || die "démon absent : cd controlplane && go build -o ../output/bin/tinyvmosd ./cmd/tinyvmosd"
id -u tinyvmos >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin tinyvmos

"${REPO_ROOT}/images/kernel/fetch-kernel.sh" >/dev/null 2>&1 || die "noyau invité indisponible"
if [ ! -f "${IMAGES_OUT}/${SERVICE}.ext4" ]; then
    "${REPO_ROOT}/images/build-image.sh" "$SERVICE" >/dev/null || die "construction de l'image impossible"
fi

cleanup
rm -rf "$TEST_RUN"
mkdir -p "$TEST_RUN"

cat > "$ZONES_FILE" <<ZONES
[
  {"name": "${Z_PUB}", "ipv4": "10.42.95.0/24", "comment": "sonde autorisee"},
  {"name": "${Z_INT}", "ipv4": "10.42.94.0/24", "comment": "la vm qui sert"},
  {"name": "${Z_DAT}", "ipv4": "10.42.93.0/24", "comment": "zone sans aucun flux"}
]
ZONES

# Un seul flux autorisé. Tout le reste doit être refusé — c'est l'objet du test.
cat > "$FLOWS_FILE" <<FLOWS
[
  {"from": "${Z_PUB}", "to": "${Z_INT}", "proto": "tcp", "ports": ["80"],
   "comment": "seul flux autorise de ce test"}
]
FLOWS

# Une table nftables étrangère, posée avant le démon. Elle doit survivre à tout :
# TinyVMOS ne possède que sa propre table et ne doit jamais toucher au reste.
nft -f - <<'VOISIN'
table inet essai_voisin {
	chain surveille {
		type filter hook forward priority 0; policy accept;
		counter
	}
}
VOISIN

# ------------------------------------------------------- Démarrage

printf '\n'
log "=== démarrage ==="

cd "$REPO_ROOT" || die "cd impossible"
setsid "$DAEMON_BIN" \
    -state "$STATE_FILE" \
    -run-dir "${TEST_RUN}/run" \
    -jail-dir "${TEST_RUN}/jail" \
    -zones "$ZONES_FILE" \
    -flows "$FLOWS_FILE" \
    -log-level debug \
    -vm "name=${VM},service=${SERVICE},zone=${Z_INT},mem=256" \
    < /dev/null > "$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!

if wait_for 60 curl -fsS --max-time 2 -o /dev/null "http://${VM_IP}/"; then
    check 0 "la vm répond depuis l'hôte : le plan de contrôle garde accès à ses vm"
else
    check 1 "la vm ne répond pas depuis l'hôte"
    printf '\n--- journal ---\n' >&2; tail -40 "$DAEMON_LOG" >&2
    exit 1
fi

nft list table inet tinyvmos >/dev/null 2>&1
check $? "la table nftables tinyvmos est en place"

[ -f "$RULESET" ]
check $? "le jeu de règles appliqué est écrit sur disque, lisible par un opérateur"

# ------------------------------------------------------------ Sondes

printf '\n'
log "=== mise en place des sondes ==="

Z_PUB_V6=$(jq -r ".zones[] | select(.name==\"${Z_PUB}\") | .ipv6" "$STATE_FILE")
Z_DAT_V6=$(jq -r ".zones[] | select(.name==\"${Z_DAT}\") | .ipv6" "$STATE_FILE")
VM_V6=$(jq -r ".alloc.leases.${VM}.ipv6" "$STATE_FILE")
VM_V6_ADDR=${VM_V6%/*}

make_probe probe-pub vpp "br-${Z_PUB}" 10.42.95.9/24 10.42.95.1 \
    "${Z_PUB_V6%::/64}::9/64" "${Z_PUB_V6%::/64}::1"
make_probe probe-dat vpd "br-${Z_DAT}" 10.42.93.9/24 10.42.93.1 \
    "${Z_DAT_V6%::/64}::9/64" "${Z_DAT_V6%::/64}::1"

ip netns exec probe-pub ip addr show peer0 | grep -q '10.42.95.9'
check $? "la sonde de la zone autorisée est en place"

ip netns exec probe-dat ip addr show peer0 | grep -q '10.42.93.9'
check $? "la sonde de la zone sans flux est en place"

# ------------------------------------------------- Le flux déclaré passe

printf '\n'
log "=== ce qui est déclaré passe ==="

if ip netns exec probe-pub curl -fsS --max-time 5 -o /dev/null "http://${VM_IP}/"; then
    check 0 "zone autorisée vers la vm sur le port 80 : passe, comme déclaré"
else
    check 1 "le flux déclaré ne passe pas"
    printf '    règles en place :\n' >&2
    nft list table inet tinyvmos >&2
fi

if [ -n "$VM_V6_ADDR" ] && [ "$VM_V6_ADDR" != "null" ]; then
    if ip netns exec probe-pub curl -gfsS --max-time 8 -o /dev/null "http://[${VM_V6_ADDR}]/"; then
        check 0 "le même flux passe aussi en IPv6 : une seule table inet couvre les deux familles"
    else
        check 1 "le flux déclaré ne passe pas en IPv6"
    fi
else
    check 1 "pas d'adresse IPv6 pour la vm"
fi

# --------------------------------------------- Ce qui n'est pas déclaré tombe

printf '\n'
log "=== ce qui n'est pas déclaré est jeté ==="

AVANT=$(drop_counter)

! ip netns exec probe-dat curl -fsS --max-time 4 -o /dev/null "http://${VM_IP}/"
check $? "zone sans aucun flux vers la vm : refusé"

! ip netns exec probe-pub curl -fsS --max-time 4 -o /dev/null "http://${VM_IP}:443/"
check $? "zone autorisée mais port non déclaré : refusé"

! ip netns exec probe-dat ping -c1 -W2 "$VM_IP"
check $? "ping entre zones sans flux : refusé"

! ip netns exec probe-dat curl -fsS --max-time 4 -o /dev/null "http://10.42.95.9/"
check $? "zone sans flux vers l'autre zone de sonde : refusé"

APRES=$(drop_counter)
[ "$APRES" -gt "$AVANT" ]
check $? "les compteurs de refus ont progressé (${AVANT} puis ${APRES}) : les paquets sont bien jetés par nos règles, pas perdus ailleurs"

if [ -n "$Z_DAT_V6" ] && [ "$Z_DAT_V6" != "null" ]; then
    ! ip netns exec probe-dat curl -gfsS --max-time 4 -o /dev/null "http://[${VM_V6_ADDR}]/"
    check $? "refus effectif en IPv6 aussi : aucun contournement par la seconde famille"
fi

# ----------------------------------------------- Accès à l'hôte filtré

printf '\n'
log "=== accès des zones à l'hôte ==="

! ip netns exec probe-dat ping -c1 -W2 10.42.93.1
check $? "ping d'une zone vers l'hôte sans flux déclaré : refusé"

# L'hôte garde son accès sortant : couper l'administration de la machine serait
# le défaut le plus grave que ce pare-feu pourrait avoir.
ping -c1 -W2 10.42.94.2 >/dev/null 2>&1
check $? "l'hôte joint toujours ses vm : aucun verrouillage de l'administration"

# ------------------------------------------- Traduction d'adresses

printf '\n'
log "=== sortie Internet ==="

# Aucun flux vers Internet n'est déclaré dans ce test : aucune zone ne doit
# avoir de traduction d'adresses. C'est la seconde barrière qui tient la
# promesse « aucune donnée ne sort ».
! nft list table inet tinyvmos | grep -q 'masquerade'
check $? "aucune traduction d'adresses, puisqu'aucune sortie Internet n'est déclarée"

! ip netns exec probe-dat curl -fsS --max-time 4 -o /dev/null "http://1.1.1.1/"
check $? "zone sans flux vers Internet : refusé"

# ------------------------------------- La table du voisin est intacte

printf '\n'
log "=== respect du reste de la machine ==="

nft list table inet essai_voisin >/dev/null 2>&1
check $? "la table nftables étrangère a survécu : TinyVMOS ne possède que sa table"

! nft list table inet tinyvmos | grep -q 'policy drop'
check $? "aucune chaîne en policy drop : la table ne casse pas le trafic des autres outils"

# ------------------------------------------------------- Arrêt propre

printf '\n'
log "=== arrêt propre ==="

stop_daemon

! nft list table inet tinyvmos >/dev/null 2>&1
check $? "la table tinyvmos a été retirée à l'extinction"

nft list table inet essai_voisin >/dev/null 2>&1
check $? "la table étrangère est toujours là après le retrait"

! pgrep -x firecracker >/dev/null 2>&1
check $? "aucun processus Firecracker ne survit"

# ---------------------------------------------------------------- Bilan

printf '\n'
if [ "$FAIL" -eq 0 ]; then
    ok "lot B2 de la Phase 2 validé : $PASS vérification(s) au vert."
    exit 0
else
    printf '  %d vérification(s) en échec sur %d.\n' "$FAIL" "$((PASS + FAIL))" >&2
    exit 1
fi
