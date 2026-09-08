# AWS kubeadm (Stage 0 / campaign closeout path)

1 control-plane (`m6i.large`) + 2 workers (`c6id.xlarge` with local NVMe). Dual ENI per worker: primary for CNI/pod traffic, secondary in a dedicated registry subnet for pull-path shaping. This is the environment that can close **IO PSI** and **registry-vs-pod-network independence** (not closeable on Multipass).

## Prerequisites

- AWS CLI configured as `reloc-disrupt-admin` (`aws configure` already done)
- Region **ap-northeast-2**
- EC2 key pair **`reloc-disrupt-key`** already created; private key on this machine (default `~\.ssh\reloc-disrupt-key.pem`)
- Terraform ≥ 1.5
- PowerShell
- **Operator public IP** for SG allowlist (SSH 22 + kube-apiserver 6443). `up.ps1` auto-detects via checkip.amazonaws.com and writes gitignored `terraform.tfvars`. Terraform **rejects** `0.0.0.0/0`.

**Budget:** ~$100 credit; treat a manual **$85** alert as the stop signal. Prefer `.\down.ps1` over leaving instances up.

## One-command lifecycle

```powershell
cd deploy\aws
.\up.ps1          # terraform apply + node prep + kubeadm bootstrap
.\down.ps1        # terraform destroy -auto-approve  (STOP COST HERE)
```

Optional: `.\down.ps1 -StopOnly` stops instances without destroy (keeps disks/ENIs billed; prefer full destroy).

## Layout

```text
deploy/aws/
├── README.md
├── up.ps1 / down.ps1
├── config/paths.env          # mount + iface names probes should use
├── terraform/                # VPC, subnets, CP, workers, secondary ENIs
└── scripts/
    ├── k8s-node-common.sh    # copy of local-vm script (Ubuntu kubeadm prereqs)
    ├── aws-node-prep.sh      # NVMe mount + secondary ENI (AWS-only)
    ├── bootstrap-cluster.sh  # kubeadm init/join + Calico
    ├── shape-registry-eni.sh # tc on secondary iface only
    └── fetch-kubeconfig.ps1
```

## Networking

| Path | Interface | Subnet role |
| --- | --- | --- |
| CNI / pod / ClusterIP | primary ENI (`ens5` on Ubuntu Noble Nitro) | `subnet-primary-*` |
| Registry pull shaping | secondary ENI (`ens6`) | `subnet-registry-*` |

Security group: **SSH :22 and kube-apiserver :6443 accept only `operator_cidr`** (your `/32`). Intra-VPC traffic is open within the VPC CIDR. Egress is unrestricted (package/AMI pulls).

`tc` qdisc is applied **only** to the secondary iface (`shape-registry-eni.sh`). Verify with:

```text
go run ./cmd/stage0/netprobe
```

### What `netprobe` asserts (design defaults — **not yet run on a live AWS cluster**)

**Shape direction / magnitude (registry path):** downward only — `tc tbf` rate cap (default **20 Mbit/s**) + `netem` delay (default **100 ms**) on the secondary iface. Not an upward saturation / contention load. That matters because `c6id.xlarge` has **one** physical network card; primary+secondary ENIs share it (see `docs/implementation-log.md` §14).

| Check | Metric | Pass rule (defaults) | Grade |
| --- | --- | --- | --- |
| Registry degraded (RTT) | ICMP avg RTT to peer secondary IP | RTT > baseline + 0.5 × `shape-delay-ms` | operational sanity |
| Registry degraded (BW) | iperf3 Mbps to peer secondary IP | Mbps < 0.6 × baseline **or** near `shape-rate-mbit` | operational sanity |
| Primary stable (RTT) | ICMP avg RTT to peer primary IP | RTT < baseline × 1.25 + 5 ms | operational sanity — **too loose alone for paper independence** |
| Primary stable (BW) | iperf3 Mbps to peer primary IP | Mbps > 0.7 × baseline | same caveat (allows 30% drop) |
| Primary unshaped | `tc qdisc show` on primary iface | no `netem` / `tbf` | structural |
| **Raw deltas** | signed Δ RTT (ms, %) and Δ BW (Mbit/s, %) for both paths | **always logged** (`shaping_raw_deltas`), pass or fail | paper record |

Packet loss is **not** asserted. Do not treat boolean PASS as settled independence until live deltas exist and Discussion uses the logical-isolation framing above.

### NVMe identification (IO PSI path)

`aws-node-prep.sh` does **not** hardcode `/dev/nvme1n1`. It resolves instance-store via:

1. `/dev/disk/by-id/*Instance_Storage*` (Amazon symlink), else
2. `nvme id-ctrl` model matching Instance Storage,

then refuses anything that is the root mount source or whose model is `Elastic Block Store`.

### `rp_filter`

Set to **loose (=2) on the secondary iface only** (`net.ipv4.conf.<registry-iface>.rp_filter=2`). Not disabled (=0), not applied via `all`/`default`.

### Deviations from `k8s-node-common.sh`

`k8s-node-common.sh` is reused as-is for kubeadm/containerd (held at K8s 1.30). AWS-specific work is **not** folded into that script:

1. **Secondary ENI** — Terraform attaches the ENI; `aws-node-prep.sh` brings it up via netplan DHCP and sets per-iface loose `rp_filter`. ENA itself needs no custom driver on Ubuntu 24.04.
2. **Source/dest check** — disabled on secondary ENIs in Terraform (required for multi-homed nodes).
3. **AMI** — Ubuntu **24.04** Noble (not Multipass 22.04). Re-check `SystemdCgroup = true` after first bring-up.

## Storage (IO PSI)

Workers format and mount the **instance-store NVMe** (not root EBS) at:

```text
/mnt/reloc-nvme
```

Declared in `config/paths.env` as `RELOC_IO_STRESS_PATH`. `psiprobe` IO isolation must stress that path:

```text
go run ./cmd/stage0/psiprobe -skip-io-isolation=false -io-path /mnt/reloc-nvme
```

## Post-`up.ps1` checklist

```powershell
# nodes Ready
kubectl --kubeconfig .\.kube\aws.conf get nodes -o wide

# cgroup driver
# (SSH or SSM) grep SystemdCgroup /etc/containerd/config.toml

# NVMe mount present on workers
# findmnt /mnt/reloc-nvme

# secondary iface up
# ip -br link show ens6
```

## Cost note

`c6id.xlarge` ×2 + `m6i.large` in Seoul is the bulk of spend. Run Stage 0 instruments, then **`.\down.ps1` immediately**. Do not leave overnight.
