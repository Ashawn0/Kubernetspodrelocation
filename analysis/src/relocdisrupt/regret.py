"""Regret and GapCaptured against the bias-corrected empirical oracle.

Go exports baseline predictions + Smith–Winkler-corrected oracle values
(``oracle-baseline-export.json``). This module loads that file, merges
LightGBM (or any learned) per-cell predictions, and computes:

- **choice**: each predictor picks ``argmin`` predicted cost (lexicographic
  ``cell_id`` tie-break). Predictions themselves are fit on calibration only
  (Go baselines / Python LightGBM); that is not the leak fixed here.
- **eval regret(π)** = eval_mean(cell chosen by π) − oracle_corrected
  (evaluation set scores an already-chosen comparison only).
- **best-existing-baseline**: chosen by **calibration regret only** —
  cal_mean(chosen) − oracle_corrected — never by comparing eval regrets.
  Evaluation is then used solely to score GapCaptured against that
  predetermined baseline (same category of split as oracle selection).
- **GapCaptured** = 1 − regret_eval(learned) / regret_eval(best-existing-baseline)

See docs/research-design.md and docs/campaign-design.md §8.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Mapping

BASELINE_NAMES = ("fixed-cost", "ImageLocality")
LEARNED_NAME = "lightgbm"


@dataclass
class CellView:
    cell_id: str
    cal_mean: float
    cal_n: int
    eval_mean: float
    eval_n: int
    predictions: dict[str, float] = field(default_factory=dict)


@dataclass
class ExportBundle:
    """In-memory form of experiments/results/{run}/oracle-baseline-export.json."""

    schema_version: int
    oracle_selected_cell: str
    oracle_corrected_cost: float
    oracle_eval_cost: float
    cells: dict[str, CellView]

    @classmethod
    def from_dict(cls, data: dict) -> ExportBundle:
        cells: dict[str, CellView] = {}
        for row in data.get("cells", []):
            cid = row["cell_id"]
            cells[cid] = CellView(
                cell_id=cid,
                cal_mean=float(row.get("cal_mean", 0.0)),
                cal_n=int(row.get("cal_n", 0)),
                eval_mean=float(row.get("eval_mean", 0.0)),
                eval_n=int(row.get("eval_n", 0)),
                predictions={
                    str(k): float(v)
                    for k, v in (row.get("baseline_predictions") or {}).items()
                },
            )
        return cls(
            schema_version=int(data.get("schema_version", 0)),
            oracle_selected_cell=str(data["oracle_selected_cell"]),
            oracle_corrected_cost=float(data["oracle_corrected_cost"]),
            oracle_eval_cost=float(data.get("oracle_eval_cost", 0.0)),
            cells=cells,
        )


def load_export(path: str | Path) -> ExportBundle:
    """Load a Go oracle-baseline export. Swap the path for a real campaign file later."""
    p = Path(path)
    with p.open(encoding="utf-8") as f:
        return ExportBundle.from_dict(json.load(f))


def merge_learned_predictions(
    bundle: ExportBundle,
    learned: Mapping[str, float],
    *,
    name: str = LEARNED_NAME,
) -> None:
    """Attach per-cell learned predictions (same cell_ids as the export)."""
    missing = [cid for cid in bundle.cells if cid not in learned]
    if missing:
        raise ValueError(f"learned predictions missing cells: {missing}")
    extra = [cid for cid in learned if cid not in bundle.cells]
    if extra:
        raise ValueError(f"learned predictions have unknown cells: {extra}")
    for cid, pred in learned.items():
        bundle.cells[cid].predictions[name] = float(pred)


def choose_cell(predictions: Mapping[str, float]) -> str:
    """Lowest predicted cost; ties broken by cell_id ascending."""
    if not predictions:
        raise ValueError("no predictions to choose from")
    return min(predictions.items(), key=lambda kv: (kv[1], kv[0]))[0]


def predictor_predictions(bundle: ExportBundle, name: str) -> dict[str, float]:
    out: dict[str, float] = {}
    for cid, cell in bundle.cells.items():
        if name not in cell.predictions:
            raise ValueError(f"predictor {name!r} missing prediction for cell {cid!r}")
        out[cid] = cell.predictions[name]
    return out


def _chosen_cell(bundle: ExportBundle, predictor_name: str) -> str:
    return choose_cell(predictor_predictions(bundle, predictor_name))


def calibration_regret(bundle: ExportBundle, predictor_name: str) -> float:
    """Calibration-only regret for baseline *selection* (never for GapCaptured score).

    cal_regret(π) = cal_mean(chosen_cell) − oracle_corrected_cost
    """
    chosen = _chosen_cell(bundle, predictor_name)
    return bundle.cells[chosen].cal_mean - bundle.oracle_corrected_cost


def regret(bundle: ExportBundle, predictor_name: str) -> float:
    """Evaluation regret vs corrected empirical oracle value (seconds).

    regret = eval_mean(chosen_cell) − oracle_corrected_cost
    """
    chosen = _chosen_cell(bundle, predictor_name)
    return bundle.cells[chosen].eval_mean - bundle.oracle_corrected_cost


def select_best_baseline(
    bundle: ExportBundle,
    baseline_names: tuple[str, ...] = BASELINE_NAMES,
) -> tuple[str, dict[str, float]]:
    """Pick best-existing-baseline using calibration regret only.

    Tie-break: lexicographically smaller baseline name (stable, documented).
    """
    cal_regrets = {n: calibration_regret(bundle, n) for n in baseline_names}
    best = min(cal_regrets.items(), key=lambda kv: (kv[1], kv[0]))[0]
    return best, cal_regrets


@dataclass
class RegretReport:
    regrets: dict[str, float]  # evaluation regrets (scoring)
    chosen_cells: dict[str, str]
    best_baseline: str
    best_baseline_cal_regrets: dict[str, float]  # selection criterion
    best_baseline_regret: float  # evaluation regret of selected baseline
    learned_name: str
    learned_regret: float
    gap_captured: float


def compute_gap_captured(
    bundle: ExportBundle,
    *,
    learned_name: str = LEARNED_NAME,
    baseline_names: tuple[str, ...] = BASELINE_NAMES,
) -> RegretReport:
    """Headline metric: select baseline on cal, score GapCaptured on eval only."""
    names = list(baseline_names) + [learned_name]
    chosen: dict[str, str] = {}
    eval_regrets: dict[str, float] = {}
    for name in names:
        chosen[name] = _chosen_cell(bundle, name)
        eval_regrets[name] = (
            bundle.cells[chosen[name]].eval_mean - bundle.oracle_corrected_cost
        )

    best_baseline, cal_regrets = select_best_baseline(bundle, baseline_names)
    best_r = eval_regrets[best_baseline]
    learned_r = eval_regrets[learned_name]
    if best_r <= 0:
        raise ValueError(
            f"best-existing-baseline eval regret is {best_r}; GapCaptured undefined "
            f"(need a positive baseline regret for the ratio)"
        )
    gap = 1.0 - learned_r / best_r
    return RegretReport(
        regrets=eval_regrets,
        chosen_cells=chosen,
        best_baseline=best_baseline,
        best_baseline_cal_regrets=cal_regrets,
        best_baseline_regret=best_r,
        learned_name=learned_name,
        learned_regret=learned_r,
        gap_captured=gap,
    )


def evaluate_export(
    export_path: str | Path,
    learned_predictions: Mapping[str, float],
    *,
    learned_name: str = LEARNED_NAME,
) -> RegretReport:
    """End-to-end: load JSON path → merge learned preds → GapCaptured.

    Point ``export_path`` at a real ``oracle-baseline-export.json`` later with no
    code changes — only the path (and learned prediction source) change.
    """
    bundle = load_export(export_path)
    merge_learned_predictions(bundle, learned_predictions, name=learned_name)
    return compute_gap_captured(bundle, learned_name=learned_name)
