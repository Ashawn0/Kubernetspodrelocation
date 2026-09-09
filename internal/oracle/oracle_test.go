package oracle_test

import (
	"math"
	"testing"

	"reloc-disrupt/internal/baseline"
	"reloc-disrupt/internal/oracle"
)

// Synthetic multi-cell, multi-replicate fixture. Swap for real calibration/
// evaluation loaders under experiments/results/ when campaign data lands.
func synthCampaign() []baseline.Trial {
	// Cell A genuinely better than B; cal and eval both reflect that, with noise.
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
	// cal reps 1,2 for cell_a: mean (3+5)/2 = 4
	if math.Abs(res.CalCost-4.0) > 1e-9 {
		t.Fatalf("CalCost=%v want 4", res.CalCost)
	}
	// eval rep 3 for cell_a: 4.0
	if res.EvalN != 1 || math.Abs(res.EvalCost-4.0) > 1e-9 {
		t.Fatalf("EvalCost=%v n=%d", res.EvalCost, res.EvalN)
	}
	if res.Corrected {
		t.Fatal("bias correction must remain open (Corrected=false)")
	}
	if res.BiasCorrectedCost != res.CalCost {
		t.Fatal("placeholder corrected cost should equal CalCost until formula is chosen")
	}
	if res.CorrectionNote == "" {
		t.Fatal("expected OPEN DECISION note")
	}
}

func TestOptimizersCurseSelectionUsesCalibrationOnly(t *testing.T) {
	// Calibration alone makes cell_b look better (lucky low draws); evaluation
	// shows cell_a is better. Oracle must still select cell_b from cal only.
	trials := []baseline.Trial{
		{ID: "a1", CellID: "cell_a", Replicate: 1, Cost: 8.0},
		{ID: "a2", CellID: "cell_a", Replicate: 2, Cost: 9.0},
		{ID: "a3", CellID: "cell_a", Replicate: 3, Cost: 3.0}, // eval: actually good
		{ID: "b1", CellID: "cell_b", Replicate: 1, Cost: 4.0},
		{ID: "b2", CellID: "cell_b", Replicate: 2, Cost: 5.0},
		{ID: "b3", CellID: "cell_b", Replicate: 3, Cost: 12.0}, // eval: actually bad
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
}
