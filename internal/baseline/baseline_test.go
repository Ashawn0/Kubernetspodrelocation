package baseline_test

import (
	"math"
	"testing"

	"reloc-disrupt/internal/baseline"
)

// Synthetic fixture shaped like future campaign rows: replace constructors with
// CSV/JSONL loaders when experiments/results/{calibration,evaluation} exist.
func synthTrials() []baseline.Trial {
	return []baseline.Trial{
		{ID: "t1", CellID: "warm_none", Replicate: 1, Cost: 2.0, Features: warmFeat()},
		{ID: "t2", CellID: "warm_none", Replicate: 2, Cost: 4.0, Features: warmFeat()},
		{ID: "t3", CellID: "cold_none", Replicate: 1, Cost: 10.0, Features: coldFeat()},
		{ID: "t4", CellID: "cold_none", Replicate: 2, Cost: 14.0, Features: coldFeat()},
	}
}

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
		PresentImages: nil, // nothing present → ImageLocality min threshold path
		TotalNodes:    2,
		NumContainers: 1,
	}
}

func TestPredictorInterface(t *testing.T) {
	var _ baseline.Predictor = baseline.NewFixedCost()
	var _ baseline.Predictor = baseline.NewImageLocality()
}

func TestFixedCostMean(t *testing.T) {
	p := baseline.NewFixedCost()
	if err := p.Fit(synthTrials()); err != nil {
		t.Fatal(err)
	}
	// mean of 2,4,10,14 = 7.5
	got, err := p.Predict(warmFeat())
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got-7.5) > 1e-9 {
		t.Fatalf("fixed-cost: got %v want 7.5", got)
	}
	cold, _ := p.Predict(coldFeat())
	if cold != got {
		t.Fatalf("fixed-cost must ignore features: %v vs %v", cold, got)
	}
}

func TestFixedCostMedian(t *testing.T) {
	p := baseline.NewFixedCost()
	p.Aggregate = baseline.AggregateMedian
	if err := p.Fit([]baseline.Trial{
		{Cost: 1}, {Cost: 2}, {Cost: 100},
	}); err != nil {
		t.Fatal(err)
	}
	got, _ := p.Predict(baseline.Features{})
	if got != 2 {
		t.Fatalf("median: got %v want 2", got)
	}
}

func TestFixedCostRequiresFit(t *testing.T) {
	p := baseline.NewFixedCost()
	if _, err := p.Predict(baseline.Features{}); err == nil {
		t.Fatal("expected error before Fit")
	}
}

func TestImageLocalityScoreMatchesUpstreamThresholds(t *testing.T) {
	// No images present → sum=0 → clamped to minThreshold → score 0.
	sc, err := baseline.Score(coldFeat())
	if err != nil {
		t.Fatal(err)
	}
	if sc != 0 {
		t.Fatalf("empty present: score=%d want 0", sc)
	}

	// Large image on all nodes: size*spread = 500Mi * 1 = 500Mi
	// maxThreshold=1000Mi, min=23Mi → score = 100*(500-23)/(1000-23) ≈ 48
	sc, err = baseline.Score(warmFeat())
	if err != nil {
		t.Fatal(err)
	}
	want := int64(100 * (500*1024*1024 - 23*1024*1024) / (1000*1024*1024 - 23*1024*1024))
	if sc != want {
		t.Fatalf("warm score=%d want %d", sc, want)
	}
}

func TestImageLocalityPredictSeparatesColdWarm(t *testing.T) {
	p := baseline.NewImageLocality()
	if err := p.Fit(synthTrials()); err != nil {
		t.Fatal(err)
	}
	warm, err := p.Predict(warmFeat())
	if err != nil {
		t.Fatal(err)
	}
	cold, err := p.Predict(coldFeat())
	if err != nil {
		t.Fatal(err)
	}
	if !(cold > warm) {
		t.Fatalf("expected cold cost > warm cost; warm=%v cold=%v", warm, cold)
	}
}

func TestNormalizedImageName(t *testing.T) {
	if got := baseline.NormalizedImageName("nginx"); got != "nginx:latest" {
		t.Fatalf("got %q", got)
	}
	if got := baseline.NormalizedImageName("nginx:1.25"); got != "nginx:1.25" {
		t.Fatalf("got %q", got)
	}
	if got := baseline.NormalizedImageName("registry.example/foo/bar"); got != "registry.example/foo/bar:latest" {
		t.Fatalf("got %q", got)
	}
}
