# Analysis (Python)

- Package: `analysis/src/relocdisrupt/`
- Pilot replicate sizing (stdlib): `scripts/power_analysis.py`
- Predictor scaffold: `relocdisrupt.lgbm.LightGBMQuantilePredictor` (Q50/Q95 on log-cost), matching the Go `internal/baseline.Predictor` surface
- GapCaptured: `relocdisrupt.regret` loads Go `oracle-baseline-export.json` + LightGBM per-cell preds

```text
cd analysis
pip install -e ".[dev,lgbm]"
pytest
```

```text
python scripts/power_analysis.py experiments/results/pilot/pilot-variance-YYYYMMDD.csv --delta <seconds>
```
