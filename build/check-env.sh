#!/usr/bin/env bash
#
# TinyVMOS — vérification de l'environnement de développement (Phase 0.5).
#
# Contrôle que la machine de développement dispose de tout ce qu'il faut pour
# construire l'OS et lancer des micro-VM. Sort en code non nul si un élément
# manque, avec un message explicite par problème.
#
# Usage : ./build/check-env.sh [--quiet]

set -uo pipefail

QUIET=0
[ "${1:-}" = "--quiet" ] && QUIET=1

FAILURES=0
WARNINGS=0

if [ -t 1 ] && [ "$QUIET" -eq 0 ]; then
    C_OK=$'\033[32m'; C_KO=$'\033[31m'; C_WARN=$'\033[33m'
    C_HEAD=$'\033[1m'; C_OFF=$'\033[0m'
else
    C_OK=""; C_KO=""; C_WARN=""; C_HEAD=""; C_OFF=""
fi

section() { printf '\n%s== %s ==%s\n' "$C_HEAD" "$1" "$C_OFF"; }
ok()      { printf '  %s[ OK ]%s %s\n' "$C_OK" "$C_OFF" "$1"; }
ko()      { printf '  %s[FAIL]%s %s\n' "$C_KO" "$C_OFF" "$1"; FAILURES=$((FAILURES + 1)); }
warn()    { printf '  %s[WARN]%s %s\n' "$C_WARN" "$C_OFF" "$1"; WARNINGS=$((WARNINGS + 1)); }

# Vérifie qu'une commande existe et affiche sa version.
need_cmd() {
    local cmd="$1" label="${2:-$1}"
    if command -v "$cmd" >/dev/null 2>&1; then
        ok "$label ($(command -v "$cmd"))"
    else
        ko "$label absent — installer le paquet correspondant"
    fi
}

# ---------------------------------------------------------------- Système

section "Système"

ARCH=$(uname -m)
case "$ARCH" in
    x86_64)  ok "architecture $ARCH" ;;
    aarch64) warn "architecture $ARCH — cible secondaire, la priorité est x86_64" ;;
    *)       ko "architecture $ARCH non supportée (x86_64 ou aarch64 attendus)" ;;
esac

ok "noyau $(uname -r)"

if [ -r /etc/os-release ]; then
    # shellcheck disable=SC1091
    . /etc/os-release
    ok "distribution ${PRETTY_NAME:-inconnue}"
else
    warn "/etc/os-release illisible — distribution inconnue"
fi

