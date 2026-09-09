package evalexport

import (
	"testing"

	"reloc-disrupt/internal/baseline"
)

func TestFeaturesFromCacheRowNumNodesStructuralGuarantee(t *testing.T) {
	warm := featuresFromCacheRow("warm", 0, 0.3)
	if len(warm.PresentImages) != 1 {
		t.Fatalf("warm PresentImages len=%d want 1 (target image only)", len(warm.PresentImages))
	}
	if warm.PresentImages[0].NumNodes != 1 {
		t.Fatalf("warm NumNodes=%d want 1 (image on Target only; LoadNode never holds app image)",
			warm.PresentImages[0].NumNodes)
	}
	if warm.TotalNodes < 2 {
		t.Fatalf("TotalNodes=%d want >=2 for local-VM workers", warm.TotalNodes)
	}

	cold := featuresFromCacheRow("cold", 3_147_220, 1.0)
	if len(cold.PresentImages) != 0 {
		t.Fatalf("cold PresentImages=%v want empty (NumNodesWithImage=0)", cold.PresentImages)
	}

	// Uncached-bytes=0 without explicit warm label still means present on target only.
	viaUnc := featuresFromCacheRow("cold", 0, 0)
	if len(viaUnc.PresentImages) != 1 || viaUnc.PresentImages[0].NumNodes != 1 {
		t.Fatalf("uncached=0 path: got %+v", viaUnc.PresentImages)
	}
}

func TestImageLocalityScoreUsesNumNodesOneNotTwo(t *testing.T) {
	// Regression: old synthesis used NumNodes=TotalNodes=2 (spread=1), which
	// overstated locality vs the verified NumNodes=1 (spread=0.5).
	f1 := featuresFromCacheRow("warm", 0, 0)
	fWrong := f1
	fWrong.PresentImages = []baseline.ImagePresence{{
		Name: f1.PresentImages[0].Name, Size: f1.PresentImages[0].Size, NumNodes: 2,
	}}
	s1, err := baseline.Score(f1)
	if err != nil {
		t.Fatal(err)
	}
	sWrong, err := baseline.Score(fWrong)
	if err != nil {
		t.Fatal(err)
	}
	if s1 >= sWrong {
		t.Fatalf("NumNodes=1 must score strictly below NumNodes=2: got %d vs %d", s1, sWrong)
	}
	if f1.PresentImages[0].Name != "reloc/app-a:v1" {
		t.Fatalf("unexpected image name %q", f1.PresentImages[0].Name)
	}
}
