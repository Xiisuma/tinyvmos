#!/usr/bin/env bash
#
# TinyVMOS — test d'acceptation du lot B1 de la Phase 2.
#
# Vérifie que le démon pose les zones, attribue l'adressage des deux familles
# et raccorde les VM, sans qu'aucune adresse ne soit indiquée à la main.
#
# Ce que ce test ne prouve PAS : l'isolation entre zones. Le pare-feu arrive au
# lot B2. Ici les deux zones existent et sont routées, donc elles se parlent —
# c'est attendu à ce stade, et c'est précisément ce que B2 viendra couper.
#
# Usage : sudo ./tests/phase2b1-zones.sh

set -uo pipefail
export SCRIPT_NAME="test-phase2b1"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

need_root

SERVICE="web-apache"
TEST_RUN="${OUTPUT_DIR}/test-phase2b1"
STATE_FILE="${TEST_RUN}/state.json"
DAEMON_LOG="${TEST_RUN}/tinyvmosd.log"
DAEMON_BIN="${OUTPUT_DIR}/bin/tinyvmosd"
ZONES_FILE="${TEST_RUN}/zones.json"

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
    for t in tap-web1 tap-web2; do ip link del "$t" 2>/dev/null || true; done
    for b in br-t2b1-pub br-t2b1-int; do ip link del "$b" 2>/dev/null || true; done
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
        -run-dir "${TEST_RUN}/run" \
        -jail-dir "${TEST_RUN}/jail" \
        -zones "$ZONES_FILE" \
        -log-level debug \
        -vm "name=web1,service=${SERVICE},zone=t2b1-pub,mem=256" \
        -vm "name=web2,service=${SERVICE},zone=t2b1-int,mem=256" \
        < /dev/null >> "$DAEMON_LOG" 2>&1 &
    DAEMON_PID=$!
}

# ------------------------------------------------------------ Préparation

log "préparation"
command -v jq >/dev/null || die "jq requis"
[ -x "$DAEMON_BIN" ] || die "démon absent : cd controlplane && go build -o ../output/bin/tinyvmosd ./cmd/tinyvmosd"
id -u tinyvmos >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin tinyvmos

"${REPO_ROOT}/images/kernel/fetch-kernel.sh" >/dev/null 2>&1 || die "noyau invité indisponible"

# L'image doit être reconstruite : son service réseau gère désormais l'IPv6.
log "reconstruction de l'image ${SERVICE} (service réseau IPv6)"
"${REPO_ROOT}/images/build-image.sh" "$SERVICE" >/dev/null || die "construction de l'image impossible"

cleanup
rm -rf "$TEST_RUN"
mkdir -p "$TEST_RUN"

# Des zones propres au test, pour ne pas entrer en conflit avec un essai manuel.
cat > "$ZONES_FILE" <<'ZONES'
[
  {"name": "t2b1-pub", "ipv4": "10.42.97.0/24", "comment": "zone de test, publique"},
  {"name": "t2b1-int", "ipv4": "10.42.96.0/24", "comment": "zone de test, interne"}
]
ZONES

# ------------------------------------------------------- Démarrage

printf '\n'
log "=== démarrage du démon ==="
start_daemon

sleep 0.5
kill -0 "$DAEMON_PID" 2>/dev/null
check $? "le démon tourne (pid $DAEMON_PID)"

if wait_for 60 curl -fsS --max-time 2 -o /dev/null "http://10.42.97.2/"; then
    check 0 "web1 répond en IPv4 dans sa zone, sans adresse indiquée à la main"
else
    check 1 "web1 ne répond pas en IPv4"
    printf '\n--- journal du démon ---\n' >&2; tail -40 "$DAEMON_LOG" >&2
    printf '\n--- console web1 ---\n' >&2; tail -25 "${TEST_RUN}/run/web1/console.log" 2>/dev/null >&2
    exit 1
fi

if wait_for 60 curl -fsS --max-time 2 -o /dev/null "http://10.42.96.2/"; then
    check 0 "web2 répond en IPv4 dans l'autre zone"
else
    check 1 "web2 ne répond pas en IPv4"
    tail -40 "$DAEMON_LOG" >&2
fi

# ------------------------------------------------------------ Zones

printf '\n'
log "=== zones posées sur l'hôte ==="

for b in br-t2b1-pub br-t2b1-int; do
    ip link show "$b" >/dev/null 2>&1
    check $? "le bridge $b existe"
done

ip -4 addr show br-t2b1-pub | grep -q '10.42.97.1/24'
check $? "le bridge publique porte la passerelle IPv4 10.42.97.1/24"

ip -6 addr show br-t2b1-pub | grep -qE 'inet6 fd[0-9a-f]{2}:'
check $? "le bridge publique porte une adresse IPv6 locale unique"

ip link show tap-web1 | grep -q 'master br-t2b1-pub'
check $? "tap-web1 est rattachée au bridge de sa zone"

ip link show tap-web2 | grep -q 'master br-t2b1-int'
check $? "tap-web2 est rattachée au bridge de son autre zone"

[ "$(cat /proc/sys/net/ipv4/ip_forward)" = "1" ]
check $? "le routage IPv4 est actif"

[ "$(cat /proc/sys/net/ipv6/conf/all/forwarding)" = "1" ]
check $? "le routage IPv6 est actif"

# Une VM ne doit pas pouvoir annoncer une passerelle à l'hôte.
[ "$(cat /proc/sys/net/ipv6/conf/br-t2b1-pub/accept_ra)" = "0" ]
check $? "l'hôte refuse les annonces de routeur venant de la zone"

# ------------------------------------------------------ Adressage IPv6

