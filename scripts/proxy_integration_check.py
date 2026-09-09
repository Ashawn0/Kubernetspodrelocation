#!/usr/bin/env python3
"""PROXY INTEGRATION CHECK — not a campaign GapCaptured result.

Runs the finished 18-trial smoke pilot CSV through:

  Go evalexport (baseline + Smith–Winkler oracle JSON)
  → fit LightGBM on artificial calibration replicates
  → relocdisrupt.regret GapCaptured

Every output path and JSON field is labeled PROXY so this can never be mistaken
for a real campaign metric. See docs/campaign-design.md §10.

Does NOT read experiments/results/pilot/ live files.
"""

from __future__ import annotations

import argparse
import csv
import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

# Allow running without editable install when cwd is repo root / analysis.
_REPO = Path(__file__).resolve().parents[1]
_ANALYSIS_SRC = _REPO / "analysis" / "src"
if str(_ANALYSIS_SRC) not in sys.path:
    sys.path.insert(0, str(_ANALYSIS_SRC))

from relocdisrupt.lgbm import Features, LightGBMQuantilePredictor, Trial  # noqa: E402
from relocdisrupt.regret import evaluate_export  # noqa: E402

PROXY_NOTE = (
    "PROXY INTEGRATION CHECK: real pilot trial costs with an artificial "
    "cal/eval split (replicates 1-2 calibration, 3 evaluation per cell). "
    "Not a methodological campaign result; GapCaptured here is plumbing "
    "evidence only - not model performance."
)


def load_cal_trials_for_lgbm(csv_path: Path, cal_reps: set[int]) -> list[Trial]:
    """Load calibration rows for LightGBM Fit (same artificial split as Go)."""
    trials: list[Trial] = []
    with csv_path.open(newline="", encoding="utf-8") as f:
        for row in csv.DictReader(f):
            if row.get("pass", "true").lower() != "true":
                continue
            rep = int(row["replicate"])
            if rep not in cal_reps:
                continue
            cache = row["image_cache_state"].lower()
            cpu = row["cpu_psi_level"].lower()
            cell = f"{cache}_{cpu}"
            unc = int(float(row.get("uncached_bytes") or 0))
            psi = float(row.get("psi_cpu_avg10") or 0.0)
            present = 500_000_000 if (cache == "warm" or unc == 0) else 0
            trials.append(
                Trial(
                    id=row["trial_id"],
                    cell_id=cell,
                    replicate=rep,
                    split="calibration",
                    cost=float(row["ttfs_clusterip_sec"]),
                    features=Features(
                        uncached_bytes=unc,
                        cpu_psi_avg10=psi,
                        present_image_bytes=present,
                    ),
                )
            )
    if not trials:
        raise SystemExit(f"no calibration trials in {csv_path}")
    return trials


