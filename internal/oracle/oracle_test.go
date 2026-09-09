package oracle_test

import (
	"math"
	"math/rand"
	"testing"

	"reloc-disrupt/internal/baseline"
	"reloc-disrupt/internal/oracle"
)

// Synthetic multi-cell, multi-replicate fixture. Swap for real calibration/
// evaluation loaders under experiments/results/ when campaign data lands.
func synthCampaign() []baseline.Trial {
	return []baseline.Trial{
		{ID: "a1", CellID: "cell_a", Replicate: 1, Cost: 3.0},
		{ID: "a2", CellID: "cell_a", Replicate: 2, Cost: 5.0},
		{ID: "a3", CellID: "cell_a", Replicate: 3, Cost: 4.0},
		{ID: "b1", CellID: "cell_b", Replicate: 1, Cost: 9.0},
		{ID: "b2", CellID: "cell_b", Replicate: 2, Cost: 11.0},
		{ID: "b3", CellID: "cell_b", Replicate: 3, Cost: 10.0},
	}
}

func TestPartitionDisjoint(t *testing.T) {
	trials, err := oracle.PartitionByReplicate(synthCampaign(), []int{1, 2}, []int{3})
	if err != nil {
		t.Fatal(err)
	}
	var nCal, nEval int
	for _, tr := range trials {
		switch tr.Split {
		case oracle.SplitCalibration:
			nCal++
		case oracle.SplitEvaluation:
			nEval++
		default:
			t.Fatalf("unset split on %s", tr.ID)
		}
	}
	if nCal != 4 || nEval != 2 {
		t.Fatalf("cal=%d eval=%d", nCal, nEval)
	}
}

func TestPartitionRejectsOverlap(t *testing.T) {
	_, err := oracle.PartitionByReplicate(synthCampaign(), []int{1, 2}, []int{2, 3})
	if err == nil {
		t.Fatal("expected overlap error")
	}
}

func TestBuildEmpiricalOracleSelectsBestOnCalibration(t *testing.T) {
	trials, err := oracle.PartitionByReplicate(synthCampaign(), []int{1, 2}, []int{3})
	if err != nil {
		t.Fatal(err)
	}
	res, err := oracle.BuildEmpiricalOracle(trials, baseline.AggregateMean)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedCell != "cell_a" {
		t.Fatalf("selected %q want cell_a", res.SelectedCell)
	}
	if math.Abs(res.CalCost-4.0) > 1e-9 {
		t.Fatalf("CalCost=%v want 4", res.CalCost)
	}
	if res.EvalN != 1 || math.Abs(res.EvalCost-4.0) > 1e-9 {
		t.Fatalf("EvalCost=%v n=%d", res.EvalCost, res.EvalN)
	}
	if !res.Corrected {
		t.Fatal("expected Corrected=true after Smith–Winkler")
	}
	// Two cells: means 4 and 10 → prior=7; shrinkage pulls selected (4) toward 7.
	if !(res.BiasCorrectedCost > res.CalCost && res.BiasCorrectedCost < res.PriorMean+1e-9) {
		t.Fatalf("expected shrinkage toward prior: raw=%v corrected=%v prior=%v alpha=%v",
			res.CalCost, res.BiasCorrectedCost, res.PriorMean, res.ShrinkageAlpha)
	}
}

func TestOptimizersCurseSelectionUsesCalibrationOnly(t *testing.T) {
	trials := []baseline.Trial{
		{ID: "a1", CellID: "cell_a", Replicate: 1, Cost: 8.0},
		{ID: "a2", CellID: "cell_a", Replicate: 2, Cost: 9.0},
		{ID: "a3", CellID: "cell_a", Replicate: 3, Cost: 3.0},
		{ID: "b1", CellID: "cell_b", Replicate: 1, Cost: 4.0},
		{ID: "b2", CellID: "cell_b", Replicate: 2, Cost: 5.0},
		{ID: "b3", CellID: "cell_b", Replicate: 3, Cost: 12.0},
	}
	part, err := oracle.PartitionByReplicate(trials, []int{1, 2}, []int{3})
	if err != nil {
		t.Fatal(err)
	}
	res, err := oracle.BuildEmpiricalOracle(part, baseline.AggregateMean)
	if err != nil {
		t.Fatal(err)
	}
	if res.SelectedCell != "cell_b" {
		t.Fatalf("cal argmin should pick cell_b, got %q", res.SelectedCell)
	}
	if !(res.EvalCost > res.CalCost) {
		t.Fatalf("classic optimism: eval (%v) should exceed lucky cal (%v)", res.EvalCost, res.CalCost)
	}
	if !res.Corrected {
		t.Fatal("expected Corrected=true")
	}
	// Lucky low cal mean should shrink upward toward prior.
	if !(res.BiasCorrectedCost > res.CalCost) {
		t.Fatalf("correction should reduce optimism: raw=%v corrected=%v", res.CalCost, res.BiasCorrectedCost)
	}
}

