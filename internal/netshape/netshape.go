// Package netshape applies and clears Linux tc shaping on the registry
// (secondary) ENI only — the Stage 0 netprobe pattern, extracted for campaign
// reuse the same way disrupt.RunPodIPPoll extracted TTFS/UID polling.
//
// LIVE STATUS: command construction and clear-verification logic are unit-tested
// against the netprobe-validated tc shape. End-to-end application on a live AWS
// dual-ENI node is UNVERIFIED until the next AWS provision (cluster is torn
// down between sessions). Do not treat campaign wiring as Stage-0-closed for
// shaping until that re-run lands. See docs/campaign-design.md §7 / §9.
package netshape

import (
	"context"
	"fmt"
	"strings"
	"time"

	"reloc-disrupt/internal/k8s"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// Defaults match Stage 0 AWS workers (deploy/aws/, netprobe flags).
const (
	DefaultRegistryIface = "ens6"
	DefaultPrimaryIface  = "ens5"

	// NetprobeValidatedDelayMS / RateMbit are the single shape netprobe closed
	// on AWS (implementation-log §14). Level 2 uses the same delay; level 3
	// reuses the same tbf rate with a higher delay (campaign-design §7).
	NetprobeValidatedDelayMS = 100
	NetprobeValidatedRateMbit = 20
	TBFBurst                 = "32kbit"
	TBFLatency               = "400ms"
)

// Profile is one campaign-design §7 network level mapped to tc parameters.
type Profile struct {
	Level      int
	Name       string // control | mild | moderate | severe
	DelayMS    int    // 0 = no netem
	RateMbit   int    // 0 = no tbf rate cap
	IntentNote string
}

// ProfileForLevel returns the §7 severity profile.
//
// Approximate midpoints of the design bands:
//
//	0 control   — no impairment
//	1 mild      — ~2 ms (band 1–3 ms, cross-AZ scale)
//	2 moderate  — 100 ms (band 80–100 ms; matches netprobe validated delay)
//	3 severe    — 200 ms (band 150–250) + 20 Mbit tbf (netprobe validated rate)
func ProfileForLevel(level int) (Profile, error) {
	switch level {
	case 0:
		return Profile{Level: 0, Name: "control", DelayMS: 0, RateMbit: 0,
			IntentNote: "no added slowdown (baseline path)"}, nil
	case 1:
		return Profile{Level: 1, Name: "mild", DelayMS: 2, RateMbit: 0,
			IntentNote: "~1–3 ms added delay (cross-AZ scale)"}, nil
	case 2:
		return Profile{Level: 2, Name: "moderate", DelayMS: NetprobeValidatedDelayMS, RateMbit: 0,
			IntentNote: "~80–100 ms added delay (cross-region scale); delay matches netprobe Stage-0 value"}, nil
	case 3:
		return Profile{Level: 3, Name: "severe", DelayMS: 200, RateMbit: NetprobeValidatedRateMbit,
			IntentNote: "~150–250 ms delay plus bandwidth cap; tbf rate matches netprobe Stage-0 value"}, nil
	default:
		return Profile{}, fmt.Errorf("netshape: network_level %d (want 0–3)", level)
	}
}

// ApplyScript builds the host shell that shapes registryIface.
// When both DelayMS and RateMbit are set, the command matches Stage 0 netprobe
// (tbf root + netem child). Delay-only levels omit tbf. Level 0 clears.
//
// Refuse if registryIface == primaryIface (never shape the CNI path).
func ApplyScript(registryIface, primaryIface string, p Profile) (string, error) {
	if registryIface == "" || primaryIface == "" {
		return "", fmt.Errorf("netshape: registry and primary ifaces required")
	}
	if registryIface == primaryIface {
		return "", fmt.Errorf("netshape: refuse shape primary=%q", primaryIface)
	}
	if p.Level == 0 || (p.DelayMS <= 0 && p.RateMbit <= 0) {
		return ClearScript(registryIface), nil
	}
	if p.RateMbit > 0 && p.DelayMS > 0 {
		// Exact netprobe shape (cmd/stage0/netprobe/main.go shapeCmd).
		return fmt.Sprintf(`
set -euo pipefail
IF=%q
PRI=%q
DELAY=%d
RATE=%d
if [[ "$IF" == "$PRI" ]]; then echo "refuse shape primary"; exit 1; fi
tc qdisc del dev "$IF" root 2>/dev/null || true
tc qdisc add dev "$IF" root handle 1: tbf rate "${RATE}mbit" burst %s latency %s
tc qdisc add dev "$IF" parent 1:1 handle 10: netem delay "${DELAY}ms"
tc qdisc show dev "$IF"
`, registryIface, primaryIface, p.DelayMS, p.RateMbit, TBFBurst, TBFLatency), nil
	}
	if p.DelayMS > 0 {
		return fmt.Sprintf(`
set -euo pipefail
IF=%q
PRI=%q
DELAY=%d
if [[ "$IF" == "$PRI" ]]; then echo "refuse shape primary"; exit 1; fi
tc qdisc del dev "$IF" root 2>/dev/null || true
tc qdisc add dev "$IF" root handle 1: netem delay "${DELAY}ms"
tc qdisc show dev "$IF"
`, registryIface, primaryIface, p.DelayMS), nil
	}
	return "", fmt.Errorf("netshape: profile level %d has rate without delay (unsupported)", p.Level)
}

// ClearScript removes the root qdisc on the registry iface (netprobe defer clear).
func ClearScript(registryIface string) string {
	return fmt.Sprintf(`set -euo pipefail
tc qdisc del dev %q root 2>/dev/null || true
tc qdisc show dev %q || true
`, registryIface, registryIface)
}

// ShowScript returns tc qdisc show for verification polls.
func ShowScript(iface string) string {
	return fmt.Sprintf(`tc qdisc show dev %q || true`, iface)
}

// ShapingPresent reports whether qdisc show output still has netem or tbf.
func ShapingPresent(qdiscShow string) bool {
	s := strings.ToLower(qdiscShow)
	return strings.Contains(s, "netem") || strings.Contains(s, "tbf")
}

// Apply runs ApplyScript on node via HostExec.
//
// LIVE UNVERIFIED on current hardware (no AWS cluster provisioned tonight).
func Apply(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node, registryIface, primaryIface string, p Profile) (string, error) {
	script, err := ApplyScript(registryIface, primaryIface, p)
	if err != nil {
		return "", err
	}
	return k8s.HostExec(ctx, cs, cfg, namespace, node, script, 2*time.Minute)
}

// ClearAndWait deletes registry-iface shaping and polls until qdisc show has no
// netem/tbf — same "confirm, don't fire-and-forget" discipline as pod teardown
// and PSI cooldown. Host network config is not instant just because del returned.
//
// LIVE UNVERIFIED until next AWS provision.
func ClearAndWait(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node, registryIface string, poll, timeout time.Duration) (lastShow string, timedOut bool, err error) {
	if poll <= 0 {
		poll = 1 * time.Second
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if _, err := k8s.HostExec(ctx, cs, cfg, namespace, node, ClearScript(registryIface), time.Minute); err != nil {
		return "", false, fmt.Errorf("netshape clear: %w", err)
	}
	deadline := time.Now().Add(timeout)
	for {
		out, err := k8s.HostExec(ctx, cs, cfg, namespace, node, ShowScript(registryIface), time.Minute)
		if err != nil {
			return out, false, fmt.Errorf("netshape clear verify: %w", err)
		}
		lastShow = out
		if !ShapingPresent(out) {
			return lastShow, false, nil
		}
		if time.Now().After(deadline) {
			return lastShow, true, fmt.Errorf("netshape: shaping still present on %s after %s: %q", registryIface, timeout, strings.TrimSpace(out))
		}
		select {
		case <-ctx.Done():
			return lastShow, false, ctx.Err()
		case <-time.After(poll):
		}
		// Re-issue clear in case the first del raced with a lingering classful root.
		_, _ = k8s.HostExec(ctx, cs, cfg, namespace, node, ClearScript(registryIface), time.Minute)
	}
}

// EnsureLevel applies the profile for level (0 clears). Convenience for campaign setup.
func EnsureLevel(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node, registryIface, primaryIface string, level int) (Profile, string, error) {
	p, err := ProfileForLevel(level)
	if err != nil {
		return Profile{}, "", err
	}
	if registryIface == "" {
		registryIface = DefaultRegistryIface
	}
	if primaryIface == "" {
		primaryIface = DefaultPrimaryIface
	}
	out, err := Apply(ctx, cs, cfg, namespace, node, registryIface, primaryIface, p)
	return p, out, err
}
