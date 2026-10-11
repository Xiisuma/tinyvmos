#!/usr/bin/env bash
#
# TinyVMOS — test de la publication de services vers l'extérieur.
#
# Une publication expose un port d'une VM sur l'interface de sortie de la
# machine. C'est ce qui manquait au lot B2, et ce qu'il faut pour qu'un service
# soit joignable depuis le réseau, point 9.4 du plan.
#
# Le test fabrique un faux extérieur : une paire veth dont un bout sert
# d'interface de sortie à la machine, l'autre vivant dans un espace de noms
# réseau qui joue le client distant. C'est nécessaire parce qu'un paquet émis
# par l'hôte vers sa propre adresse ne traverse pas la chaîne prerouting : un
# test lancé depuis l'hôte ne prouverait rien de la redirection.
#
# Usage : sudo ./tests/phase2b3-publication.sh

set -uo pipefail
export SCRIPT_NAME="test-phase2b3"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

need_root

SERVICE="web-apache"
VM="web3"
ZONE="t2b3-pub"
TEST_RUN="${OUTPUT_DIR}/test-phase2b3"
STATE_FILE="${TEST_RUN}/state.json"
DAEMON_LOG="${TEST_RUN}/tinyvmosd.log"
DAEMON_BIN="${OUTPUT_DIR}/bin/tinyvmosd"
ZONES_FILE="${TEST_RUN}/zones.json"
PUBS_FILE="${TEST_RUN}/publications.json"

# Faux extérieur. 203.0.113.0/24 est la plage réservée à la documentation
# (RFC 5737) : aucun risque d'entrer en conflit avec un vrai réseau.
UPLINK="up-t2b3"
OUT_NS="dehors"
HOST_EXT="203.0.113.1"
CLIENT_EXT="203.0.113.9"
HOST_PORT=8080

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
    ip netns del "$OUT_NS" 2>/dev/null || true
    ip link del "$UPLINK" 2>/dev/null || true
    ip link del "tap-${VM}" 2>/dev/null || true
    ip link del "br-${ZONE}" 2>/dev/null || true
    nft delete table inet tinyvmos 2>/dev/null || true
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

dehors() { ip netns exec "$OUT_NS" "$@"; }

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
  {"name": "${ZONE}", "ipv4": "10.42.92.0/24", "comment": "zone du service publie"}
]
ZONES

cat > "$PUBS_FILE" <<PUBS
[
  {"vm": "${VM}", "port": 80, "host_port": ${HOST_PORT},
   "comment": "serveur web expose vers l exterieur"}
]
PUBS

# Faux extérieur : une paire veth, un bout sur l'hôte comme interface de
# sortie, l'autre dans un espace de noms qui joue le client distant.
log "mise en place du faux extérieur"
ip link add "$UPLINK" type veth peer name ext0 netns-none 2>/dev/null \
    || ip link add "$UPLINK" type veth peer name ext0
ip netns add "$OUT_NS"
ip link set ext0 netns "$OUT_NS"
ip addr add "${HOST_EXT}/24" dev "$UPLINK"
ip link set "$UPLINK" up
dehors ip link set lo up
dehors ip link set ext0 up
dehors ip addr add "${CLIENT_EXT}/24" dev ext0
dehors ip route add default via "$HOST_EXT"

dehors ping -c1 -W2 "$HOST_EXT" >/dev/null 2>&1
check $? "le faux extérieur joint la machine"

# ------------------------------------------------------- Démarrage

printf '\n'
log "=== démarrage avec un service publié ==="

cd "$REPO_ROOT" || die "cd impossible"
setsid "$DAEMON_BIN" \
    -state "$STATE_FILE" \
    -run-dir "${TEST_RUN}/run" \
    -jail-dir "${TEST_RUN}/jail" \
    -zones "$ZONES_FILE" \
    -publications "$PUBS_FILE" \
    -uplink "$UPLINK" \
    -log-level debug \
    -vm "name=${VM},service=${SERVICE},zone=${ZONE},mem=256" \
    < /dev/null > "$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!

