#!/usr/bin/env bash
#
# TinyVMOS — test du filtrage de l'hôte vers les zones.
#
# Rien ne filtrait ce sens : l'hôte joignait n'importe quelle VM sur n'importe
# quel port, et un flux « from: host » déclaré ne produisait aucune règle — il
# était silencieusement ignoré, ce qui est pire que de ne pas exister.
#
# Ce que le filtrage apporte, et il faut être précis : il ne contient pas un
# attaquant qui a déjà root sur l'hôte, rien dans nftables ne le pourrait. Il
# contient un service non privilégié compromis sur la machine, et les accidents
# de configuration. Le test vérifie donc exactement cela : root passe, un compte
# ordinaire est refusé, et un flux déclaré le laisse passer.
#
# Aucune micro-VM n'est démarrée : une paire veth et un espace de noms réseau
# suffisent à jouer le rôle d'une VM dans la zone, et le test tourne donc sans
# KVM.
#
# Usage : sudo ./tests/phase2f-hote-vers-zones.sh

set -uo pipefail
export SCRIPT_NAME="test-phase2f"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

need_root

ZONE="t2f-int"
BRIDGE="br-${ZONE}"
NETNS="t2f-vm"
VETH_HOTE="t2f-h"
VETH_NS="t2f-n"
ZONE_CIDR="10.42.90.0/24"
FAUSSE_VM="10.42.90.5"
PORT_FERME=8080
PORT_OUVERT=8081

TEST_RUN="${OUTPUT_DIR}/test-phase2f"
STATE_FILE="${TEST_RUN}/state.json"
DAEMON_LOG="${TEST_RUN}/tinyvmosd.log"
DAEMON_BIN="${OUTPUT_DIR}/bin/tinyvmosd"
ZONES_FILE="${TEST_RUN}/zones.json"
FLOWS_FILE="${TEST_RUN}/flows.json"

# Compte non privilégié servant de service compromis sur l'hôte.
UID_SERVICE=65534

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
    ip netns pids "$NETNS" 2>/dev/null | xargs -r kill -9 2>/dev/null || true
    ip netns del "$NETNS" 2>/dev/null || true
    ip link del "$VETH_HOTE" 2>/dev/null || true
    ip link del "$BRIDGE" 2>/dev/null || true
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

start_daemon() {
    cd "$REPO_ROOT" || die "cd impossible"
    setsid "$DAEMON_BIN" \
        -state "$STATE_FILE" \
        -run-dir "${TEST_RUN}/run" \
        -jail-dir "${TEST_RUN}/jail" \
        -zones "$ZONES_FILE" \
        -flows "$FLOWS_FILE" \
        -log-level debug \
        < /dev/null >> "$DAEMON_LOG" 2>&1 &
    DAEMON_PID=$!
}

# compteur_drop relève le nombre de paquets jetés par la règle de refus de la
# chaîne output. Un refus prouvé par un compteur vaut mieux qu'un refus déduit
# d'un échec de connexion, qui peut avoir dix autres causes.
compteur_drop() {
    # Piege a ne pas reintroduire : dans la sortie JSON de nft, un verdict
    # s ecrit {"drop": null}. Tester .drop != null est donc toujours faux, et
    # le compteur remonte zero quoi qu il arrive — une verification qui ne
    # verifie rien. Il faut tester la presence de la cle.
    nft -j list chain inet tinyvmos output 2>/dev/null \
        | jq '[.nftables[].rule? // empty
               | select(any(.expr[]?; type == "object" and has("drop")))
               | .expr[]? | select(type == "object" and has("counter"))
               | .counter.packets] | add // 0'
}

# ------------------------------------------------------------ Préparation

log "préparation"
command -v jq >/dev/null || die "jq requis"
command -v setpriv >/dev/null || die "setpriv requis (paquet util-linux)"
command -v curl >/dev/null || die "curl requis"
command -v python3 >/dev/null || die "python3 requis"
[ -x "$DAEMON_BIN" ] || die "démon absent : cd controlplane && go build -o ../output/bin/tinyvmosd ./cmd/tinyvmosd"

cleanup
rm -rf "$TEST_RUN"
mkdir -p "$TEST_RUN"

cat > "$ZONES_FILE" <<ZONES
[
  {"name": "${ZONE}", "ipv4": "${ZONE_CIDR}", "comment": "zone du test hote vers zones"}
]
ZONES

# Un seul flux déclaré : l'hôte vers la zone, sur un seul port. Tout le reste
# doit être refusé pour un compte ordinaire.
cat > "$FLOWS_FILE" <<FLOWS
[
  {"from": "host", "to": "${ZONE}", "proto": "tcp", "ports": ["${PORT_OUVERT}"],
   "comment": "service de l hote autorise a joindre cette zone"}
]
FLOWS

printf '\n'
log "=== mise en place de la zone et d une fausse vm ==="

start_daemon

if wait_for 30 ip link show "$BRIDGE"; then
    check 0 "le bridge de zone est en place"
else
    check 1 "le bridge de zone n a pas été créé"
    tail -30 "$DAEMON_LOG" >&2
    exit 1
fi

