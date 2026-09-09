// Package oracle implements the bias-corrected empirical oracle used for Paper 1
// regret: calibration replicates select the oracle cell; evaluation replicates
// measure regret. Bias correction (Smith & Winkler 2006 empirical Bayes) shrinks
// the selected cell's calibration mean toward the grand mean of cell means.
// No Kubernetes API usage.
//
// Primary method (formula implemented below):
//
//	Smith, J.E. & Winkler, R.L. (2006). The Optimizer's Curse: Skepticism and
//	Postdecision Surprise in Decision Analysis. Management Science, 52(3),
//	311–322.
//
// Framing / active literature (not replacements for the formula above):
//
//	van Hasselt, H. (2010). Double Q-learning. Advances in Neural Information
//	Processing Systems (NeurIPS), 23.  — same selection optimism known in RL as
//	"maximization bias," the reason Double Q-learning exists.
//
//	Iyengar, G., Lam, H., & Wang, T. (2023; revised 2025). Optimizer's Information
//	Criterion: Dissecting and Correcting Bias in Data-Driven Optimization.
//	arXiv:2306.10081.  — frames the bias as intimately related to overfitting in
//	machine learning and builds a more general correction on the same idea;
//	evidence the topic remains active after 2006.
package oracle

import (
	"fmt"
	"math"
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
	Cost   baseline.Cost // AggregateFn over the split's costs (mean for SW correction)
}

