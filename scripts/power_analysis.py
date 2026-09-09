#!/usr/bin/env python3
"""Pilot replicate sizing from trialrunner pilot-mode CSVs.

Two-sample (equal-n) sample-size formula for a mean difference:

    n_per_group = 2 * (z_{α/2} + z_β)^2 * σ² / δ²

Defaults: α = 0.05 → z_{α/2} = 1.96; power = 0.9 → z_β ≈ 1.2816.
Power 0.9 (not the common 0.8) is intentional per the project's standing
rigor directive -- underpowering the campaign is worse than a larger n.

δ is either supplied with --delta (same value for every cache) or derived
with --auto-delta from the smallest adjacent PSI-level mean gap *within
each* image-cache state. Each cache uses its own delta for n and config
rows; a global min across caches may be printed as FYI but never feeds
sizing.

--write-config PATH writes a local-VM-only campaign JSON (6 cells; no AWS)
using per-cache-state recommended n (cold vs warm), after checking that
within-cache PSI-level variances are not also heterogeneous at the same
ratio threshold (would repeat the pooling mistake at finer grain).
Recommended n above --max-replicates-flag (default 500) triggers a loud
plausibility warning with wall-clock estimate at observed/assumed trial duration.

Usage:
  python scripts/power_analysis.py path/to/pilot-variance.csv --delta 1.0
  python scripts/power_analysis.py path/to/pilot-variance.csv --auto-delta
  python scripts/power_analysis.py path/to/pilot-variance.csv --auto-delta \\
      --write-config experiments/config/campaign-from-pilot.json
"""

from __future__ import annotations

import argparse
import csv
import json
import math
import sys
from collections import defaultdict
from pathlib import Path

COST_COL = "ttfs_clusterip_sec"
CACHE_COL = "image_cache_state"
PSI_COL = "cpu_psi_level"

# Adjacent PSI ladder for the local-VM grid (none < threshold < high).
PSI_ORDER = ("none", "threshold", "high")
CACHE_ORDER = ("cold", "warm")

DEFAULT_Z_ALPHA_HALF = 1.96  # α = 0.05, two-sided
DEFAULT_Z_BETA = 1.2816  # power = 0.9
VARIANCE_RATIO_FLAG = 3.0
SENSITIVITY_MULTS = (0.5, 1.0, 2.0, 3.0)
# Plausibility: flag recommended replicate counts that would dominate wall-clock.
# 500 trials × ~2 min/trial ≈ 17 h per cell before cal+eval doubling — rethink δ/α/power.
DEFAULT_MAX_REPLICATES_FLAG = 500
DEFAULT_TRIAL_WALL_SEC = 120.0  # fallback if timestamps unavailable


def sample_mean_var_std(xs: list[float]) -> tuple[float, float, float]:
    n = len(xs)
    if n == 0:
        return float("nan"), float("nan"), float("nan")
    mean = sum(xs) / n
    if n < 2:
        return mean, float("nan"), float("nan")
    var = sum((x - mean) ** 2 for x in xs) / (n - 1)
    return mean, var, math.sqrt(var)


def n_per_group(sigma: float, delta: float, z_a: float, z_b: float) -> float:
    if delta <= 0:
        raise ValueError("delta must be > 0")
    if not math.isfinite(sigma) or sigma < 0:
        raise ValueError("sigma must be a non-negative finite number")
    return 2.0 * (z_a + z_b) ** 2 * (sigma**2) / (delta**2)


def load_costs(path: Path) -> list[dict]:
    with path.open(newline="", encoding="utf-8") as f:
        rows = list(csv.DictReader(f))
    if not rows:
        raise SystemExit(f"no data rows in {path}")
    for col in (COST_COL, CACHE_COL, PSI_COL):
        if col not in rows[0]:
            raise SystemExit(f"missing column {col!r} in {path}")
    out = []
    for i, r in enumerate(rows, start=2):
        try:
            cost = float(r[COST_COL])
        except (TypeError, ValueError) as e:
            raise SystemExit(f"row {i}: bad {COST_COL}={r.get(COST_COL)!r}: {e}") from e
        cache = (r.get(CACHE_COL) or "").strip()
        psi = (r.get(PSI_COL) or "").strip()
        if not cache or not psi:
            raise SystemExit(f"row {i}: empty cache/psi cell")
        out.append({"cache": cache, "psi": psi, "cost": cost})
    return out


