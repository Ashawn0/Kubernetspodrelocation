"""End-to-end GapCaptured tests on synthetic export JSON (no live cluster).

The fixture path is the only thing that must change to point at a real
``experiments/results/{run}/oracle-baseline-export.json`` later.
"""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from relocdisrupt.regret import (
    calibration_regret,
    choose_cell,
    compute_gap_captured,
    evaluate_export,
    load_export,
    merge_learned_predictions,
    regret,
    select_best_baseline,
)

# Default synthetic fixture shipped with the package tests. Swap this Path
# (or pass another path into evaluate_export) for a real campaign export.
SYNTHETIC_EXPORT = (
    Path(__file__).resolve().parent.parent / "testdata" / "oracle-baseline-export.synth.json"
)


@pytest.fixture
def synth_export_path(tmp_path: Path) -> Path:
    """Known numbers → known GapCaptured = 0.75 (cal and eval agree on best baseline).

    Cells A/B/C; oracle_corrected = 2.0
    fixed-cost → A; ImageLocality → B; lightgbm → C
    cal regrets: FC 9-2=7, IL 5.5-2=3.5 → best baseline = ImageLocality (cal)
    eval regrets: FC 8, IL 4, LGBM 1 → GapCaptured = 1 - 1/4 = 0.75
    """
    payload = {
        "schema_version": 1,
        "oracle_selected_cell": "cell_oracle",
        "oracle_raw_cal_cost": 1.8,
        "oracle_corrected_cost": 2.0,
        "oracle_eval_cost": 2.1,
        "oracle_eval_n": 4,
        "oracle_shrinkage_alpha": 0.5,
        "oracle_prior_mean": 5.0,
        "cells": [
            {
                "cell_id": "cell_a",
                "cal_mean": 9.0,
                "cal_n": 4,
                "eval_mean": 10.0,
                "eval_n": 4,
                "baseline_predictions": {
                    "fixed-cost": 7.0,
                    "ImageLocality": 9.0,
                },
                "representative_features_ok": True,
            },
            {
                "cell_id": "cell_b",
                "cal_mean": 5.5,
                "cal_n": 4,
                "eval_mean": 6.0,
                "eval_n": 4,
                "baseline_predictions": {
                    "fixed-cost": 7.0,
                    "ImageLocality": 4.0,
                },
                "representative_features_ok": True,
            },
            {
                "cell_id": "cell_c",
                "cal_mean": 2.5,
                "cal_n": 4,
                "eval_mean": 3.0,
                "eval_n": 4,
                "baseline_predictions": {
                    "fixed-cost": 7.0,
                    "ImageLocality": 5.0,
                },
                "representative_features_ok": True,
            },
        ],
    }
    path = tmp_path / "oracle-baseline-export.json"
    path.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
    return path


LEARNED = {"cell_a": 8.0, "cell_b": 5.0, "cell_c": 1.0}  # picks C


def test_choose_cell_tie_break():
    assert choose_cell({"b": 1.0, "a": 1.0}) == "a"


def test_gap_captured_known_answer(synth_export_path: Path):
    report = evaluate_export(synth_export_path, LEARNED)
    assert report.regrets["fixed-cost"] == pytest.approx(8.0)
    assert report.regrets["ImageLocality"] == pytest.approx(4.0)
    assert report.regrets["lightgbm"] == pytest.approx(1.0)
    assert report.best_baseline == "ImageLocality"
    assert report.best_baseline_cal_regrets["ImageLocality"] == pytest.approx(3.5)
    assert report.gap_captured == pytest.approx(0.75)


def test_path_swap_uses_same_api(synth_export_path: Path, tmp_path: Path):
    """Same evaluate_export entrypoint; only the path differs (real run later)."""
    alt = tmp_path / "campaign-run" / "oracle-baseline-export.json"
    alt.parent.mkdir(parents=True)
    alt.write_text(synth_export_path.read_text(encoding="utf-8"), encoding="utf-8")
    report = evaluate_export(alt, LEARNED)
    assert report.gap_captured == pytest.approx(0.75)


