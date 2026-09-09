#!/usr/bin/env python3
"""Pilot replicate sizing from trialrunner pilot-mode CSVs.

Two-sample (equal-n) sample-size formula for a mean difference:

    n_per_group = 2 * (z_{α/2} + z_β)^2 * σ² / δ²

Defaults: α = 0.05 → z_{α/2} = 1.96; power = 0.9 → z_β ≈ 1.2816.
Power 0.9 (not the common 0.8) is intentional per the project's standing
rigor directive -- underpowering the campaign is worse than a larger n.

σ for the single headline recommendation is the *most conservative*
(largest) cell sample standard deviation. δ is either supplied with
--delta or derived with --auto-delta from the smallest adjacent PSI-level
mean gap within each image-cache state (then taking the global min).

Usage:
  python scripts/power_analysis.py path/to/pilot-variance.csv --delta 1.0
  python scripts/power_analysis.py path/to/pilot-variance.csv --auto-delta
"""

from __future__ import annotations

import argparse
import csv
import math
import sys
from collections import defaultdict
from pathlib import Path

COST_COL = "ttfs_clusterip_sec"
CACHE_COL = "image_cache_state"
PSI_COL = "cpu_psi_level"

# Adjacent PSI ladder for the local-VM grid (none < threshold < high).
PSI_ORDER = ("none", "threshold", "high")

DEFAULT_Z_ALPHA_HALF = 1.96  # α = 0.05, two-sided
DEFAULT_Z_BETA = 1.2816  # power = 0.9
VARIANCE_RATIO_FLAG = 3.0
SENSITIVITY_MULTS = (0.5, 1.0, 2.0, 3.0)


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
            "set delta from data: min |mean difference| between adjacent PSI "
            "levels within each cache state; headline delta = min across caches"
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
        help=f"flag cold/warm variance ratio above this (default {VARIANCE_RATIO_FLAG})",
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
    for cache, psi in sorted(by_cell.keys()):
        xs = by_cell[(cache, psi)]
        mean, var, std = sample_mean_var_std(xs)
        label = f"{cache}_{psi}"
        cell_stats.append((label, len(xs), mean, var, std))
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

    # --- resolve delta ---
    auto_by_cache = auto_delta_by_cache(by_cell, sorted(by_cache.keys()))
    if args.auto_delta:
        if not auto_by_cache:
            raise SystemExit(
                "--auto-delta: need adjacent PSI cells (none/threshold/high) with data "
                "within at least one cache state"
            )
        print()
        print("=== Auto-delta (adjacent PSI mean gaps within each cache state) ===")
        for cache in sorted(auto_by_cache.keys()):
            d, a, b, ma, mb = auto_by_cache[cache]
            print(
                f"  {cache}: min |mean diff| = {fmt(d)}s "
                f"from {cache}_{a} (mean={fmt(ma)}) vs {cache}_{b} (mean={fmt(mb)})"
            )
            # Also list all adjacent gaps for traceability
            for gd, ga, gb, gma, gmb in adjacent_mean_gaps(by_cell, cache):
                mark = "  <-- min" if (ga, gb) == (a, b) else ""
                print(
                    f"    |{cache}_{ga} - {cache}_{gb}| = {fmt(gd)}s "
                    f"(means {fmt(gma)}, {fmt(gmb)}){mark}"
                )
        chosen_delta = min(t[0] for t in auto_by_cache.values())
        # Identify which cache/pair produced the global min
        winners = [
            (cache, auto_by_cache[cache])
            for cache in sorted(auto_by_cache.keys())
            if abs(auto_by_cache[cache][0] - chosen_delta) < 1e-15
            or auto_by_cache[cache][0] == chosen_delta
        ]
        wcache, (wd, wa, wb, wma, wmb) = min(winners, key=lambda t: t[1][0])
        print(
            f"headline delta (min across caches): {fmt(chosen_delta)}s "
            f"[{wcache}_{wa} vs {wcache}_{wb}]"
        )
        delta_source = "auto-delta"
    else:
        if args.delta is None or args.delta <= 0:
            raise SystemExit("--delta must be > 0")
        chosen_delta = args.delta
        delta_source = "manual --delta"
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
    print(
        f"chosen delta: {fmt(chosen_delta)}s ({delta_source})  "
        f"alpha: {args.alpha}  power: {args.power}  z_a/2: {z_a}  z_b: {z_b}"
    )

    finite_stds = [(lab, std) for lab, _, _, _, std in cell_stats if math.isfinite(std)]
    if not finite_stds:
        raise SystemExit("no cell has n>=2; cannot estimate sigma")
    worst_label, worst_std = max(finite_stds, key=lambda t: t[1])
    n_rec = n_per_group(worst_std, chosen_delta, z_a, z_b)
    n_ceil = int(math.ceil(n_rec))

    print()
    print("=== Recommended n (most conservative cell std, chosen delta) ===")
    print(f"largest cell std: {fmt(worst_std)}  (cell={worst_label})")
    print(f"n_per_group (continuous): {fmt(n_rec, 2)}")
    print(f"n_per_group (ceil):       {n_ceil}")
    print(
        "Interpretation: equal-n two-sample comparison of means with the "
        "stated alpha/power/delta, using the largest per-cell sample SD as sigma."
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
            for cache in ("cold", "warm"):
                std = cache_stds.get(cache, float("nan"))
                if math.isfinite(std):
                    n_c = n_per_group(std, chosen_delta, z_a, z_b)
                    print(
                        f"  if sized on {cache}-pooled sigma={fmt(std)} at chosen delta: "
                        f"n_per_group ceil={int(math.ceil(n_c))}"
                    )
        else:
            print(
                f"(ratio < {args.variance_ratio_flag:g}x -- single grid-wide n may be defensible; "
                "still review campaign-design section 2 structural cold-pull variance.)"
            )

    # --- sensitivity table: per-cache sigma, multipliers of chosen delta ---
    print()
    print(
        "=== Sensitivity: n_per_group (ceil) vs delta multipliers "
        "(per-cache pooled sigma) ==="
    )
    print(
        "Uses cold-pooled and warm-pooled sample SD separately "
        "(not the single largest-cell sigma). Suitable for methodology tradeoff tables."
    )
    mult_hdr = " ".join(f"{'d*' + fmt(m, 1):>10}" for m in SENSITIVITY_MULTS)
    print(f"{'cache':<8} {'sigma':>10} {mult_hdr}")
    # second header row with absolute deltas
    abs_hdr = " ".join(f"{fmt(chosen_delta * m, 3):>10}" for m in SENSITIVITY_MULTS)
    print(f"{'':<8} {'':>10} {abs_hdr}")
    print("-" * (8 + 10 + 1 + 11 * len(SENSITIVITY_MULTS)))
    for cache in sorted(cache_stds.keys()):
        std = cache_stds[cache]
        if not math.isfinite(std):
            continue
        cells = []
        for m in SENSITIVITY_MULTS:
            d = chosen_delta * m
            n_c = n_per_group(std, d, z_a, z_b)
            cells.append(f"{int(math.ceil(n_c)):>10}")
        print(f"{cache:<8} {fmt(std):>10} {' '.join(cells)}")

    return 0


if __name__ == "__main__":
    raise SystemExit(main())
