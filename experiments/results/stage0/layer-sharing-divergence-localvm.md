# Stage 0 artifact: layer-sharing divergence (local-VM)

**Result:** PASS — confirms the measurement-spec design justification for uncached bytes vs total image size (shared layers register as already present; they are not double-counted).

**Not the same as:** the earlier `pause:3.9` plumbing run (`imageprobe -suite plumbing`), which only validated cold/warm CRI-present accounting on a single-layer image. That run does **not** exercise layer sharing. This artifact is the paper-relevant claim.

**Environment:** local-VM Multipass kubeadm; in-cluster `registry:2` (`deploy/registry/`); `go run ./cmd/stage0/imageprobe -suite layer-share`.

**Instrument:** per-layer presence via `ctr -n k8s.io content ls -q` exact digest membership against the registry manifest (real pull path with `ctr images pull --plain-http`).

## Ground-truth ledger

From ConfigMap `reloc-registry/layer-ground-truth` (build Job after `--layers` + SHA-256-counter payloads). File payloads: L0=1MiB, L1=2MiB, L2=3MiB. Assertions use **compressed** blob sizes:

| Layer | Compressed blob size (bytes) | Digest |
|---|---|---|
| L0 | 1,049,277 | `sha256:98b7428b063f1210c02c86d90786b3395a694ce08705c554ae2d92dba0e06055` |
| L1 | 2,097,943 | `sha256:cbe6ac0104cbfe7b2dbcf04f14a2d37769751257041612e1ff6d9499bd7f4d4c` |
| L2 | 3,146,611 | `sha256:12f15f1eab601281c2c8d2cbc1f4eec285078a44179262f8f115dc841cf9326f` |

| Image | Layers | Total layer bytes |
|---|---|---|
| `reloc/base:v1` | L0 | 1,049,277 |
| `reloc/app-a:v1` | L0 + L1 | 3,147,220 |
| `reloc/app-b:v1` | L0 + L2 | 4,195,888 |

Build Job verification: L0 digest identical across `base`, `app-a`, and `app-b`.

## Sequence and results

```
PASS layer_share_worker1_cleared
PASS layer_share_worker1_cold_app_a
PASS layer_share_worker1_pull_app_a
PASS layer_share_worker1_warm_app_a_zero
PASS layer_share_worker1_app_b_shared_L0
PASS layer_share_worker1_total_ne_uncached_app_b
PASS layer_share_worker2_cleared
PASS layer_share_worker2_cold_app_a
PASS layer_share_worker2_cold_app_b
```

| Check | Observed | Expected |
|---|---|---|
| worker1 cold `uncached(app-a)` | 3,147,220 | L0+L1 = 3,147,220 |
| worker1 after pull `uncached(app-a)` | 0 | 0 (CRI present / layers cached) |
| worker1 `uncached(app-b)` after `app-a` warm | **3,146,611** | **L2 only** (not L0+L2) |
| worker1 `total(app-b)` vs `uncached(app-b)` | 4,195,888 ≠ 3,146,611 | divergence |
| Per-digest on that check | L0 `cached=1`, L2 `cached=0` | share L0, fetch L2 |
| worker2 cold `uncached(app-a)` | 3,147,220 | full cold (per-node cache) |
| worker2 cold `uncached(app-b)` | 4,195,888 | full cold |

## Interpretation

This is the claim in `docs/measurement-spec.md`: after a shared base layer is already on the node, uncached bytes for a second image that reuses that layer equal the **missing** unique layers only. Total image size still sums all layers. Those two numbers diverge exactly when layer sharing matters for relocation cost — which is why the study measures uncached bytes rather than total image size.

Plumbing (`pause:3.9`) remains separate evidence that the content-store / CRI accounting path works. It does not substitute for this result.
