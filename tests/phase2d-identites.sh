#!/usr/bin/env bash
#
# TinyVMOS — test de l'isolation par identité confinée.
#
# Toutes les VM tournaient sous le même utilisateur. Une seule qui se serait
# échappée de Firecracker aurait eu les droits de toutes les autres : leurs
# images de travail, leurs jails, leurs sockets. L'isolation réseau tenait,
# celle du système de fichiers tombait d'un coup.
#
# Ce test ne se contente pas de constater que les identifiants diffèrent : il
# prend l'identité d'une VM et tente de lire les fichiers d'une autre. C'est la
# seule façon de prouver que la séparation est réelle.
#
# Usage : sudo ./tests/phase2d-identites.sh

set -uo pipefail
export SCRIPT_NAME="test-phase2d"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

need_root

SERVICE="web-apache"
ZONE="t2d-int"
VM1="alpha"
VM2="beta"
TEST_RUN="${OUTPUT_DIR}/test-phase2d"
# Hors de /home, dont les droits 0750 empecheraient toute vm de traverser
# jusqu'a ses propres fichiers. C'est l'emplacement de production.
EXEC_DIR="/var/lib/tinyvmos-test-phase2d"
STATE_FILE="${TEST_RUN}/state.json"
DAEMON_LOG="${TEST_RUN}/tinyvmosd.log"
DAEMON_BIN="${OUTPUT_DIR}/bin/tinyvmosd"
ZONES_FILE="${TEST_RUN}/zones.json"

# Doivent correspondre à IdentityMin et IdentityMax dans vm/identity.go.
UID_MIN=60000
UID_MAX=60999

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
    ip link del "tap-${VM1}" 2>/dev/null || true
    ip link del "tap-${VM2}" 2>/dev/null || true
    ip link del "br-${ZONE}" 2>/dev/null || true
    nft delete table inet tinyvmos 2>/dev/null || true
    rm -rf "${EXEC_DIR:?}" 2>/dev/null || true
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

start_daemon() {
    cd "$REPO_ROOT" || die "cd impossible"
    setsid "$DAEMON_BIN" \
        -state "$STATE_FILE" \
        -run-dir "${EXEC_DIR}/run" \
        -jail-dir "${EXEC_DIR}/jail" \
        -zones "$ZONES_FILE" \
        -log-level debug \
        -vm "name=${VM1},service=${SERVICE},zone=${ZONE},mem=256" \
        -vm "name=${VM2},service=${SERVICE},zone=${ZONE},mem=256" \
        < /dev/null >> "$DAEMON_LOG" 2>&1 &
    DAEMON_PID=$!
}

# uid_of_vm retrouve l'uid du processus Firecracker d'une VM, via l'identifiant
# passé à jailer sur sa ligne de commande.
uid_of_vm() {
    local nom="$1" pid
    for pid in $(pgrep -x firecracker); do
        if tr '\0' ' ' < "/proc/${pid}/cmdline" 2>/dev/null | grep -q -- "--id ${nom}"; then
            stat -c '%u' "/proc/${pid}" 2>/dev/null
            return 0
        fi
    done
    return 1
}

# ------------------------------------------------------------ Préparation

log "préparation"
command -v jq >/dev/null || die "jq requis"
command -v setpriv >/dev/null || die "setpriv requis (paquet util-linux)"
[ -x "$DAEMON_BIN" ] || die "démon absent : cd controlplane && go build -o ../output/bin/tinyvmosd ./cmd/tinyvmosd"

"${REPO_ROOT}/images/kernel/fetch-kernel.sh" >/dev/null 2>&1 || die "noyau invité indisponible"
if [ ! -f "${IMAGES_OUT}/${SERVICE}.ext4" ]; then
    "${REPO_ROOT}/images/build-image.sh" "$SERVICE" >/dev/null || die "construction de l'image impossible"
fi

cleanup
rm -rf "$TEST_RUN" "$EXEC_DIR"
mkdir -p "$TEST_RUN" "$EXEC_DIR"

cat > "$ZONES_FILE" <<ZONES
[
  {"name": "${ZONE}", "ipv4": "10.42.88.0/24", "comment": "zone du test d identites"}
]
ZONES

# ------------------------------------------------------- Démarrage

printf '\n'
log "=== démarrage de deux vm ==="

start_daemon

if wait_for 90 curl -fsS --max-time 2 -o /dev/null "http://10.42.88.2/"; then
    check 0 "${VM1} répond"
else
    check 1 "${VM1} ne répond pas"
    tail -40 "$DAEMON_LOG" >&2
    exit 1
fi

if wait_for 90 curl -fsS --max-time 2 -o /dev/null "http://10.42.88.3/"; then
    check 0 "${VM2} répond"
else
    check 1 "${VM2} ne répond pas"
    tail -40 "$DAEMON_LOG" >&2
    exit 1
fi

# --------------------------------------- Identités distinctes et conformes

printf '\n'
log "=== identités confinées ==="

U1=$(jq -r ".identities.${VM1}" "$STATE_FILE")
U2=$(jq -r ".identities.${VM2}" "$STATE_FILE")

