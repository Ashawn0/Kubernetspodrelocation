# Research design (fixed)

Field: distributed systems / cluster resource management (learned-systems pattern). Not an ML paper.

- Measurement-and-prediction study. Single-shot / contextual decision. **No RL.**
- Predictor (later stages): LightGBM quantile regression (Q50/Q95) over log-cost. **Not a neural network.**
- Forced placement: `nodeSelector` / required `nodeAffinity` only. **Never** `spec.nodeName`. **Never** a custom scheduler. **Never** a harness-created Binding. This keeps the real kube-scheduler Filter → Score → Bind path.
- Disruption: time-to-first-success from the **replacement pod**, identified by a pod-UID response header. A terminating pod can still serve 200s.
- Target-node state: uncached image bytes (not total image size); CPU/memory/IO contention via Linux PSI (not raw utilization); registry network shaped independently of pod networking.
- Evaluation (later): regret vs a bias-corrected empirical oracle. Calibration replicates (oracle target selection) stay strictly separate from evaluation replicates (regret). Headline metric: GapCaptured against the strongest existing baseline.

See `docs/non-goals.md` and `docs/measurement-spec.md`.
