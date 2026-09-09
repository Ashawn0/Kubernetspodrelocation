package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndValidateExampleShape(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.json")
	raw := `{
  "local_vm_cells": [
    {"image_cache_state":"cold","cpu_psi_level":"none","calibration_n":2,"evaluation_n":3},
    {"image_cache_state":"warm","cpu_psi_level":"high","calibration_n":1,"evaluation_n":1,"held_out":false}
  ],
  "aws_cells": [
    {"network_level":0,"cpu_psi_level":"none","calibration_n":1,"evaluation_n":1},
    {"network_level":2,"cpu_psi_level":"high","calibration_n":5,"evaluation_n":2,"held_out":true}
  ]
}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCampaignConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.LocalVMCells) != 2 || len(cfg.AWSCells) != 2 {
		t.Fatalf("counts local=%d aws=%d", len(cfg.LocalVMCells), len(cfg.AWSCells))
	}
}

func TestRejectAWSThreshold(t *testing.T) {
	err := validateCampaignConfig(CampaignConfig{
		AWSCells: []AWSCellConfig{{
			NetworkLevel: 1, CPUPSILevel: "threshold", CalibrationN: 1, EvaluationN: 1,
		}},
	})
	if err == nil {
		t.Fatal("expected reject threshold on AWS")
	}
}

func TestRejectZeroEval(t *testing.T) {
	err := validateLocalVMCell(LocalVMCellConfig{
		ImageCacheState: "cold", CPUPSILevel: "none", CalibrationN: 1, EvaluationN: 0,
	})
	if err == nil {
		t.Fatal("expected reject evaluation_n=0")
	}
}

func TestHeldOutForcesZeroCalibration(t *testing.T) {
	// Config may still list calibration_n>0; effective count must be 0.
	if got := effectiveCalN(12, true); got != 0 {
		t.Fatalf("held_out effectiveCalN=%d want 0", got)
	}
	if got := effectiveCalN(12, false); got != 12 {
		t.Fatalf("non-held_out effectiveCalN=%d want 12", got)
	}

	cfg := CampaignConfig{
		LocalVMCells: []LocalVMCellConfig{{
			ImageCacheState: "cold",
			CPUPSILevel:     "none",
			CalibrationN:    7, // ignored
			EvaluationN:     3,
			HeldOut:         true,
		}},
	}
	specs, err := buildCampaignSchedule(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 3 {
		t.Fatalf("held_out schedule len=%d want 3 (eval only)", len(specs))
	}
	for _, sp := range specs {
		if sp.Split != "evaluation" {
			t.Fatalf("held_out trial split=%q want evaluation (not merely fewer cal reps)", sp.Split)
		}
		if !sp.HeldOut {
			t.Fatal("HeldOut flag not set on spec")
		}
	}
}

func TestSplitAssignmentByReplicateNumber(t *testing.T) {
	// cal_n=2, eval_n=3 → reps 1–2 calibration, 3–5 evaluation
	for r, want := range map[int]string{1: "calibration", 2: "calibration", 3: "evaluation", 4: "evaluation", 5: "evaluation"} {
		got, err := splitForReplicate(r, 2, 3)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("r=%d got %q want %q", r, got, want)
		}
	}
	if _, err := splitForReplicate(0, 2, 3); err == nil {
		t.Fatal("expected out of range")
	}
	if _, err := splitForReplicate(6, 2, 3); err == nil {
		t.Fatal("expected out of range")
	}
}

func TestScheduleSplitsSurviveShuffle(t *testing.T) {
	cfg := CampaignConfig{
		LocalVMCells: []LocalVMCellConfig{
			{ImageCacheState: "cold", CPUPSILevel: "none", CalibrationN: 2, EvaluationN: 2},
			{ImageCacheState: "warm", CPUPSILevel: "none", CalibrationN: 1, EvaluationN: 1},
		},
	}
	base, err := buildCampaignSchedule(cfg)
	if err != nil {
		t.Fatal(err)
	}
	key := func(cell string, rep int) string { return cell + "#" + itoa(rep) }
	wantSplit := map[string]string{}
	for _, sp := range base {
		wantSplit[key(sp.CellName, sp.Replicate)] = sp.Split
	}

	shuffled := shuffleCampaignSchedule(base, 42)
	if len(shuffled) != len(base) {
		t.Fatalf("len %d", len(shuffled))
	}
	// Order changed (highly likely with seed 42 and 6 trials), but splits fixed.
	sameOrder := true
	for i := range base {
		if shuffled[i].CellName != base[i].CellName || shuffled[i].Replicate != base[i].Replicate {
			sameOrder = false
			break
		}
	}
	if sameOrder {
		t.Fatal("expected shuffle to change order")
	}
	for _, sp := range shuffled {
		if sp.Split != wantSplit[key(sp.CellName, sp.Replicate)] {
			t.Fatalf("split mutated for %s r%d: %q", sp.CellName, sp.Replicate, sp.Split)
		}
		if sp.ExecOrder < 1 || sp.TrialID == "" {
			t.Fatal("missing ExecOrder/TrialID after shuffle")
		}
	}

	// Same seed → same execution order
	again := shuffleCampaignSchedule(base, 42)
	for i := range shuffled {
		if shuffled[i].CellName != again[i].CellName || shuffled[i].Replicate != again[i].Replicate {
			t.Fatalf("seed not deterministic at i=%d", i)
		}
	}
	other := shuffleCampaignSchedule(base, 99)
	diff := false
	for i := range shuffled {
		if shuffled[i].CellName != other[i].CellName || shuffled[i].Replicate != other[i].Replicate {
			diff = true
			break
		}
	}
	if !diff {
		t.Fatal("different seeds should differ (unlikely equality)")
	}
}

func TestAWSCellsAreColdOnly(t *testing.T) {
	cfg := CampaignConfig{
		AWSCells: []AWSCellConfig{{
			NetworkLevel: 3, CPUPSILevel: "high", CalibrationN: 1, EvaluationN: 1,
		}},
	}
	specs, err := buildCampaignSchedule(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].ImageCacheState != cacheCold {
		t.Fatalf("AWS cell cache=%s want cold", specs[0].ImageCacheState)
	}
	if specs[0].NetworkLevel != 3 || specs[0].Infra != infraAWS {
		t.Fatalf("aws fields: net=%d infra=%s", specs[0].NetworkLevel, specs[0].Infra)
	}
}

func TestRejectAWSWarmViaUnknownField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	raw := `{
  "aws_cells": [
    {"network_level":1,"cpu_psi_level":"none","calibration_n":1,"evaluation_n":1,"image_cache_state":"warm"}
  ]
}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadCampaignConfig(path); err == nil {
		t.Fatal("expected reject image_cache_state on aws_cells")
	}
}

func TestDuplicateCellRejected(t *testing.T) {
	err := validateCampaignConfig(CampaignConfig{
		LocalVMCells: []LocalVMCellConfig{
			{ImageCacheState: "cold", CPUPSILevel: "none", CalibrationN: 1, EvaluationN: 1},
			{ImageCacheState: "cold", CPUPSILevel: "none", CalibrationN: 1, EvaluationN: 1},
		},
	})
	if err == nil {
		t.Fatal("expected duplicate reject")
	}
}

func TestJSONRoundTripHeldOutField(t *testing.T) {
	cfg := CampaignConfig{
		LocalVMCells: []LocalVMCellConfig{{
			ImageCacheState: "warm", CPUPSILevel: "threshold",
			CalibrationN: 0, EvaluationN: 2, HeldOut: true,
		}},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var back CampaignConfig
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if !back.LocalVMCells[0].HeldOut {
		t.Fatal("held_out lost")
	}
	if err := validateCampaignConfig(back); err != nil {
		t.Fatal(err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
