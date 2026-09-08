# Stage 0 validation

Instrumentation only. Success means the measurement pipeline is causally the thing the paper will claim. No predictor, no oracle, no GapCaptured.

Local environments:

- **kind** — shared kernel (Docker/WSL VM). Plumbing rehearsal. Weak or invalid for node-local PSI and registry isolation.
- **local-VM** — Multipass kubeadm (`deploy/local-vm/`), separate guest kernels, shared **host** disk and NIC.
- **lab / university kubeadm** — required for IO PSI and registry-vs-pod-network independence.

## Transfer table

Closeable here means Stage 0 for that instrument can be **finished** on that environment.

| Instrument | kind plumbing | kind construct | local-VM (Multipass kubeadm) | lab / university kubeadm |
| --- | --- | --- | --- | --- |
| Scheduler path (`nodeSelector` bind) | yes | yes | **confirmed pass**, see [experiments/results/stage0/scheduler-path-localvm.md](../experiments/results/stage0/scheduler-path-localvm.md) | smoke only |
| UID-pinned TTFS | yes | yes if overlap proven | **confirmed pass**, see [uid-pinned-ttfs-localvm.md](../experiments/results/stage0/uid-pinned-ttfs-localvm.md) (`Connection: close`; ClusterIP + pod-IP both reported) | smoke only |
| Uncached image bytes | yes | yes (per-node containerd) | **confirmed pass (layer-sharing divergence validated)**, see [layer-sharing-divergence-localvm.md](../experiments/results/stage0/layer-sharing-divergence-localvm.md); plumbing-only pause run is separate (cold/warm CRI accounting, not sharing) | confirm same containerd GC setting |
| CPU PSI | plumbing | often not (shared kernel) | **confirmed pass (automated, psiprobe)**; manual evidence [cpu-psi-isolation-localvm.md](../experiments/results/stage0/cpu-psi-isolation-localvm.md) | required if local-VM isolation fails |
| Memory PSI | plumbing | often not | **confirmed pass (automated, psiprobe)**; manual evidence [memory-psi-isolation-localvm.md](../experiments/results/stage0/memory-psi-isolation-localvm.md) (shallow; automated delta weaker than manual — see artifact) | required if local-VM isolation fails |
| IO PSI | plumbing | no (shared disk) | **not closeable on this machine** (one physical disk) | **required** |
| Registry vs pod-network independence | maybe | weak | **not closeable on this machine** (one NIC) | **required** (or cloud fallback) |

UID TTFS is **confirmed** on the local-VM topology (artifact above). Re-run: `go run ./cmd/stage0/uidprobe` (or `make uidprobe`) against a Multipass kubeconfig.

## Claims (summary)

### 2.2 Scheduler binding path

Positive: `nodeSelector` / required `nodeAffinity` → `Scheduled` from `default-scheduler` and POST `pods/binding`. Negative: `nodeName` has no scheduler bind. Impossible selector stays `Pending`.

### 2.3 UID-pinned TTFS

Frozen in `docs/measurement-spec.md`: headline client is `Connection: close`; keep-alive is sensitivity-only. Report both ClusterIP TTFS and direct pod-IP TTFS (decomposition for Discussion).

`uidprobe` checks: overlap (old UID after DELETE), pinning (old before first new), naive any-200 bias, both path T1s, Downward API header integrity.

### 2.4 PSI

Isolation experiment: stress one worker, sample all. CPU/memory: confirmed pass on local-VM (see Stage 0 artifacts). IO: skip construct-validity on this machine.

`psiprobe` requirements baked in from the manual runs:

- CPU stress **oversubscribes** (2× `nproc`); 1:1 capacity match is a null result.
- Memory stress sizes past kernel **MemAvailable**, not a %% of total.
- If isolation fails for a resource, the probe **refuses to emit** that resource as a target-node feature (`emit=false`, reason set) rather than recording a leaked value.

### 2.5 Uncached bytes

Ground-truth layered images via in-cluster registry; per-node containerd; sharing ≠ total size.

- **Plumbing** (`imageprobe -suite plumbing`): `discard_unpacked_layers` + warm/CRI accounting (e.g. pause). Does **not** close the layer-sharing claim.
- **Layer-share** (`imageprobe -suite layer-share`): **confirmed pass** on local-VM — see [layer-sharing-divergence-localvm.md](../experiments/results/stage0/layer-sharing-divergence-localvm.md). After `app-a` warm, `uncached(app-b)=L2` only; `total(app-b)≠uncached(app-b)`.

### 2.6 Registry vs pod network

Not closeable on this workstation. Defer to lab kubeadm or cloud.

## Post-provision verification (local-VM)

Operator checklist lives next to the scripts: [`deploy/local-vm/README.md`](../deploy/local-vm/README.md).

- `SystemdCgroup = true` on every node after `k8s-node-common.sh`
- `discard_unpacked_layers` present or “absent → containerd default,” copied into `docs/measurement-spec.md`

## Stage 0 exit

JSONL (or equivalent) under `experiments/results/stage0/` for each closeable claim, plus this transfer table updated if a cell changes.

Probes: `schedprobe`, `psiprobe`, `imageprobe`, `uidprobe` under `cmd/stage0/`. TTFS client/serving-path rules are frozen in `docs/measurement-spec.md`; run `go run ./cmd/stage0/uidprobe` (or `make uidprobe`) to confirm claim 2.3.