def fmt(x: float, nd: int = 4) -> str:
    if x is None or (isinstance(x, float) and not math.isfinite(x)):
        return "n/a"
    return f"{x:.{nd}f}"


def adjacent_mean_gaps(
    by_cell: dict[tuple[str, str], list[float]], cache: str
) -> list[tuple[float, str, str, float, float]]:
    """Return (abs_delta, psi_a, psi_b, mean_a, mean_b) for adjacent PSI levels present."""
    means: dict[str, float] = {}
    for psi in PSI_ORDER:
        key = (cache, psi)
        if key not in by_cell or not by_cell[key]:
            continue
        mean, _, _ = sample_mean_var_std(by_cell[key])
        if math.isfinite(mean):
            means[psi] = mean
    gaps: list[tuple[float, str, str, float, float]] = []
    present = [p for p in PSI_ORDER if p in means]
    for a, b in zip(present, present[1:]):
        # Only count as "adjacent" if they are neighbors on the full ladder
        # (none-threshold, threshold-high), not none-high if threshold missing.
        ia, ib = PSI_ORDER.index(a), PSI_ORDER.index(b)
        if ib != ia + 1:
            continue
        d = abs(means[a] - means[b])
        gaps.append((d, a, b, means[a], means[b]))
    return gaps


def auto_delta_by_cache(
    by_cell: dict[tuple[str, str], list[float]], caches: list[str]
) -> dict[str, tuple[float, str, str, float, float]]:
    """Per cache: (min_delta, psi_a, psi_b, mean_a, mean_b)."""
    out: dict[str, tuple[float, str, str, float, float]] = {}
    for cache in caches:
        gaps = adjacent_mean_gaps(by_cell, cache)
        if not gaps:
            continue
        out[cache] = min(gaps, key=lambda t: t[0])
    return out


def within_cache_psi_variance_ratio(
    by_cell: dict[tuple[str, str], list[float]], cache: str
) -> tuple[float, dict[str, float]]:
    """Max/min sample-variance ratio across PSI levels within one cache state.

    Returns (ratio, {psi: variance}) using only levels with n>=2 finite var.
    ratio is nan if fewer than two usable levels.
    """
    vars_by_psi: dict[str, float] = {}
    for psi in PSI_ORDER:
        xs = by_cell.get((cache, psi), [])
        _, var, _ = sample_mean_var_std(xs)
        if math.isfinite(var) and var >= 0:
            vars_by_psi[psi] = var
    if len(vars_by_psi) < 2:
        return float("nan"), vars_by_psi
    vals = list(vars_by_psi.values())
    lo = min(vals)
    if lo <= 0:
        hi = max(vals)
        return (float("inf") if hi > 0 else 1.0), vars_by_psi
    return max(vals) / lo, vars_by_psi


def split_cal_eval(recommended_n: int, cal_eval_ratio: float) -> tuple[int, int]:
    """Map a recommended per-group n into (calibration_n, evaluation_n).

    Absent a specific reason to weight calibration and evaluation differently, a
    symmetric split (ratio=1) is the simplest defensible default: both the
    oracle's Smith-Winkler shrinkage (cal cell means) and the evaluation-side
    power target benefit from comparably precise cell-mean estimates. Adjust
    only via --cal-eval-ratio with a stated reason -- never silently.
    """
    if recommended_n < 1:
        raise ValueError("recommended_n must be >= 1")
    if cal_eval_ratio <= 0:
        raise ValueError("cal_eval_ratio must be > 0")
    evaluation_n = int(math.ceil(recommended_n))
    calibration_n = int(math.ceil(recommended_n * cal_eval_ratio))
    if calibration_n < 1:
        calibration_n = 1
    if evaluation_n < 1:
        evaluation_n = 1
    return calibration_n, evaluation_n


