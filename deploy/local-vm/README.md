# Local Multipass kubeadm (Stage 0)

1 control-plane (`cp1`) + 2 workers. Separate guest kernels, unlike kind. Host disk and NIC are still shared, so IO PSI and registry-vs-pod-network independence are **not** closeable here.

## Bring-up

Requires Multipass and Hyper-V. From this directory:

```powershell
.\provision-cluster.ps1
```

`k8s-node-common.sh` must sit next to the script (the provisioner transfers that filename).

Pod CIDR is `10.244.0.0/16` on both `kubeadm init --pod-network-cidr` and the Calico IPPool (manifest rewritten at apply). That range is outside Hyper-V Default Switch (`192.168.0.0/16` or `172.17.0.0`–`172.31.255.255`), which can change across reboots.

## Post-provision checklist

Do not skip these. Failures here look like later kubelet or image-accounting bugs.

### After `k8s-node-common.sh` (each node)

```bash
grep SystemdCgroup /etc/containerd/config.toml
```

Must show `SystemdCgroup = true`. If the `sed` no-op'd on a different containerd default template, the mismatch shows up later as kubelet cgroup-driver errors, not as an obvious install failure.

Via Multipass:

```powershell
foreach ($n in @("cp1","worker1","worker2")) {
  Write-Host "=== $n ==="
  multipass exec $n -- grep SystemdCgroup /etc/containerd/config.toml
}
```

### After the cluster is up (each node)

```bash
grep discard_unpacked_layers /etc/containerd/config.toml
```

- If the key is present, record the value.
- If it is **absent**, record that the effective value is this containerd version's default (do not assume `false`).

Copy that result into `docs/measurement-spec.md` (uncached-bytes interpretation). Content-store misses are not the same as “kubelet will pull” when unpacked layers may have been discarded.

```powershell
foreach ($n in @("cp1","worker1","worker2")) {
  Write-Host "=== $n ==="
  multipass exec $n -- grep discard_unpacked_layers /etc/containerd/config.toml
}
```

### Smoke

```powershell
multipass exec cp1 -- kubectl get nodes -o wide
```

Wait until workers are `Ready` (~30–60s after Calico apply).

## What this cluster can close

See the transfer table in `docs/stage0-validation.md`. Scheduler path, UID-pinned TTFS, and uncached image bytes are fully closeable here. CPU/memory PSI are testable pending a stress-ng isolation run. IO PSI and registry independence are not closeable on this machine.
