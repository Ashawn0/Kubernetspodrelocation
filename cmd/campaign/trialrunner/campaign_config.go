// Campaign schedule: config parsing and cal/eval assignment (no cluster I/O).
//
// held_out semantics (easy to misread):
//
//	held_out=true does NOT mean “fewer calibration replicates.”
//	It forces calibration_n to 0 for that cell: EVERY trial for the cell is
//	labeled evaluation. Predictor Fit must never see that cell’s rows, so
//	evaluation of the cell is a genuine generalization test (conditions never
//	trained on), not interpolation within seen cells.
package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"strings"
)

const (
	infraLocalVM = "local_vm"
	infraAWS     = "aws"
)

// CampaignConfig is the -campaign-config JSON document.
type CampaignConfig struct {
	LocalVMCells []LocalVMCellConfig `json:"local_vm_cells"`
	AWSCells     []AWSCellConfig     `json:"aws_cells"`
}

// LocalVMCellConfig is one local-VM grid cell (docs/campaign-design.md §1).
type LocalVMCellConfig struct {
	ImageCacheState string `json:"image_cache_state"` // cold | warm
	CPUPSILevel     string `json:"cpu_psi_level"`     // none | threshold | high
	CalibrationN    int    `json:"calibration_n"`
	EvaluationN     int    `json:"evaluation_n"`
	// HeldOut: if true, calibration_n is forced to 0 and all trials are
	// evaluation-only (generalization cell). See package comment.
	HeldOut bool `json:"held_out"`
}

// AWSCellConfig is one AWS network×CPU cell (docs/campaign-design.md §7).
// Image cache is cold-only on this axis (warm × network is excluded by design):
// there is no image_cache_state field — DisallowUnknownFields rejects configs
// that try to set warm (network shaping is causally inert when the image is local).
type AWSCellConfig struct {
	NetworkLevel int    `json:"network_level"` // 0–3 (control/mild/moderate/severe)
	CPUPSILevel  string `json:"cpu_psi_level"` // none | high only
	CalibrationN int    `json:"calibration_n"`
	EvaluationN  int    `json:"evaluation_n"`
	HeldOut      bool   `json:"held_out"`
}

func loadCampaignConfig(path string) (CampaignConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return CampaignConfig{}, err
	}
	var cfg CampaignConfig
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return CampaignConfig{}, fmt.Errorf("campaign config: %w", err)
	}
	if err := validateCampaignConfig(cfg); err != nil {
		return CampaignConfig{}, err
	}
	return cfg, nil
}

func validateCampaignConfig(cfg CampaignConfig) error {
	if len(cfg.LocalVMCells) == 0 && len(cfg.AWSCells) == 0 {
		return fmt.Errorf("campaign config: need at least one local_vm_cells or aws_cells entry")
	}
	seen := map[string]bool{}
	for i, c := range cfg.LocalVMCells {
		if err := validateLocalVMCell(c); err != nil {
			return fmt.Errorf("local_vm_cells[%d]: %w", i, err)
		}
		name := localVMCellName(c.ImageCacheState, c.CPUPSILevel)
		if seen[name] {
			return fmt.Errorf("local_vm_cells: duplicate cell %q", name)
		}
		seen[name] = true
	}
	for i, c := range cfg.AWSCells {
		if err := validateAWSCell(c); err != nil {
			return fmt.Errorf("aws_cells[%d]: %w", i, err)
		}
		name := awsCellName(c.NetworkLevel, c.CPUPSILevel)
		if seen[name] {
			return fmt.Errorf("aws_cells: duplicate cell %q", name)
		}
		seen[name] = true
	}
	return nil
}

func validateLocalVMCell(c LocalVMCellConfig) error {
	switch strings.ToLower(c.ImageCacheState) {
	case "cold", "warm":
	default:
		return fmt.Errorf("image_cache_state %q (want cold|warm)", c.ImageCacheState)
	}
	if _, err := cpuRatio(c.CPUPSILevel); err != nil {
		return err
	}
	// threshold allowed on local-VM only
	switch strings.ToLower(c.CPUPSILevel) {
	case "none", "threshold", "high":
	default:
		return fmt.Errorf("cpu_psi_level %q", c.CPUPSILevel)
	}
	return validateCounts(c.CalibrationN, c.EvaluationN, c.HeldOut)
}

func validateAWSCell(c AWSCellConfig) error {
	if c.NetworkLevel < 0 || c.NetworkLevel > 3 {
		return fmt.Errorf("network_level %d (want 0–3)", c.NetworkLevel)
	}
	switch strings.ToLower(c.CPUPSILevel) {
	case "none", "high":
	case "threshold":
		return fmt.Errorf("cpu_psi_level %q not allowed on AWS network grid (none|high only; §7)", c.CPUPSILevel)
	default:
		return fmt.Errorf("cpu_psi_level %q (want none|high)", c.CPUPSILevel)
	}
	return validateCounts(c.CalibrationN, c.EvaluationN, c.HeldOut)
}

