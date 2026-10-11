#!/usr/bin/env bash
#
# TinyVMOS — test de la boucle de réconciliation.
#
# Le démon posait son réseau et son pare-feu une seule fois au démarrage puis
# ne corrigeait plus rien. Un « nft flush ruleset » lancé par n'importe quel
# autre outil détruisait donc toute l'isolation, définitivement et sans un mot.
#
# Ce test détruit volontairement ce que le démon a posé, et vérifie qu'il le
# rétablit. Il vérifie aussi le contraire : avec la réconciliation désactivée,
# rien n'est réparé — sinon le drapeau ne voudrait rien dire.
#
# Usage : sudo ./tests/phase2c-reconciliation.sh

set -uo pipefail
export SCRIPT_NAME="test-phase2c"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

need_root

SERVICE="web-apache"
VM="web4"
ZONE="t2c-int"
ZONE_VIDE="t2c-libre"
TEST_RUN="${OUTPUT_DIR}/test-phase2c"
STATE_FILE="${TEST_RUN}/state.json"
DAEMON_LOG="${TEST_RUN}/tinyvmosd.log"
DAEMON_BIN="${OUTPUT_DIR}/bin/tinyvmosd"
ZONES_FILE="${TEST_RUN}/zones.json"
INTERVAL=2

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
    ip link del "tap-${VM}" 2>/dev/null || true
    ip link del "br-${ZONE}" 2>/dev/null || true
    ip link del "br-${ZONE_VIDE}" 2>/dev/null || true
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

table_presente() { nft list table inet tinyvmos >/dev/null 2>&1; }