printf '\n'
log "=== adressage IPv6 ==="

ULA=$(jq -r '.alloc.ula_prefix' "$STATE_FILE")
printf '%s' "$ULA" | grep -qE '^fd[0-9a-f]{2}:'
check $? "le préfixe de l'installation est bien un préfixe local unique (${ULA})"

printf '%s' "$ULA" | grep -q '/48$'
check $? "le préfixe de l'installation est un /48"

V6_WEB1=$(jq -r '.alloc.leases.web1.ipv6' "$STATE_FILE")
[ -n "$V6_WEB1" ] && [ "$V6_WEB1" != "null" ]
check $? "web1 a reçu une adresse IPv6 (${V6_WEB1})"

# L'identifiant IPv6 reprend le dernier octet de l'IPv4 : les deux adresses
# d'une VM se lisent du même coup d'œil.
printf '%s' "$V6_WEB1" | grep -q '::2/64$'
check $? "l'IPv6 de web1 finit en ::2, comme son IPv4 en .2"

V6_ADDR=${V6_WEB1%/*}
if wait_for 45 curl -gfsS --max-time 2 -o /dev/null "http://[${V6_ADDR}]/"; then
    check 0 "web1 répond en IPv6 sur ${V6_ADDR}"
else
    check 1 "web1 ne répond pas en IPv6 sur ${V6_ADDR}"
    printf '\n--- console web1 ---\n' >&2
    tail -25 "${TEST_RUN}/run/web1/console.log" 2>/dev/null >&2
fi

# Les deux zones doivent avoir des /64 distincts.
Z1=$(jq -r '.zones[] | select(.name=="t2b1-pub") | .ipv6' "$STATE_FILE")
Z2=$(jq -r '.zones[] | select(.name=="t2b1-int") | .ipv6' "$STATE_FILE")
[ -n "$Z1" ] && [ "$Z1" != "$Z2" ]
check $? "les deux zones ont des /64 distincts (${Z1} et ${Z2})"

# ------------------------------------------- Stabilité de l'adressage

printf '\n'
log "=== stabilité de l'adressage après redémarrage du démon ==="

stop_daemon
start_daemon

if wait_for 60 curl -fsS --max-time 2 -o /dev/null "http://10.42.97.2/"; then
    check 0 "web1 répond de nouveau après redémarrage du démon"
else
    check 1 "web1 ne répond pas après redémarrage du démon"
    tail -30 "$DAEMON_LOG" >&2
fi

ULA_APRES=$(jq -r '.alloc.ula_prefix' "$STATE_FILE")
[ "$ULA_APRES" = "$ULA" ]
check $? "le préfixe de l'installation n'a pas changé : toutes les adresses IPv6 en dépendent"

V6_APRES=$(jq -r '.alloc.leases.web1.ipv6' "$STATE_FILE")
[ "$V6_APRES" = "$V6_WEB1" ]
check $? "web1 a gardé la même adresse IPv6"

[ "$(jq -r '.alloc.leases.web1.ipv4' "$STATE_FILE")" = "10.42.97.2/24" ]
check $? "web1 a gardé la même adresse IPv4"

# ------------------------------------------------- Refus des conflits

printf '\n'
log "=== refus des configurations dangereuses ==="

cat > "${TEST_RUN}/zones-conflit.json" <<'ZONES'
[
  {"name": "a", "ipv4": "10.42.97.0/24"},
  {"name": "b", "ipv4": "10.42.97.0/24"}
]
ZONES
# Le démon sort en code non nul, et c'est justement ce qu'on attend. Sa sortie
# est donc capturée avant d'être examinée, sinon pipefail ferait échouer le tube
# alors que le message cherché est bien là.
expect_refusal() {
    local motif="$1" libelle="$2"; shift 2
    local out
    out=$("$DAEMON_BIN" -state "${TEST_RUN}/jetable.json" "$@" 2>&1 || true)
    if printf '%s' "$out" | grep -q "$motif"; then
        check 0 "$libelle"
    else
        check 1 "$libelle"
        printf '    sortie obtenue : %s\n' "$(printf '%s' "$out" | tail -2)" >&2
    fi
}

expect_refusal 'recouvrent' \
    "deux zones sur le même réseau sont refusées, pas acceptées en silence" \
    -zones "${TEST_RUN}/zones-conflit.json" -vm "name=x,service=${SERVICE},zone=a"

expect_refusal 'zone .* inconnue' \
    "une zone inconnue est refusée avec la liste des zones déclarées" \
    -zones "$ZONES_FILE" -vm "name=x,service=${SERVICE},zone=inexistante"

expect_refusal "n'est plus accepté" \
    "indiquer une adresse à la main est refusé : la zone l'attribue" \
    -zones "$ZONES_FILE" -vm "name=x,service=${SERVICE},zone=t2b1-pub,ip=10.0.0.5/24"

# ------------------------------------------------------- Arrêt propre

printf '\n'
log "=== arrêt propre ==="

stop_daemon

! pgrep -x firecracker >/dev/null 2>&1
check $? "aucun processus Firecracker ne survit"

! ip link show tap-web1 >/dev/null 2>&1
check $? "tap-web1 a été supprimée"

! ip link show br-t2b1-pub >/dev/null 2>&1
check $? "le bridge de zone a été supprimé"

# ---------------------------------------------------------------- Bilan

printf '\n'
if [ "$FAIL" -eq 0 ]; then
    ok "lot B1 de la Phase 2 validé : $PASS vérification(s) au vert."
    exit 0
else
    printf '  %d vérification(s) en échec sur %d.\n' "$FAIL" "$((PASS + FAIL))" >&2
    exit 1
fi
