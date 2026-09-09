# Go harness libraries

| Package | Role |
| --- | --- |
| `k8s` | client-go helpers, host-exec via privileged nsenter pod |
| `place` | `nodeSelector` / required `nodeAffinity` builders; `nodeName` only as Stage 0 negative control |
| `nodeobs` | PSI collect/isolation gates; uncached image-byte accounting |
| `logevent` | Stage 0 JSONL writer (`split=stage0`) |
| `baseline` | Offline predictors for regret: `fixed-cost`, `ImageLocality` (shared `Predictor` interface for LightGBM later) |
| `oracle` | Calibration/evaluation replicate partition + empirical oracle scaffold (bias-correction formula still open) |
