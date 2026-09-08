# Stage 0 probes

| Probe | Status | Claim |
| --- | --- | --- |
| `schedprobe` | implemented | 2.2 scheduler binding path |
| `psiprobe` | implemented | 2.4 PSI collect + isolation + fail-loud |
| `imageprobe` | implemented | 2.5 uncached layer bytes (plumbing + layer-share) |
| `uidprobe` | implemented | 2.3 UID-pinned TTFS (`Connection: close`; ClusterIP + pod-IP) |

```text
go run ./cmd/stage0/schedprobe
go run ./cmd/stage0/psiprobe
go run ./cmd/stage0/imageprobe -suite plumbing
go run ./cmd/stage0/imageprobe -suite layer-share
go run ./cmd/stage0/uidprobe -suite construct
go run ./cmd/stage0/uidprobe -suite decomp-repeat -repeats 8
```

TTFS definition is frozen in `docs/measurement-spec.md`. Construct suite closes claim 2.3; `decomp-repeat` only checks ClusterIP-vs-pod-IP ordering stability.

JSONL lands under `experiments/results/stage0/`.
