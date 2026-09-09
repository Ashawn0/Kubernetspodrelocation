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

**Scope of this grid.** Only axes that isolate cleanly on the Multipass local-VM path. **Not yet grid-designed** (AWS-only Stage 0 gates, still out of the local pilot cell cross):

- Registry-network shaping (independent of pod networking)
- IO PSI (dedicated NVMe / instance-store path)

Those remain measurement-capable on AWS; folding them into the campaign grid is a later design pass.

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

## 5. Open statistical decisions

Scaffolded in code; **not** finalized:

| Topic | Current placeholder | Location |
| --- | --- | --- |
| **Oracle bias correction** | Identity: `BiasCorrectedCost == CalCost`, `Corrected=false`, explicit OPEN note (optimizer’s-curse selection still uses calibration-only argmin) | `internal/oracle` |
| **ImageLocality → cost map** | After faithful kube-scheduler score (v1.30 thresholds / spread), affine OLS `cost ≈ a + b·(1 − score/100)`. May need isotonic or cold/warm table mapping once campaign data exists | `internal/baseline` |

Do **not** invent formulas in analysis papers until these are closed. Mean vs median aggregation is swappable via `AggregateFn` (default **mean**) for fixed-cost and cell estimates.

**Baselines (for regret / GapCaptured later):** `fixed-cost` and `ImageLocality`, sharing `baseline.Predictor` with the eventual LightGBM fit. Calibration vs evaluation replicate sets must remain disjoint (`oracle.PartitionByReplicate`).

---

## 6. Pending: full-campaign replicate count

**Not decided.** Power-size *n* after the **90-trial variance pilot** completes (`-mode pilot -replicates 15 -seed 42`, or the same seed’s full run when finished). Use pilot cell variances with the cold/warm structural split (§2) in mind.

Until then: 15/cell is the harness default for that pilot only — not the final campaign *n*.

---

## Related code

| Path | Role |
| --- | --- |
| `cmd/campaign/trialrunner` | dryrun + pilot; isolation, cooldown, dwell, thermal, CSV |
| `internal/baseline` | `fixed-cost`, `ImageLocality`, shared `Predictor` |
| `internal/oracle` | cal/eval partition, empirical oracle scaffold |
| `experiments/results/pilot/` | pilot CSV outputs (gitignored contents) |
