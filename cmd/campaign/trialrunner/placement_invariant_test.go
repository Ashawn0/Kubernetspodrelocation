package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestTrialrunnerPlacementContractForImageLocalityNumNodes guards the structural
// guarantee that evalexport.featuresFromCacheRow relies on for NumNodes=1:
//
//	app pods are forced onto Target via nodeSelector; load/measurement runs on
//	LoadNode; miss-placement is a hard error. Empirically (2026-09-09) LoadNode
//	never holds reloc/app-a|app-b. If this source contract changes, ImageLocality
//	NumNodes synthesis must be revisited — fail here loudly instead of silently.
func TestTrialrunnerPlacementContractForImageLocalityNumNodes(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("caller")
	}
	srcPath := filepath.Join(filepath.Dir(thisFile), "main.go")
	raw, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)

	needles := []string{
		`place.WithNodeSelector(newPod, place.Stage0LabelKey, opt.Target)`,
		`disrupt.RunClusterLoad(context.Background(), cs, cfg, opt.Namespace, opt.LoadNode,`,
		`if placed.Spec.NodeName != opt.Target`,
		`return rec, fmt.Errorf("forced placement missed: want %s got %s", opt.Target, placed.Spec.NodeName)`,
	}
	for _, n := range needles {
		if !strings.Contains(src, n) {
			t.Fatalf("placement contract broken or rewritten: missing %q\n"+
				"ImageLocality NumNodes=1 synthesis in evalexport assumes Target-only app placement "+
				"and LoadNode-only load; update featuresFromCacheRow if this is intentional", n)
		}
	}

	// Target and LoadNode must remain distinct assignments from the worker list.
	if !strings.Contains(src, `target := workersNodes[0].Name`) {
		t.Fatal("expected Target = workersNodes[0]; NumNodes synthesis assumes a dedicated target worker")
	}
	if !strings.Contains(src, `loadNode = workersNodes[1].Name`) {
		t.Fatal("expected LoadNode = workersNodes[1] when available; synthesis assumes load node ≠ target")
	}
	// Cache prep and uncached accounting must hit Target, not LoadNode.
	for _, frag := range []string{
		`nodeobs.RemoveImageAndContent(ctx, cs, cfg, opt.Namespace, opt.Target,`,
		`nodeobs.PullImagePlainHTTP(ctx, cs, cfg, opt.Namespace, opt.Target,`,
		`nodeobs.UncachedBytesViaRegistryManifest(ctx, cs, cfg, opt.Namespace, opt.Target,`,
	} {
		if !strings.Contains(src, frag) {
			t.Fatalf("cache/uncached path must use opt.Target (not LoadNode); missing %q", frag)
		}
	}
}
