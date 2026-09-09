# Campaign design (canonical)

Design decisions for Paper 1’s experimental campaign and pilot harness, consolidated from the post–Stage 0 session that landed `cmd/campaign/trialrunner` pilot mode and the offline baseline/oracle scaffolds. This file is the **canonical reference** for grid, isolation, and open statistical choices.

Bug-by-bug trails, false starts, and fix narratives stay in [`docs/implementation-log.md`](implementation-log.md). Fixed research invariants stay in [`docs/research-design.md`](research-design.md) and [`docs/measurement-spec.md`](measurement-spec.md).

---

## 1. Local-VM pilot / campaign grid

**Cells (6):** cross of

| Axis | Levels |
| --- | --- |
| Image-cache state | `cold`, `warm` (ground-truth `reloc/app-a:v1` via in-cluster registry; same imageprobe setup) |
| CPU-PSI stress | `none`; `threshold` (≈2:1 stress-ng workers:vCPU); `high` (≈3:1) |

**Pilot schedule (harness default):** 15 replicates × 6 cells = **90 trials**, full order randomized with a seeded PRNG (`-seed`, always logged), `split=pilot`. Smoke / diagnostic batches have used `-replicates 3` (18 trials) with `seed=42`.

**Scope of this grid.** Only axes that isolate cleanly on the Multipass local-VM path. Registry-network shaping (independent of pod networking) is **AWS-only** and is designed in **§7** (proposed; replicate counts pending). IO PSI remains measurement-capable on AWS (dedicated NVMe / instance-store) but is **not** part of the §7 network grid.

**Output.** One file per pilot run: `experiments/results/pilot/pilot-variance-<YYYYMMDD>.csv` (trial ID, cell params, TTFS clocks, PSI landed + pretrial baseline, thermal flags, …).

---

## 2. Empirical finding: cold TTFS variance ≫ warm

Across **two** 18-trial diagnostic pilots (`seed=42` lineage), **cold-cell** ClusterIP TTFS spanned roughly **~2.5–15.3 s**, while **warm-cell** TTFS stayed roughly **~2.2–3.3 s**.

**Interpretation.** Structural, not “noise to average away”: cold cells force a real registry/network image pull; warm cells start from a local present image. Pull latency and path variability dominate cold spreads.

**Sizing implication (candidate policy).** Prefer **per–image-cache-state** replicate / power sizing (cold vs warm separately) over a **single pooled** variance estimate across all six cells. Final *n* still pending (§6).

---

## 3. Memory PSI: infrastructure only

**Decision:** memory PSI is **excluded as a predictor feature** for Paper 1’s learned model / feature set.

**Retained:** measurement and stress plumbing (`psiprobe`, trialrunner dry-run ExtraPSI memory path, AWS memory isolation path) so the quantity remains observable and documented.

**Empirical basis (Stage 0, not re-litigated here):** shallow or zero stall under real stress once page-cache reclaim dominates — see implementation-log entry 5 (local-VM) and §14 / memory subsection (AWS zero-signal after readiness fixes). CPU PSI remains the contention axis in the local-VM grid (§1).

---

## 4. Trial isolation (harness fixes this session)

Randomized trial order made cross-trial contamination visible; the following are **in** `trialrunner` and stay. Detail: implementation-log **§17–§19**.

| Fix | What it does |
| --- | --- |
| **Synchronous teardown (§17)** | `TerminationGracePeriodSeconds: 0` on stress pods and trialrunner UID pods; force-delete with `GracePeriodSeconds: 0`; poll until `Get` → NotFound (15 s bound, hard error). Disruption Delete of the old pod still passes an explicit grace for preStop overlap. |
| **PSI cooldown (§18)** | After teardown, poll host CPU `avg10` every ~2 s until `< 3.0` or 30 s timeout (warn + proceed). Then record `psi_cpu_avg10_pretrial_baseline`. Distinct from leftover-pod races: `avg10` is a ~10 s decaying average. |
| **Stress dwell (§18)** | After stress pod Running (and host `stress-ng` ready for memory), dwell **12 s** (was 3 s) before landed PSI / disrupt setup so `avg10` approaches steady state. |
| **Thermal recording (§19)** | Per trial: count Microsoft-Windows-Kernel-Processor-Power **event ID 37** in the trial wall window (`Get-WinEvent`). Fields: `thermal_throttle_events_during_trial`, `thermal_throttle_detected`. Record-only — no auto-exclude. **Not** Get-Counter “% of Maximum Frequency” (flat ~90% on this Hyper-V host). |