def cell_features_from_csv(csv_path: Path, cal_reps: set[int]) -> dict[str, Features]:
    """Per-cell mean covariates from calibration rows (same units as Go CSV load)."""
    sums: dict[str, list[float]] = {}  # cell -> [unc, psi, present, n]
    with csv_path.open(newline="", encoding="utf-8") as f:
        for row in csv.DictReader(f):
            if row.get("pass", "true").lower() != "true":
                continue
            if int(row["replicate"]) not in cal_reps:
                continue
            cache = row["image_cache_state"].lower()
            cpu = row["cpu_psi_level"].lower()
            cid = f"{cache}_{cpu}"
            unc = float(row.get("uncached_bytes") or 0)
            psi = float(row.get("psi_cpu_avg10") or 0.0)
            present = 500_000_000.0 if (cache == "warm" or unc == 0) else 0.0
            if cid not in sums:
                sums[cid] = [0.0, 0.0, 0.0, 0.0]
            sums[cid][0] += unc
            sums[cid][1] += psi
            sums[cid][2] += present
            sums[cid][3] += 1.0
    out: dict[str, Features] = {}
    for cid, (unc, psi, present, n) in sums.items():
        out[cid] = Features(
            uncached_bytes=int(unc / n),
            cpu_psi_avg10=psi / n,
            present_image_bytes=int(present / n),
        )
    return out


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--csv",
        type=Path,
        default=_REPO / "analysis" / "testdata" / "pilot-variance-18trial-smoke.csv",
        help="finished 18-trial pilot CSV (not the live 90-trial file)",
    )
    ap.add_argument(
        "--out-dir",
        type=Path,
        default=_REPO / "experiments" / "results" / "integration-check",
    )
    ap.add_argument("--skip-go", action="store_true", help="reuse existing proxy export JSON")
    ap.add_argument("--export", type=Path, default=None, help="proxy export path when --skip-go")
    args = ap.parse_args()

    day = datetime.now(timezone.utc).strftime("%Y%m%d")
    args.out_dir.mkdir(parents=True, exist_ok=True)

    if args.skip_go:
        if not args.export:
            raise SystemExit("--export required with --skip-go")
        export_path = args.export
    else:
        # Go: CSV → baselines + SW oracle → proxy export JSON
        cmd = [
            "go",
            "run",
            "./cmd/analysis/proxycheck",
            "-csv",
            str(args.csv),
            "-out-dir",
            str(args.out_dir),
        ]
        print("PROXY: running", " ".join(cmd), flush=True)
        subprocess.check_call(cmd, cwd=str(_REPO))
        export_path = args.out_dir / f"proxy-oracle-baseline-export-{day}.json"

    if not export_path.is_file():
        raise SystemExit(f"missing proxy export: {export_path}")

    with export_path.open(encoding="utf-8") as f:
        export = json.load(f)
    if not export.get("proxy_integration_check"):
        raise SystemExit(
            "refusing: export lacks proxy_integration_check=true "
            "(would risk treating a non-proxy file as this check)"
        )

    cal = load_cal_trials_for_lgbm(args.csv, {1, 2})
    # Small-n pilot: keep leaves tiny so Fit succeeds on 12 cal rows.
    model = LightGBMQuantilePredictor(
        n_estimators=40,
        num_leaves=8,
        min_data_in_leaf=2,
        random_state=42,
    )
    model.fit(cal)
    feats = cell_features_from_csv(args.csv, {1, 2})
    missing = [c["cell_id"] for c in export["cells"] if c["cell_id"] not in feats]
    if missing:
        raise SystemExit(f"feature cells missing from CSV cal rows: {missing}")
    learned = {cid: model.predict(feats[cid]) for cid in feats}

    # Learned name in regret defaults to "lightgbm"; export baselines use Go names.
    report = evaluate_export(export_path, learned, learned_name="lightgbm")

    out = {
        "proxy_integration_check": True,
        "proxy_note": PROXY_NOTE,
        "date_utc": day,
        "source_csv": str(args.csv.resolve()),
        "export_json": str(export_path.resolve()),
        "pipeline": [
            "evalexport.LoadTrialsCSV",
            "evalexport.Build (cal reps 1-2, eval rep 3)",
            "LightGBMQuantilePredictor.fit(calibration)",
            "relocdisrupt.regret.evaluate_export",
        ],
        "oracle_selected_cell": export["oracle_selected_cell"],
        "oracle_corrected_cost": export["oracle_corrected_cost"],
        "best_baseline": report.best_baseline,
        "best_baseline_cal_regrets": report.best_baseline_cal_regrets,
        "eval_regrets": report.regrets,
        "chosen_cells": report.chosen_cells,
        "learned_predictions": learned,
        "gap_captured_PROXY_ONLY": report.gap_captured,
        "interpretation": (
            "gap_captured_PROXY_ONLY is evidence the chain completed without error. "
            "It is NOT a preliminary estimate of real model GapCaptured."
        ),
    }
    out_path = args.out_dir / f"proxy-gapcaptured-{day}.json"
    out_path.write_text(json.dumps(out, indent=2) + "\n", encoding="utf-8")
    print("PROXY INTEGRATION CHECK complete (NOT a campaign result).")
    print(f"  export: {export_path}")
    print(f"  result: {out_path}")
    print(
        f"  gap_captured_PROXY_ONLY={report.gap_captured!r} "
        f"(plumbing evidence only - ignore as performance)"
    )


if __name__ == "__main__":
    main()
