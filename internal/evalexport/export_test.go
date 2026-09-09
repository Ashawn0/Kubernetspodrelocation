package evalexport_test

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"reloc-disrupt/internal/baseline"
	"reloc-disrupt/internal/evalexport"
)

func warmFeat() baseline.Features {
	return baseline.Features{
		UncachedBytes: 0,
		PresentImages: []baseline.ImagePresence{{Name: "reloc/app-a:v1", Size: 500 * 1024 * 1024, NumNodes: 2}},
		TotalNodes:    2,
		NumContainers: 1,
	}
}

func coldFeat() baseline.Features {
	return baseline.Features{
		UncachedBytes: 3_147_220,
		PresentImages: nil,
		TotalNodes:    2,
		NumContainers: 1,
	}
}

// Two cells × four replicates: 1–2 calibration, 3–4 evaluation.
// warm is clearly cheaper → oracle selects warm; SW needs n≥2 per cell on cal.
func synthPartitioned() []baseline.Trial {
	return []baseline.Trial{
		{ID: "w1", CellID: "warm_none", Replicate: 1, Cost: 2.0, Features: warmFeat()},
		{ID: "w2", CellID: "warm_none", Replicate: 2, Cost: 4.0, Features: warmFeat()},
		{ID: "w3", CellID: "warm_none", Replicate: 3, Cost: 2.5, Features: warmFeat()},
		{ID: "w4", CellID: "warm_none", Replicate: 4, Cost: 3.5, Features: warmFeat()},
		{ID: "c1", CellID: "cold_none", Replicate: 1, Cost: 10.0, Features: coldFeat()},
		{ID: "c2", CellID: "cold_none", Replicate: 2, Cost: 14.0, Features: coldFeat()},
		{ID: "c3", CellID: "cold_none", Replicate: 3, Cost: 11.0, Features: coldFeat()},
		{ID: "c4", CellID: "cold_none", Replicate: 4, Cost: 13.0, Features: coldFeat()},
	}
}

func TestBuildAndWrite(t *testing.T) {
	exp, err := evalexport.Build(synthPartitioned(), []int{1, 2}, []int{3, 4})
	if err != nil {
		t.Fatal(err)
	}
	if exp.SchemaVersion != evalexport.SchemaVersion {
		t.Fatalf("schema %d", exp.SchemaVersion)
	}
	if exp.OracleSelectedCell != "warm_none" {
		t.Fatalf("oracle cell=%q want warm_none", exp.OracleSelectedCell)
	}
	if !math.IsInf(exp.OracleCorrectedCost, 0) && exp.OracleCorrectedCost <= 0 {
		t.Fatalf("corrected cost should be positive, got %v", exp.OracleCorrectedCost)
	}
	if len(exp.Cells) != 2 {
		t.Fatalf("cells=%d want 2", len(exp.Cells))
	}
	by := map[string]evalexport.CellRecord{}
	for _, c := range exp.Cells {
		by[c.CellID] = c
	}
	warm := by["warm_none"]
	if _, ok := warm.BaselinePredictions["fixed-cost"]; !ok {
		t.Fatal("missing fixed-cost prediction")
	}
	if _, ok := warm.BaselinePredictions["ImageLocality"]; !ok {
		t.Fatal("missing ImageLocality prediction")
	}
	cold := by["cold_none"]
	if warm.EvalN != 2 || cold.EvalN != 2 {
		t.Fatalf("eval n warm=%d cold=%d", warm.EvalN, cold.EvalN)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "oracle-baseline-export.json")
	if err := evalexport.WriteFile(path, exp); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var round evalexport.Export
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatal(err)
	}
	if round.OracleSelectedCell != exp.OracleSelectedCell {
		t.Fatalf("round-trip cell %q", round.OracleSelectedCell)
	}
}

func TestDefaultPath(t *testing.T) {
	got := evalexport.DefaultPath("pilot")
	want := filepath.Join("experiments", "results", "pilot", "oracle-baseline-export.json")
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
