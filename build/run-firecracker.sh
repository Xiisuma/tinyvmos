#!/usr/bin/env bash
#
# TinyVMOS — lance une micro-VM Firecracker (Phase 1).
#
# La VM est configurée par l'API REST de Firecracker sur socket Unix, et non
# par un fichier de configuration : c'est exactement le chemin qu'empruntera
# le démon Go en Phase 2, donc autant le valider tout de suite.
#
# Deux modes :
#   --jailer   lance via jailer : chroot, namespaces, cgroup, uid et gid
#              dédiés, filtre seccomp. C'est le mode attendu en production.
#   sans       lance Firecracker directement. Pour diagnostic seulement.
#
# Usage :
#   sudo ./build/run-firecracker.sh --name web1 --service web-apache [--jailer]
#   sudo ./build/run-firecracker.sh --name web1 --stop

set -euo pipefail
export SCRIPT_NAME="run-firecracker"
# shellcheck source=../images/common.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/images/common.sh"

NAME="vm0"
SERVICE="web-apache"
VCPU=1
MEM_MIB=""
GUEST_IP="10.42.10.2"
GUEST_PREFIX="24"
GATEWAY="10.42.10.1"
TAP="tap0"
USE_JAILER=0
STOP=0
WAIT_SECONDS=30

while [ $# -gt 0 ]; do
    case "$1" in
        --name)    NAME="$2";     shift 2 ;;
        --service) SERVICE="$2";  shift 2 ;;
        --vcpu)    VCPU="$2";     shift 2 ;;
        --mem)     MEM_MIB="$2";  shift 2 ;;
        --ip)      GUEST_IP="$2"; shift 2 ;;
        --gw)      GATEWAY="$2";  shift 2 ;;
        --tap)     TAP="$2";      shift 2 ;;
        --jailer)  USE_JAILER=1;  shift ;;
        --stop)    STOP=1;        shift ;;
        --wait)    WAIT_SECONDS="$2"; shift 2 ;;
        *) die "option inconnue : $1" ;;
    esac
done

RUN_DIR="${OUTPUT_DIR}/run/${NAME}"
API_SOCK="${RUN_DIR}/firecracker.sock"
PID_FILE="${RUN_DIR}/firecracker.pid"
LOG_FILE="${RUN_DIR}/console.log"
JAILER_ROOT="${OUTPUT_DIR}/jail"
JAIL_ID="${NAME}"
JAIL_CHROOT="${JAILER_ROOT}/firecracker/${JAIL_ID}/root"

# --------------------------------------------------------------- Arrêt

do_stop() {
    if [ -f "$PID_FILE" ]; then
        local pid
        pid=$(cat "$PID_FILE")
        if kill -0 "$pid" 2>/dev/null; then
            log "arrêt de la VM $NAME (pid $pid)"
            kill "$pid" 2>/dev/null || true
            local i=0
            while kill -0 "$pid" 2>/dev/null && [ "$i" -lt 50 ]; do
                sleep 0.1; i=$((i + 1))
            done
            kill -9 "$pid" 2>/dev/null || true
        fi
        rm -f "$PID_FILE"
    fi
    rm -f "$API_SOCK"
    [ -d "$JAIL_CHROOT" ] && rm -rf "${JAILER_ROOT}/firecracker/${JAIL_ID}"
    ok "VM $NAME arrêtée et nettoyée"
}

if [ "$STOP" -eq 1 ]; then
    need_root
    do_stop
    exit 0
fi

# ------------------------------------------------------------ Préalables

need_root

[ -e /dev/kvm ] || die "/dev/kvm absent : Firecracker ne peut pas démarrer. Lancer ./build/check-env.sh."
command -v firecracker >/dev/null || die "firecracker absent du PATH"

KERNEL="${KERNEL_OUT}/vmlinux"
IMAGE="${IMAGES_OUT}/${SERVICE}.ext4"
META="${IMAGES_OUT}/${SERVICE}.json"

[ -f "$KERNEL" ] || die "noyau invité absent : lancer ./images/kernel/fetch-kernel.sh"
[ -f "$IMAGE" ]  || die "image absente : lancer sudo ./images/build-image.sh ${SERVICE}"

# Le budget mémoire vient des métadonnées de l'image, pas d'une valeur choisie
# au hasard à l'exécution. La machine cible est plus faible que celle de dev.
if [ -z "$MEM_MIB" ]; then
    if [ -f "$META" ] && command -v jq >/dev/null; then
        MEM_MIB=$(jq -r '.memory_budget_mib' "$META")
    else
        MEM_MIB=256
    fi
fi

ip link show "$TAP" >/dev/null 2>&1 \
    || die "interface $TAP absente : lancer sudo ./build/net-setup.sh up --tap $TAP"

do_stop >/dev/null 2>&1 || true
mkdir -p "$RUN_DIR"

# Adresse MAC déterministe dérivée du nom : une VM garde la même à chaque
# démarrage, ce qui rend les baux et les règles reproductibles.
MAC=$(printf '%s' "$NAME" | sha256sum | sed 's/^\(..\)\(..\)\(..\)\(..\).*/02:FC:\1:\2:\3:\4/')

# ip= est interprété par le noyau avant tout espace utilisateur ; tinyvmos.ip
# est relu par le service tinyvmos-net de l'image. Les deux disent la même
# chose, l'un pour le noyau, l'autre pour OpenRC.
CMDLINE="console=ttyS0 reboot=k panic=1 pci=off i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
CMDLINE="${CMDLINE} root=/dev/vda rw"
CMDLINE="${CMDLINE} ip=${GUEST_IP}::${GATEWAY}:255.255.255.0::eth0:off"
CMDLINE="${CMDLINE} tinyvmos.ip=${GUEST_IP}/${GUEST_PREFIX} tinyvmos.gw=${GATEWAY}"

