#!/usr/bin/env bash
# k8s-node-common.sh
# Common prerequisites for kubeadm on Ubuntu 22.04. Run as root (via sudo) on
# every node, control-plane and workers, before kubeadm init / kubeadm join.

set -euo pipefail

K8S_MINOR="1.30"   # pins the apt repo minor version; kubelet/kubeadm/kubectl track this

# --- Disable swap (kubelet refuses to start with swap on) ---
swapoff -a
sed -i '/ swap / s/^/#/' /etc/fstab

# --- Kernel modules and sysctl required for container networking ---
cat <<EOF | tee /etc/modules-load.d/k8s.conf
overlay
br_netfilter
EOF

modprobe overlay
modprobe br_netfilter

cat <<EOF | tee /etc/sysctl.d/k8s.conf
net.bridge.bridge-nf-call-iptables  = 1
net.bridge.bridge-nf-call-ip6tables = 1
net.ipv4.ip_forward                 = 1
EOF

sysctl --system

# --- containerd ---
apt-get update
apt-get install -y containerd

mkdir -p /etc/containerd
containerd config default | tee /etc/containerd/config.toml >/dev/null

# systemd cgroup driver, must match kubelet's default
sed -i 's/SystemdCgroup = false/SystemdCgroup = true/' /etc/containerd/config.toml

systemctl restart containerd
systemctl enable containerd

# --- kubeadm, kubelet, kubectl ---
apt-get install -y apt-transport-https ca-certificates curl gpg

mkdir -p /etc/apt/keyrings
curl -fsSL "https://pkgs.k8s.io/core:/stable:/v${K8S_MINOR}/deb/Release.key" | \
    gpg --batch --yes --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg

echo "deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v${K8S_MINOR}/deb/ /" | \
    tee /etc/apt/sources.list.d/kubernetes.list

apt-get update
apt-get install -y kubelet kubeadm kubectl
apt-mark hold kubelet kubeadm kubectl

echo "Node prerequisites installed. Ready for kubeadm init / kubeadm join."
