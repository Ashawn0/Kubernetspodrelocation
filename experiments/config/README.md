# Campaign config (example)

`campaign-config.example.json` is **illustrative only**.

- `calibration_n: 1` and `evaluation_n: 1` are fake placeholders so the schema is
  readable and unit tests / dry parses have something to load.
- Real per-cell counts come from the power-analysis pass on the local-VM variance
  pilot (`scripts/power_analysis.py`), not from this file.
- `held_out: true` on a cell forces **calibration_n → 0**: every trial for that
  cell is evaluation-only. The predictor never sees that cell during Fit
  (generalization test). It is not “fewer calibration replicates.”

Usage (after real *n* are filled in a working copy of the config):

```text
go run ./cmd/campaign/trialrunner -mode campaign \
  -campaign-config experiments/config/campaign-config.example.json \
  -seed 42
```

Outputs:

- `experiments/results/calibration/campaign-<YYYYMMDD>.csv`
- `experiments/results/evaluation/campaign-<YYYYMMDD>.csv`

See `docs/campaign-design.md` §9.
