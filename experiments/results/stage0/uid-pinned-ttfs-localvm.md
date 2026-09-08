# Stage 0 artifact: UID-pinned TTFS (local-VM)

**Result:** PASS (construct-valid on Multipass kubeadm for claim 2.3).

**Environment:** local-VM path (`deploy/local-vm/`), Multipass kubeadm (cp1 + worker1 + worker2).

**Frozen rules** (`docs/measurement-spec.md`): headline client is HTTP/1.1 `Connection: close` (no keep-alive). Keep-alive is sensitivity-only. Report **both** ClusterIP TTFS and direct pod-IP TTFS. T0 = API `creationTimestamp` of the replacement pod; T1 = first 200 whose `X-Pod-Uid` equals that pod’s Downward API `metadata.uid`.

**Setup:** `cmd/stage0/uidprobe` — Service + old/new pods with `preStop` sleep for graceful overlap; in-cluster `Connection: close` load via HostExec+Python on a worker (ClusterIP/pod IP not reachable from the Windows host).

| Check | Result |
|---|---|
| baseline_clusterip_old_uid | PASS |
| clusterip_load | PASS (~136k samples / 45s) |
| overlap_old_uid_after_delete | PASS (8688 old-UID 200s after DELETE) |
| clusterip_first_new_uid | PASS |
| pinning_old_before_first_new | PASS (8683 old hits before first new) |
| naive_any200_bias | PASS |
| podip_first_new_uid | PASS |
| ttfs_decomposition_reported | PASS |
| header_uids_are_downward_api | PASS |
| replacement_uid_distinct | PASS |

## Headline numbers (construct run)

Client wall clock is noisy (HostExec sampler start); prefer **vs API T0** for the clocks below.

| Path | TTFS vs API T0 | TTFS vs client wall |
|---|---|---|
| ClusterIP (`Connection: close`) | **2134 ms** | 8228 ms |
| Direct pod-IP (`Connection: close`) | **2446 ms** | 8540 ms |

Naive first-any-200 after client T0 was the **old** UID at ~5931 ms; UID-pinned ClusterIP T1 was ~8228 ms — naive clock understates disruption during overlap.

**Decomposition magnitudes from this single construct trial are illustrative only** — see [Decomposition repeat check](#decomposition-repeat-check) below. Do not treat ClusterIP 2134 ms vs pod-IP 2446 ms as a stable ordering for the paper.

JSONL: `experiments/results/stage0/uidprobe.jsonl` (pass run only).

## Interpretation

- **Overlap PASS:** old UID continues to answer on ClusterIP for a measurable window after DELETE while `preStop`/grace keeps the pod serving — without UID pinning, T1 would credit the wrong generation.
- **Pinning / naive bias PASS:** thousands of old-UID successes precede the first new-UID success; first any-200 is the old UID. The measurement construct is not “first HTTP 200.”
- **Both paths PASS:** ClusterIP and pod-IP each deliver a new-UID T1 under `Connection: close`. Analysis must report the decomposition, not only the Service number (EndpointSlice/kube-proxy lag is control-plane, not target-node-conditioned).
- **Header integrity PASS:** every 200 under load carried either the old or new Downward API UID — no app-generated UUID.

Claim 2.3 (UID-pinning construct) is closed on the local-VM environment. Lab/university kubeadm remains smoke-only for this instrument.

## Decomposition repeat check

**Question:** the construct trial’s pod-IP TTFS (2446 ms) > ClusterIP TTFS (2134 ms) is mechanistically backwards (EndpointSlice join can only finish after the pod is individually reachable). Was that stable, or single-trial noise?

**Method:** `uidprobe -suite decomp-repeat -repeats 8` — fair dual poller in one HostExec, equal worker count per path, both clocks start together once replacement PodIP exists (removes construct-suite sampler-start asymmetry). JSONL: `experiments/results/stage0/uidprobe-decomp-repeat.jsonl`.

| Trial | ClusterIP TTFS (ms, vs API T0) | Pod-IP TTFS (ms) | Δ cluster−pod (ms) |
|---|---:|---:|---:|
| 1 | 2939 | 2942 | −2 (tie) |
| 2 | 2958 | 2961 | −3 (tie) |
| 3 | 2973 | 2976 | −3 (tie) |
| 4 | 2979 | 2982 | −3 (tie) |
| 5 | 3483 | 3008 | **+475** (ClusterIP slower) |
| 6 | 3005 | 3008 | −2 (tie) |
| 7 | 3497 | 3029 | **+468** (ClusterIP slower) |
| 8 | 3509 | 3044 | **+465** (ClusterIP slower) |

**Counts** (|Δ| ≤ 5 ms = tie): ClusterIP slower = 3, pod-IP slower = 0, near-tie = 5.

**Verdict:** noise / no stable backwards finding. When the gap is clear, it leans the **expected** way (ClusterIP ≥ pod-IP). The construct-run ordering was not reproducible under fair sampling and must not be written into the paper as a decomposition magnitude. Pinning/overlap evidence above is unchanged.
