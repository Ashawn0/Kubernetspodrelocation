# Stage 0 probes

| Probe | Status | Claim |
| --- | --- | --- |
| `schedprobe` | implemented | 2.2 scheduler binding path |
| `psiprobe` | implemented | 2.4 PSI collect + isolation + fail-loud |
| `imageprobe` | implemented | 2.5 uncached layer bytes (plumbing + layer-share) |
| `uidprobe` | implemented | 2.3 UID-pinned TTFS (`Connection: close`; ClusterIP + pod-IP) |
| `netprobe` | implemented | 2.6 registry ENI shaping vs primary CNI path (AWS) |

```text
go run ./cmd/stage0/schedprobe
go run ./cmd/stage0/psiprobe
go run ./cmd/stage0/psiprobe -skip-io-isolation=false -io-path /mnt/reloc-nvme   # AWS NVMe
go run ./cmd/stage0/imageprobe -suite plumbing
go run ./cmd/stage0/imageprobe -suite layer-share
go run ./cmd/stage0/uidprobe -suite construct
go run ./cmd/stage0/uidprobe -suite decomp-repeat -repeats 8
go run ./cmd/stage0/netprobe   # AWS dual-ENI
```

TTFS definition is frozen in `docs/measurement-spec.md`. Construct suite closes claim 2.3; `decomp-repeat` only checks ClusterIP-vs-pod-IP ordering stability.

AWS bring-up/teardown: `deploy/aws/up.ps1` / `deploy/aws/down.ps1`.

JSONL lands under `experiments/results/stage0/`.