def estimate_trial_wall_sec(csv_path: Path, fallback: float = DEFAULT_TRIAL_WALL_SEC) -> float:
    """Median positive inter-trial gap from ts_utc if present; else fallback seconds."""
    try:
        with csv_path.open(newline="", encoding="utf-8") as f:
            rows = list(csv.DictReader(f))
    except OSError:
        return fallback
    if not rows or "ts_utc" not in (rows[0] or {}):
        return fallback
    from datetime import datetime

    times: list[datetime] = []
    for r in rows:
        s = (r.get("ts_utc") or "").strip()
        if not s:
            continue
        try:
            times.append(datetime.fromisoformat(s.replace("Z", "+00:00")))
        except ValueError:
            continue
    times.sort()
    gaps = [(times[i] - times[i - 1]).total_seconds() for i in range(1, len(times))]
    gaps = [g for g in gaps if 5.0 < g < 3600.0]  # ignore clock jumps / sub-second noise
    if not gaps:
        return fallback
    gaps.sort()
    return gaps[len(gaps) // 2]


def flag_implausible_n(
    *,
    cell: str,
    recommended_n: int,
    cal_n: int,
    eval_n: int,
    trial_wall_sec: float,
    max_replicates_flag: int,
) -> None:
    """Loud stderr warning when n exceeds the wall-clock plausibility budget."""
    if recommended_n <= max_replicates_flag:
        return
    trials = cal_n + eval_n
    hours = trials * trial_wall_sec / 3600.0
    print(
        f"\n*** PLAUSIBILITY FLAG: {cell} recommended_n={recommended_n} "
        f"> {max_replicates_flag} (cal+eval={trials} trials) ***\n"
        f"    At ~{trial_wall_sec:.0f}s observed/assumed wall per trial ≈ {hours:.1f} h "
        f"for this cell alone.\n"
        f"    Do not blindly run this: reconsider alpha/power/delta (or accept a "
        f"larger detectable effect), not a multi-week cell.\n",
        file=sys.stderr,
    )


def build_local_vm_config(
    *,
    cell_ns: dict[tuple[str, str], dict],
) -> dict:
    """Campaign-mode schema: local_vm_cells only; aws_cells empty (not provisioned)."""
    cells = []
    for cache in CACHE_ORDER:
        for psi in PSI_ORDER:
            meta = cell_ns[(cache, psi)]
            cells.append(
                {
                    "image_cache_state": cache,
                    "cpu_psi_level": psi,
                    "calibration_n": meta["calibration_n"],
                    "evaluation_n": meta["evaluation_n"],
                    "held_out": False,
                }
            )
    return {"local_vm_cells": cells, "aws_cells": []}


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        description="Replicate sizing from reloc-disrupt pilot CSV (ttfs_clusterip_sec).",
    )
    p.add_argument("csv", type=Path, help="pilot-variance-*.csv from trialrunner -mode pilot")
    delta_group = p.add_mutually_exclusive_group(required=True)
    delta_group.add_argument(
        "--delta",
        type=float,
        help="minimum detectable effect size in seconds (manual mode)",
    )
    delta_group.add_argument(
        "--auto-delta",
        action="store_true",
        help=(
            "set delta from data: for each cache state, min |mean difference| "
            "between adjacent PSI levels within that cache only (never cross-cache)"
        ),
    )
    p.add_argument("--alpha", type=float, default=0.05, help="two-sided type I error (default 0.05)")
    p.add_argument("--power", type=float, default=0.9, help="target power (default 0.9, not 0.8)")
    p.add_argument(
        "--z-alpha-half",
        type=float,
        default=None,
        help=f"override z_{{alpha/2}} (default {DEFAULT_Z_ALPHA_HALF} for alpha=0.05)",
    )
    p.add_argument(
        "--z-beta",
        type=float,
        default=None,
        help=f"override z_beta (default {DEFAULT_Z_BETA} for power=0.9)",
    )
    p.add_argument(
        "--variance-ratio-flag",
        type=float,
        default=VARIANCE_RATIO_FLAG,
        help=(
            f"flag cold/warm (and within-cache PSI) variance ratio above this "
            f"(default {VARIANCE_RATIO_FLAG})"
        ),
    )
    p.add_argument(
        "--write-config",
        type=Path,
        default=None,
        help=(
            "write a local-VM campaign JSON (6 cells; aws_cells=[]) with per-cache "
            "recommended n; uses per-PSI n when within-cache variance ratio exceeds "
            "--variance-ratio-flag (never silently pool)"
        ),
    )
    p.add_argument(
        "--cal-eval-ratio",
        type=float,
        default=1.0,
        help=(
            "calibration_n relative to recommended n "
            "(default 1.0 => calibration_n = evaluation_n = ceil(n); "
            "cal = ceil(n * ratio), eval = ceil(n))"
        ),
    )
    p.add_argument(
        "--max-replicates-flag",
        type=int,
        default=DEFAULT_MAX_REPLICATES_FLAG,
        help=(
            f"warn loudly if any cell recommended_n exceeds this "
            f"(default {DEFAULT_MAX_REPLICATES_FLAG}); rethink alpha/power/delta"
        ),
    )
    p.add_argument(
        "--trial-wall-sec",
        type=float,
        default=None,
        help=(
            f"assumed wall-clock seconds per trial for plausibility hours "
            f"(default: median inter-trial gap from ts_utc, else {DEFAULT_TRIAL_WALL_SEC:g})"
        ),
    )
    args = p.parse_args(argv)

    z_a = args.z_alpha_half if args.z_alpha_half is not None else DEFAULT_Z_ALPHA_HALF
    z_b = args.z_beta if args.z_beta is not None else DEFAULT_Z_BETA
    if args.z_alpha_half is None and abs(args.alpha - 0.05) > 1e-12:
        print(
            f"warning: --alpha={args.alpha} but using default z_alpha/2={z_a}; "
            "pass --z-alpha-half for a matching quantile",
            file=sys.stderr,
        )
    if args.z_beta is None and abs(args.power - 0.9) > 1e-12:
        print(
            f"warning: --power={args.power} but using default z_beta={z_b}; "
            "pass --z-beta for a matching quantile",
            file=sys.stderr,
        )
    if args.write_config is not None and args.cal_eval_ratio <= 0:
        raise SystemExit("--cal-eval-ratio must be > 0")

    rows = load_costs(args.csv)

    by_cell: dict[tuple[str, str], list[float]] = defaultdict(list)
    by_cache: dict[str, list[float]] = defaultdict(list)
    for r in rows:
        by_cell[(r["cache"], r["psi"])].append(r["cost"])
        by_cache[r["cache"]].append(r["cost"])

    print(f"file: {args.csv}")
    print(f"metric: {COST_COL}")
    print(f"N_trials: {len(rows)}")
    print()
    print("=== Per cell (image_cache_state x cpu_psi_level) ===")
    hdr = f"{'cell':<22} {'n':>4} {'mean':>10} {'var':>12} {'std':>10}"
    print(hdr)
    print("-" * len(hdr))
    cell_stats: list[tuple[str, int, float, float, float]] = []
    cell_stds: dict[tuple[str, str], float] = {}
    for cache, psi in sorted(by_cell.keys()):
        xs = by_cell[(cache, psi)]
        mean, var, std = sample_mean_var_std(xs)
        label = f"{cache}_{psi}"
        cell_stats.append((label, len(xs), mean, var, std))
        cell_stds[(cache, psi)] = std
        print(f"{label:<22} {len(xs):>4} {fmt(mean):>10} {fmt(var):>12} {fmt(std):>10}")

    print()
    print("=== Pooled by image_cache_state only (cold vs warm) ===")
    print(f"{'cache':<10} {'n':>4} {'mean':>10} {'var':>12} {'std':>10}")
    print("-" * 52)
    cache_stds: dict[str, float] = {}
    cache_vars: dict[str, float] = {}
    for cache in sorted(by_cache.keys()):
        xs = by_cache[cache]
        mean, var, std = sample_mean_var_std(xs)
        cache_vars[cache] = var
        cache_stds[cache] = std
        print(f"{cache:<10} {len(xs):>4} {fmt(mean):>10} {fmt(var):>12} {fmt(std):>10}")

    # --- resolve per-cache deltas (never borrow warm delta to size cold) ---
    # delta_by_cache[cache] is the ONLY delta used for that cache's n / config rows.
    auto_by_cache = auto_delta_by_cache(by_cell, sorted(by_cache.keys()))
    delta_by_cache: dict[str, float] = {}
    delta_source_by_cache: dict[str, str] = {}

    if args.auto_delta:
        if not auto_by_cache:
            raise SystemExit(
                "--auto-delta: need adjacent PSI cells (none/threshold/high) with data "
                "within at least one cache state"
            )
        print()
        print("=== Auto-delta (adjacent PSI mean gaps within each cache state) ===")
        print(
            "Sizing rule: each cache state uses ITS OWN min adjacent gap as delta. "
            "A global min across caches is FYI only and is never fed into n."
        )
        for cache in sorted(auto_by_cache.keys()):
            d, a, b, ma, mb = auto_by_cache[cache]
            delta_by_cache[cache] = d
            delta_source_by_cache[cache] = (
                f"auto-delta within {cache} ({cache}_{a} vs {cache}_{b})"
            )
            print(
                f"  {cache}: sizing delta = {fmt(d)}s "
                f"from {cache}_{a} (mean={fmt(ma)}) vs {cache}_{b} (mean={fmt(mb)})"
            )
            for gd, ga, gb, gma, gmb in adjacent_mean_gaps(by_cell, cache):
                mark = "  <-- min (used for this cache)" if (ga, gb) == (a, b) else ""
                print(
                    f"    |{cache}_{ga} - {cache}_{gb}| = {fmt(gd)}s "
                    f"(means {fmt(gma)}, {fmt(gmb)}){mark}"
                )
        fyi_min = min(delta_by_cache.values())
        fyi_cache = min(delta_by_cache.keys(), key=lambda c: delta_by_cache[c])
        print(
            f"FYI headline min across caches (NOT used for sizing): {fmt(fyi_min)}s "
            f"[{fyi_cache}]"
        )
    else:
        if args.delta is None or args.delta <= 0:
            raise SystemExit("--delta must be > 0")
        for cache in CACHE_ORDER:
            if cache in by_cache:
                delta_by_cache[cache] = args.delta
                delta_source_by_cache[cache] = "manual --delta (same value for every cache)"
        if auto_by_cache:
            print()
            print("=== Adjacent PSI mean gaps (informational; --delta overrides) ===")
            for cache in sorted(auto_by_cache.keys()):
                d, a, b, ma, mb = auto_by_cache[cache]
                print(
                    f"  {cache}: data min adjacent |diff| = {fmt(d)}s "
                    f"({cache}_{a} vs {cache}_{b}; means {fmt(ma)}, {fmt(mb)})"
                )

    print()
    print(f"alpha: {args.alpha}  power: {args.power}  z_a/2: {z_a}  z_b: {z_b}")
    for cache in CACHE_ORDER:
        if cache not in delta_by_cache:
            continue
        print(
            f"  sizing delta[{cache}] = {fmt(delta_by_cache[cache])}s "
            f"({delta_source_by_cache[cache]})"
        )

    finite_stds = [(lab, std) for lab, _, _, _, std in cell_stats if math.isfinite(std)]
    if not finite_stds:
        raise SystemExit("no cell has n>=2; cannot estimate sigma")

    print()
    print("=== Recommended n by cache (own delta x cache-pooled sigma) ===")
    for cache in CACHE_ORDER:
        if cache not in delta_by_cache or cache not in cache_stds:
            continue
        d = delta_by_cache[cache]
        std = cache_stds[cache]
        if not math.isfinite(std):
            continue
        n_c = n_per_group(std, d, z_a, z_b)
        print(
            f"  {cache}: sigma={fmt(std)}  delta={fmt(d)}s  "
            f"n_per_group ceil={int(math.ceil(n_c))}"
        )
    print(
        "Interpretation: equal-n two-sample comparison at the stated alpha/power, "
        "with sigma and delta both taken within the same cache state."
    )

    cold_v = cache_vars.get("cold")
    warm_v = cache_vars.get("warm")
    if (
        cold_v is not None
        and warm_v is not None
        and math.isfinite(cold_v)
        and math.isfinite(warm_v)
        and min(cold_v, warm_v) > 0
    ):
        ratio = max(cold_v, warm_v) / min(cold_v, warm_v)
        print()
        print("=== Cold vs warm variance ratio ===")
        print(f"var(cold)/var(warm) ordered ratio: {fmt(ratio, 2)}x")
        if ratio >= args.variance_ratio_flag:
            print(
                f"FLAG: cold/warm variance ratio >= {args.variance_ratio_flag:g}x -- "
                "prefer per-cache-state replicate sizing rather than one pooled n "
                "for the whole 6-cell grid (see docs/campaign-design.md section 2)."
            )
        else:
            print(
                f"(ratio < {args.variance_ratio_flag:g}x -- single grid-wide n may be defensible; "
                "still review campaign-design section 2 structural cold-pull variance.)"
            )

    print()
    print("=== Within-cache PSI variance (none vs threshold vs high) ===")
    within_flags: dict[str, float] = {}
    for cache in CACHE_ORDER:
        if cache not in by_cache:
            continue
        wratio, vars_by_psi = within_cache_psi_variance_ratio(by_cell, cache)
        within_flags[cache] = wratio
        bits = ", ".join(
            f"{psi} var={fmt(vars_by_psi[psi])}" for psi in PSI_ORDER if psi in vars_by_psi
        )
        print(f"  {cache}: max/min var ratio = {fmt(wratio, 2)}x  [{bits}]")
        if math.isfinite(wratio) and wratio >= args.variance_ratio_flag:
            print(
                f"  FLAG: {cache} within-cache PSI variance ratio >= {args.variance_ratio_flag:g}x -- "
                "do NOT apply one n to all three PSI levels for this cache "
                "(same pooling mistake as cold/warm, finer grain)."
            )
        elif math.isfinite(wratio):
            print(
                f"  ok: {cache} within-cache ratio < {args.variance_ratio_flag:g}x -- "
                "one n per cache state across PSI levels is defensible."
            )

    print()
    print(
        "=== Sensitivity: n_per_group (ceil) vs delta multipliers "
        "(per-cache pooled sigma x that cache sizing delta) ==="
    )
    print(
        "Each row uses that cache's own sizing delta as the 1.0x base "
        "(not a cross-cache headline min)."
    )
    mult_hdr = " ".join(f"{'d*' + fmt(m, 1):>10}" for m in SENSITIVITY_MULTS)
    print(f"{'cache':<8} {'sigma':>10} {'delta':>10} {mult_hdr}")
    print("-" * (8 + 10 + 10 + 1 + 11 * len(SENSITIVITY_MULTS)))
    for cache in CACHE_ORDER:
        if cache not in cache_stds or cache not in delta_by_cache:
            continue
        std = cache_stds[cache]
        base_d = delta_by_cache[cache]
        if not math.isfinite(std):
            continue
        cells = []
        for m in SENSITIVITY_MULTS:
            d = base_d * m
            n_c = n_per_group(std, d, z_a, z_b)
            cells.append(f"{int(math.ceil(n_c)):>10}")
        print(f"{cache:<8} {fmt(std):>10} {fmt(base_d):>10} {' '.join(cells)}")

    trial_wall = (
        args.trial_wall_sec
        if args.trial_wall_sec is not None
        else estimate_trial_wall_sec(args.csv)
    )
    max_rep_flag = args.max_replicates_flag
    print()
    print(
        f"plausibility: flag recommended_n > {max_rep_flag} "
        f"at ~{trial_wall:.0f}s/trial wall"
    )

    if args.write_config is not None:
        cache_n: dict[str, int] = {}
        for cache in CACHE_ORDER:
            std = cache_stds.get(cache)
            d = delta_by_cache.get(cache)
            if std is None or not math.isfinite(std) or d is None:
                raise SystemExit(
                    f"--write-config: need finite pooled std and sizing delta for cache={cache!r}"
                )
            cache_n[cache] = int(math.ceil(n_per_group(std, d, z_a, z_b)))

        cell_meta: dict[tuple[str, str], dict] = {}
        used_per_psi = False
        for cache in CACHE_ORDER:
            wratio = within_flags.get(cache, float("nan"))
            use_per_psi = math.isfinite(wratio) and wratio >= args.variance_ratio_flag
            d = delta_by_cache[cache]
            if use_per_psi:
                used_per_psi = True
                print()
                print(
                    f"*** REFUSING uniform-n for cache={cache}: within-PSI variance "
                    f"ratio {fmt(wratio, 2)}x >= {args.variance_ratio_flag:g}x ***"
                )
                print(
                    "    Sizing each PSI cell from its own cell std "
                    f"at this cache delta={fmt(d)}s (not silent pool)."
                )
            for psi in PSI_ORDER:
                key = (cache, psi)
                if use_per_psi:
                    cstd = cell_stds.get(key, float("nan"))
                    if not math.isfinite(cstd):
                        raise SystemExit(
                            f"--write-config: cell {cache}_{psi} needs n>=2 for "
                            "per-PSI sizing after within-cache FLAG"
                        )
                    rec_n = int(math.ceil(n_per_group(cstd, d, z_a, z_b)))
                    sigma_src = f"cell_std({cache}_{psi})={fmt(cstd)}"
                    pool = "per-psi (within-cache FLAG)"
                else:
                    rec_n = cache_n[cache]
                    sigma_src = f"cache_pooled_std({cache})={fmt(cache_stds[cache])}"
                    pool = f"cache-pooled ({cache})"
                cal_n, eval_n = split_cal_eval(rec_n, args.cal_eval_ratio)
                cell_meta[key] = {
                    "recommended_n": rec_n,
                    "calibration_n": cal_n,
                    "evaluation_n": eval_n,
                    "sigma_source": sigma_src,
                    "pool": pool,
                    "delta": d,
                    "delta_source": delta_source_by_cache[cache],
                }
                flag_implausible_n(
                    cell=f"{cache}_{psi}",
                    recommended_n=rec_n,
                    cal_n=cal_n,
                    eval_n=eval_n,
                    trial_wall_sec=trial_wall,
                    max_replicates_flag=max_rep_flag,
                )

        print()
        print("=== Campaign config preview (local-VM only; aws_cells=[]) ===")
        print(
            f"cal_eval_ratio={args.cal_eval_ratio:g}  "
            f"(cal=ceil(n*ratio), eval=ceil(n)); deltas are per-cache"
        )
        preview_hdr = (
            f"{'cell':<18} {'delta':>8} {'rec_n':>6} {'cal_n':>6} {'eval_n':>6}  "
            f"{'variance / n source'}"
        )
        print(preview_hdr)
        print("-" * max(len(preview_hdr), 78))
        for cache in CACHE_ORDER:
            for psi in PSI_ORDER:
                m = cell_meta[(cache, psi)]
                print(
                    f"{cache + '_' + psi:<18} {fmt(m['delta']):>8} "
                    f"{m['recommended_n']:>6} {m['calibration_n']:>6} "
                    f"{m['evaluation_n']:>6}  "
                    f"{m['pool']}; {m['sigma_source']}"
                )
        if used_per_psi:
            print()
            print(
                "NOTE: one or more caches used per-PSI n because within-cache "
                "variance heterogeneity met the FLAG threshold."
            )

        cfg = build_local_vm_config(cell_ns=cell_meta)
        args.write_config.parent.mkdir(parents=True, exist_ok=True)
        args.write_config.write_text(json.dumps(cfg, indent=2) + "\n", encoding="utf-8")
        print()
        print(f"wrote campaign config: {args.write_config}")
        print(
            "(AWS cells omitted intentionally -- add manually after live netshape verification.)"
        )
        print(json.dumps(cfg, indent=2))

    return 0



if __name__ == "__main__":
    raise SystemExit(main())
