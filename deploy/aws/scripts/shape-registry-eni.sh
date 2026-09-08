#!/usr/bin/env bash
# shape-registry-eni.sh — apply/clear tc netem on the registry (secondary) iface only.
# Never touch the primary CNI iface.
#
# Usage:
#   shape-registry-eni.sh apply [delay_ms] [rate_mbit]
#   shape-registry-eni.sh clear

set -euo pipefail

ACTION="${1:-apply}"
DELAY_MS="${2:-100}"
RATE_MBIT="${3:-20}"
IFACE="${RELOC_REGISTRY_IFACE:-ens6}"
PRIMARY="${RELOC_PRIMARY_IFACE:-ens5}"

if [[ "$IFACE" == "$PRIMARY" ]]; then
  echo "refusing: registry iface must not equal primary ($PRIMARY)" >&2
  exit 1
fi

if ! ip link show "$IFACE" >/dev/null 2>&1; then
  echo "iface $IFACE missing" >&2
  exit 1
fi

case "$ACTION" in
  apply)
    tc qdisc del dev "$IFACE" root 2>/dev/null || true
    # netem delay + tbf rate limit on registry path only
    tc qdisc add dev "$IFACE" root handle 1: tbf rate "${RATE_MBIT}mbit" burst 32kbit latency 400ms
    tc qdisc add dev "$IFACE" parent 1:1 handle 10: netem delay "${DELAY_MS}ms" 10ms distribution normal
    echo "shaped $IFACE delay=${DELAY_MS}ms rate=${RATE_MBIT}mbit"
    tc qdisc show dev "$IFACE"
    echo "primary $PRIMARY untouched:"
    tc qdisc show dev "$PRIMARY" || true
    ;;
  clear)
    tc qdisc del dev "$IFACE" root 2>/dev/null || true
    echo "cleared tc on $IFACE"
    ;;
  *)
    echo "usage: $0 apply|clear [delay_ms] [rate_mbit]" >&2
    exit 2
    ;;
esac