**Thermal in diagnostics.** Manual log checks and automatic recording: throttling is real on this host historically, but **neither** 18-trial diagnostic run showed ID 37 **inside** the run windows; heat was ruled out for those specific elevated-TTFS outliers. Keep recording for the full pilot/campaign.

---

## 4b. Host-level CPU contention (hypervisor / steal-time blind spot)

**Limitation.** Guest-internal Linux PSI (what `nodeobs` / trialrunner sample inside Multipass workers) does **not** observe hypervisor scheduling contention between VMs and host processes — often discussed as **steal time**. Browser, IDE, and other Windows-host activity on the same physical machine can lengthen TTFS without moving guest `avg10` at all.

**Harness response.** Each trial records informational `host_cpu_pct_start` and `host_cpu_pct_end` from the Windows parent via `Get-Counter '\Processor(_Total)\% Processor Time'` (bookend samples; not a gate, delay policy, or fail condition). Analysis may treat them as covariates or exclusion flags.

**Practical campaign mitigation.** Minimize host foreground load while trials run; use the new fields to detect residual contention that could not be avoided. Distinct from thermal ID 37 (§4) and from guest PSI cooldown/dwell (§4).

---

## 5. Bias correction and baselines (oracle)

**Baselines (for regret / GapCaptured later):** `fixed-cost` and `ImageLocality`, sharing `baseline.Predictor` with the eventual LightGBM fit. Calibration vs evaluation replicate sets must remain disjoint (`oracle.PartitionByReplicate`).

**Optimizer's-curse bias correction — resolved.** The calibration-selected cell's raw mean is shrunk with the **Smith & Winkler (2006)** empirical Bayes correction (implemented in `internal/oracle`; `Corrected=true`):

- prior = grand mean of calibration cell means  
- within-cell estimation-error variance = sample variance / *n*  
- τ² = max(0, Var(cell means) − mean(within-cell SE²))  
- α = τ² / (τ² + within_selected)  
- corrected = prior + α · (raw_selected − prior)

This directly addresses post-decision optimism from selecting the apparent best cell on noisy estimates. It is **complementary to**, not a replacement for, the calibration/evaluation replicate split already required by `docs/research-design.md` (split blocks using the same draws for selection and regret; Smith–Winkler adjusts the selected calibration value).

**Citations (tracked in [`docs/references.md`](references.md)):**

1. **Smith & Winkler (2006)** — primary; formula actually implemented.  
2. **van Hasselt (2010), Double Q-learning (NeurIPS)** — same phenomenon as RL **maximization bias** (name many systems/ML-adjacent reviewers recognize).  
3. **Iyengar, Lam & Wang (2023/2025), arXiv:2306.10081** — active follow-on; frames the bias as intimately related to overfitting in ML.

**Still open:** ImageLocality → cost map (affine OLS placeholder in `internal/baseline`; may need isotonic or cold/warm table once campaign data exists). Mean vs median aggregation remains swappable via `AggregateFn` (default **mean**).

---

## 6. Replicate sizing (pending final *n*)

**Not decided** for the full campaign. Power-size after the **90-trial variance pilot** completes (`-mode pilot -replicates 15 -seed 42`). Use pilot cell variances with the cold/warm structural split (§2) in mind. Until then: 15/cell is the harness default for that pilot only — not the final campaign *n*.

