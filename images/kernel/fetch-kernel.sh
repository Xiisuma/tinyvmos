#!/usr/bin/env bash
#
# TinyVMOS — récupère le noyau invité utilisé par toutes les micro-VM.
#
# Firecracker démarre le noyau directement, sans UEFI ni bootloader : il lui
# faut un vmlinux non compressé. Ce script récupère celui de la chaîne
# d'intégration continue de Firecracker et vérifie son empreinte.
#
# Usage : ./images/kernel/fetch-kernel.sh

set -euo pipefail
export SCRIPT_NAME="fetch-kernel"
# shellcheck source=../common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/common.sh"

mkdir -p "$KERNEL_OUT"
DEST="${KERNEL_OUT}/vmlinux-${GUEST_KERNEL_VERSION}"

fetch_verified "$GUEST_KERNEL_URL" "$DEST" "$GUEST_KERNEL_SHA256"
ln -sfn "vmlinux-${GUEST_KERNEL_VERSION}" "${KERNEL_OUT}/vmlinux"

ok "noyau invité prêt : $DEST"
ok "lien stable : ${KERNEL_OUT}/vmlinux"
