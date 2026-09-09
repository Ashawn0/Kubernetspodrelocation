package evalexport_test

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"reloc-disrupt/internal/evalexport"
	"reloc-disrupt/internal/oracle"
)

func TestLoadTrialsCSVSmoke18(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	csvPath := filepath.Join(root, "analysis", "testdata", "pilot-variance-18trial-smoke.csv")
	trials, err := evalexport.LoadTrialsCSV(csvPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(trials) != 18 {
		t.Fatalf("got %d trials want 18", len(trials))
	}
	cells := map[string]int{}
	for _, tr := range trials {
		if tr.Split != "" {
			t.Fatalf("pilot split should clear for partition, got %q on %s", tr.Split, tr.ID)
		}
		cells[tr.CellID]++
		if tr.Cost <= 0 {
			t.Fatalf("non-positive cost on %s", tr.ID)
		}
	}
	if len(cells) != 6 {
		t.Fatalf("cells=%d want 6: %v", len(cells), cells)
	}
	for id, n := range cells {
		if n != 3 {
			t.Fatalf("cell %s n=%d want 3", id, n)
		}
	}

	part, err := oracle.PartitionByReplicate(trials, []int{1, 2}, []int{3})
	if err != nil {
		t.Fatal(err)
	}
	exp, err := evalexport.Build(part, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if exp.OracleSelectedCell == "" || exp.OracleCorrectedCost <= 0 {
		t.Fatalf("oracle incomplete: %+v", exp)
	}
	if len(exp.Cells) != 6 {
		t.Fatalf("export cells=%d", len(exp.Cells))
	}
	for _, c := range exp.Cells {
		if _, ok := c.BaselinePredictions["fixed-cost"]; !ok {
			t.Fatalf("missing fixed-cost on %s", c.CellID)
		}
		if _, ok := c.BaselinePredictions["ImageLocality"]; !ok {
			t.Fatalf("missing ImageLocality on %s", c.CellID)
		}
		if c.CalN != 2 || c.EvalN != 1 {
			t.Fatalf("%s cal=%d eval=%d want 2/1", c.CellID, c.CalN, c.EvalN)
		}
	}
}

func TestLoadTrialsCSVMissingColumn(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.csv")
	if err := os.WriteFile(path, []byte("trial_id,replicate\nx,1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := evalexport.LoadTrialsCSV(path); err == nil {
		t.Fatal("expected missing column error")
	}
}
