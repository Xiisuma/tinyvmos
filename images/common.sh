#!/usr/bin/env bash
# shellcheck shell=bash
# shellcheck disable=SC2034  # bibliotheque : variables consommees par les scripts appelants
#
# TinyVMOS — variables et fonctions communes à la construction des images.
#
# Ce fichier est destiné à être sourcé, pas exécuté.

# Versions figées : une entrée identique doit produire une sortie identique.
ALPINE_BRANCH="v3.24"
ALPINE_VERSION="3.24.2"
ALPINE_ARCH="x86_64"
ALPINE_MINIROOTFS="alpine-minirootfs-${ALPINE_VERSION}-${ALPINE_ARCH}.tar.gz"
ALPINE_MIRROR="https://dl-cdn.alpinelinux.org/alpine"
ALPINE_MINIROOTFS_URL="${ALPINE_MIRROR}/${ALPINE_BRANCH}/releases/${ALPINE_ARCH}/${ALPINE_MINIROOTFS}"
ALPINE_MINIROOTFS_SHA256="c5ca053cfe1d85c5b96dff8b9bc57045f7f184a30ffb6b65776409ca90388677"

# Noyau invité. Firecracker démarre le noyau directement : pas d'UEFI, pas de
# bootloader, un simple vmlinux non compressé. Celui-ci vient de la chaîne
# d'intégration continue de Firecracker ; un noyau construit sur mesure et
# réduit au strict nécessaire le remplacera en Phase 4.
GUEST_KERNEL_VERSION="6.1.155"
GUEST_KERNEL_URL="https://s3.amazonaws.com/spec.ccfc.min/firecracker-ci/v1.15/${ALPINE_ARCH}/vmlinux-${GUEST_KERNEL_VERSION}"
GUEST_KERNEL_SHA256="e20e46d0c36c55c0d1014eb20576171b3f3d922260d9f792017aeff53af3d4f2"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUTPUT_DIR="${REPO_ROOT}/output"
DL_DIR="${OUTPUT_DIR}/dl"
IMAGES_OUT="${OUTPUT_DIR}/images"
KERNEL_OUT="${OUTPUT_DIR}/kernel"

if [ -t 2 ]; then
    _C_INFO=$'\033[36m'; _C_OK=$'\033[32m'; _C_ERR=$'\033[31m'; _C_OFF=$'\033[0m'
else
    _C_INFO=""; _C_OK=""; _C_ERR=""; _C_OFF=""
fi

log()  { printf '%s[%s]%s %s\n' "$_C_INFO" "${SCRIPT_NAME:-tinyvmos}" "$_C_OFF" "$*" >&2; }
ok()   { printf '%s[%s]%s %s\n' "$_C_OK" "${SCRIPT_NAME:-tinyvmos}" "$_C_OFF" "$*" >&2; }
die()  { printf '%s[%s] erreur :%s %s\n' "$_C_ERR" "${SCRIPT_NAME:-tinyvmos}" "$_C_OFF" "$*" >&2; exit 1; }

need_root() {
    [ "$(id -u)" -eq 0 ] || die "ce script doit être lancé en root (chroot et création de périphériques). Relancer avec sudo."
}

# texte_contient cherche un motif dans un texte, sans tube.
#
# « commande | grep -q motif » est un piège sous « set -o pipefail » : grep -q
# ferme le tube dès qu'il trouve, le producteur reçoit SIGPIPE et sort en code
# non nul, ce qui fait échouer tout le pipeline alors que le motif était bien
# présent. Le résultat dépend alors de l'ordonnancement : une vérification qui
# passe une fois sur deux ne prouve rien.
texte_contient() {
    local texte="$1" motif="$2"
    case "$texte" in
        *"$motif"*) return 0 ;;
        *) return 1 ;;
    esac
}

# nft_contient cherche un motif dans la table nftables de TinyVMOS.
# Renvoie 2 si la table n'existe pas, pour distinguer « absent » de « motif
# introuvable ».
nft_contient() {
    local motif="$1" sortie
    sortie=$(nft list table inet "${NFT_TABLE:-tinyvmos}" 2>/dev/null) || return 2
    texte_contient "$sortie" "$motif"
}

# nft_correspond cherche une EXPRESSION REGULIERE dans la table nftables.
#
# Distinct de nft_contient, qui cherche une chaîne littérale. Confondre les
# deux donne une vérification qui ne peut pas réussir : « counter.*drop » n'est
# jamais présent mot pour mot. L'entrée est passée par here-string, donc sans
# producteur à qui envoyer un SIGPIPE.
nft_correspond() {
    local motif="$1" sortie
    sortie=$(nft list table inet "${NFT_TABLE:-tinyvmos}" 2>/dev/null) || return 2
    grep -qE -- "$motif" <<< "$sortie"
}

# fichier_contient cherche un motif dans un fichier, sans tube.
fichier_contient() {
    local fichier="$1" motif="$2"
    [ -f "$fichier" ] || return 2
    grep -q -- "$motif" "$fichier"
}

# Télécharge une URL dans le cache et vérifie son empreinte.
# Un fichier déjà présent et valide n'est pas retéléchargé.
fetch_verified() {
    local url="$1" dest="$2" want="$3"
    mkdir -p "$(dirname "$dest")"

    if [ -f "$dest" ]; then
        local have
        have=$(sha256sum "$dest" | cut -d' ' -f1)
        if [ "$have" = "$want" ]; then
            log "déjà en cache et vérifié : $(basename "$dest")"
            return 0
        fi
        log "empreinte du cache invalide, retéléchargement de $(basename "$dest")"
        rm -f "$dest"
    fi

    log "téléchargement de $url"
    curl -fsSL --retry 3 --retry-delay 2 -o "$dest.part" "$url" \
        || die "téléchargement impossible : $url"

    local have
    have=$(sha256sum "$dest.part" | cut -d' ' -f1)
    if [ "$have" != "$want" ]; then
        rm -f "$dest.part"
        die "empreinte SHA-256 incorrecte pour $(basename "$dest") : attendu $want, obtenu $have"
    fi
    mv "$dest.part" "$dest"
    ok "vérifié : $(basename "$dest")"
}