# Une paire veth et un espace de noms jouent le rôle d'une VM dans la zone :
# côté hôte le paquet sort bien par le bridge, donc par la chaîne output, ce
# qui est exactement ce qu'on veut éprouver.
ip netns add "$NETNS" || die "création de l espace de noms impossible"
ip link add "$VETH_HOTE" type veth peer name "$VETH_NS" || die "paire veth impossible"
ip link set "$VETH_NS" netns "$NETNS"
ip link set "$VETH_HOTE" master "$BRIDGE"
ip link set "$VETH_HOTE" up
ip netns exec "$NETNS" ip link set lo up
ip netns exec "$NETNS" ip addr add "${FAUSSE_VM}/24" dev "$VETH_NS"
ip netns exec "$NETNS" ip link set "$VETH_NS" up
ip netns exec "$NETNS" ip route add default via "${ZONE_CIDR%.*/*}.1" 2>/dev/null || true

# Deux services dans la fausse vm : un sur un port non déclaré, un sur le port
# déclaré. La différence entre les deux est tout l objet du test.
# disown : sans cela, le controle de taches de bash annonce « Killed » pour
# chaque serveur au nettoyage, ce qui ressemble a un echec alors que c est le
# deroulement normal.
ip netns exec "$NETNS" python3 -m http.server "$PORT_FERME" --bind "$FAUSSE_VM" \
    >/dev/null 2>&1 &
disown
ip netns exec "$NETNS" python3 -m http.server "$PORT_OUVERT" --bind "$FAUSSE_VM" \
    >/dev/null 2>&1 &
disown

if wait_for 15 curl -fsS --max-time 2 -o /dev/null "http://${FAUSSE_VM}:${PORT_FERME}/"; then
    check 0 "la fausse vm répond à root : le chemin réseau fonctionne"
else
    check 1 "la fausse vm ne répond pas, le reste du test ne prouverait rien"
    tail -30 "$DAEMON_LOG" >&2
    exit 1
fi

# ------------------------------------------- La chaîne output existe et filtre

printf '\n'
log "=== la chaîne output est posée ==="

nft_contient "chain output"
check $? "la table porte une chaîne output"

nft_correspond 'oifname @zones[[:space:]]+meta skuid 0'
check $? "le trafic des processus de root est explicitement autorisé"

nft_correspond 'oifname @zones counter.*drop'
check $? "le reste de ce qui part vers une zone est refusé"

# Le flux declare nomme le bridge de la zone, pas l ensemble : il ne vise
# qu une zone, pas toutes.
nft_correspond "oifname \"${BRIDGE}\"[[:space:]]+tcp dport ${PORT_OUVERT}"
check $? "le flux déclaré de l hôte vers la zone produit bien une règle"

# ------------------------------------------- Root passe, un compte ordinaire non

printf '\n'
log "=== un service non privilégié est contenu ==="

AVANT=$(compteur_drop)

# Le contrôle positif d abord : sans lui, les refus qui suivent pourraient
# venir d un réseau cassé plutôt que d une règle.
curl -fsS --max-time 3 -o /dev/null "http://${FAUSSE_VM}:${PORT_FERME}/"
check $? "root joint la fausse vm sur un port non déclaré : l opérateur n est pas entravé"

! setpriv --reuid="$UID_SERVICE" --regid="$UID_SERVICE" --clear-groups \
    curl -fsS --max-time 3 -o /dev/null "http://${FAUSSE_VM}:${PORT_FERME}/" 2>/dev/null
check $? "un compte ordinaire est refusé sur un port non déclaré"

APRES=$(compteur_drop)
[ "${APRES:-0}" -gt "${AVANT:-0}" ]
check $? "le compteur de refus a progressé : c est bien la règle qui a jeté le paquet (${AVANT} puis ${APRES})"

# Et le flux déclaré, lui, passe pour ce même compte : sinon le filtrage ne
# serait pas une politique, juste un mur.
setpriv --reuid="$UID_SERVICE" --regid="$UID_SERVICE" --clear-groups \
    curl -fsS --max-time 3 -o /dev/null "http://${FAUSSE_VM}:${PORT_OUVERT}/"
check $? "le même compte joint la zone sur le port déclaré"

# ------------------------------------------------- Le diagnostic reste possible

printf '\n'
log "=== diagnostic et IPv6 ==="

ping -c 1 -W 2 "$FAUSSE_VM" >/dev/null 2>&1
check $? "le ping vers la zone fonctionne : la première chose qu on tente ne doit pas être cassée"

nft_contient "nd-neighbor-solicit"
check $? "la découverte de voisins IPv6 est autorisée en sortie : sans elle IPv6 ne fonctionne pas"

nft_correspond 'oifname @zones[[:space:]]+tcp flags rst'
check $? "un RST engendré par le noyau passe : un refus franc vaut mieux qu une attente sans fin"

stop_daemon

# ---------------------------------------------------------------- Bilan

printf '\n'
if [ "$FAIL" -eq 0 ]; then
    ok "filtrage de l hôte vers les zones validé : $PASS vérification(s) au vert."
    exit 0
else
    printf '  %d vérification(s) en échec sur %d.\n' "$FAIL" "$((PASS + FAIL))" >&2
    exit 1
fi