if grep -qi microsoft /proc/version 2>/dev/null; then
    ok "exécution sous WSL2"
    case "$PWD" in
        /mnt/*) ko "le dépôt est sous $PWD, donc sur le système de fichiers Windows. Les builds Buildroot y sont plusieurs fois plus lents et les permissions et liens symboliques cassent. Déplacer le dépôt dans ~/tinyvmos." ;;
        *)      ok "dépôt sur le système de fichiers Linux ($PWD)" ;;
    esac
fi

# ------------------------------------------------------------ Virtualisation

section "Virtualisation"

if grep -qw svm /proc/cpuinfo 2>/dev/null; then
    ok "CPU AMD avec SVM — virtualisation matérielle disponible"
elif grep -qw vmx /proc/cpuinfo 2>/dev/null; then
    ok "CPU Intel avec VT-x — virtualisation matérielle disponible"
else
    ko "aucun flag de virtualisation (svm ou vmx) dans /proc/cpuinfo — activer AMD-V ou Intel VT-x dans le BIOS, et nestedVirtualization=true dans .wslconfig si l'on est sous WSL2"
fi

if [ -e /dev/kvm ]; then
    ok "/dev/kvm présent"
    if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
        ok "/dev/kvm accessible en lecture et écriture par $(id -un)"
    else
        ko "/dev/kvm présent mais inaccessible à $(id -un) — ajouter l'utilisateur au groupe kvm : sudo usermod -aG kvm \$USER, puis rouvrir la session"
    fi
else
    ko "/dev/kvm absent — Firecracker ne peut pas fonctionner. Vérifier AMD-V ou VT-x dans le BIOS, et nestedVirtualization=true dans .wslconfig"
fi

# -------------------------------------------------------------- Ressources

section "Ressources"

MEM_MB=$(awk '/MemTotal/ {print int($2 / 1024)}' /proc/meminfo)
if   [ "$MEM_MB" -ge 8192 ]; then ok "RAM disponible : ${MEM_MB} Mio"
elif [ "$MEM_MB" -ge 4096 ]; then warn "RAM disponible : ${MEM_MB} Mio — 8192 Mio recommandés pour Buildroot"
else ko "RAM disponible : ${MEM_MB} Mio — insuffisant, ajuster memory= dans .wslconfig"
fi

CPUS=$(nproc)
if [ "$CPUS" -ge 4 ]; then ok "${CPUS} cœurs disponibles"
else warn "${CPUS} cœurs seulement — les builds Buildroot seront longs"
fi

DISK_GB=$(df -BG --output=avail . 2>/dev/null | tail -1 | tr -dc '0-9')
if [ -z "$DISK_GB" ]; then
    warn "espace disque indéterminable"
elif [ "$DISK_GB" -ge 150 ]; then ok "espace disque libre : ${DISK_GB} Gio"
elif [ "$DISK_GB" -ge 50 ];  then warn "espace disque libre : ${DISK_GB} Gio — 150 Gio recommandés pour Buildroot et les images"
else ko "espace disque libre : ${DISK_GB} Gio — insuffisant"
fi

SWAP_MB=$(awk '/SwapTotal/ {print int($2 / 1024)}' /proc/meminfo)
if [ "$SWAP_MB" -ge 2048 ]; then ok "swap : ${SWAP_MB} Mio"
else warn "swap : ${SWAP_MB} Mio — un build Buildroot peut saturer la RAM"
fi

# ------------------------------------------------------ Outils de build

section "Chaîne de compilation"
for c in gcc g++ make bc bison flex rsync cpio unzip wget file python3 pkg-config git curl; do
    need_cmd "$c"
done

section "Outils d'image"
need_cmd qemu-system-x86_64 "qemu-system-x86_64"
need_cmd qemu-img
need_cmd xorriso
need_cmd mkfs.ext4
need_cmd mkfs.vfat
need_cmd mksquashfs

section "Outils réseau"
for c in ip nft tcpdump; do need_cmd "$c"; done
need_cmd dnsmasq
need_cmd wg "wireguard-tools (wg)"

section "Chiffrement"
need_cmd cryptsetup

# ------------------------------------------------------------------- Go

section "Go"
GO_BIN=""
if command -v go >/dev/null 2>&1; then GO_BIN=go
elif [ -x /usr/local/go/bin/go ]; then GO_BIN=/usr/local/go/bin/go
fi

if [ -n "$GO_BIN" ]; then
    GO_RAW=$("$GO_BIN" version | awk '{print $3}')
    GO_MAJ=$(printf '%s' "$GO_RAW" | sed 's/^go//' | cut -d. -f1)
    GO_MIN=$(printf '%s' "$GO_RAW" | sed 's/^go//' | cut -d. -f2)
    if [ "$GO_MAJ" -gt 1 ] || { [ "$GO_MAJ" -eq 1 ] && [ "$GO_MIN" -ge 22 ]; }; then
        ok "$GO_RAW"
    else
        ko "$GO_RAW trop ancien — Go 1.22 ou plus récent requis"
    fi
    [ "$GO_BIN" != "go" ] && warn "go absent du PATH — ajouter /usr/local/go/bin (voir /etc/profile.d/go.sh)"
else
    ko "Go absent — installer depuis https://go.dev/dl/"
fi

# ---------------------------------------------------------- Firecracker

section "Firecracker"
if command -v firecracker >/dev/null 2>&1; then
    ok "firecracker $(firecracker --version 2>&1 | head -1 | awk '{print $2}')"
else
    ko "firecracker absent — installer le binaire statique depuis les releases GitHub"
fi

if command -v jailer >/dev/null 2>&1; then
    ok "jailer présent"
else
    ko "jailer absent — il est livré dans la même archive que firecracker, et c'est lui qui assure le confinement des VM"
fi

# --------------------------------------------------------------- Réseau

section "Connectivité"
if curl -fsS --max-time 8 -o /dev/null https://go.dev 2>/dev/null; then
    ok "accès Internet (nécessaire pour Buildroot, Go et les images Alpine)"
else
    warn "pas d'accès Internet détecté — le téléchargement des sources échouera"
fi

# ---------------------------------------------------------------- Bilan

section "Bilan"
if [ "$FAILURES" -eq 0 ] && [ "$WARNINGS" -eq 0 ]; then
    printf '  %sEnvironnement complet. Phase 0 validée.%s\n\n' "$C_OK" "$C_OFF"
    exit 0
elif [ "$FAILURES" -eq 0 ]; then
    printf '  %s%d avertissement(s), aucun blocage. Phase 0 validée.%s\n\n' "$C_WARN" "$WARNINGS" "$C_OFF"
    exit 0
else
    printf '  %s%d blocage(s) et %d avertissement(s). Corriger les blocages avant de continuer.%s\n\n' "$C_KO" "$FAILURES" "$WARNINGS" "$C_OFF"
    exit 1
fi