**Tool:** [`scripts/power_analysis.py`](../scripts/power_analysis.py) — stdlib-only. Loads a trialrunner pilot CSV, prints per-cell and cold/warm-pooled mean/variance/std of `ttfs_clusterip_sec`, then recommends equal-*n* two-sample size

`n = 2 (z_{α/2} + z_β)² σ² / δ²`

with defaults **α = 0.05**, **power = 0.9** (not 0.8; project rigor). Headline σ = **largest** per-cell sample SD. Flags cold/warm variance ratios ≥ 3× as evidence for per-cache-state sizing.

**Delta modes (mutually exclusive):**

| Mode | Flag | Meaning |
| --- | --- | --- |
| Manual | `--delta <seconds>` | Explicit minimum detectable effect (still valid; use when the scientific gap is known a priori). |
| Auto | `--auto-delta` | Within each cache state, take min \|mean diff\| between **adjacent** PSI levels (`none`–`threshold`, `threshold`–`high`); print the winning pair per cache; headline δ = the smaller of those cache minima. |

**Sensitivity table (always printed):** for δ multipliers 0.5× / 1× / 2× / 3× of the chosen δ, recommended *n* using **cold-pooled** and **warm-pooled** σ separately (methodology tradeoff table — not the single largest-cell headline).

```text
python scripts/power_analysis.py experiments/results/pilot/pilot-variance-YYYYMMDD.csv --delta 1.0
python scripts/power_analysis.py experiments/results/pilot/pilot-variance-YYYYMMDD.csv --auto-delta
```

---

## 7. AWS network-slowdown axis (proposed)

**Status:** proposed. Cell layout below is fixed in intent; **replicate count per cell is pending** final local-VM power analysis (`scripts/power_analysis.py` on the completed 90-trial pilot). Do not treat *n* as decided until that run lands.

This axis is the campaign’s registry-path / relocation-distance covariate on AWS (Stage 0 already closed IO PSI and registry-path netprobe with measured deltas). Local-VM cannot isolate registry from pod networking on one NIC.

### Network condition levels

Four levels, grounded in real-world AWS latency scales (not arbitrary `tc` knobs):

| Level | Intent | Approx. added impairment | Real-world analogue |
| --- | --- | --- | --- |
| **0 — control** | No added slowdown | (baseline path) | Relocation within the same data-center building |
| **1 — mild** | Small added delay | ~**1–3 ms** | Different building, same metro (**cross-AZ, same region**) |
| **2 — moderate** | Cross-region scale | ~**80–100 ms** | Different AWS region on the **same continent** |
| **3 — severe** | Distant / degraded | ~**150–250 ms** delay **plus** a bandwidth cap | Distant region, or congested/degraded registry path |

**Latency citations (order-of-magnitude, for design justification):**

- **Cross-AZ (same region):** AWS documents **single-digit millisecond** AZ-to-AZ latency. Independent third-party measurement of AZ pairs is consistent with that claim: **most pairs under ~1 ms**, with slower outliers around **~2–2.4 ms** — hence Level 1’s ~1–3 ms band rather than tens of milliseconds.
- **Cross-region:** public measurements and the **speed-of-light floor** on long-haul paths put typical inter-region RTTs in the **~80–250 ms+** range depending on distance — hence Levels 2 and 3.

Exact `tc` parameters for campaign levels live in `internal/netshape` (extracted from Stage 0 netprobe). Live re-apply on AWS is **unverified** until the next provision — see §9.

### Cold-only crossing (do not waste warm × network cells)

Network levels are applied **only to cold** trials (replacement must **fetch** image layers from the registry). **Warm** trials are **excluded** from this axis: the image is already local, so registry-path slowdown cannot change the pull outcome; running warm × network cells would spend AWS budget on trials with no information gain for this covariate.

### CPU stress on AWS (reduced ladder)

