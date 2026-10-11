#!/usr/bin/env bash
#
# TinyVMOS — test d'acceptation du lot A de la Phase 2.
#
# Vérifie que le démon fait tourner une vraie micro-VM de bout en bout : il la
# prépare sous jailer, la démarre, écrit son état de façon atomique, la relance
# si elle meurt sans qu'on le lui ait demandé, et l'arrête proprement au signal.
#
# Les tests unitaires Go couvrent la machine d'état et la supervision avec un
# hyperviseur simulé. Celui-ci couvre ce qu'un simulacre ne peut pas prouver :
# que ça marche avec Firecracker, jailer et KVM.
#
# Usage : sudo ./tests/phase2a-demon.sh

set -uo pipefail
export SCRIPT_NAME="test-phase2a"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

need_root

SERVICE="web-apache"
VM="web1"
ZONE="t2a"
BRIDGE="br-t2a"
# L'adressage n'est plus indiqué à la main : la zone l'attribue. Cette adresse
# est celle que le démon doit nécessairement donner à la première VM de la zone,
# l'hôte prenant .1.
GUEST_IP="10.42.98.2"

TEST_RUN="${OUTPUT_DIR}/test-phase2a"
STATE_FILE="${TEST_RUN}/state.json"
ZONES_FILE="${TEST_RUN}/zones.json"
JAIL_DIR="${TEST_RUN}/jail"
DAEMON_LOG="${TEST_RUN}/tinyvmosd.log"
DAEMON_BIN="${OUTPUT_DIR}/bin/tinyvmosd"

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

cleanup() {
    if [ -n "$DAEMON_PID" ] && kill -0 "$DAEMON_PID" 2>/dev/null; then
        kill -TERM "$DAEMON_PID" 2>/dev/null || true
        local i=0
        while kill -0 "$DAEMON_PID" 2>/dev/null && [ "$i" -lt 100 ]; do sleep 0.1; i=$((i + 1)); done
        kill -9 "$DAEMON_PID" 2>/dev/null || true
    fi
    pkill -x firecracker 2>/dev/null || true
    # Le démon nettoie son réseau à l'arrêt propre, mais pas s'il est tué de
    # force : on repasse derrière.
    ip link del "tap-${VM}" 2>/dev/null || true
    ip link del "$BRIDGE" 2>/dev/null || true
}
trap cleanup EXIT

# Attend qu'une commande réussisse, au plus N secondes.
wait_for() {
    local seconds="$1"; shift
    local deadline=$(( $(date +%s) + seconds ))
    until "$@" >/dev/null 2>&1; do
        [ "$(date +%s)" -ge "$deadline" ] && return 1
        sleep 0.25
    done
    return 0
}

# ------------------------------------------------------------ Préparation

log "préparation"

command -v jq >/dev/null || die "jq requis pour ce test"

[ -x "$DAEMON_BIN" ] || die "démon absent : lancer « cd controlplane && go build -o ../output/bin/tinyvmosd ./cmd/tinyvmosd »"

id -u "${JailUser:-tinyvmos}" >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin tinyvmos

"${REPO_ROOT}/images/kernel/fetch-kernel.sh" >/dev/null 2>&1 || die "noyau invité indisponible"
if [ ! -f "${IMAGES_OUT}/${SERVICE}.ext4" ]; then
    "${REPO_ROOT}/images/build-image.sh" "$SERVICE" >/dev/null || die "construction de l'image $SERVICE impossible"
fi

rm -rf "$TEST_RUN"
mkdir -p "$TEST_RUN"

cleanup

# Une zone propre au test. Le démon crée le bridge et l'interface tap lui-même,
# et attribue l'adressage : rien n'est posé à la main ici.
cat > "$ZONES_FILE" <<ZONES
[
  {"name": "${ZONE}", "ipv4": "10.42.98.0/24", "comment": "zone du test du lot A"}
]
ZONES

# ------------------------------------------------------- Démarrage du démon

printf '\n'
log "=== démarrage du démon ==="

cd "$REPO_ROOT" || die "cd impossible"

setsid "$DAEMON_BIN" \
    -state "$STATE_FILE" \
    -run-dir "${TEST_RUN}/run" \
    -jail-dir "$JAIL_DIR" \
    -zones "$ZONES_FILE" \
    -log-level debug \
    -vm "name=${VM},service=${SERVICE},zone=${ZONE},mem=256,restart=true" \
    < /dev/null > "$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!

sleep 0.5
kill -0 "$DAEMON_PID" 2>/dev/null
check $? "le démon tourne (pid $DAEMON_PID)"

if wait_for 40 curl -fsS --max-time 2 -o /dev/null "http://${GUEST_IP}/"; then
    check 0 "la vm répond en HTTP, démarrée par le démon"