log "VM        : $NAME"
log "service   : $SERVICE"
log "vCPU      : $VCPU, mémoire : ${MEM_MIB} Mio"
log "réseau    : ${GUEST_IP}/${GUEST_PREFIX} via $TAP, passerelle $GATEWAY"
log "MAC       : $MAC"
log "mode      : $([ "$USE_JAILER" -eq 1 ] && echo 'jailer (confiné)' || echo 'direct (diagnostic)')"

# ------------------------------------------------------------ Lancement

if [ "$USE_JAILER" -eq 1 ]; then
    id -u tinyvmos >/dev/null 2>&1 || {
        log "création de l'utilisateur système tinyvmos"
        useradd --system --no-create-home --shell /usr/sbin/nologin tinyvmos
    }
    UID_VM=$(id -u tinyvmos)
    GID_VM=$(id -g tinyvmos)

    rm -rf "${JAILER_ROOT}/firecracker/${JAIL_ID}"
    mkdir -p "$JAIL_CHROOT"

    # jailer confine Firecracker dans ce chroot : tout ce dont la VM a besoin
    # doit s'y trouver, rien d'autre ne doit y être accessible.
    cp "$KERNEL" "${JAIL_CHROOT}/vmlinux"
    cp "$IMAGE"  "${JAIL_CHROOT}/rootfs.ext4"
    chown -R "${UID_VM}:${GID_VM}" "${JAILER_ROOT}/firecracker/${JAIL_ID}"

    KERNEL_IN_VM="/vmlinux"
    IMAGE_IN_VM="/rootfs.ext4"
    API_SOCK="${JAIL_CHROOT}/run/firecracker.socket"

    jailer \
        --id "$JAIL_ID" \
        --exec-file "$(command -v firecracker)" \
        --uid "$UID_VM" --gid "$GID_VM" \
        --chroot-base-dir "$JAILER_ROOT" \
        --daemonize \
        -- --api-sock /run/firecracker.socket \
        > "$LOG_FILE" 2>&1 &
else
    KERNEL_IN_VM="$KERNEL"
    IMAGE_IN_VM="$IMAGE"
    setsid firecracker --api-sock "$API_SOCK" < /dev/null > "$LOG_FILE" 2>&1 &
    echo $! > "$PID_FILE"
    disown 2>/dev/null || true
fi

# Attendre que le socket d'API réponde.
i=0
until [ -S "$API_SOCK" ] || [ "$i" -ge 100 ]; do sleep 0.05; i=$((i + 1)); done
[ -S "$API_SOCK" ] || { cat "$LOG_FILE" >&2; die "le socket d'API n'est jamais apparu"; }

api() {
    local method="$1" path="$2" body="$3"
    local out
    out=$(curl -fsS --unix-socket "$API_SOCK" -X "$method" \
        --header 'Content-Type: application/json' \
        --data "$body" "http://localhost${path}" 2>&1) || {
        printf 'echec API %s %s\n%s\n' "$method" "$path" "$out" >&2
        cat "$LOG_FILE" >&2
        die "configuration de la VM impossible"
    }
}

log "configuration par l'API sur socket Unix"

api PUT /boot-source "$(printf '{"kernel_image_path":"%s","boot_args":"%s"}' "$KERNEL_IN_VM" "$CMDLINE")"
api PUT /drives/rootfs "$(printf '{"drive_id":"rootfs","path_on_host":"%s","is_root_device":true,"is_read_only":false}' "$IMAGE_IN_VM")"
api PUT /network-interfaces/eth0 "$(printf '{"iface_id":"eth0","guest_mac":"%s","host_dev_name":"%s"}' "$MAC" "$TAP")"
api PUT /machine-config "$(printf '{"vcpu_count":%s,"mem_size_mib":%s,"smt":false}' "$VCPU" "$MEM_MIB")"

START_NS=$(date +%s%N)
api PUT /actions '{"action_type":"InstanceStart"}'

# Le pid de Firecracker sous jailer n'est connu qu'une fois le processus lancé.
if [ "$USE_JAILER" -eq 1 ]; then
    pgrep -f "firecracker --id $JAIL_ID" > "$PID_FILE" 2>/dev/null || \
    pgrep -x firecracker | tail -1 > "$PID_FILE"
fi

ok "VM démarrée"

# ------------------------------------------------------------ Vérification

log "attente de la réponse HTTP sur http://${GUEST_IP}/ (max ${WAIT_SECONDS}s)"
deadline=$(( $(date +%s) + WAIT_SECONDS ))
until curl -fsS --max-time 2 -o /dev/null "http://${GUEST_IP}/" 2>/dev/null; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
        printf '\n--- console de la VM ---\n' >&2
        tail -40 "$LOG_FILE" >&2
        die "pas de réponse HTTP après ${WAIT_SECONDS}s"
    fi
    sleep 0.25
done
END_NS=$(date +%s%N)

ELAPSED_MS=$(( (END_NS - START_NS) / 1000000 ))

ok "HTTP répond depuis l'hôte"
ok "démarrage jusqu'à la première réponse HTTP : ${ELAPSED_MS} ms"
ok "console : $LOG_FILE"
ok "arrêt   : sudo $0 --name $NAME --stop"
