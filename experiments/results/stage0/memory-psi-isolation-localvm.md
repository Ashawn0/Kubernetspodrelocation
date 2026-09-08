# Stage 0 artifact: Memory PSI isolation (local-VM)

> **Automated confirmation.** Full clean `psiprobe` run (`go run ./cmd/stage0/psiprobe`) passes `isolation_memory` (host-nsenter stress sized past live `MemAvailable`) along with all six collectors and the other isolation checks. Manual data below is kept as independent supporting evidence. **Read the magnitude caveat** under Automated before treating this covariate as strong.

**Result:** PASS (construct-valid isolation on Multipass kubeadm workers for memory PSI) — manual and automated.

**Caveat (read this first):** unlike the CPU isolation result, this is a **shallow signal**. Stall on worker1 accumulated almost entirely in the first ~8 seconds of each manual run, then went flat for the remainder of the load and stayed flat after stress ended. That shape matches fast page-cache eviction (`buff/cache` dropped sharply within those first seconds), not sustained reclaim contention of the kind that made CPU PSI's response curve informative. Escalating manual run 2's demand by roughly 300MB over run 1 only moved peak `some` total from 12,030 µs to 16,057 µs — a small increase for a real jump past "available," consistent with cache eviction being fast regardless of how far past available you push. Isolation (worker1 reacts, worker2 does not) is solid; **magnitude is not comparable to CPU PSI**, and a third escalation was not run because it would mostly trade OOM risk for no new information.

**Environment:** local-VM path (`deploy/local-vm/`), separate guest kernels. Sampled memory PSI on both workers. Node under test: ~3.8Gi RAM, 0 swap.

## Automated (`psiprobe`)

From `experiments/results/stage0/psiprobe.jsonl`, latest full-green run (`isolation_memory` PASS at 2026-09-07T06:10:38Z):

| | value |
|---|---|
| stressed delta (`some` total, µs) | **3,113** |
| control delta (µs) | **0** |
| vm-bytes | 1862M x2 (combined ~3724Mi, MemAvailable=3290388KiB + margin 512Mi) |
| stress mode | host-nsenter |
| sample offset | ~5.0 s after stress Running |
| `min` threshold | 1000 (unchanged) |

**Magnitude vs manual:** the automated delta (3,113 µs) clears `min 1000`, so this is a real PASS under the stated gate — not a null. It is **not** in the same rough range as the manual peaks (12,030 and 16,057 µs). It sits much closer to the threshold than to those manual figures (~4× below the lower manual peak). Record that plainly: isolation is confirmed and reproducible via `psiprobe`, but the automated magnitude is weaker than the hand runs. Do not overweight this covariate later as if it matched the manual stall scale; the shallow-signal caveat applies even more to the automated number.

## Manual (kept)

**Setup:** worker1 loaded with `stress-ng --vm 2 --vm-bytes <size> --vm-keep`, worker2 idle as control. Two runs, escalating size, both against a 3.8Gi/0-swap node.

| Run | vm-bytes (combined) | Baseline available | Peak worker1 total (some, µs) | worker2 total throughout |
|---|---|---|---|---|
| 1 | 1700M x2 (~3.32Gi) | ~3.1Gi | 12,030 | 0 |
| 2 | 1800M x2 (~3.6Gi) | ~3.2Gi | 16,057 | 0 |

Both runs: worker1's stall accumulated almost entirely in the first ~8 seconds, then went flat for the remainder of the run and stayed flat after stress ended. worker2 recorded exactly 0 in every sample, both runs. `MemoryPressure` condition on worker1 stayed `False` throughout both runs, confirmed via `kubectl describe node`.

## Interpretation

Isolation claim: **pass** (manual and automated). worker2 stays flat; stressed node moves. Kubernetes node `MemoryPressure` stayed `False` in the manual runs, so the PSI movement is not an artifact of the kubelet pressure condition flipping.

Do not treat absolute stall totals, or PASS alone, as a calibrated demand→pressure curve. For feature design later: memory PSI on this path can discriminate "this node is under memory load" from "that node is idle," but it should not be expected to look like CPU PSI's sustained response, and the automated 3,113 µs delta should not be narrated as matching the manual 12k–16k peaks.

IO PSI remains not closeable on this machine (shared physical disk). See `cpu-psi-isolation-localvm.md` for the CPU result.