else
    check 1 "la vm ne répond pas en HTTP"
    printf '\n--- journal du démon ---\n' >&2
    tail -40 "$DAEMON_LOG" >&2
    printf '\n--- console de la vm ---\n' >&2
    tail -30 "${TEST_RUN}/run/${VM}/console.log" 2>/dev/null >&2
    exit 1
fi

# ----------------------------------------------------------- Confinement

printf '\n'
log "=== confinement ==="

FC_PID=$(pgrep -x firecracker | head -1)
if [ -n "$FC_PID" ]; then
    OWNER=$(stat -c '%U' "/proc/${FC_PID}" 2>/dev/null)
    [ "$OWNER" = "tinyvmos" ]
    check $? "Firecracker tourne en utilisateur tinyvmos, pas en root (obtenu ${OWNER})"

    grep -qE '^Seccomp:[[:space:]]*2' "/proc/${FC_PID}/status"
    check $? "filtre seccomp en mode strict"
else
    check 1 "processus Firecracker introuvable"
fi

# --------------------------------------------------------------- État

printf '\n'
log "=== magasin d'état ==="

[ -f "$STATE_FILE" ]
check $? "le fichier d'état existe"

jq -e . "$STATE_FILE" >/dev/null 2>&1
check $? "le fichier d'état est du JSON valide, lisible avec jq"

[ "$(stat -c '%a' "$STATE_FILE")" = "640" ]
check $? "droits du fichier d'état en 640 (obtenu $(stat -c '%a' "$STATE_FILE"))"

[ "$(jq -r ".vms.\"${VM}\".status" "$STATE_FILE")" = "running" ]
check $? "l'état enregistre la vm comme running"

# Aucun temporaire ne doit subsister : la réécriture est atomique.
[ "$(find "$(dirname "$STATE_FILE")" -maxdepth 1 -name '.tinyvmos-state-*' | wc -l)" -eq 0 ]
check $? "aucun fichier temporaire d'état résiduel"

# Les métriques instantanées ne doivent pas être écrites sur disque.
[ "$(jq -r '[.. | objects | keys[]] | map(select(. == "rss_mib" or . == "uptime")) | length' "$STATE_FILE")" -eq 0 ]
check $? "aucune métrique instantanée écrite dans l'état"

# ------------------------------------------------------ Relance automatique

printf '\n'
log "=== relance automatique après mort de la vm ==="

RESTARTS_BEFORE=$(jq -r ".vms.\"${VM}\".restarts" "$STATE_FILE")

pkill -9 -x firecracker
check $? "vm tuée de force"

if wait_for 60 curl -fsS --max-time 2 -o /dev/null "http://${GUEST_IP}/"; then
    check 0 "la vm est relancée toute seule et répond de nouveau"
else
    check 1 "la vm n'a pas été relancée"
    tail -40 "$DAEMON_LOG" >&2
fi

RESTARTS_AFTER=$(jq -r ".vms.\"${VM}\".restarts" "$STATE_FILE")
[ "$RESTARTS_AFTER" -gt "$RESTARTS_BEFORE" ]
check $? "le compteur de relances a progressé (${RESTARTS_BEFORE} puis ${RESTARTS_AFTER})"

grep -q 'relance programmée' "$DAEMON_LOG"
check $? "le démon a journalisé la temporisation avant relance"

# -------------------------------------------------------- Arrêt propre

printf '\n'
log "=== arrêt propre au signal ==="

kill -TERM "$DAEMON_PID"

if wait_for 40 sh -c "! kill -0 $DAEMON_PID 2>/dev/null"; then
    check 0 "le démon s'arrête sur SIGTERM"
else
    check 1 "le démon ne s'est pas arrêté sur SIGTERM"
fi
DAEMON_PID=""

! pgrep -x firecracker >/dev/null 2>&1
check $? "aucun processus Firecracker ne survit au démon"

[ "$(jq -r ".vms.\"${VM}\".status" "$STATE_FILE")" = "stopped" ]
check $? "l'état final enregistre la vm comme stopped"

# L'état doit survivre à l'arrêt : c'est tout l'intérêt de le persister.
"$DAEMON_BIN" -state "$STATE_FILE" -list | grep -q "$VM"
check $? "l'état relu après extinction contient encore la vm"

# ---------------------------------------------------------------- Bilan

printf '\n'
if [ "$FAIL" -eq 0 ]; then
    ok "lot A de la Phase 2 validé : $PASS vérification(s) au vert."
    exit 0
else
    printf '  %d vérification(s) en échec sur %d.\n' "$FAIL" "$((PASS + FAIL))" >&2
    exit 1
fi