Local-VM fully characterizes three CPU-PSI levels (`none` / `threshold` / `high`). On AWS this axis is only a **re-check for interaction** with network conditions, not a re-measurement of CPU-PSI from scratch. Use **two** points:

- `none`
- `high` (≈3:1 oversubscription)

**Skip** `threshold` on the AWS network grid.

### AWS cell count

| Factor | Levels | Count |
| --- | --- | --- |
| Network | 0 / 1 / 2 / 3 | 4 |
| CPU-PSI | none / high | 2 |
| Image cache | **cold only** | 1 |

**Total: 4 × 2 = 8 cells.** Replicates per cell: **TBD** after §6 power analysis on the real 90-trial local-VM pilot (cold-side variance is the relevant σ family for this grid).

---

## 8. Go → Python handoff (oracle / baseline export → GapCaptured)

**Why the split.** Trial collection, `fixed-cost` / `ImageLocality`, calibration/evaluation partition, and the Smith–Winkler-corrected empirical oracle are Go (`internal/baseline`, `internal/oracle`, `internal/evalexport`). The LightGBM quantile predictor and the headline metric live in Python because that is where the LightGBM library and analysis tooling sit (`analysis/src/relocdisrupt/lgbm.py`, `regret.py`).

**Handoff file.** Go writes a single JSON dump (not a new abstraction layer):

`experiments/results/{run}/oracle-baseline-export.json`

Contents: per-cell calibration/evaluation means, baseline predictions, and the selected cell’s corrected oracle cost (plus diagnostics). Python `relocdisrupt.regret` loads that path, merges LightGBM per-cell predictions, and computes:

`GapCaptured = 1 − R_eval(learned) / R_eval(best-existing-baseline)`

with `R_eval(π) = eval_mean(cell chosen by π) − oracle_corrected`.

**Baseline-selection rule (resolved — same split discipline as the oracle).** “Best-existing-baseline” is **not** whichever of `fixed-cost` / `ImageLocality` looks better on the evaluation set. Choosing the comparator with evaluation outcomes is the same *category* of selection bias the Smith–Winkler / cal–eval split addresses for the oracle (post-decision optimism from picking the apparent winner on the same draws used to score).

- **Selection (calibration only):** for each baseline π, `R_cal(π) = cal_mean(chosen_by_π) − oracle_corrected`. Best-existing-baseline = argmin of those cal regrets (name tie-break: lexicographic).  
- **Scoring (evaluation only):** compute `R_eval` for the learned model and for that **already-chosen** baseline; form GapCaptured from those two numbers. Evaluation never re-ranks which baseline is the denominator.

Pointing tests or analysis at a real campaign export is a path change only.

---

## 9. Campaign mode vs pilot mode (`trialrunner -mode campaign`)

**Pilot** (`-mode pilot`) is a variance-estimation harness: fixed 6-cell local-VM grid × one `-replicates` count, `split=pilot`, single CSV under `experiments/results/pilot/`. It deliberately does **not** partition calibration vs evaluation.

**Campaign** (`-mode campaign -campaign-config …`) is the real experiment runner once per-cell *n* are finalized:

| | Pilot | Campaign |
| --- | --- | --- |
| Grid | Hardcoded 6 local-VM cells | JSON config: `local_vm_cells` + `aws_cells` |
| Replicates | One `-replicates` for all cells | Per-cell `calibration_n` and `evaluation_n` |
| `split` field | `pilot` | `calibration` or `evaluation` |
| Output | `experiments/results/pilot/pilot-variance-<date>.csv` | `experiments/results/calibration/campaign-<date>.csv` **and** `…/evaluation/campaign-<date>.csv` |
| Execution order | Seeded full-list shuffle | Same: build full list, assign splits by replicate index, **then** seeded shuffle (not blocked by cell or by split) |

Trial execution is shared: campaign calls the same `runOneTrial` as pilot (synchronous teardown, PSI cooldown, stress dwell, thermal ID 37, host-CPU bookends). No reimplementation.

