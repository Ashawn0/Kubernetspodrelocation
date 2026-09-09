// Package oracle scaffolds the bias-corrected empirical oracle used for Paper 1
// regret: calibration replicates select / estimate the oracle; evaluation
// replicates measure regret. No Kubernetes API usage.
package oracle

import (
	"fmt"
	"sort"

	"reloc-disrupt/internal/baseline"
)

const (
	SplitCalibration = "calibration"
	SplitEvaluation  = "evaluation"
)

// PartitionByReplicate assigns Split labels from replicate ID sets.
// Calibration and evaluation replicate sets must be non-empty and disjoint.
func PartitionByReplicate(trials []baseline.Trial, calReps, evalReps []int) ([]baseline.Trial, error) {
	if len(calReps) == 0 || len(evalReps) == 0 {
		return nil, fmt.Errorf("oracle: calibration and evaluation replicate sets must both be non-empty")
	}
	cal := toSet(calReps)
	eval := toSet(evalReps)
	for r := range cal {
		if eval[r] {
			return nil, fmt.Errorf("oracle: replicate %d appears in both calibration and evaluation (optimizer's-curse guard)", r)
		}
	}
	out := make([]baseline.Trial, len(trials))
	for i, t := range trials {
		switch {
		case cal[t.Replicate]:
			t.Split = SplitCalibration
		case eval[t.Replicate]:
			t.Split = SplitEvaluation
		default:
			return nil, fmt.Errorf("oracle: trial %q replicate %d not in calibration or evaluation sets", t.ID, t.Replicate)
		}
		out[i] = t
	}
	return out, nil
}

func toSet(xs []int) map[int]bool {
	m := map[int]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// FilterSplit returns trials with the given Split label.
func FilterSplit(trials []baseline.Trial, split string) []baseline.Trial {
	var out []baseline.Trial
	for _, t := range trials {
		if t.Split == split {
			out = append(out, t)
		}
	}
	return out
}

// CellEstimate is the empirical cost summary for one state-cell on one split.
type CellEstimate struct {
	CellID string
	N      int
	Cost   baseline.Cost // AggregateFn over the split's costs
}

// EstimateCells aggregates costs per CellID within trials (typically one split).
func EstimateCells(trials []baseline.Trial, agg baseline.AggregateFn) []CellEstimate {
	if agg == nil {
		agg = baseline.AggregateMean
	}
	by := map[string][]baseline.Cost{}
	for _, t := range trials {
		by[t.CellID] = append(by[t.CellID], t.Cost)
	}
	ids := make([]string, 0, len(by))
	for id := range by {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]CellEstimate, 0, len(ids))
	for _, id := range ids {
		cs := by[id]
		out = append(out, CellEstimate{CellID: id, N: len(cs), Cost: agg(cs)})
	}
	return out
}

// SelectBestCell returns the cell with lowest aggregate cost on the provided
// estimates (intended: calibration-only). This is the optimizer's-curse step:
// argmin over noisy cell means is optimistically biased relative to evaluation.
func SelectBestCell(estimates []CellEstimate) (CellEstimate, error) {
	if len(estimates) == 0 {
		return CellEstimate{}, fmt.Errorf("oracle: SelectBestCell requires at least one cell")
	}
	best := estimates[0]
	for _, e := range estimates[1:] {
		if e.Cost < best.Cost {
			best = e
		}
	}
	return best, nil
}

// OracleResult holds the calibration-selected cell and cost estimates.
type OracleResult struct {
	SelectedCell string
	// CalCost is the calibration aggregate for the selected cell (uncorrected).
	CalCost baseline.Cost
	CalN    int
	// EvalCost is the evaluation aggregate for the same cell (for regret wiring).
	// Zero and EvalN=0 if evaluation rows for that cell were absent.
	EvalCost baseline.Cost
	EvalN    int

	// BiasCorrectedCost is the oracle cost after bias correction.
	// OPEN DECISION: formula not finalized (options include Gaussian selection
	// bias adjustments, cross-fit / holdout correction, bootstrap optimism, …).
	// Until decided, BiasCorrectedCost == CalCost and Corrected == false.
	BiasCorrectedCost baseline.Cost
	Corrected         bool
	CorrectionNote    string
}

// BuildEmpiricalOracle estimates per-cell costs on calibration, selects the
// best cell there, reports evaluation cost for that cell, and leaves bias
// correction as an explicit open decision (identity placeholder).
func BuildEmpiricalOracle(trials []baseline.Trial, agg baseline.AggregateFn) (OracleResult, error) {
	if agg == nil {
		agg = baseline.AggregateMean
	}
	cal := FilterSplit(trials, SplitCalibration)
	eval := FilterSplit(trials, SplitEvaluation)
	if len(cal) == 0 {
		return OracleResult{}, fmt.Errorf("oracle: no calibration trials")
	}
	calEst := EstimateCells(cal, agg)
	best, err := SelectBestCell(calEst)
	if err != nil {
		return OracleResult{}, err
	}
	out := OracleResult{
		SelectedCell:      best.CellID,
		CalCost:           best.Cost,
		CalN:              best.N,
		BiasCorrectedCost: best.Cost,
		Corrected:         false,
		CorrectionNote: "OPEN DECISION: bias-correction formula not finalized; " +
			"returning uncorrected calibration aggregate (Corrected=false). " +
			"Do not treat BiasCorrectedCost as optimism-adjusted until a method is chosen.",
	}
	for _, e := range EstimateCells(eval, agg) {
		if e.CellID == best.CellID {
			out.EvalCost = e.Cost
			out.EvalN = e.N
			break
		}
	}
	return out, nil
}
