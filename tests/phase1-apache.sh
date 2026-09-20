#!/usr/bin/env bash
#
# TinyVMOS — test d'acceptation de la Phase 1.
#
# Critère : « la page Apache répond depuis l'hôte via un réseau virtuel
# (tap + bridge) », et la même VM démarre sous jailer.
#
# Le test monte le réseau, construit l'image si besoin, lance la VM dans les
# deux modes, vérifie la réponse HTTP et le durcissement, puis nettoie tout.
# Il ne doit laisser aucune interface ni aucun processus résiduel.
#
# Usage : sudo ./tests/phase1-apache.sh

set -uo pipefail
export SCRIPT_NAME="test-phase1"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

need_root

SERVICE="web-apache"
VM="test-phase1"
GUEST_IP="10.42.99.2"
TAP="tap-test1"
BRIDGE="br-test1"
CIDR="10.42.99.1/24"

PASS=0
FAIL=0

check()  { if [ "$1" -eq 0 ]; then ok "$2"; PASS=$((PASS+1)); else printf '  ECHEC : %s\n' "$2" >&2; FAIL=$((FAIL+1)); fi; }

cleanup() {
    "${REPO_ROOT}/build/run-firecracker.sh" --name "$VM" --stop >/dev/null 2>&1 || true
    "${REPO_ROOT}/build/net-setup.sh" down --bridge "$BRIDGE" --tap "$TAP" >/dev/null 2>&1 || true
}
trap cleanup EXIT

# ------------------------------------------------------------ Préparation

log "préparation : noyau, image, réseau"
"${REPO_ROOT}/images/kernel/fetch-kernel.sh" >/dev/null 2>&1 \
    || die "récupération du noyau invité impossible"

if [ ! -f "${IMAGES_OUT}/${SERVICE}.ext4" ]; then
    "${REPO_ROOT}/images/build-image.sh" "$SERVICE" >/dev/null \
        || die "construction de l'image $SERVICE impossible"
fi

cleanup
"${REPO_ROOT}/build/net-setup.sh" up --bridge "$BRIDGE" --tap "$TAP" --cidr "$CIDR" >/dev/null \
    || die "mise en place du réseau de test impossible"

# ------------------------------------------- Scénario commun aux deux modes

run_scenario() {
    local mode="$1" extra="$2"
    printf '\n'
    log "=== mode : $mode ==="

    # shellcheck disable=SC2086
    if "${REPO_ROOT}/build/run-firecracker.sh" \
        --name "$VM" --service "$SERVICE" --tap "$TAP" --ip "$GUEST_IP" \
        --wait 30 $extra >/dev/null 2>&1; then
        check 0 "$mode : la VM démarre et répond en HTTP"
    else
        check 1 "$mode : la VM ne répond pas en HTTP"
        tail -30 "${OUTPUT_DIR}/run/${VM}/console.log" 2>/dev/null >&2
        return
    fi

    curl -fsS --max-time 5 "http://${GUEST_IP}/" 2>/dev/null | grep -q 'web-apache'
    check $? "$mode : la page servie est bien celle de l'image"

    local code
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "http://${GUEST_IP}/" 2>/dev/null)
    [ "$code" = "200" ]
    check $? "$mode : code HTTP 200 (obtenu $code)"

    # ServerTokens Prod : l'en-tete Server ne doit pas divulguer la version
    # ni le systeme. Cette image finira face a des joueurs de CTF.
    local server
    server=$(curl -sSI --max-time 5 "http://${GUEST_IP}/" 2>/dev/null | grep -i '^server:' | tr -d '\r')
    if printf '%s' "$server" | grep -qiE 'unix|alpine|[0-9]+\.[0-9]+\.[0-9]+'; then
        check 1 "$mode : l'en-tête Server divulgue trop ($server)"
    else
        check 0 "$mode : l'en-tête Server ne divulgue rien (${server:-absent})"
    fi

    if [ "$mode" = "jailer" ]; then
        local pid
        pid=$(pgrep -x firecracker | head -1)
        if [ -n "$pid" ]; then
            local owner
            owner=$(stat -c '%U' "/proc/$pid" 2>/dev/null)
            [ "$owner" != "root" ]
            check $? "jailer : Firecracker ne tourne pas en root (utilisateur $owner)"

            grep -q 'Seccomp:\s*2' "/proc/$pid/status" 2>/dev/null
            check $? "jailer : filtre seccomp actif en mode strict"
        else
            check 1 "jailer : processus Firecracker introuvable"
        fi
    fi

    "${REPO_ROOT}/build/run-firecracker.sh" --name "$VM" --stop >/dev/null 2>&1
    check $? "$mode : arrêt et nettoyage"
}

run_scenario "direct" ""
run_scenario "jailer" "--jailer"

# ------------------------------------------------------ Absence de résidus

printf '\n'
log "=== vérification de l'absence de résidus ==="

"${REPO_ROOT}/build/net-setup.sh" down --bridge "$BRIDGE" --tap "$TAP" >/dev/null 2>&1

! ip link show "$TAP" >/dev/null 2>&1
check $? "l'interface tap $TAP a bien été supprimée"

! ip link show "$BRIDGE" >/dev/null 2>&1
check $? "le bridge $BRIDGE a bien été supprimé"

! pgrep -x firecracker >/dev/null 2>&1
check $? "aucun processus firecracker résiduel"

# ---------------------------------------------------------------- Bilan

printf '\n'
if [ "$FAIL" -eq 0 ]; then
    ok "Phase 1 validée : $PASS vérification(s) au vert."
    exit 0
else
    printf '  %d vérification(s) en échec sur %d.\n' "$FAIL" "$((PASS + FAIL))" >&2
    exit 1
fi
