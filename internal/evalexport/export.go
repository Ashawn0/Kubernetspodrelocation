// Package evalexport dumps calibration-derived baseline predictions and
// Smith–Winkler-corrected oracle values to JSON for the Python regret /
// GapCaptured pipeline. Straightforward data handoff — not a new abstraction.
//
// Go owns trial collection, fixed-cost / ImageLocality baselines, and the
// empirical oracle. Python owns LightGBM and the final metric. See
// docs/campaign-design.md §8.
package evalexport

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"reloc-disrupt/internal/baseline"
	"reloc-disrupt/internal/oracle"
)

const SchemaVersion = 1

// CellRecord is one state-cell row in the export.
type CellRecord struct {
	CellID               string             `json:"cell_id"`
	CalMean              float64            `json:"cal_mean"`
	CalN                 int                `json:"cal_n"`
	EvalMean             float64            `json:"eval_mean"`
	EvalN                int                `json:"eval_n"`
	BaselinePredictions  map[string]float64 `json:"baseline_predictions"` // name -> predicted cost (seconds)
	RepresentativeFeatOK bool               `json:"representative_features_ok"`
}

// Export is the single JSON file written under experiments/results/{run}/.
type Export struct {
	SchemaVersion         int          `json:"schema_version"`
	OracleSelectedCell    string       `json:"oracle_selected_cell"`
	OracleRawCalCost      float64      `json:"oracle_raw_cal_cost"`
	OracleCorrectedCost   float64      `json:"oracle_corrected_cost"`
	OracleEvalCost        float64      `json:"oracle_eval_cost"`
	OracleEvalN           int          `json:"oracle_eval_n"`
	OracleShrinkageAlpha  float64      `json:"oracle_shrinkage_alpha"`
	OraclePriorMean       float64      `json:"oracle_prior_mean"`
	Cells                 []CellRecord `json:"cells"`
	// ProxyIntegrationCheck marks artificial-split plumbing checks so they can
	// never be mistaken for campaign GapCaptured results.
	ProxyIntegrationCheck bool   `json:"proxy_integration_check,omitempty"`
	ProxyNote             string `json:"proxy_note,omitempty"`
	SourceCSV             string `json:"source_csv,omitempty"`
}

// Build constructs an Export from partitioned (or partitionable) trials.
// calReps/evalReps are applied when trials do not already carry Split labels.
func Build(trials []baseline.Trial, calReps, evalReps []int) (Export, error) {
	var err error
	if len(trials) == 0 {
		return Export{}, fmt.Errorf("evalexport: no trials")
	}
	needPartition := false
	for _, t := range trials {
		if t.Split == "" {
			needPartition = true
			break
		}
	}
	if needPartition {
		trials, err = oracle.PartitionByReplicate(trials, calReps, evalReps)
		if err != nil {
			return Export{}, err
		}
	}

	cal := oracle.FilterSplit(trials, oracle.SplitCalibration)
	eval := oracle.FilterSplit(trials, oracle.SplitEvaluation)
	if len(cal) == 0 {
		return Export{}, fmt.Errorf("evalexport: no calibration trials")
	}

	ora, err := oracle.BuildEmpiricalOracle(trials, baseline.AggregateMean)
	if err != nil {
		return Export{}, err
	}

	fc := baseline.NewFixedCost()
	if err := fc.Fit(cal); err != nil {
		return Export{}, fmt.Errorf("evalexport: fixed-cost fit: %w", err)
	}
	il := baseline.NewImageLocality()
	// ImageLocality needs TotalNodes on features; fill defaults from trials when missing.
	calIL := withDefaultILFeatures(cal)
	if err := il.Fit(calIL); err != nil {
		return Export{}, fmt.Errorf("evalexport: ImageLocality fit: %w", err)
	}

	calBy := groupByCell(cal)
	evalBy := groupByCell(eval)
	cellIDs := unionKeys(calBy, evalBy)
	sort.Strings(cellIDs)

	cells := make([]CellRecord, 0, len(cellIDs))
	for _, id := range cellIDs {
		rec := CellRecord{
			CellID:              id,
			BaselinePredictions: map[string]float64{},
		}
		if xs := calBy[id]; len(xs) > 0 {
			rec.CalN = len(xs)
			rec.CalMean = float64(baseline.AggregateMean(costsOf(xs)))
		}
		if xs := evalBy[id]; len(xs) > 0 {
			rec.EvalN = len(xs)
			rec.EvalMean = float64(baseline.AggregateMean(costsOf(xs)))
		}
		feat, ok := representativeFeatures(calBy[id])
		rec.RepresentativeFeatOK = ok
		if !ok {
			feat, ok = representativeFeatures(evalBy[id])
			rec.RepresentativeFeatOK = ok
		}
		if pc, err := fc.Predict(feat); err == nil {
			rec.BaselinePredictions[fc.Name()] = float64(pc)
		}
		if ok {
			if pc, err := il.Predict(feat); err == nil {
				rec.BaselinePredictions[il.Name()] = float64(pc)
			}
		}
		cells = append(cells, rec)
	}

	return Export{
		SchemaVersion:        SchemaVersion,
		OracleSelectedCell:   ora.SelectedCell,
		OracleRawCalCost:     float64(ora.CalCost),
		OracleCorrectedCost:  float64(ora.BiasCorrectedCost),
		OracleEvalCost:       float64(ora.EvalCost),
		OracleEvalN:          ora.EvalN,
		OracleShrinkageAlpha: ora.ShrinkageAlpha,
		OraclePriorMean:      float64(ora.PriorMean),
		Cells:                cells,
	}, nil
}

// WriteFile marshals the export to path (directories created as needed).
func WriteFile(path string, exp Export) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(exp, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o644)
}

// DefaultPath returns experiments/results/{run}/oracle-baseline-export.json.
func DefaultPath(run string) string {
	if run == "" {
		run = "default"
	}
	return filepath.Join("experiments", "results", run, "oracle-baseline-export.json")
}

func groupByCell(trials []baseline.Trial) map[string][]baseline.Trial {
	m := map[string][]baseline.Trial{}
	for _, t := range trials {
		m[t.CellID] = append(m[t.CellID], t)
	}
	return m
}

func unionKeys(a, b map[string][]baseline.Trial) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

func costsOf(trials []baseline.Trial) []baseline.Cost {
	out := make([]baseline.Cost, len(trials))
	for i, t := range trials {
		out[i] = t.Cost
	}
	return out
}

func representativeFeatures(trials []baseline.Trial) (baseline.Features, bool) {
	if len(trials) == 0 {
		return baseline.Features{}, false
	}
	// Mean numeric covariates; keep first trial's ImageLocality presence list.
	var unc int64
	var psi float64
	for _, t := range trials {
		unc += t.Features.UncachedBytes
		psi += t.Features.CPUPSIAvg10
	}
	n := float64(len(trials))
	f := trials[0].Features
	f.UncachedBytes = unc / int64(len(trials))
	f.CPUPSIAvg10 = psi / n
	if f.TotalNodes < 1 {
		f.TotalNodes = 2
	}
	if f.NumContainers < 1 {
		f.NumContainers = 1
	}
	return f, true
}

func withDefaultILFeatures(trials []baseline.Trial) []baseline.Trial {
	out := make([]baseline.Trial, len(trials))
	for i, t := range trials {
		f := t.Features
		if f.TotalNodes < 1 {
			f.TotalNodes = 2
		}
		if f.NumContainers < 1 {
			f.NumContainers = 1
		}
		t.Features = f
		out[i] = t
	}
	return out
}