func validateCounts(calN, evalN int, heldOut bool) error {
	if evalN < 1 {
		return fmt.Errorf("evaluation_n must be >= 1 (got %d)", evalN)
	}
	if heldOut {
		// calibration_n in the file is ignored; may be any non-negative value.
		if calN < 0 {
			return fmt.Errorf("calibration_n must be >= 0 (got %d)", calN)
		}
		return nil
	}
	if calN < 1 {
		return fmt.Errorf("calibration_n must be >= 1 unless held_out=true (got %d)", calN)
	}
	return nil
}

func cpuRatio(level string) (int, error) {
	switch strings.ToLower(level) {
	case "none":
		return 0, nil
	case "threshold":
		return 2, nil
	case "high":
		return 3, nil
	default:
		return 0, fmt.Errorf("cpu_psi_level %q (want none|threshold|high)", level)
	}
}

func localVMCellName(cache, cpu string) string {
	return fmt.Sprintf("%s_%s", strings.ToLower(cache), strings.ToLower(cpu))
}

func awsCellName(net int, cpu string) string {
	return fmt.Sprintf("aws_net%d_%s", net, strings.ToLower(cpu))
}

// effectiveCalN returns the calibration replicate count after held_out forcing.
//
// held_out=true → always 0, regardless of the configured calibration_n.
// That is intentional: the cell is evaluation-only so Fit never sees it.
func effectiveCalN(calN int, heldOut bool) int {
	if heldOut {
		return 0
	}
	return calN
}

// splitForReplicate assigns calibration vs evaluation from replicate index.
// Replicates are 1-based. The first effectiveCalN indices are calibration;
// the remaining evaluationN indices are evaluation.
func splitForReplicate(replicate, effectiveCal, evaluationN int) (string, error) {
	total := effectiveCal + evaluationN
	if replicate < 1 || replicate > total {
		return "", fmt.Errorf("replicate %d out of range [1,%d]", replicate, total)
	}
	if replicate <= effectiveCal {
		return "calibration", nil
	}
	return "evaluation", nil
}

// buildCampaignSchedule expands the config into trialSpecs with Split assigned
// deterministically from (cell, replicate). Order is cell-major before shuffle.
func buildCampaignSchedule(cfg CampaignConfig) ([]trialSpec, error) {
	var out []trialSpec
	for _, c := range cfg.LocalVMCells {
		ratio, err := cpuRatio(c.CPUPSILevel)
		if err != nil {
			return nil, err
		}
		calN := effectiveCalN(c.CalibrationN, c.HeldOut)
		evalN := c.EvaluationN
		name := localVMCellName(c.ImageCacheState, c.CPUPSILevel)
		cache := cacheKind(strings.ToLower(c.ImageCacheState))
		cpu := cpuPSILevel(strings.ToLower(c.CPUPSILevel))
		for r := 1; r <= calN+evalN; r++ {
			split, err := splitForReplicate(r, calN, evalN)
			if err != nil {
				return nil, err
			}
			out = append(out, trialSpec{
				CellName:        name,
				ImageCacheState: cache,
				CPUPSILevel:     cpu,
				CPURatio:        ratio,
				Replicate:       r,
				Split:           split,
				Infra:           infraLocalVM,
				HeldOut:         c.HeldOut,
				NetworkLevel:    -1, // N/A for local-VM
			})
		}
	}
	for _, c := range cfg.AWSCells {
		ratio, err := cpuRatio(c.CPUPSILevel)
		if err != nil {
			return nil, err
		}
		calN := effectiveCalN(c.CalibrationN, c.HeldOut)
		evalN := c.EvaluationN
		name := awsCellName(c.NetworkLevel, c.CPUPSILevel)
		cpu := cpuPSILevel(strings.ToLower(c.CPUPSILevel))
		for r := 1; r <= calN+evalN; r++ {
			split, err := splitForReplicate(r, calN, evalN)
			if err != nil {
				return nil, err
			}
			out = append(out, trialSpec{
				CellName:        name,
				ImageCacheState: cacheCold, // §7: network axis is cold-only
				CPUPSILevel:     cpu,
				CPURatio:        ratio,
				Replicate:       r,
				Split:           split,
				Infra:           infraAWS,
				HeldOut:         c.HeldOut,
				NetworkLevel:    c.NetworkLevel,
			})
		}
	}
	return out, nil
}

// shuffleCampaignSchedule randomizes execution order with a seeded PRNG.
// Split labels stay attached to each (cell, replicate); only ExecOrder/TrialID change.
func shuffleCampaignSchedule(specs []trialSpec, seed int64) []trialSpec {
	out := make([]trialSpec, len(specs))
	copy(out, specs)
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	for i := range out {
		out[i].ExecOrder = i + 1
		out[i].TrialID = fmt.Sprintf("camp-%03d-%s-r%d", out[i].ExecOrder, out[i].CellName, out[i].Replicate)
	}
	return out
}