start_daemon() {
    local interval="$1"
    cd "$REPO_ROOT" || die "cd impossible"
    setsid "$DAEMON_BIN" \
        -state "$STATE_FILE" \
        -run-dir "${TEST_RUN}/run" \
        -jail-dir "${TEST_RUN}/jail" \
        -zones "$ZONES_FILE" \
        -reconcile-interval "$interval" \
        -log-level debug \
        -vm "name=${VM},service=${SERVICE},zone=${ZONE},mem=256" \
        < /dev/null >> "$DAEMON_LOG" 2>&1 &
    DAEMON_PID=$!
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

# Une zone sans VM, pour vérifier que son bridge est aussi réparé.
cat > "$ZONES_FILE" <<ZONES
[
  {"name": "${ZONE}", "ipv4": "10.42.91.0/24", "comment": "zone de la vm"},
  {"name": "${ZONE_VIDE}", "ipv4": "10.42.90.0/24", "comment": "zone sans vm"}
]
ZONES

# ------------------------------------------------------- Démarrage

printf '\n'
log "=== démarrage avec réconciliation toutes les ${INTERVAL}s ==="

start_daemon "${INTERVAL}s"

if wait_for 60 curl -fsS --max-time 2 -o /dev/null "http://10.42.91.2/"; then
    check 0 "la vm répond"
else
    check 1 "la vm ne répond pas"
    tail -40 "$DAEMON_LOG" >&2
    exit 1
fi

grep -q 'réconciliation démarrée' "$DAEMON_LOG"
check $? "la boucle de réconciliation est démarrée"

table_presente
check $? "le pare-feu est en place"

nft_contient 'tinyvmos-empreinte-'
check $? "la règle témoin est posée : c'est elle qui permet de reconnaître la table"

# ------------------------------- Stabilité : aucune réécriture inutile

printf '\n'
log "=== stabilité au repos ==="

# Si le pare-feu était réécrit à chaque passe, ses compteurs repartiraient de
# zéro en permanence et on perdrait toute trace des refus. Le témoin de ce
# défaut est le handle des règles, qui change à chaque recréation de la table.
HANDLE_AVANT=$(nft -a list table inet tinyvmos | grep -oE 'handle [0-9]+' | tail -1)
AVANT_REPOS=$(grep -c 'dérive corrigée' "$DAEMON_LOG" || true)

sleep $((INTERVAL * 4))

HANDLE_APRES=$(nft -a list table inet tinyvmos | grep -oE 'handle [0-9]+' | tail -1)
[ "$HANDLE_AVANT" = "$HANDLE_APRES" ]
check $? "la table n'est pas recréée au repos (${HANDLE_AVANT} puis ${HANDLE_APRES})"

APRES_REPOS=$(grep -c 'dérive corrigée' "$DAEMON_LOG" || true)
[ "$AVANT_REPOS" = "$APRES_REPOS" ]
check $? "aucune dérive n'est signalée alors que rien n'a bougé"

# ----------------------------------- Destruction totale du pare-feu

printf '\n'
log "=== destruction du pare-feu par un tiers ==="

AVANT=$(wc -l < "$DAEMON_LOG")

nft flush ruleset
! table_presente
check $? "le pare-feu a bien été détruit"

if wait_for $((INTERVAL * 6)) table_presente; then
    check 0 "le pare-feu est rétabli tout seul"
else
    check 1 "le pare-feu n'a pas été rétabli"
    tail -n +"$AVANT" "$DAEMON_LOG" >&2
fi

nft_contient 'tinyvmos-empreinte-'
check $? "la table rétablie porte bien la règle témoin"

nft_correspond 'iifname @zones counter.*drop'
check $? "les règles de refus sont de retour : l'isolation est rétablie"

texte_contient "$(tail -n +"$AVANT" "$DAEMON_LOG")" 'pare-feu dérivé'
check $? "le démon a journalisé la dérive, elle ne passe pas en silence"

texte_contient "$(tail -n +"$AVANT" "$DAEMON_LOG")" 'dérive corrigée'
check $? "le démon a journalisé la correction"

# La vm doit toujours répondre après le rétablissement.
curl -fsS --max-time 5 -o /dev/null "http://10.42.91.2/"
check $? "la vm répond toujours après rétablissement du pare-feu"

# ----------------------------------------- Suppression d'un bridge

printf '\n'
log "=== suppression du bridge d'une zone sans vm ==="

AVANT=$(wc -l < "$DAEMON_LOG")
ip link del "br-${ZONE_VIDE}"

! ip link show "br-${ZONE_VIDE}" >/dev/null 2>&1
check $? "le bridge a bien été supprimé"

if wait_for $((INTERVAL * 6)) ip link show "br-${ZONE_VIDE}"; then
    check 0 "le bridge de zone est recréé tout seul"
else
    check 1 "le bridge n'a pas été recréé"
    tail -n +"$AVANT" "$DAEMON_LOG" >&2
fi

texte_contient "$(ip -4 addr show "br-${ZONE_VIDE}")" '10.42.90.1/24'
check $? "le bridge recréé porte de nouveau son adressage"

# ------------------------- Interface d'une vm qui tourne : signalé, pas réparé

printf '\n'
log "=== interface d'une vm en marche ==="

AVANT=$(wc -l < "$DAEMON_LOG")
ip link del "tap-${VM}"

sleep $((INTERVAL * 3))

texte_contient "$(tail -n +"$AVANT" "$DAEMON_LOG")" 'seule une relance'
check $? "l'écart est signalé explicitement : recréer l'interface ne rebrancherait pas Firecracker"

# ------------------------------- Réconciliation désactivée : rien n'est réparé

printf '\n'
log "=== avec la réconciliation désactivée ==="

stop_daemon
cleanup
rm -f "$DAEMON_LOG"
start_daemon 0

if wait_for 60 curl -fsS --max-time 2 -o /dev/null "http://10.42.91.2/"; then
    check 0 "la vm répond"
else
    check 1 "la vm ne répond pas"
    tail -30 "$DAEMON_LOG" >&2
fi

grep -q 'réconciliation désactivée' "$DAEMON_LOG"
check $? "le démon dit clairement que la réparation automatique est désactivée"

nft flush ruleset
sleep $((INTERVAL * 4))

! table_presente
check $? "sans réconciliation, le pare-feu n'est pas rétabli : le drapeau fait bien ce qu'il annonce"

stop_daemon

# ---------------------------------------------------------------- Bilan

printf '\n'
if [ "$FAIL" -eq 0 ]; then
    ok "réconciliation validée : $PASS vérification(s) au vert."
    exit 0
else
    printf '  %d vérification(s) en échec sur %d.\n' "$FAIL" "$((PASS + FAIL))" >&2
    exit 1
fi
