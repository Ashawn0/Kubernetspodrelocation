package nodeobs

import "testing"

func TestParsePSISome(t *testing.T) {
	raw := "some avg10=50.56 avg60=11.25 avg300=2.43 total=15499927\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n"
	s, err := ParsePSISome(raw)
	if err != nil {
		t.Fatal(err)
	}
	if s.Avg10 != 50.56 || s.Avg60 != 11.25 || s.Avg300 != 2.43 || s.Total != 15499927 {
		t.Fatalf("unexpected parse: %+v", s)
	}
}

func TestParseTaggedPSI_ProcExactRaw(t *testing.T) {
	// Verbatim host-exec blob from local-VM psiprobe failure (worker1).
	raw := `===PROC:cpu===
some avg10=0.00 avg60=0.44 avg300=2.33 total=88794235
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
===PROC:memory===
some avg10=0.00 avg60=0.00 avg300=0.00 total=18427
full avg10=0.00 avg60=0.00 avg300=0.00 total=18211
===PROC:io===
some avg10=0.00 avg60=0.00 avg300=0.00 total=2841373
full avg10=0.00 avg60=0.00 avg300=0.00 total=2713605
`
	snaps, err := parseTaggedPSI(raw, "proc")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("want 3 snapshots, got %d: %+v", len(snaps), snaps)
	}
	want := map[string]uint64{
		"cpu":    88794235,
		"memory": 18427,
		"io":     2841373,
	}
	for _, s := range snaps {
		if s.Source != "proc" {
			t.Fatalf("source: %s", s.Source)
		}
		tot, ok := want[s.Resource]
		if !ok {
			t.Fatalf("unexpected resource %q", s.Resource)
		}
		if s.Some.Total != tot {
			t.Fatalf("%s total: got %d want %d", s.Resource, s.Some.Total, tot)
		}
		if s.Resource == "cpu" && s.Some.Avg60 != 0.44 {
			t.Fatalf("cpu avg60: got %v want 0.44", s.Some.Avg60)
		}
		delete(want, s.Resource)
	}
	if len(want) != 0 {
		t.Fatalf("missing resources: %v", want)
	}
}

func TestParseTaggedPSI_CgroupExactRaw(t *testing.T) {
	raw := `===CGROUP:cpu===
some avg10=0.00 avg60=0.44 avg300=2.33 total=88794235
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
===CGROUP:memory===
some avg10=0.00 avg60=0.00 avg300=0.00 total=18427
full avg10=0.00 avg60=0.00 avg300=0.00 total=18211
===CGROUP:io===
some avg10=0.00 avg60=0.00 avg300=0.00 total=2841373
full avg10=0.00 avg60=0.00 avg300=0.00 total=2713605
`
	snaps, err := parseTaggedPSI(raw, "cgroup")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 3 {
		t.Fatalf("want 3 snapshots, got %d", len(snaps))
	}
	byRes := map[string]PSISnapshot{}
	for _, s := range snaps {
		if s.Source != "cgroup" {
			t.Fatalf("source: %s", s.Source)
		}
		byRes[s.Resource] = s
	}
	for _, res := range []string{"cpu", "memory", "io"} {
		if _, ok := byRes[res]; !ok {
			t.Fatalf("missing %s", res)
		}
	}
	if byRes["cpu"].Some.Total != 88794235 || byRes["memory"].Some.Total != 18427 || byRes["io"].Some.Total != 2841373 {
		t.Fatalf("totals mismatch: cpu=%d mem=%d io=%d", byRes["cpu"].Some.Total, byRes["memory"].Some.Total, byRes["io"].Some.Total)
	}
}

func TestIsolationPass(t *testing.T) {
	ok, _ := IsolationPass(15_000_000, 50_000, 1_000_000)
	if !ok {
		t.Fatal("expected pass")
	}
	ok, reason := IsolationPass(100, 0, 1_000)
	if ok {
		t.Fatalf("expected fail, got %s", reason)
	}
	ok, _ = IsolationPass(10_000, 9_000, 1_000)
	if ok {
		t.Fatal("expected leakage fail")
	}
}

func TestRefuseOrEmit(t *testing.T) {
	g := RefuseOrEmit("cpu", false, "leaked", PSISome{Avg10: 1})
	if g.Emit || g.Value != nil {
		t.Fatalf("must refuse: %+v", g)
	}
	g = RefuseOrEmit("cpu", true, "ok", PSISome{Avg10: 9})
	if !g.Emit || g.Value == nil || g.Value.Avg10 != 9 {
		t.Fatalf("must emit: %+v", g)
	}
}
