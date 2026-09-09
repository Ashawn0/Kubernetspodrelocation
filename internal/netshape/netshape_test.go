package netshape_test

import (
	"strings"
	"testing"

	"reloc-disrupt/internal/netshape"
)

func TestProfileForLevelBands(t *testing.T) {
	cases := []struct {
		level        int
		name         string
		delay        int
		rate         int
	}{
		{0, "control", 0, 0},
		{1, "mild", 2, 0},
		{2, "moderate", netshape.NetprobeValidatedDelayMS, 0},
		{3, "severe", 200, netshape.NetprobeValidatedRateMbit},
	}
	for _, tc := range cases {
		p, err := netshape.ProfileForLevel(tc.level)
		if err != nil {
			t.Fatalf("level %d: %v", tc.level, err)
		}
		if p.Name != tc.name || p.DelayMS != tc.delay || p.RateMbit != tc.rate {
			t.Fatalf("level %d: got %+v want name=%s delay=%d rate=%d", tc.level, p, tc.name, tc.delay, tc.rate)
		}
	}
	if _, err := netshape.ProfileForLevel(4); err == nil {
		t.Fatal("expected reject level 4")
	}
}

func TestApplyScriptMatchesNetprobeTBFNetem(t *testing.T) {
	// Stage 0 netprobe used delay=100, rate=20 with this exact qdisc stack.
	p := netshape.Profile{Level: 3, Name: "severe", DelayMS: 100, RateMbit: 20}
	script, err := netshape.ApplyScript("ens6", "ens5", p)
	if err != nil {
		t.Fatal(err)
	}
	wantFragments := []string{
		`IF="ens6"`,
		`PRI="ens5"`,
		`DELAY=100`,
		`RATE=20`,
		`if [[ "$IF" == "$PRI" ]]; then echo "refuse shape primary"; exit 1; fi`,
		`tc qdisc del dev "$IF" root 2>/dev/null || true`,
		`tc qdisc add dev "$IF" root handle 1: tbf rate "${RATE}mbit" burst 32kbit latency 400ms`,
		`tc qdisc add dev "$IF" parent 1:1 handle 10: netem delay "${DELAY}ms"`,
		`tc qdisc show dev "$IF"`,
	}
	for _, frag := range wantFragments {
		if !strings.Contains(script, frag) {
			t.Fatalf("missing netprobe fragment %q in:\n%s", frag, script)
		}
	}
}

func TestApplyScriptDelayOnlyMild(t *testing.T) {
	p, err := netshape.ProfileForLevel(1)
	if err != nil {
		t.Fatal(err)
	}
	script, err := netshape.ApplyScript("ens6", "ens5", p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "tbf") {
		t.Fatal("mild must not install tbf")
	}
	if !strings.Contains(script, `netem delay "${DELAY}ms"`) || !strings.Contains(script, "DELAY=2") {
		t.Fatalf("mild delay script unexpected:\n%s", script)
	}
}

func TestApplyScriptLevel0Clears(t *testing.T) {
	p, _ := netshape.ProfileForLevel(0)
	script, err := netshape.ApplyScript("ens6", "ens5", p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(script, "netem") || strings.Contains(script, "tbf rate") {
		t.Fatalf("control must clear only, got:\n%s", script)
	}
	if !strings.Contains(script, `tc qdisc del dev "ens6" root`) {
		t.Fatalf("missing clear:\n%s", script)
	}
}

func TestRefuseShapePrimary(t *testing.T) {
	p, _ := netshape.ProfileForLevel(2)
	if _, err := netshape.ApplyScript("ens5", "ens5", p); err == nil {
		t.Fatal("expected refuse")
	}
}

func TestShapingPresent(t *testing.T) {
	if !netshape.ShapingPresent("qdisc netem 10: parent 1:1") {
		t.Fatal("netem should count")
	}
	if !netshape.ShapingPresent("qdisc tbf 1: root") {
		t.Fatal("tbf should count")
	}
	if netshape.ShapingPresent("qdisc noqueue 0: root") {
		t.Fatal("noqueue is clear")
	}
	if netshape.ShapingPresent("") {
		t.Fatal("empty is clear")
	}
}

func TestClearScript(t *testing.T) {
	s := netshape.ClearScript("ens6")
	if !strings.Contains(s, `tc qdisc del dev "ens6" root`) {
		t.Fatal(s)
	}
}
