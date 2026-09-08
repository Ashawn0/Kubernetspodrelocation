# reloc-disrupt

Measurement-and-prediction study of target-conditioned Kubernetes pod-relocation disruption (IEEE/ACM CCGrid 2027, Paper 1). The predictor is an instrument, not the contribution.

**Stage 0 only right now:** validate the measurement pipeline. No LightGBM campaign, no RL, no custom scheduler, no `nodeName` placement except as a Stage 0 negative control.

- Harness (later): Go + client-go
- Analysis (later): Python + LightGBM
- Local closeout cluster: Multipass kubeadm — `deploy/local-vm/`
- Harness iteration: kind — `deploy/kind/`

All four Stage 0 probes are implemented: `schedprobe`, `psiprobe`, `imageprobe`, `uidprobe`. TTFS rules are frozen in `docs/measurement-spec.md`.