func TestSmithWinklerPullsTowardPriorWhenWithinNoiseDominates(t *testing.T) {
	// Identical true structure: large within-cell noise, similar means → τ²≈0 → α≈0 → corrected≈prior.
	all := []oracle.CellMoments{
		{CellID: "a", N: 10, Mean: 5.0, SampleVar: 100, EstErrorVar: 10},
		{CellID: "b", N: 10, Mean: 5.2, SampleVar: 100, EstErrorVar: 10},
		{CellID: "c", N: 10, Mean: 4.8, SampleVar: 100, EstErrorVar: 10},
	}
	sw, err := oracle.SmithWinklerCorrect(all[0], all)
	if err != nil {
		t.Fatal(err)
	}
	if sw.Alpha > 0.2 {
		t.Fatalf("expected strong shrinkage when between << within; alpha=%v tau2=%v", sw.Alpha, sw.TrueBetweenVariance)
	}
	if math.Abs(float64(sw.Corrected-sw.PriorMean)) > 0.5 {
		t.Fatalf("corrected should sit near prior: %v vs prior %v", sw.Corrected, sw.PriorMean)
	}
}

// TestSmithWinklerReducesOptimisticError constructs cells with known true means
// and Gaussian noise. On trials where calibration selects a cell that looks
// better than its true mean (optimizer's curse for minimization), the
// Smith–Winkler corrected estimate is closer to the true mean than the raw mean,
// on average across Monte Carlo replications.
func TestSmithWinklerReducesOptimisticError(t *testing.T) {
	trueMeans := map[string]float64{
		"c0": 5.0,
		"c1": 5.5,
		"c2": 6.0,
		"c3": 6.5,
		"c4": 7.0,
		"c5": 8.0,
	}
	const nRep = 8
	const nMC = 400
	rng := rand.New(rand.NewSource(42))

	var sumRawErr, sumCorrErr float64
	var nOptimistic int

	for trial := 0; trial < nMC; trial++ {
		var cal []baseline.Trial
		rep := 1
		for id, mu := range trueMeans {
			for r := 0; r < nRep; r++ {
				cal = append(cal, baseline.Trial{
					ID:        id,
					CellID:    id,
					Replicate: rep,
					Split:     oracle.SplitCalibration,
					Cost:      mu + rng.NormFloat64()*1.5, // σ=1.5 noise
				})
				rep++
			}
		}
		moments := oracle.CellMomentsFromTrials(cal)
		est := make([]oracle.CellEstimate, len(moments))
		for i, m := range moments {
			est[i] = oracle.CellEstimate{CellID: m.CellID, N: m.N, Cost: m.Mean}
		}
		best, err := oracle.SelectBestCell(est)
		if err != nil {
			t.Fatal(err)
		}
		var sel oracle.CellMoments
		for _, m := range moments {
			if m.CellID == best.CellID {
				sel = m
				break
			}
		}
		sw, err := oracle.SmithWinklerCorrect(sel, moments)
		if err != nil {
			t.Fatal(err)
		}
		trueMu := trueMeans[best.CellID]
		// Optimistic for cost minimization: raw mean below true mean.
		if float64(sel.Mean) < trueMu {
			nOptimistic++
			sumRawErr += math.Abs(float64(sel.Mean) - trueMu)
			sumCorrErr += math.Abs(float64(sw.Corrected) - trueMu)
		}
	}
	if nOptimistic < nMC/10 {
		t.Fatalf("too few optimistic selections (%d/%d) to test correction", nOptimistic, nMC)
	}
	meanRaw := sumRawErr / float64(nOptimistic)
	meanCorr := sumCorrErr / float64(nOptimistic)
	if !(meanCorr < meanRaw) {
		t.Fatalf("expected corrected MAE < raw MAE on optimistic selections: raw=%v corr=%v (n=%d)",
			meanRaw, meanCorr, nOptimistic)
	}
	t.Logf("optimistic selections %d/%d: MAE raw=%.4f corrected=%.4f", nOptimistic, nMC, meanRaw, meanCorr)
}
