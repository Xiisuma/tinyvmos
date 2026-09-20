#!/usr/bin/env bash
#
# TinyVMOS — réseau virtuel de test (Phase 1).
#
# Crée un bridge de zone et une interface tap par micro-VM, avec sortie NAT
# vers l'hôte. C'est une maquette : en Phase 2 le démon fait la même chose
# par netlink et applique une politique nftables « tout refuser » entre zones.
# Ici la politique reste permissive, uniquement pour valider la chaîne.
#
# Usage :
#   sudo ./build/net-setup.sh up   [--bridge br-tinyvmos] [--tap tap0] [--cidr 10.42.10.1/24]
#   sudo ./build/net-setup.sh down [--bridge br-tinyvmos] [--tap tap0]
#   sudo ./build/net-setup.sh status

set -euo pipefail
export SCRIPT_NAME="net-setup"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

ACTION="${1:-}"
shift || true

BRIDGE="br-tinyvmos"
TAP="tap0"
CIDR="10.42.10.1/24"
NAT_TABLE="tinyvmos_nat"

while [ $# -gt 0 ]; do
    case "$1" in
        --bridge) BRIDGE="$2"; shift 2 ;;
        --tap)    TAP="$2";    shift 2 ;;
        --cidr)   CIDR="$2";   shift 2 ;;
        *) die "option inconnue : $1" ;;
    esac
done

SUBNET_BASE="${CIDR%.*}"        # 10.42.10.1/24 -> 10.42.10
PREFIX="${CIDR##*/}"
SUBNET="${SUBNET_BASE}.0/${PREFIX}"

uplink() {
    ip -4 route show default | awk '{print $5; exit}'
}

do_up() {
    need_root

    # Un sous-reseau deja route par une autre interface ferait disparaitre le
    # trafic sans le moindre message d'erreur. Echouer tout de suite, et dire
    # ou regarder.
    local holder
    holder=$(ip -4 route show "$SUBNET" 2>/dev/null | awk '{for(i=1;i<=NF;i++) if($i=="dev") print $(i+1)}' | head -1)
    if [ -n "$holder" ] && [ "$holder" != "$BRIDGE" ]; then
        die "le sous-reseau $SUBNET est deja route par $holder. Liberer cette interface (sudo $0 down --bridge $holder) ou choisir un autre --cidr."
    fi

    if ! ip link show "$BRIDGE" >/dev/null 2>&1; then
        log "création du bridge $BRIDGE"
        ip link add name "$BRIDGE" type bridge
        ip addr add "$CIDR" dev "$BRIDGE"
        ip link set "$BRIDGE" up
    else
        log "bridge $BRIDGE déjà présent"
    fi

    if ! ip link show "$TAP" >/dev/null 2>&1; then
        log "création de l'interface tap $TAP"
        ip tuntap add dev "$TAP" mode tap
        ip link set "$TAP" master "$BRIDGE"
        ip link set "$TAP" up
    else
        log "interface tap $TAP déjà présente"
    fi

    log "activation du routage IPv4"
    sysctl -qw net.ipv4.ip_forward=1

    local up
    up=$(uplink)
    if [ -n "$up" ]; then
        log "NAT de $SUBNET vers $up"
        nft list table ip "$NAT_TABLE" >/dev/null 2>&1 && nft delete table ip "$NAT_TABLE"
        nft -f - <<NFT
table ip ${NAT_TABLE} {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept;
		ip saddr ${SUBNET} oifname "${up}" masquerade
	}
}
NFT
    else
        log "aucune route par défaut : pas de NAT installé"
    fi

    ok "réseau de test prêt"
    ok "  bridge   $BRIDGE ($CIDR)"
    ok "  tap      $TAP"
    ok "  invité   ${SUBNET_BASE}.2, passerelle ${SUBNET_BASE}.1"
}

do_down() {
    need_root

    if ip link show "$TAP" >/dev/null 2>&1; then
        log "suppression de $TAP"
        ip link del "$TAP"
    fi

    # Ne supprimer le bridge que s'il ne porte plus aucune interface.
    if ip link show "$BRIDGE" >/dev/null 2>&1; then
        local members
        members=$(ip link show master "$BRIDGE" 2>/dev/null | grep -c '^[0-9]' || true)
        if [ "$members" -eq 0 ]; then
            log "suppression du bridge $BRIDGE"
            ip link del "$BRIDGE"
            nft list table ip "$NAT_TABLE" >/dev/null 2>&1 && nft delete table ip "$NAT_TABLE"
        else
            log "bridge $BRIDGE conservé : $members interface(s) encore attachée(s)"
        fi
    fi

    ok "nettoyage terminé"
}

do_status() {
    printf '\n--- bridges ---\n'
    ip -br link show type bridge 2>/dev/null || echo "aucun"
    printf '\n--- interfaces tap ---\n'
    ip -br link show type tun 2>/dev/null || echo "aucune"
    printf '\n--- table NAT ---\n'
    nft list table ip "$NAT_TABLE" 2>/dev/null || echo "aucune"
    printf '\n'
}

case "$ACTION" in
    up)     do_up ;;
    down)   do_down ;;
    status) do_status ;;
    *)      die "usage : sudo $0 {up|down|status} [--bridge NOM] [--tap NOM] [--cidr A.B.C.D/N]" ;;
esac
