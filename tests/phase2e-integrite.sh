#!/usr/bin/env bash
#
# TinyVMOS — test de la vérification d'intégrité des artefacts démarrables.
#
# L'empreinte SHA-256 de chaque image était calculée à la construction et
# écrite dans ses métadonnées, mais personne ne la relisait au démarrage. Une
# image modifiée démarrait sans un mot. Le noyau invité était dans le même cas.
#
# Ce test ne se contente pas de vérifier que les empreintes sont enregistrées :
# il modifie réellement une image déjà acceptée et exige que le démon refuse de
# démarrer. Il vérifie surtout le point qui distingue ce dispositif d'une simple
# relecture des métadonnées : une image modifiée *dont les métadonnées ont été
# mises à jour en même temps* doit être refusée elle aussi, parce que l'autorité
# est l'empreinte retenue dans l'état, pas le fichier qui accompagne l'image.
#
# Le catalogue utilisé est une copie dans le dossier du test : on ne modifie
# jamais les artefacts du dépôt.
#
# Usage : sudo ./tests/phase2e-integrite.sh

set -uo pipefail
export SCRIPT_NAME="test-phase2e"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

need_root

SERVICE="web-apache"
ZONE="t2e-int"
VM="gamma"
VM_IP="10.42.89.2"
TEST_RUN="${OUTPUT_DIR}/test-phase2e"
# Hors de /home, dont les droits 0750 empêcheraient une vm de traverser
# jusqu'à ses propres fichiers. C'est l'emplacement de production.
EXEC_DIR="/var/lib/tinyvmos-test-phase2e"
STATE_FILE="${TEST_RUN}/state.json"
DAEMON_LOG="${TEST_RUN}/tinyvmosd.log"
DAEMON_BIN="${OUTPUT_DIR}/bin/tinyvmosd"
ZONES_FILE="${TEST_RUN}/zones.json"

# Catalogue propre au test : jamais les artefacts du dépôt.
CATALOG="${TEST_RUN}/catalogue"
IMAGE="${CATALOG}/${SERVICE}.ext4"
IMAGE_META="${CATALOG}/${SERVICE}.json"
KERNEL="${CATALOG}/vmlinux"
IMAGE_SAIN="${TEST_RUN}/reference/${SERVICE}.ext4"
META_SAINE="${TEST_RUN}/reference/${SERVICE}.json"
KERNEL_SAIN="${TEST_RUN}/reference/vmlinux"

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
        -vm "name=${VM},service=${SERVICE},zone=${ZONE},mem=256,image=${IMAGE},kernel=${KERNEL}" \
        < /dev/null >> "$DAEMON_LOG" 2>&1 &
    DAEMON_PID=$!
}

# attendre_sortie_demon attend que le démon se termine de lui-même, ce qu'il
# fait quand une vérification d'intégrité échoue : le refus est un arrêt, pas
# un avertissement.
attendre_sortie_demon() {
    local i=0
    while kill -0 "$DAEMON_PID" 2>/dev/null && [ "$i" -lt 120 ]; do sleep 0.25; i=$((i + 1)); done
    if kill -0 "$DAEMON_PID" 2>/dev/null; then
        return 1
    fi
    DAEMON_PID=""
    return 0
}

# marque_journal insère un repère dans le journal pour que les vérifications
# suivantes ne puissent pas être satisfaites par une ligne d'une étape passée.
marque_journal() {
    printf '\n===== %s =====\n' "$1" >> "$DAEMON_LOG"
}

# journal_depuis_marque renvoie la fin du journal à partir du dernier repère.
journal_depuis_marque() {
    sed -n '/^===== /h; /^===== /!H; ${x;p}' "$DAEMON_LOG" 2>/dev/null
}

# empreinte_retenue lit dans l'état l'empreinte enregistrée pour un chemin.
empreinte_retenue() {
    jq -r --arg p "$1" '.artifacts[$p].sha256 // ""' "$STATE_FILE" 2>/dev/null
}

origine_retenue() {
    jq -r --arg p "$1" '.artifacts[$p].source // ""' "$STATE_FILE" 2>/dev/null
}

# ------------------------------------------------------------ Préparation

log "préparation"
command -v jq >/dev/null || die "jq requis"
[ -x "$DAEMON_BIN" ] || die "démon absent : cd controlplane && go build -o ../output/bin/tinyvmosd ./cmd/tinyvmosd"

"${REPO_ROOT}/images/kernel/fetch-kernel.sh" >/dev/null 2>&1 || die "noyau invité indisponible"
if [ ! -f "${IMAGES_OUT}/${SERVICE}.ext4" ]; then
    "${REPO_ROOT}/images/build-image.sh" "$SERVICE" >/dev/null || die "construction de l'image impossible"