[ -n "$U1" ] && [ "$U1" != "null" ] && [ -n "$U2" ] && [ "$U2" != "null" ]
check $? "les deux vm ont une identité enregistrée (${U1} et ${U2})"

[ "$U1" != "$U2" ]
check $? "les deux identités sont distinctes : une évasion ne donne pas accès à l'autre vm"

for u in "$U1" "$U2"; do
    [ "$u" -ge "$UID_MIN" ] && [ "$u" -le "$UID_MAX" ]
    check $? "l'identifiant ${u} est dans la plage réservée ${UID_MIN}-${UID_MAX}, pas sur un compte réel"
done

PU1=$(uid_of_vm "$VM1")
PU2=$(uid_of_vm "$VM2")

[ "$PU1" = "$U1" ]
check $? "le processus de ${VM1} tourne bien sous son identité (${PU1})"

[ "$PU2" = "$U2" ]
check $? "le processus de ${VM2} tourne bien sous son identité (${PU2})"

[ "$PU1" != "0" ] && [ "$PU2" != "0" ]
check $? "aucun processus de vm ne tourne en root"

# ------------------------------- Propriété des fichiers de travail

printf '\n'
log "=== fichiers de travail ==="

ROOTFS1="${EXEC_DIR}/run/${VM1}/rootfs.ext4"
ROOTFS2="${EXEC_DIR}/run/${VM2}/rootfs.ext4"

[ "$(stat -c '%u' "$ROOTFS1")" = "$U1" ]
check $? "l'image de travail de ${VM1} lui appartient"

[ "$(stat -c '%u' "$ROOTFS2")" = "$U2" ]
check $? "l'image de travail de ${VM2} lui appartient"

[ "$(stat -c '%a' "$ROOTFS1")" = "600" ]
check $? "l'image de travail est en 600 : illisible par les autres identités"

[ "$(stat -c '%a' "${EXEC_DIR}/run/${VM1}")" = "700" ]
check $? "le dossier de travail de ${VM1} est en 700"

# ------------------- La preuve : une vm ne peut pas lire les fichiers d'une autre

printf '\n'
log "=== lecture croisée refusée ==="

# setpriv prend l'identité de la seconde vm et tente de lire l'image de la
# première. C'est exactement ce que ferait un processus échappé.
! setpriv --reuid="$U2" --regid="$U2" --clear-groups dd if="$ROOTFS1" of=/dev/null bs=1 count=1 2>/dev/null
check $? "sous l'identité de ${VM2}, l'image de ${VM1} est illisible"

! setpriv --reuid="$U2" --regid="$U2" --clear-groups ls "${EXEC_DIR}/run/${VM1}" 2>/dev/null
check $? "sous l'identité de ${VM2}, le dossier de travail de ${VM1} est inaccessible"

# Et le contrôle inverse : chacune lit bien la sienne, sinon le test ne
# prouverait rien d'autre qu'un droit cassé pour tout le monde.
setpriv --reuid="$U1" --regid="$U1" --clear-groups dd if="$ROOTFS1" of=/dev/null bs=1 count=1 2>/dev/null
check $? "sous sa propre identité, ${VM1} lit bien son image"

# Ce contrôle positif compte autant que les refus : sans lui, les deux
# vérifications précédentes seraient satisfaites par un dossier inaccessible à
# tout le monde, et on ne saurait pas si c'est la propriété des fichiers qui
# protège ou simplement l'absence de droits pour quiconque.

# Le dossier des exécutions doit être traversable sans être listable : une vm
# atteint son propre sous-dossier mais ne découvre pas les autres.
[ "$(stat -c '%a' "${EXEC_DIR}/run")" = "711" ]
check $? "le dossier des exécutions est en 711 : traversable, pas listable"

! setpriv --reuid="$U2" --regid="$U2" --clear-groups ls "${EXEC_DIR}/run" 2>/dev/null
check $? "une vm ne peut pas lister les autres vm" 

# ---------------------------------------- Stabilité après redémarrage

printf '\n'
log "=== stabilité après redémarrage du démon ==="

stop_daemon
pkill -x firecracker 2>/dev/null || true
start_daemon

if wait_for 90 curl -fsS --max-time 2 -o /dev/null "http://10.42.88.2/"; then
    check 0 "${VM1} répond de nouveau"
else
    check 1 "${VM1} ne répond pas après redémarrage"
    tail -30 "$DAEMON_LOG" >&2
fi

[ "$(jq -r ".identities.${VM1}" "$STATE_FILE")" = "$U1" ]
check $? "${VM1} a gardé son identité : sinon ses fichiers de travail lui deviendraient illisibles"

[ "$(jq -r ".identities.${VM2}" "$STATE_FILE")" = "$U2" ]
check $? "${VM2} a gardé son identité"

stop_daemon

# ---------------------------------------------------------------- Bilan

printf '\n'
if [ "$FAIL" -eq 0 ]; then
    ok "isolation par identité validée : $PASS vérification(s) au vert."
    exit 0
else
    printf '  %d vérification(s) en échec sur %d.\n' "$FAIL" "$((PASS + FAIL))" >&2
    exit 1
fi