def test_shipped_fixture_matches_formula():
    """Committed fixture so CI does not depend on tmp_path construction alone."""
    assert SYNTHETIC_EXPORT.is_file(), f"missing {SYNTHETIC_EXPORT}"
    report = evaluate_export(
        SYNTHETIC_EXPORT,
        {"cell_a": 8.0, "cell_b": 5.0, "cell_c": 1.0},
    )
    assert report.gap_captured == pytest.approx(0.75)
    assert report.best_baseline == "ImageLocality"


def test_merge_and_regret_helpers(synth_export_path: Path):
    bundle = load_export(synth_export_path)
    merge_learned_predictions(bundle, LEARNED)
    assert regret(bundle, "ImageLocality") == pytest.approx(4.0)
    assert calibration_regret(bundle, "ImageLocality") == pytest.approx(3.5)
    report = compute_gap_captured(bundle)
    assert report.gap_captured == pytest.approx(0.75)


def test_baseline_selection_uses_calibration_not_evaluation(tmp_path: Path):
    """Baselines swap which is better between cal and eval; pipeline must follow cal.

    fixed-cost → cell_a; ImageLocality → cell_b; lightgbm → cell_c
    oracle_corrected = 2.0

    Calibration (selection):
      FC:  cal(a)=4 → regret 2
      IL:  cal(b)=8 → regret 6
      → best-existing = fixed-cost

    Evaluation (scoring only; IL would win if we leaked):
      FC:  eval(a)=10 → regret 8
      IL:  eval(b)=3  → regret 1
      LGBM: eval(c)=5 → regret 3

    GapCaptured = 1 - 3/8 = 0.625  (denominator = FC, not IL)
    Eval-leak would wrongly use IL and get 1 - 3/1 = -2.
    """
    payload = {
        "schema_version": 1,
        "oracle_selected_cell": "cell_oracle",
        "oracle_raw_cal_cost": 1.5,
        "oracle_corrected_cost": 2.0,
        "oracle_eval_cost": 2.2,
        "oracle_eval_n": 4,
        "oracle_shrinkage_alpha": 0.4,
        "oracle_prior_mean": 5.0,
        "cells": [
            {
                "cell_id": "cell_a",
                "cal_mean": 4.0,
                "cal_n": 4,
                "eval_mean": 10.0,
                "eval_n": 4,
                "baseline_predictions": {
                    "fixed-cost": 5.0,
                    "ImageLocality": 9.0,
                },
                "representative_features_ok": True,
            },
            {
                "cell_id": "cell_b",
                "cal_mean": 8.0,
                "cal_n": 4,
                "eval_mean": 3.0,
                "eval_n": 4,
                "baseline_predictions": {
                    "fixed-cost": 5.0,
                    "ImageLocality": 2.0,
                },
                "representative_features_ok": True,
            },
            {
                "cell_id": "cell_c",
                "cal_mean": 6.0,
                "cal_n": 4,
                "eval_mean": 5.0,
                "eval_n": 4,
                "baseline_predictions": {
                    "fixed-cost": 5.0,
                    "ImageLocality": 4.0,
                },
                "representative_features_ok": True,
            },
        ],
    }
    path = tmp_path / "oracle-baseline-export.swap.json"
    path.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
    learned = {"cell_a": 7.0, "cell_b": 6.0, "cell_c": 1.0}

    report = evaluate_export(path, learned)

    # Selection criterion: cal only
    assert report.best_baseline_cal_regrets["fixed-cost"] == pytest.approx(2.0)
    assert report.best_baseline_cal_regrets["ImageLocality"] == pytest.approx(6.0)
    assert report.best_baseline == "fixed-cost"

    # Eval regrets (for scoring) — IL looks better here, but must not be selected
    assert report.regrets["fixed-cost"] == pytest.approx(8.0)
    assert report.regrets["ImageLocality"] == pytest.approx(1.0)
    assert report.regrets["lightgbm"] == pytest.approx(3.0)

    assert report.gap_captured == pytest.approx(0.625)
    # Explicit anti-leak check: eval-min baseline would be ImageLocality
    eval_best = min(
        ("fixed-cost", "ImageLocality"),
        key=lambda n: report.regrets[n],
    )
    assert eval_best == "ImageLocality"
    assert report.best_baseline != eval_best

    bundle = load_export(path)
    merge_learned_predictions(bundle, learned)
    name, cal = select_best_baseline(bundle)
    assert name == "fixed-cost"
    assert cal["fixed-cost"] < cal["ImageLocality"]