fi
[ -f "${IMAGES_OUT}/${SERVICE}.json" ] || die "métadonnées de l'image absentes : reconstruire l'image"

cleanup
rm -rf "$TEST_RUN" "$EXEC_DIR"
mkdir -p "$TEST_RUN" "$EXEC_DIR" "$CATALOG" "${TEST_RUN}/reference"

cp "${IMAGES_OUT}/${SERVICE}.ext4" "$IMAGE"
cp "${IMAGES_OUT}/${SERVICE}.json" "$IMAGE_META"
cp "$(readlink -f "${KERNEL_OUT}/vmlinux")" "$KERNEL"
cp "$IMAGE" "$IMAGE_SAIN"
cp "$IMAGE_META" "$META_SAINE"
cp "$KERNEL" "$KERNEL_SAIN"

cat > "$ZONES_FILE" <<ZONES
[
  {"name": "${ZONE}", "ipv4": "10.42.89.0/24", "comment": "zone du test d integrite"}
]
ZONES

# --------------------------------------- Premier démarrage, enregistrement

printf '\n'
log "=== premier démarrage : les empreintes sont enregistrées ==="

marque_journal "premier demarrage"
start_daemon

if wait_for 90 curl -fsS --max-time 2 -o /dev/null "http://${VM_IP}/"; then
    check 0 "${VM} répond : une image conforme démarre normalement"
else
    check 1 "${VM} ne répond pas"
    tail -40 "$DAEMON_LOG" >&2
    exit 1
fi

stop_daemon

EMPREINTE_IMAGE=$(sha256sum "$IMAGE" | cut -d' ' -f1)
EMPREINTE_NOYAU=$(sha256sum "$KERNEL" | cut -d' ' -f1)

[ "$(empreinte_retenue "$IMAGE")" = "$EMPREINTE_IMAGE" ]
check $? "l'empreinte de l'image est retenue dans l'état"

[ "$(empreinte_retenue "$KERNEL")" = "$EMPREINTE_NOYAU" ]
check $? "l'empreinte du noyau est retenue elle aussi : c'est du code, au même titre que l'image"

# L'origine dit ce que la vérification vaut. L'image a des métadonnées de
# construction qui la confirment ; le noyau n'en a pas.
[ "$(origine_retenue "$IMAGE")" = "métadonnées de construction" ]
check $? "l'empreinte de l'image a été confirmée par ses métadonnées de construction"

[ "$(origine_retenue "$KERNEL")" = "première utilisation" ]
check $? "l'empreinte du noyau est marquée comme non confirmée : aucune métadonnée à côté du fichier"

[ "$(jq -r '.version' "$STATE_FILE")" = "2" ]
check $? "l'état est en version 2 : un démon antérieur, qui ne vérifierait rien, refusera de repartir dessus"

# ------------------------------------------- Une image modifiée est refusée

printf '\n'
log "=== image modifiée après enregistrement ==="

printf 'charge utile' >> "$IMAGE"

marque_journal "image modifiee"
start_daemon

attendre_sortie_demon
check $? "le démon s'arrête au lieu de démarrer une image modifiée"

texte_contient "$(journal_depuis_marque)" "intégrité d'artefact"
check $? "le journal nomme la cause : intégrité d'artefact"

texte_contient "$(journal_depuis_marque)" "-trust"
check $? "le message dit quoi faire, il ne se contente pas de refuser"

[ -z "$(pgrep -x firecracker)" ]
check $? "aucun processus de vm n'a été lancé sur l'image modifiée"

! curl -fsS --max-time 2 -o /dev/null "http://${VM_IP}/" 2>/dev/null
check $? "la vm ne répond pas : elle n'a pas démarré du tout"

# ------------------------- L'image saine redémarre, sans intervention

printf '\n'
log "=== retour à l'image d'origine ==="

cp "$IMAGE_SAIN" "$IMAGE"

marque_journal "image restauree"
start_daemon

if wait_for 90 curl -fsS --max-time 2 -o /dev/null "http://${VM_IP}/"; then
    check 0 "l'image d'origine redémarre sans rien avoir à accepter : l'empreinte correspond de nouveau"
else
    check 1 "l'image d'origine ne redémarre pas"
    tail -40 "$DAEMON_LOG" >&2
fi

stop_daemon

# ------- Le point central : les métadonnées ne sont pas l'autorité

printf '\n'
log "=== image modifiée ET métadonnées mises à jour ==="

