#!/usr/bin/env bash
# configure-insecure-registry.sh
# Configure containerd certs.d so HTTP pulls from the in-cluster registry work.
# Run as root on every node (cp + workers), then restart containerd.
#
# Usage:
#   REGISTRY_HOSTPORT=192.168.x.y:30500 sudo bash configure-insecure-registry.sh
# Or with Service DNS (nodes using cluster DNS — uncommon for hostNetwork):
#   REGISTRY_HOSTPORT=registry.reloc-registry.svc.cluster.local:5000 sudo bash ...

set -euo pipefail

HOSTPORT="${REGISTRY_HOSTPORT:?set REGISTRY_HOSTPORT to host:port (e.g. <nodePort-ip>:30500)}"

DIR="/etc/containerd/certs.d/${HOSTPORT}"
mkdir -p "$DIR"
cat > "${DIR}/hosts.toml" <<EOF
server = "http://${HOSTPORT}"

[host."http://${HOSTPORT}"]
  capabilities = ["pull", "resolve", "push"]
  skip_verify = true
EOF

# Ensure CRI uses certs.d (containerd 1.5+)
if grep -q 'config_path' /etc/containerd/config.toml 2>/dev/null; then
  sed -i 's#config_path = ""#config_path = "/etc/containerd/certs.d"#' /etc/containerd/config.toml || true
else
  # Insert under registry plugin if missing — best-effort for default Ubuntu kubeadm layout.
  if ! grep -q 'certs.d' /etc/containerd/config.toml; then
    echo "NOTE: ensure plugins.cri.registry.config_path = \"/etc/containerd/certs.d\" in config.toml"
  fi
fi

systemctl restart containerd
echo "Configured insecure registry mirror for ${HOSTPORT} and restarted containerd."
