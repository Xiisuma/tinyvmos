#!/usr/bin/env bash
#
# TinyVMOS — constructeur générique d'images de micro-VM.
#
# Une image égale un service, sans exception. Chaque service décrit ce dont il
# a besoin dans images/<service>/image.conf, et l'image produite est un système
# de fichiers ext4 démarrable directement par Firecracker.
#
# Usage : sudo ./images/build-image.sh <service>
# Exemple : sudo ./images/build-image.sh web-apache

set -euo pipefail
export SCRIPT_NAME="build-image"
# shellcheck source=common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

SERVICE="${1:-}"
[ -n "$SERVICE" ] || die "usage : sudo $0 <service>"

SERVICE_DIR="${REPO_ROOT}/images/${SERVICE}"
[ -d "$SERVICE_DIR" ] || die "service inconnu : $SERVICE (pas de dossier $SERVICE_DIR)"
[ -f "${SERVICE_DIR}/image.conf" ] || die "il manque ${SERVICE_DIR}/image.conf"

need_root

# ------------------------------------------------------------ Configuration

# Valeurs par défaut, écrasables par image.conf.
PACKAGES=""
RC_SERVICES=""
IMAGE_SIZE_MB=128
MEMORY_BUDGET_MIB=128
DEFAULT_IP="10.42.10.2"
DEFAULT_PREFIX="24"
DEFAULT_GATEWAY="10.42.10.1"

# shellcheck source=/dev/null
. "${SERVICE_DIR}/image.conf"

log "service            : $SERVICE"
log "paquets Alpine     : ${PACKAGES:-aucun}"
log "services OpenRC    : ${RC_SERVICES:-aucun}"
log "taille de l'image  : ${IMAGE_SIZE_MB} Mio"
log "budget mémoire     : ${MEMORY_BUDGET_MIB} Mio"

# ------------------------------------------------------------- Préparation

mkdir -p "$DL_DIR" "$IMAGES_OUT"
TARBALL="${DL_DIR}/${ALPINE_MINIROOTFS}"
fetch_verified "$ALPINE_MINIROOTFS_URL" "$TARBALL" "$ALPINE_MINIROOTFS_SHA256"

ROOTFS=$(mktemp -d /tmp/tinyvmos-rootfs-XXXXXX)
MOUNTED=""

cleanup() {
    local m
    for m in $MOUNTED; do
        umount -l "$m" 2>/dev/null || true
    done
    rm -rf "$ROOTFS"
}
trap cleanup EXIT

log "extraction du minirootfs Alpine ${ALPINE_VERSION}"
tar -xzf "$TARBALL" -C "$ROOTFS"

# ------------------------------------------------- Installation des paquets

if [ -n "$PACKAGES" ]; then
    cp /etc/resolv.conf "${ROOTFS}/etc/resolv.conf"
    printf '%s/%s/main\n%s/%s/community\n' \
        "$ALPINE_MIRROR" "$ALPINE_BRANCH" "$ALPINE_MIRROR" "$ALPINE_BRANCH" \
        > "${ROOTFS}/etc/apk/repositories"

    for d in proc sys dev; do
        mount --bind "/$d" "${ROOTFS}/$d"
        MOUNTED="${ROOTFS}/$d $MOUNTED"
    done

    log "installation des paquets dans le chroot"
    # shellcheck disable=SC2086
    chroot "$ROOTFS" /sbin/apk add --no-cache openrc busybox-openrc $PACKAGES

    for m in $MOUNTED; do umount -l "$m" 2>/dev/null || true; done
    MOUNTED=""
    rm -f "${ROOTFS}/etc/resolv.conf"
fi

# ----------------------------------------------------- Socle système commun

log "configuration du socle système"

echo "${SERVICE}" > "${ROOTFS}/etc/hostname"

cat > "${ROOTFS}/etc/hosts" <<HOSTS
127.0.0.1	localhost
::1		localhost
127.0.1.1	${SERVICE}
HOSTS

# Firecracker n'expose qu'un port série. C'est la seule console de la VM,
# et la seule façon de diagnostiquer un démarrage qui échoue.
cat > "${ROOTFS}/etc/inittab" <<'INITTAB'
::sysinit:/sbin/openrc sysinit
::sysinit:/sbin/openrc boot
::wait:/sbin/openrc default

ttyS0::respawn:/sbin/getty -L ttyS0 115200 vt100

::shutdown:/sbin/openrc shutdown
INITTAB

cat > "${ROOTFS}/etc/fstab" <<'FSTAB'
/dev/vda	/		ext4	rw,relatime	0 1
proc		/proc		proc	defaults	0 0
sysfs		/sys		sysfs	defaults	0 0
devpts		/dev/pts	devpts	defaults	0 0
tmpfs		/tmp		tmpfs	defaults	0 0
FSTAB

# Configuration réseau. Par défaut statique, mais surchargeable depuis la
# ligne de commande du noyau via tinyvmos.ip / tinyvmos.gw : la même image
# sert ainsi plusieurs VM, conformément au principe « images jetables ».
cat > "${ROOTFS}/etc/network/interfaces" <<IFACES
auto lo
iface lo inet loopback

auto eth0
iface eth0 inet static
	address ${DEFAULT_IP}/${DEFAULT_PREFIX}
	gateway ${DEFAULT_GATEWAY}