# C'est ce que ferait un attaquant un peu soigneux, et c'est aussi ce que
# produit une reconstruction légitime. Les deux doivent être refusées : le
# fichier de métadonnées se modifie aussi facilement que l'image, il ne peut
# donc pas servir d'autorité. Seule l'empreinte retenue dans l'état en est une.
printf 'charge utile soignee' >> "$IMAGE"
NOUVELLE=$(sha256sum "$IMAGE" | cut -d' ' -f1)
jq --arg s "$NOUVELLE" '.sha256 = $s' "$META_SAINE" > "$IMAGE_META"

[ "$(jq -r '.sha256' "$IMAGE_META")" = "$NOUVELLE" ]
check $? "les métadonnées annoncent bien la nouvelle empreinte : elles ne contredisent plus l'image"

marque_journal "image et metadonnees modifiees"
start_daemon

attendre_sortie_demon
check $? "le démon refuse quand même : les métadonnées ne sont pas l'autorité, l'état l'est"

texte_contient "$(journal_depuis_marque)" "empreinte retenue"
check $? "le refus porte sur l'empreinte retenue, pas sur les métadonnées"

# ------------------------------------------------- L'échappatoire explicite

printf '\n'
log "=== acceptation explicite par l'opérateur ==="

"$DAEMON_BIN" -state "$STATE_FILE" -trust "$IMAGE" > "${TEST_RUN}/trust.log" 2>&1
check $? "tinyvmosd -trust accepte l'empreinte actuelle"

[ "$(empreinte_retenue "$IMAGE")" = "$NOUVELLE" ]
check $? "l'état retient désormais la nouvelle empreinte"

[ "$(origine_retenue "$IMAGE")" = "acceptée par l'opérateur" ]
check $? "l'origine indique une acceptation humaine, et non une confirmation automatique"

marque_journal "apres acceptation"
start_daemon

if wait_for 90 curl -fsS --max-time 2 -o /dev/null "http://${VM_IP}/"; then
    check 0 "la vm démarre après acceptation explicite"
else
    check 1 "la vm ne démarre pas après acceptation"
    tail -40 "$DAEMON_LOG" >&2
fi

stop_daemon

# ------------------------------------------------ Le noyau est protégé aussi

printf '\n'
log "=== noyau modifié ==="

printf 'noyau altere' >> "$KERNEL"

marque_journal "noyau modifie"
start_daemon

attendre_sortie_demon
check $? "le démon refuse un noyau modifié : il s'exécute avec tous les droits dans la vm"

texte_contient "$(journal_depuis_marque)" "noyau"
check $? "le message nomme l'artefact en cause"

cp "$KERNEL_SAIN" "$KERNEL"

# --------------- Première utilisation : métadonnées qui contredisent l'image

printf '\n'
log "=== première utilisation avec des métadonnées discordantes ==="

# État neuf : aucune empreinte retenue. Les métadonnées de construction sont
# alors le seul contrôle disponible. Elles ne valent pas grand-chose face à un
# attaquant, mais elles attrapent une image corrompue, et il n'y a aucune
# raison de démarrer sur un fichier qui contredit ce qui l'accompagne.
STATE_NEUF="${TEST_RUN}/state-neuf.json"
cp "$IMAGE_SAIN" "$IMAGE"
cp "$META_SAINE" "$IMAGE_META"
printf 'corruption' >> "$IMAGE"

marque_journal "premiere utilisation discordante"
cd "$REPO_ROOT" || die "cd impossible"
setsid "$DAEMON_BIN" \
    -state "$STATE_NEUF" \
    -run-dir "${EXEC_DIR}/run2" \
    -jail-dir "${EXEC_DIR}/jail2" \
    -zones "$ZONES_FILE" \
    -log-level debug \
    -vm "name=${VM},service=${SERVICE},zone=${ZONE},mem=256,image=${IMAGE},kernel=${KERNEL}" \
    < /dev/null >> "$DAEMON_LOG" 2>&1 &
DAEMON_PID=$!

attendre_sortie_demon
check $? "une image qui contredit ses métadonnées est refusée dès la première utilisation"

texte_contient "$(journal_depuis_marque)" "annoncée par"
check $? "le refus nomme les métadonnées qui ont servi de contrôle"

# Et rien n'a été retenu : une empreinte enregistrée au passage ferait
# accepter l'image corrompue au démarrage suivant.
[ ! -f "$STATE_NEUF" ] || [ -z "$(jq -r --arg p "$IMAGE" '.artifacts[$p].sha256 // ""' "$STATE_NEUF" 2>/dev/null)" ]
check $? "aucune empreinte n'a été retenue pour l'image refusée"

# ---------------------------------------------------------------- Bilan

printf '\n'
if [ "$FAIL" -eq 0 ]; then
    ok "vérification d'intégrité validée : $PASS vérification(s) au vert."
    exit 0
else
    printf '  %d vérification(s) en échec sur %d.\n' "$FAIL" "$((PASS + FAIL))" >&2
    exit 1
fi