### Held-out cells (generalization)

Optional per-cell `"held_out": true`:

- Forces **calibration_n = 0** regardless of the number written in the config.
- **All** trials for that cell are labeled `evaluation`.
- Predictor training (Fit) must never include that cell — evaluation of it is a **generalization** check (unseen condition), not interpolation within cells seen at fit time.
- Do not misread this as “fewer calibration replicates”; it is zero calibration by design.

### Config shape

Example (placeholder counts only — see `experiments/config/README.md`):

```json
{
  "local_vm_cells": [
    {
      "image_cache_state": "cold",
      "cpu_psi_level": "none",
      "calibration_n": 1,
      "evaluation_n": 1,
      "held_out": false
    }
  ],
  "aws_cells": [
    {
      "network_level": 0,
      "cpu_psi_level": "none",
      "calibration_n": 1,
      "evaluation_n": 1,
      "held_out": false
    }
  ]
}
```

- Local-VM: `image_cache_state` ∈ {cold, warm}; `cpu_psi_level` ∈ {none, threshold, high}.
- AWS (§7): `network_level` ∈ {0,1,2,3}; `cpu_psi_level` ∈ {none, high} only; image cache is cold by construction.
- Split assignment: for each cell, replicate indices `1 … calibration_n` → calibration; `calibration_n+1 … calibration_n+evaluation_n` → evaluation (after held_out forcing).

Ship path: `experiments/config/campaign-config.example.json`.

### AWS network shaping in campaign mode (extracted from netprobe)

Campaign AWS cells call `internal/netshape` during trial setup (same sequence as cache state / CPU stress): `tc` on the registry secondary iface only (`ens6` default), never the primary CNI iface. Levels map to §7 (0 clear / 1 ~2 ms delay / 2 100 ms delay / 3 200 ms + 20 Mbit tbf). Teardown uses `ClearAndWait` — poll `tc qdisc show` until no `netem`/`tbf`, not fire-and-forget `tc qdisc del`.

**Live status: UNVERIFIED tonight.** AWS is torn down between sessions; unit tests cover config validation and tc command construction (including the netprobe-validated tbf+netem stack). Do not claim Stage-0-closed live application for campaign wiring until the next AWS provision re-runs shaping end-to-end. `netshape_live_verified` is recorded `false` on campaign trial detail until that happens.

AWS config has **no** `image_cache_state` field (`DisallowUnknownFields` rejects warm); cells are cold-only by construction.

---

## Related code

| Path | Role |
| --- | --- |
| `cmd/campaign/trialrunner` | dryrun + pilot + **campaign**; isolation, cooldown, dwell, thermal, host CPU; config schedule; AWS netshape hook |
| `internal/netshape` | reusable registry-ENI tc apply/clear (from netprobe); §7 levels; clear-verify poll |
| `experiments/config/campaign-config.example.json` | illustrative campaign grid (fake *n*) |
| `internal/baseline` | `fixed-cost`, `ImageLocality`, shared `Predictor` |
| `internal/oracle` | cal/eval partition + Smith–Winkler (2006) EB bias correction |
| `internal/evalexport` | JSON dump of baseline preds + corrected oracle for Python |
| `docs/references.md` | load-bearing citation tracking (Methodology / Related Work) |
| `analysis/src/relocdisrupt/lgbm.py` | LightGBM Q50/Q95 log-cost predictor scaffold |
| `analysis/src/relocdisrupt/regret.py` | load export → regret → GapCaptured |
| `scripts/power_analysis.py` | pilot CSV → variance tables + recommended *n* |
| `experiments/results/pilot/` | pilot CSV outputs (gitignored contents; live runs — leave alone) |
| `experiments/results/calibration/` | campaign calibration CSV |
| `experiments/results/evaluation/` | campaign evaluation CSV |
| `paper/main.tex` | CCGrid draft (Overleaf Intro/RW/Method + Results/Discussion shells) |
