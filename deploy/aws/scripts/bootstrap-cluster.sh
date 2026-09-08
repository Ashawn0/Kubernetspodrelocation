#!/usr/bin/env bash
# bootstrap-cluster.sh — kubeadm init + Calico on the control-plane only.
# Does NOT SSH to workers and does NOT need any private key on the CP.
#
# Args:
#   $1  pod CIDR (e.g. 10.244.0.0/16)
#   $2  apiserver cert extra SAN (CP public IP) — required for operator kubeconfig TLS
#
# Prints a single join command between JOIN_BEGIN / JOIN_END for the operator
# machine to capture and run on each worker via its own SSH session.

set -euo pipefail

POD_CIDR="${1:-10.244.0.0/16}"
EXTRA_SAN="${2:-}"

CP_IP="$(hostname -I | awk '{print $1}')"
log() { echo "[bootstrap] $*"; }

if [[ -z "$EXTRA_SAN" ]]; then
  echo "usage: $0 <pod-cidr> <apiserver-cert-extra-san-public-ip>" >&2
  exit 2
fi

if [[ ! -f /etc/kubernetes/admin.conf ]]; then
  log "kubeadm init advertise=$CP_IP pod-cidr=$POD_CIDR extra-san=$EXTRA_SAN"
  # set -e: non-zero kubeadm init aborts the script before any JOIN_* markers.
  if ! sudo kubeadm init \
    --apiserver-advertise-address="$CP_IP" \
    --apiserver-cert-extra-sans="$EXTRA_SAN" \
    --pod-network-cidr="$POD_CIDR" \
    --ignore-preflight-errors=NumCPU,Mem
  then
    echo "[bootstrap] kubeadm init FAILED; not printing JOIN markers" >&2
    exit 1
  fi
else
  log "admin.conf already present; skipping kubeadm init"
  log "NOTE: if EXTRA_SAN was missing on first init, regenerate certs or recreate the CP"
fi

mkdir -p "$HOME/.kube"
sudo cp -f /etc/kubernetes/admin.conf "$HOME/.kube/config"
sudo chown "$(id -u):$(id -g)" "$HOME/.kube/config"

# Calico CNI — rewrite default IPPool 192.168.0.0/16 -> POD_CIDR (must match kubeadm)
if ! kubectl get ds -n kube-system calico-node >/dev/null 2>&1; then
  log "installing Calico v3.28.0 (IPPool -> $POD_CIDR)"
  curl -fsSL https://raw.githubusercontent.com/projectcalico/calico/v3.28.0/manifests/calico.yaml \
    | sed "s#192.168.0.0/16#${POD_CIDR}#" \
    | kubectl apply -f -
else
  log "Calico already present"
fi

JOIN_CMD="$(sudo kubeadm token create --print-join-command)"
if [[ -z "$JOIN_CMD" || "$JOIN_CMD" != kubeadm\ join* ]]; then
  echo "[bootstrap] token create produced no valid join command; not printing JOIN markers" >&2
  exit 1
fi
# Operator parses these markers; no key material, join token only (short-lived).
# Printed only after successful init (or existing admin.conf) + token create.
echo "JOIN_BEGIN"
echo "$JOIN_CMD"
echo "JOIN_END"

log "CP bootstrap complete (workers join from operator SSH, not from this node)"
# Ready wait is intentionaly NOT here: workers have not joined yet. up.ps1 waits after joins.