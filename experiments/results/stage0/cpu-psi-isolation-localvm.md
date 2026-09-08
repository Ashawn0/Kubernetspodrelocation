# Stage 0 artifact: CPU PSI isolation (local-VM)

> **Automated confirmation.** Full clean `psiprobe` run (`go run ./cmd/stage0/psiprobe`) passes `isolation_cpu` along with all six collectors and the other isolation checks. Same conclusion as the manual runs below; the instrument supersedes the PowerShell/`stress-ng` script for day-to-day reproduction. Manual data is kept as independent supporting evidence.

**Result:** PASS (construct-valid on Multipass kubeadm workers for CPU PSI) — manual and automated.

**Environment:** local-VM path (`deploy/local-vm/`), separate guest kernels. Sampled `/proc/pressure/cpu` (system-wide PSI inside each worker VM).

## Automated (`psiprobe`)

From `experiments/results/stage0/psiprobe.jsonl`, latest full-green run (`isolation_cpu` PASS at 2026-09-07T06:10:26Z):

| | stressed delta (µs) | control delta (µs) |
|---|---|---|
| CPU isolation | 14,697,971 | 46,837 |

Prior automated pass in the same session (2026-09-07T06:04:09Z): stressed delta 15,153,740 µs; control 37,658 µs. Both are the same order of magnitude as the manual mid-run rise and clear isolation against the idle worker.

## Manual (kept)

**Setup:** worker1 stressed with `stress-ng --cpu 8 --timeout 60s --metrics-brief` (8 workers on a 4-vCPU node, deliberately oversubscribed 2:1 to force scheduling contention), worker2 idle as control. Sampled `/proc/pressure/cpu` on both at ~8s, ~33s, and ~10s after the 60s run ended.

| Time | worker1 (stressed) some avg10/avg60/avg300 | worker1 total (µs) | worker2 (idle) some avg10/avg60/avg300 | worker2 total (µs) |
|---|---|---|---|---|
| ~8s | 50.56 / 11.25 / 2.43 | 15,499,927 | 0.00 / 0.00 / 0.00 | 6,655,861 |
| ~33s | 95.24 / 42.00 / 10.66 | 41,525,426 | 0.00 / 0.00 / 0.00 | 6,682,857 |
| ~10s post-stress | 25.51 / 49.62 / 17.46 | 65,800,915 | 0.00 / 0.00 / 0.00 | 6,727,132 |

stress-ng summary: 523,209 bogo ops, 60.02s real time, 238.79s user time across 8 workers, confirming genuine CPU saturation was applied, not a no-op.

## Interpretation

worker1's avg10 rose then fell as the load started and ended, avg60 lagged behind at the post-stress sample, avg300 was still climbing at the post-stress sample, all consistent with each average's own time constant rather than noise. worker2 never moved beyond baseline drift. Automated deltas (~15M µs stressed vs ~40k control) reproduce that isolation. This is a real isolation pass, not a coincidental correlation.

IO PSI remains not closeable on this machine (shared physical disk).