if wait_for 60 curl -fsS --max-time 2 -o /dev/null "http://10.42.92.2/"; then
    check 0 "la vm répond sur son adresse de zone"
else
    check 1 "la vm ne répond pas"
    printf '\n--- journal ---\n' >&2; tail -40 "$DAEMON_LOG" >&2
    exit 1
fi

grep -q 'service publié' "$DAEMON_LOG"
check $? "le démon a journalisé la publication"

nft_contient 'chain prerouting'
check $? "la chaîne de redirection est en place"

nft_contient '10.42.92.2:80'
check $? "la redirection vise bien l'adresse de la vm"

# ------------------------------------------- Le service est joignable

printf '\n'
log "=== le service publié est joignable depuis l'extérieur ==="

if wait_for 20 dehors curl -fsS --max-time 3 -o /dev/null "http://${HOST_EXT}:${HOST_PORT}/"; then
    check 0 "depuis le faux extérieur, http://${HOST_EXT}:${HOST_PORT} atteint la vm"
else
    check 1 "le service publié n'est pas joignable"
    printf '    règles en place :\n' >&2
    nft list table inet tinyvmos >&2
fi

texte_contient "$(dehors curl -fsS --max-time 3 "http://${HOST_EXT}:${HOST_PORT}/")" 'web-apache'
check $? "la page servie est bien celle de la vm, pas celle d'un service de l'hôte"

# ----------------------------------- Rien d'autre n'est joignable

printf '\n'
log "=== rien d'autre n'est exposé ==="

! dehors curl -fsS --max-time 3 -o /dev/null "http://${HOST_EXT}:9999/"
check $? "un port non publié n'est pas joignable"

! dehors curl -fsS --max-time 3 -o /dev/null "http://10.42.92.2/"
check $? "l'adresse privée de la vm n'est pas joignable directement depuis l'extérieur"

! dehors ping -c1 -W2 10.42.92.2
check $? "la vm ne répond pas au ping depuis l'extérieur"

# ------------------------------------- Refus des configurations fausses

printf '\n'
log "=== refus des publications incohérentes ==="

expect_refusal() {
    local motif="$1" libelle="$2" fichier="$3"
    local out
    out=$("$DAEMON_BIN" -state "${TEST_RUN}/jetable.json" -zones "$ZONES_FILE" \
        -publications "$fichier" -uplink "$UPLINK" \
        -vm "name=${VM},service=${SERVICE},zone=${ZONE}" 2>&1 || true)
    if grep -q -- "$motif" <<< "$out"; then
        check 0 "$libelle"
    else
        check 1 "$libelle"
        printf '    sortie : %s\n' "$(printf '%s' "$out" | tail -2)" >&2
    fi
}

cat > "${TEST_RUN}/pub-vm-inconnue.json" <<'PUBS'
[{"vm": "fantome", "port": 80}]
PUBS
expect_refusal "pas déclarée" \
    "publier une vm inexistante est refusé" "${TEST_RUN}/pub-vm-inconnue.json"

cat > "${TEST_RUN}/pub-doublon.json" <<PUBS
[
  {"vm": "${VM}", "port": 80, "host_port": 9000},
  {"vm": "${VM}", "port": 443, "host_port": 9000}
]
PUBS
expect_refusal "déjà pris" \
    "deux publications sur le même port exposé sont refusées" "${TEST_RUN}/pub-doublon.json"

# ------------------------------------------------------- Arrêt propre

printf '\n'
log "=== arrêt propre ==="

stop_daemon

! nft list table inet tinyvmos >/dev/null 2>&1
check $? "la table a été retirée"

! dehors curl -fsS --max-time 3 -o /dev/null "http://${HOST_EXT}:${HOST_PORT}/"
check $? "le service n'est plus joignable après l'arrêt"

# ---------------------------------------------------------------- Bilan

printf '\n'
if [ "$FAIL" -eq 0 ]; then
    ok "publication de services validée : $PASS vérification(s) au vert."
    exit 0
else
    printf '  %d vérification(s) en échec sur %d.\n' "$FAIL" "$((PASS + FAIL))" >&2
    exit 1
fi