IFACES

cat > "${ROOTFS}/etc/init.d/tinyvmos-net" <<'NETINIT'
#!/sbin/openrc-run

description="Applique l'adressage reseau passe par la ligne de commande du noyau"

depend() {
	before net
}

start() {
	local ip gw
	for arg in $(cat /proc/cmdline); do
		case "$arg" in
			tinyvmos.ip=*) ip="${arg#tinyvmos.ip=}" ;;
			tinyvmos.gw=*) gw="${arg#tinyvmos.gw=}" ;;
		esac
	done

	[ -n "$ip" ] || return 0

	ebegin "Adressage reseau depuis la ligne de commande du noyau ($ip)"
	{
		echo "auto lo"
		echo "iface lo inet loopback"
		echo ""
		echo "auto eth0"
		echo "iface eth0 inet static"
		echo "	address $ip"
		[ -n "$gw" ] && echo "	gateway $gw"
	} > /etc/network/interfaces
	eend 0
}
NETINIT
chmod 0755 "${ROOTFS}/etc/init.d/tinyvmos-net"

# Le resolveur pointe vers la passerelle : en Phase 2 ce sera la VM dhcp-dns.
echo "nameserver ${DEFAULT_GATEWAY}" > "${ROOTFS}/etc/resolv.conf"

# Compte root sans mot de passe utilisable : aucune connexion par mot de passe
# n'est prevue. L'acces se fait par la console serie, qui n'est joignable que
# depuis l'hote.
chroot "$ROOTFS" /usr/bin/passwd -l root >/dev/null 2>&1 || true

# ------------------------------------------------- Activation des services

log "activation des services OpenRC"
chroot "$ROOTFS" /sbin/rc-update add tinyvmos-net boot >/dev/null 2>&1 || true
chroot "$ROOTFS" /sbin/rc-update add networking boot   >/dev/null 2>&1 || true
chroot "$ROOTFS" /sbin/rc-update add hostname boot     >/dev/null 2>&1 || true
chroot "$ROOTFS" /sbin/rc-update add bootmisc boot     >/dev/null 2>&1 || true
chroot "$ROOTFS" /sbin/rc-update add syslog boot       >/dev/null 2>&1 || true

for svc in $RC_SERVICES; do
    chroot "$ROOTFS" /sbin/rc-update add "$svc" default >/dev/null 2>&1 \
        || die "impossible d'activer le service OpenRC « $svc »"
    log "  service activé : $svc"
done

# OpenRC dans une micro-VM : pas de tty virtuel, pas de detection de materiel.
mkdir -p "${ROOTFS}/run/openrc"
touch "${ROOTFS}/run/openrc/softlevel"

# ------------------------------------------- Surcouche propre au service

if [ -d "${SERVICE_DIR}/overlay" ]; then
    log "application de la surcouche du service"
    cp -a "${SERVICE_DIR}/overlay/." "$ROOTFS/"
fi

if [ -f "${SERVICE_DIR}/configure.sh" ]; then
    log "exécution de configure.sh dans le chroot"
    cp "${SERVICE_DIR}/configure.sh" "${ROOTFS}/tmp/configure.sh"
    chmod 0755 "${ROOTFS}/tmp/configure.sh"
    chroot "$ROOTFS" /bin/sh /tmp/configure.sh \
        || die "configure.sh du service $SERVICE a échoué"
    rm -f "${ROOTFS}/tmp/configure.sh"
fi

# ------------------------------------------------- Fabrication de l'image

IMAGE="${IMAGES_OUT}/${SERVICE}.ext4"
log "création de l'image ext4 (${IMAGE_SIZE_MB} Mio)"
rm -f "$IMAGE"

# mkfs.ext4 -d recopie l'arborescence sans montage : pas de boucle, pas de
# peripherique, et l'operation reste reproductible.
truncate -s "${IMAGE_SIZE_MB}M" "$IMAGE"
mkfs.ext4 -q -F -L "tvm-${SERVICE}" -d "$ROOTFS" "$IMAGE"

USED_KB=$(du -sk "$ROOTFS" | cut -f1)
USED_MB=$(( USED_KB / 1024 ))

cat > "${IMAGES_OUT}/${SERVICE}.json" <<META
{
  "service": "${SERVICE}",
  "alpine": "${ALPINE_VERSION}",
  "image_size_mib": ${IMAGE_SIZE_MB},
  "rootfs_used_mib": ${USED_MB},
  "memory_budget_mib": ${MEMORY_BUDGET_MIB},
  "packages": "${PACKAGES}",
  "rc_services": "${RC_SERVICES}",
  "sha256": "$(sha256sum "$IMAGE" | cut -d' ' -f1)"
}
META

ok "image construite : $IMAGE"
ok "occupation du rootfs : ${USED_MB} Mio sur ${IMAGE_SIZE_MB} Mio alloués"
ok "budget mémoire déclaré : ${MEMORY_BUDGET_MIB} Mio"
ok "métadonnées : ${IMAGES_OUT}/${SERVICE}.json"

if [ "$USED_MB" -gt $(( IMAGE_SIZE_MB * 9 / 10 )) ]; then
    log "attention : le rootfs occupe plus de 90 % de l'image. Augmenter IMAGE_SIZE_MB."
fi
