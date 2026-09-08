# Measurement spec

Frozen definitions for Stage 0 and later campaign measurement. TTFS client behavior is **settled** (see below); `uidprobe` implements this section.

## Disruption clocks (TTFS)

| Symbol | Meaning |
| --- | --- |
| T0 | API-server timestamp of replacement-pod **create** (`metadata.creationTimestamp`) |
| T1 | First successful HTTP response whose `X-Pod-Uid` equals that pod's `metadata.uid` |
| TTFS | T1 − T0 |

**Header:** Downward API `fieldRef: metadata.uid` → response header `X-Pod-Uid`. No app-generated UUID.

### Client behavior (frozen)

**Headline client:** short-lived HTTP/1.1 with `Connection: close` (and `Transport.DisableKeepAlives`) on every request.

**Sensitivity cell (not headline):** keep-alive / connection reuse. Record separately when measured.

**Why:** a persistent connection can keep hitting the terminating old pod after relocation because conntrack pins the existing TCP flow regardless of current EndpointSlice membership. That is a client connection-pooling artifact, not something target-node state can influence, so it does not belong in a metric meant to isolate target-node effects.

### Serving paths (both reported)

Do **not** collapse to a single headline number. Log and analyze both:

| Path | Definition |
| --- | --- |
| **ClusterIP TTFS** | T1 via the Service ClusterIP (includes EndpointSlice / kube-proxy join) |
| **Direct pod-IP TTFS** | T1 to the replacement pod's PodIP (bypasses Service/EndpointSlice) |

**Why both:** ClusterIP folds in EndpointSlice propagation lag — a control-plane property, not target-node-conditioned — which adds variance covariates cannot explain. That is not fatal (Q50/Q95 already treats disruption as noisy), but the decomposition lets Discussion state how much of TTFS is target-conditioned pod readiness versus roughly-constant service-path overhead. Direct-IP is not merely an internal sanity check; both paths are first-class reported measurements.

### Stage 0 UID-discrimination claims

Under graceful overlap (old pod still serving during `preStop` / grace period):

1. Responses with the **old** UID continue for a measurable interval after DELETE.
2. First **new**-UID success is strictly after the replacement is Running and after some old-UID successes (under load).
3. Time-to-first-**any**-200 ≪ time-to-first-**new-UID**-200 during overlap (naive clock bias).
4. Header UID matches the serving pod's Downward API UID under concurrency.
5. Optional: interval where replacement is Running but ClusterIP has not yet delivered new-UID (EndpointSlice lag) — belongs in ClusterIP TTFS when present.

## Uncached image bytes

On node *N* for image *I*: sum of compressed layer blob sizes in *I*'s manifest that containerd on *N* would still **fetch**, unless CRI already reports *I* present (then 0 — kubelet will not pull).

This is not `docker inspect` size and not unpacked snapshot size.

### `discard_unpacked_layers`

If containerd discards blobs after unpack, content-store misses can look “uncached” while kubelet will not pull.

**Record the live value on every collection cluster** (see `deploy/local-vm/README.md`):

```text
grep discard_unpacked_layers /etc/containerd/config.toml
```

- If the key is present, use that boolean.
- If **absent**, the effective value is that containerd version's default. Record the version (`containerd --version` on the node) next to “default, key absent.” Do not assume `false`.

**Current local-VM cluster (Multipass cp1 / worker1 / worker2):** `discard_unpacked_layers = false` confirmed on all three nodes. `imageprobe` operates under that assumption (`nodeobs.DiscardUnpackedAssumption`) but always re-reads the live config and records it; a mismatch is a failed check, not a silent reinterpretation.

Interpretation for Stage 0 check 2.5:

| CRI image present | Content-store layer missing | Uncached bytes for disruption |
| --- | --- | --- |
| yes | either | 0 (no pull) |
| no | no | 0 for that layer (shared/cached blob) |
| no | yes | add compressed blob size |

### Layer-sharing ground truth (paper claim)

Plumbing against a single-layer image (e.g. `pause:3.9`) only validates content-store / CRI accounting. The design justification for uncached bytes vs total image size requires shared layers.

Ground-truth images (built by `deploy/registry/build-layer-images-job.yaml`, pushed to the in-cluster `registry:2`):

| Image | Layers |
| --- | --- |
| `reloc/base:v1` | L0 |
| `reloc/app-a:v1` | L0 + L1 |
| `reloc/app-b:v1` | L0 + L2 |

Deterministic file payloads: L0=1MiB, L1=2MiB, L2=3MiB as SHA-256(counter) byte streams (not LCG — those compressed away). **Assertions use compressed OCI/docker layer blob sizes** recorded in ConfigMap `reloc-registry/layer-ground-truth` (`layer_blob_bytes`), not the raw file byte counts. Images are built with `buildah bud --layers` so each `COPY` is a separate layer; L0’s digest must match across `base`, `app-a`, and `app-b`.

Required sequence (`imageprobe -suite layer-share`): cold `uncached(app-a) = L0+L1`; after pull `app-a`, `uncached(app-a)=0` and `uncached(app-b)=L2` (not `L0+L2`); `total(app-b)=L0+L2 ≠ uncached(app-b)`. worker2 remains fully cold for both images (per-node cache). Confirmed on local-VM: see `experiments/results/stage0/layer-sharing-divergence-localvm.md`.

## PSI

Use per-node / per-cgroup PSI, not host-global `/proc/pressure` copied across workers. Cross-node isolation (stress one worker, others flat) is required before treating CPU or memory PSI as a target-node covariate. IO PSI is not closeable on the Multipass host (one physical disk).

## Splits

`split ∈ {stage0, calibration, evaluation}`. Calibration and evaluation replica IDs must never mix. Enforced later in `analysis/tests/test_split_guard.py`.