// CellMoments holds mean and sampling-error variance for Smith–Winkler correction.
type CellMoments struct {
	CellID string
	N      int
	Mean   baseline.Cost
	// SampleVar is the unbiased sample variance of calibration replicates (n-1).
	SampleVar float64
	// EstErrorVar is SampleVar/N (squared standard error of the mean).
	EstErrorVar float64
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

// CellMomentsFromTrials computes per-cell mean and SE² from calibration costs.
// Cells with n < 2 get EstErrorVar = +Inf (undefined sample variance); they
// still participate in selection via mean but block MoM if selected alone without
// peers that have finite SE² — BuildEmpiricalOracle requires usable moments.
func CellMomentsFromTrials(trials []baseline.Trial) []CellMoments {
	by := map[string][]baseline.Cost{}
	for _, t := range trials {
		by[t.CellID] = append(by[t.CellID], t.Cost)
	}
	ids := make([]string, 0, len(by))
	for id := range by {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]CellMoments, 0, len(ids))
	for _, id := range ids {
		xs := by[id]
		n := len(xs)
		var sum float64
		for _, x := range xs {
			sum += float64(x)
		}
		mean := sum / float64(n)
		m := CellMoments{CellID: id, N: n, Mean: baseline.Cost(mean)}
		if n < 2 {
			m.SampleVar = math.NaN()
			m.EstErrorVar = math.Inf(1)
		} else {
			var ss float64
			for _, x := range xs {
				d := float64(x) - mean
				ss += d * d
			}
			m.SampleVar = ss / float64(n-1)
			m.EstErrorVar = m.SampleVar / float64(n)
		}
		out = append(out, m)
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

// SmithWinklerResult is the empirical-Bayes shrinkage of a selected cell mean.
type SmithWinklerResult struct {
	PriorMean           baseline.Cost
	TrueBetweenVariance float64
	Alpha               float64
	Corrected           baseline.Cost
	RawSelected         baseline.Cost
}

// SmithWinklerCorrect applies the Smith & Winkler (2006) empirical Bayes
// correction for optimizer's-curse bias to the selected cell's calibration mean.
//
//	prior_mean = mean of all calibration cell means
//	within_i   = s_i² / n_i
//	τ²         = max(0, Var({cell means}) − mean({within_i}))
//	α          = τ² / (τ² + within_selected)
//	corrected  = prior_mean + α · (raw_selected − prior_mean)
//
// Selection (which cell is "best") is unchanged; only the reported value shrinks.
func SmithWinklerCorrect(selected CellMoments, all []CellMoments) (SmithWinklerResult, error) {
	if len(all) == 0 {
		return SmithWinklerResult{}, fmt.Errorf("oracle: SmithWinklerCorrect requires at least one cell")
	}
	var found bool
	for _, m := range all {
		if m.CellID == selected.CellID {
			found = true
			break
		}
	}
	if !found {
		return SmithWinklerResult{}, fmt.Errorf("oracle: selected cell %q not in moments list", selected.CellID)
	}

	k := float64(len(all))
	var sumMean float64
	for _, m := range all {
		sumMean += float64(m.Mean)
	}
	prior := sumMean / k

	var sumWithin float64
	var nWithin int
	for _, m := range all {
		if math.IsInf(m.EstErrorVar, 0) || math.IsNaN(m.EstErrorVar) {
			continue
		}
		sumWithin += m.EstErrorVar
		nWithin++
	}
	if nWithin == 0 {
		return SmithWinklerResult{}, fmt.Errorf("oracle: need at least one cell with n>=2 for within-cell SE²")
	}
	meanWithin := sumWithin / float64(nWithin)

	// Sample variance of cell means (MoM); with K=1, between-component is 0.
	var tau2 float64
	if len(all) >= 2 {
		var ss float64
		for _, m := range all {
			d := float64(m.Mean) - prior
			ss += d * d
		}
		varMeans := ss / float64(len(all)-1)
		tau2 = math.Max(0, varMeans-meanWithin)
	}

	selWithin := selected.EstErrorVar
	if math.IsInf(selWithin, 0) || math.IsNaN(selWithin) {
		return SmithWinklerResult{}, fmt.Errorf("oracle: selected cell %q needs n>=2 for Smith–Winkler", selected.CellID)
	}

	var alpha float64
	den := tau2 + selWithin
	if den <= 0 {
		alpha = 0
	} else {
		alpha = tau2 / den
	}
	corrected := prior + alpha*(float64(selected.Mean)-prior)

	return SmithWinklerResult{
		PriorMean:           baseline.Cost(prior),
		TrueBetweenVariance: tau2,
		Alpha:               alpha,
		Corrected:           baseline.Cost(corrected),
		RawSelected:         selected.Mean,
	}, nil
}

// OracleResult holds the calibration-selected cell and cost estimates.
type OracleResult struct {
	SelectedCell string
	// CalCost is the raw calibration mean for the selected cell (uncorrected).
	CalCost baseline.Cost
	CalN    int
	// EvalCost is the evaluation aggregate for the same cell (for regret wiring).
	EvalCost baseline.Cost
	EvalN    int

	// BiasCorrectedCost is Smith–Winkler (2006) EB-shrunk calibration mean.
	BiasCorrectedCost baseline.Cost
	Corrected         bool
	CorrectionNote    string

	// Diagnostics from the correction (zero if Corrected is false).
	PriorMean           baseline.Cost
	TrueBetweenVariance float64
	ShrinkageAlpha      float64
}

// BuildEmpiricalOracle estimates per-cell costs on calibration, selects the
// best cell there (lowest mean), applies Smith–Winkler EB bias correction to
// that selected mean, and reports evaluation cost for the same cell.
// Calibration/evaluation replicate separation is unchanged — correction is on
// top of selection, not a substitute for the split.
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

	moments := CellMomentsFromTrials(cal)
	var selMom CellMoments
	for _, m := range moments {
		if m.CellID == best.CellID {
			selMom = m
			break
		}
	}
	sw, err := SmithWinklerCorrect(selMom, moments)
	if err != nil {
		return OracleResult{}, err
	}

	out := OracleResult{
		SelectedCell:        best.CellID,
		CalCost:             best.Cost,
		CalN:                best.N,
		BiasCorrectedCost:   sw.Corrected,
		Corrected:           true,
		PriorMean:           sw.PriorMean,
		TrueBetweenVariance: sw.TrueBetweenVariance,
		ShrinkageAlpha:      sw.Alpha,
		CorrectionNote: "Smith & Winkler (2006) empirical Bayes shrinkage of the " +
			"calibration-selected cell mean toward the grand mean of cell means; " +
			"complementary to (not a replacement for) cal/eval replicate separation.",
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
