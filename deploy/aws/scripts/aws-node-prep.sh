#!/usr/bin/env bash
# aws-node-prep.sh
# AWS-only node preparation. Does NOT replace k8s-node-common.sh.
#
# On workers:
#   1) Format/mount local NVMe *instance store* at /mnt/reloc-nvme (IO PSI path)
#   2) Bring up secondary ENI (registry path) via netplan DHCP
#
# On control-plane: no-op for NVMe/ENI.
#
# Run as root after cloud-init packages land. Safe to re-run.

set -euo pipefail

ROLE="${1:-worker}"   # worker | control-plane
IO_MOUNT="${RELOC_IO_STRESS_PATH:-/mnt/reloc-nvme}"
REG_IFACE="${RELOC_REGISTRY_IFACE:-ens6}"
PRI_IFACE="${RELOC_PRIMARY_IFACE:-ens5}"

log() { echo "[aws-node-prep] $*"; }

# Identify the local instance-store NVMe — NOT the EBS root.
# Nitro presents both as /dev/nvmeXn1; device index is NOT stable across types/reboots.
# Prefer (in order):
#   1) /dev/disk/by-id/*Instance_Storage*  (Amazon's stable symlink for instance store)
#   2) nvme id-ctrl Model containing "Instance Storage" / "Amazon EC2 NVMe Instance Storage"
# Never fall back to "first unused nvme" — that can silently pick EBS.
find_instance_store_nvme() {
  local link model dev
  shopt -s nullglob
  for link in /dev/disk/by-id/*Instance_Storage* /dev/disk/by-id/*instance-store*; do
    [[ -e "$link" ]] || continue
    # Prefer the whole-disk node (not a partition *-partN)
    case "$link" in
      *-part*) continue ;;
    esac
    dev="$(readlink -f "$link")"
    if [[ -b "$dev" ]]; then
      echo "$dev"
      return 0
    fi
  done

  if command -v nvme >/dev/null 2>&1; then
    for dev in /dev/nvme*n1; do
      [[ -b "$dev" ]] || continue
      model="$(nvme id-ctrl "$dev" 2>/dev/null | awk -F: '/^mn / {gsub(/^[ \t]+|[ \t]+$/,"",$2); print $2; exit}')"
      # Amazon EBS model looks like "Amazon Elastic Block Store"; instance store mentions Instance Storage.
      if echo "$model" | grep -qiE 'Instance Storage|EC2 NVMe Instance'; then
        echo "$dev"
        return 0
      fi
    done
  fi

  return 1
}

assert_not_root_device() {
  local dev="$1"
  local root_src
  root_src="$(findmnt -n -o SOURCE / 2>/dev/null || true)"
  # Resolve partitions: if root is /dev/nvme0n1p1, reject /dev/nvme0n1
  if [[ -n "$root_src" ]]; then
    local root_base="${root_src%%p[0-9]*}"
    if [[ "$dev" == "$root_src" || "$dev" == "$root_base" ]]; then
      log "ERROR: refusing to use root/EBS device $dev (root SOURCE=$root_src)"
      exit 1
    fi
  fi
  # Also refuse if model says Elastic Block Store
  if command -v nvme >/dev/null 2>&1; then
    local model
    model="$(nvme id-ctrl "$dev" 2>/dev/null | awk -F: '/^mn / {gsub(/^[ \t]+|[ \t]+$/,"",$2); print $2; exit}')"
    if echo "$model" | grep -qi 'Elastic Block Store'; then
      log "ERROR: $dev model is EBS ($model); need Instance Storage"
      exit 1
    fi
  fi
}

prep_nvme() {
  mkdir -p "$IO_MOUNT"

  local dev
  if ! dev="$(find_instance_store_nvme)"; then
    log "ERROR: could not identify instance-store NVMe via by-id or nvme id-ctrl"
    log "ls /dev/disk/by-id:"
    ls -la /dev/disk/by-id 2>/dev/null || true
    lsblk -o NAME,SIZE,TYPE,MOUNTPOINT,MODEL
    exit 1
  fi
  assert_not_root_device "$dev"
  log "instance-store NVMe identified: $dev"

  if findmnt -n "$IO_MOUNT" >/dev/null 2>&1; then
    local cur
    cur="$(findmnt -n -o SOURCE "$IO_MOUNT")"
    log "already mounted: $(findmnt -n -o SOURCE,TARGET,FSTYPE "$IO_MOUNT")"
    # Re-validate: mounted source must still be the instance-store device (or a partition of it)
    if [[ "$cur" != "$dev" && "$cur" != ${dev}p* ]]; then
      log "ERROR: $IO_MOUNT is mounted from $cur, expected $dev — refusing silent EBS use"
      exit 1
    fi
  else
    if ! blkid "$dev" >/dev/null 2>&1; then
      log "formatting $dev as ext4 for IO PSI path"
      mkfs.ext4 -F -L reloc-nvme "$dev"
    fi
    UUID="$(blkid -s UUID -o value "$dev")"
    if ! grep -q "$IO_MOUNT" /etc/fstab; then
      echo "UUID=${UUID} ${IO_MOUNT} ext4 defaults,nofail 0 2" >> /etc/fstab
    fi
    mount "$IO_MOUNT"
    log "mounted $dev -> $IO_MOUNT"
  fi

  echo "$dev" > "${IO_MOUNT}/.reloc-device"
  if command -v nvme >/dev/null 2>&1; then
    nvme id-ctrl "$dev" 2>/dev/null | awk -F: '/^mn |^sn / {print}' > "${IO_MOUNT}/.reloc-nvme-id" || true
  fi
  chmod 755 "$IO_MOUNT"
}

prep_registry_eni() {
  local i
  for i in $(seq 1 60); do
    if ip link show "$REG_IFACE" >/dev/null 2>&1; then
      break
    fi
    if ip link show eth1 >/dev/null 2>&1; then
      REG_IFACE=eth1
      break
    fi
    sleep 2
  done

  if ! ip link show "$REG_IFACE" >/dev/null 2>&1; then
    log "ERROR: registry iface $REG_IFACE not present after wait"
    ip -br link
    exit 1
  fi

  log "configuring registry iface $REG_IFACE (primary stays $PRI_IFACE)"

  cat > /etc/netplan/60-reloc-registry.yaml <<EOF
network:
  version: 2
  ethernets:
    ${REG_IFACE}:
      dhcp4: true
      dhcp4-overrides:
        route-metric: 200
        use-routes: true
      optional: true
EOF
  chmod 600 /etc/netplan/60-reloc-registry.yaml
  netplan apply || true
  sleep 3

  # Loose mode (=2) on the secondary iface only. Do NOT set all/default to 0 (disable)
  # and do NOT change primary beyond what the OS already has.
  sysctl -w "net.ipv4.conf.${REG_IFACE}.rp_filter=2" >/dev/null
  # Persist for reboot
  cat > /etc/sysctl.d/99-reloc-registry-rpfilter.conf <<EOF
net.ipv4.conf.${REG_IFACE}.rp_filter = 2
EOF

  ip -br addr show "$REG_IFACE" || true
  log "registry ENI ready (tc shaping target; do not shape $PRI_IFACE)"
  log "rp_filter(${REG_IFACE})=$(sysctl -n net.ipv4.conf.${REG_IFACE}.rp_filter)"
}

case "$ROLE" in
  worker)
    prep_nvme
    prep_registry_eni
    ;;
  control-plane|cp)
    log "control-plane: skipping NVMe/registry ENI prep"
    ;;
  *)
    echo "usage: $0 worker|control-plane" >&2
    exit 2
    ;;
esac

log "done"
