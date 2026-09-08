// Command netprobe validates registry-vs-pod-network independence on AWS:
// tc shaping on the secondary (registry) ENI must degrade that path while
// leaving the primary (CNI/pod) path's latency/throughput essentially unchanged.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"reloc-disrupt/internal/k8s"
	"reloc-disrupt/internal/logevent"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	var (
		kubeconfig = flag.String("kubeconfig", "", "path to kubeconfig")
		namespace  = flag.String("namespace", "reloc-stage0", "namespace")
		outPath    = flag.String("out", "experiments/results/stage0/netprobe.jsonl", "JSONL output")
		primaryIF  = flag.String("primary-iface", "ens5", "CNI/pod primary iface")
		registryIF = flag.String("registry-iface", "ens6", "registry secondary iface (tc target)")
		delayMS    = flag.Int("shape-delay-ms", 100, "netem delay applied to registry iface")
		rateMbit   = flag.Int("shape-rate-mbit", 20, "tbf rate on registry iface")
		// Pass criteria (documented; not yet validated on a live AWS cluster — build-only so far).
		priRTTMaxRatio = flag.Float64("primary-rtt-max-ratio", 1.25, "primary RTT after shape must be < baseline*ratio + primary-rtt-slack-ms")
		priRTTSlackMS  = flag.Float64("primary-rtt-slack-ms", 5, "absolute ms slack added to primary RTT budget")
		priBWMinRatio  = flag.Float64("primary-bw-min-ratio", 0.70, "primary iperf Mbps after shape must be > baseline*ratio")
		regRTTMinAdd   = flag.Float64("registry-rtt-min-delay-frac", 0.50, "registry RTT must rise by at least frac*shape-delay-ms")
		regBWMaxRatio  = flag.Float64("registry-bw-max-ratio", 0.60, "registry Mbps must fall below baseline*ratio (or near shape-rate)")
	)
	flag.Parse()

	ctx := context.Background()
	cs, cfg, err := k8s.ClientsetFromFlags(*kubeconfig)
	must(err)
	must(k8s.EnsureNamespace(ctx, cs, *namespace))

	workers, err := k8s.WorkerNodes(ctx, cs)
	must(err)
	if len(workers) < 2 {
		fail("netprobe needs >= 2 workers")
	}
	a, b := workers[0].Name, workers[1].Name

	must(os.MkdirAll(filepath.Dir(*outPath), 0o755))
	w, err := logevent.Create(*outPath)
	must(err)
	defer w.Close()

	allPass := true
	record := func(check string, pass bool, detail map[string]any, err error) {
		rec := logevent.Record{Probe: "netprobe", Check: check, Pass: pass, Detail: detail}
		if err != nil {
			rec.Error = err.Error()
		}
		must(w.Write(rec))
		status := "PASS"
		if !pass {
			status = "FAIL"
			allPass = false
		}
		fmt.Printf("%s %s\n", status, check)
		if err != nil {
			fmt.Printf("  error: %v\n", err)
		}
	}

	if *registryIF == *primaryIF {
		fail("registry-iface must differ from primary-iface")
	}

	aPri, err := nodeIPv4(ctx, cs, cfg, *namespace, a, *primaryIF)
	must(err)
	bPri, err := nodeIPv4(ctx, cs, cfg, *namespace, b, *primaryIF)
	must(err)
	aReg, err := nodeIPv4(ctx, cs, cfg, *namespace, a, *registryIF)
	must(err)
	bReg, err := nodeIPv4(ctx, cs, cfg, *namespace, b, *registryIF)
	must(err)

	detailBase := map[string]any{
		"worker_a": a, "worker_b": b,
		"a_primary_ip": aPri, "b_primary_ip": bPri,
		"a_registry_ip": aReg, "b_registry_ip": bReg,
		"primary_iface": *primaryIF, "registry_iface": *registryIF,
	}
	record("ifaces_discovered", true, detailBase, nil)

	must(ensureIperf(ctx, cs, cfg, *namespace, a))
	must(ensureIperf(ctx, cs, cfg, *namespace, b))

	basePriRTT, err := measurePingAvg(ctx, cs, cfg, *namespace, a, bPri)
	must(err)
	baseRegRTT, err := measurePingAvg(ctx, cs, cfg, *namespace, a, bReg)
	must(err)
	basePriMbps, err := measureIperf(ctx, cs, cfg, *namespace, a, b, bPri)
	must(err)
	baseRegMbps, err := measureIperf(ctx, cs, cfg, *namespace, a, b, bReg)
	must(err)

	baseline := map[string]any{
		"primary_rtt_ms": basePriRTT, "registry_rtt_ms": baseRegRTT,
		"primary_mbps": basePriMbps, "registry_mbps": baseRegMbps,
	}
	for k, v := range detailBase {
		baseline[k] = v
	}
	record("baseline_measured", true, baseline, nil)

	shapeCmd := fmt.Sprintf(`
set -euo pipefail
IF=%q
PRI=%q
DELAY=%d
RATE=%d
if [[ "$IF" == "$PRI" ]]; then echo "refuse shape primary"; exit 1; fi
tc qdisc del dev "$IF" root 2>/dev/null || true
tc qdisc add dev "$IF" root handle 1: tbf rate "${RATE}mbit" burst 32kbit latency 400ms
tc qdisc add dev "$IF" parent 1:1 handle 10: netem delay "${DELAY}ms"
tc qdisc show dev "$IF"
`, *registryIF, *primaryIF, *delayMS, *rateMbit)
	_, err = k8s.HostExec(ctx, cs, cfg, *namespace, a, shapeCmd, 2*time.Minute)
	must(err)
	_, err = k8s.HostExec(ctx, cs, cfg, *namespace, b, shapeCmd, 2*time.Minute)
	must(err)
	defer func() {
		clear := fmt.Sprintf("tc qdisc del dev %q root 2>/dev/null || true", *registryIF)
		_, _ = k8s.HostExec(context.Background(), cs, cfg, *namespace, a, clear, time.Minute)
		_, _ = k8s.HostExec(context.Background(), cs, cfg, *namespace, b, clear, time.Minute)
	}()

	time.Sleep(2 * time.Second)

	shapedPriRTT, err := measurePingAvg(ctx, cs, cfg, *namespace, a, bPri)
	must(err)
	shapedRegRTT, err := measurePingAvg(ctx, cs, cfg, *namespace, a, bReg)
	must(err)
	shapedPriMbps, err := measureIperf(ctx, cs, cfg, *namespace, a, b, bPri)
	must(err)
	shapedRegMbps, err := measureIperf(ctx, cs, cfg, *namespace, a, b, bReg)
	must(err)

	regDegraded := shapedRegRTT > baseRegRTT+float64(*delayMS)*(*regRTTMinAdd)
	priStableRTT := shapedPriRTT < basePriRTT*(*priRTTMaxRatio)+(*priRTTSlackMS)
	priStableBW := shapedPriMbps > basePriMbps*(*priBWMinRatio)
	regBWDown := shapedRegMbps < baseRegMbps*(*regBWMaxRatio) || shapedRegMbps < float64(*rateMbit)*1.5
	pass := regDegraded && priStableRTT && priStableBW && regBWDown

	// Raw deltas always logged (signed): positive RTT delta = slower; negative BW delta = slower.
	// Same principle as naive-vs-UID-pinned TTFS: magnitude on record even when the boolean passes.
	dPriRTT := shapedPriRTT - basePriRTT
	dRegRTT := shapedRegRTT - baseRegRTT
	dPriBW := shapedPriMbps - basePriMbps
	dRegBW := shapedRegMbps - baseRegMbps
	dPriRTTPct := 0.0
	dPriBWPct := 0.0
	dRegRTTPct := 0.0
	dRegBWPct := 0.0
	if basePriRTT > 0 {
		dPriRTTPct = 100 * dPriRTT / basePriRTT
	}
	if basePriMbps > 0 {
		dPriBWPct = 100 * dPriBW / basePriMbps
	}
	if baseRegRTT > 0 {
		dRegRTTPct = 100 * dRegRTT / baseRegRTT
	}
	if baseRegMbps > 0 {
		dRegBWPct = 100 * dRegBW / baseRegMbps
	}

	fmt.Printf("deltas primary:  RTT %+.2f ms (%+.1f%%)  BW %+.2f Mbit/s (%+.1f%%)\n", dPriRTT, dPriRTTPct, dPriBW, dPriBWPct)
	fmt.Printf("deltas registry: RTT %+.2f ms (%+.1f%%)  BW %+.2f Mbit/s (%+.1f%%)\n", dRegRTT, dRegRTTPct, dRegBW, dRegBWPct)
	fmt.Printf("shape direction: downward rate cap (tbf %d mbit) + netem delay %d ms — not upward saturation\n", *rateMbit, *delayMS)

	deltas := map[string]any{
		"primary_rtt_delta_ms": dPriRTT, "primary_rtt_delta_pct": dPriRTTPct,
		"primary_bw_delta_mbps": dPriBW, "primary_bw_delta_pct": dPriBWPct,
		"registry_rtt_delta_ms": dRegRTT, "registry_rtt_delta_pct": dRegRTTPct,
		"registry_bw_delta_mbps": dRegBW, "registry_bw_delta_pct": dRegBWPct,
		"baseline_primary_rtt_ms": basePriRTT, "shaped_primary_rtt_ms": shapedPriRTT,
		"baseline_primary_mbps": basePriMbps, "shaped_primary_mbps": shapedPriMbps,
		"baseline_registry_rtt_ms": baseRegRTT, "shaped_registry_rtt_ms": shapedRegRTT,
		"baseline_registry_mbps": baseRegMbps, "shaped_registry_mbps": shapedRegMbps,
		"shape_direction": "downward_rate_cap_plus_delay",
		"shape_delay_ms":  *delayMS,
		"shape_rate_mbit": *rateMbit,
		"note":            "magnitudes recorded regardless of boolean pass/fail; thresholds are operational sanity, not paper-grade independence by themselves",
	}
	record("shaping_raw_deltas", true, deltas, nil)

	shaped := map[string]any{
		"primary_rtt_ms": shapedPriRTT, "registry_rtt_ms": shapedRegRTT,
		"primary_mbps": shapedPriMbps, "registry_mbps": shapedRegMbps,
		"baseline_primary_rtt_ms": basePriRTT, "baseline_registry_rtt_ms": baseRegRTT,
		"baseline_primary_mbps": basePriMbps, "baseline_registry_mbps": baseRegMbps,
		"deltas": deltas,
		"shape_delay_ms": *delayMS, "shape_rate_mbit": *rateMbit,
		"shape_direction": "downward_rate_cap_plus_delay",
		"reg_rtt_degraded": regDegraded, "primary_rtt_stable": priStableRTT,
		"primary_bw_stable": priStableBW, "registry_bw_down": regBWDown,
		"thresholds": map[string]any{
			"metrics":                   []string{"icmp_rtt_ms", "iperf3_mbps"},
			"packet_loss":               "not asserted",
			"primary_rtt_max":           fmt.Sprintf("< baseline*%.2f + %.0fms", *priRTTMaxRatio, *priRTTSlackMS),
			"primary_bw_min":            fmt.Sprintf("> baseline*%.2f", *priBWMinRatio),
			"registry_rtt_min_increase": fmt.Sprintf("> baseline + %.0f%% of shape delay", (*regRTTMinAdd)*100),
			"registry_bw_max":           fmt.Sprintf("< baseline*%.2f OR near %d mbit shape", *regBWMaxRatio, *rateMbit),
			"threshold_grade":           "operational_sanity_not_paper_independence",
			"live_cluster_validated":    false,
			"note":                      "boolean pass alone is insufficient for Results/Discussion; use shaping_raw_deltas magnitudes. Thresholds unsettled until live AWS numbers exist.",
		},
		"hardware_note": "c6id.xlarge Maximum Network Cards=1; primary+secondary ENIs share one physical NIC. Downward tbf avoids competing for the shared pool; do not claim physical path independence.",
		"note":          "tc on secondary ENI only; claim is logical isolation under tested downward load, not unqualified physical independence",
	}
	var shapeErr error
	if !pass {
		shapeErr = fmt.Errorf("primary moved or registry did not degrade under tc")
	}
	record("shaping_isolates_registry_eni", pass, shaped, shapeErr)

	priQ, err := k8s.HostExec(ctx, cs, cfg, *namespace, a,
		fmt.Sprintf("tc qdisc show dev %q", *primaryIF), time.Minute)
	must(err)
	noShapeOnPrimary := !strings.Contains(priQ, "netem") && !strings.Contains(priQ, "tbf")
	var priErr error
	if !noShapeOnPrimary {
		priErr = fmt.Errorf("primary iface unexpectedly shaped")
	}
	record("primary_iface_no_netem_tbf", noShapeOnPrimary, map[string]any{
		"primary_iface": *primaryIF, "tc": strings.TrimSpace(priQ),
	}, priErr)

	if !allPass {
		os.Exit(1)
	}
}

func nodeIPv4(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, node, iface string) (string, error) {
	out, err := k8s.HostExec(ctx, cs, cfg, ns, node,
		fmt.Sprintf(`ip -4 -o addr show dev %q | awk '{print $4}' | cut -d/ -f1 | head -n1`, iface),
		2*time.Minute)
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(out)
	if ip == "" {
		return "", fmt.Errorf("no IPv4 on %s/%s", node, iface)
	}
	return ip, nil
}

func ensureIperf(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, node string) error {
	out, err := k8s.HostExec(ctx, cs, cfg, ns, node, `
set -euo pipefail
if ! command -v iperf3 >/dev/null 2>&1; then
  echo "reloc-netprobe: iperf3 missing; installing via apt"
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -qq
  apt-get install -y -qq iperf3
fi
if ! command -v iperf3 >/dev/null 2>&1; then
  echo "iperf3 not installed after apt" >&2
  exit 1
fi
command -v iperf3
iperf3 --version | head -n1
`, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("iperf3 not installed on %s: %w (%s)", node, err, truncate(out, 400))
	}
	return nil
}

func measurePingAvg(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, fromNode, destIP string) (float64, error) {
	out, err := k8s.HostExec(ctx, cs, cfg, ns, fromNode,
		fmt.Sprintf(`ping -c 10 -W 2 %q | tail -n 1`, destIP), 2*time.Minute)
	if err != nil {
		return 0, err
	}
	// rtt min/avg/max/mdev = 0.123/0.456/...
	re := regexp.MustCompile(`=\s*[\d.]+/([\d.]+)/`)
	m := re.FindStringSubmatch(out)
	if len(m) < 2 {
		return 0, fmt.Errorf("parse ping avg from %q", strings.TrimSpace(out))
	}
	return strconv.ParseFloat(m[1], 64)
}

func measureIperf(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, clientNode, serverNode, serverIP string) (float64, error) {
	// Long-lived foreground server pod (same class as psi-mem-stress): keep the
	// container alive for the whole measurement. Do NOT HostExec nohup+return —
	// that tears down the hostexec cgroup and kills nsenter'd iperf3 (empty
	// /tmp/iperf-s.log + client connection refused after ss briefly passed).
	srv, err := startIperfServerPod(ctx, cs, ns, serverNode)
	if err != nil {
		return 0, fmt.Errorf("create iperf server pod: %w", err)
	}
	defer func() {
		_ = cs.CoreV1().Pods(ns).Delete(context.Background(), srv.Name, metav1.DeleteOptions{})
	}()
	if err := k8s.WaitPodRunning(ctx, cs, ns, srv.Name, 3*time.Minute); err != nil {
		logs, _ := k8s.PodLogs(ctx, cs, ns, srv.Name, "iperf-s", 100)
		return 0, fmt.Errorf("iperf server pod: %w (logs: %s)", err, truncate(logs, 400))
	}

	listenOut, err := k8s.HostExec(ctx, cs, cfg, ns, serverNode, `
set -euo pipefail
END=$((SECONDS+30))
while (( SECONDS < END )); do
  if ss -ltn 2>/dev/null | grep -qE ':5201([[:space:]]|$)'; then
    echo "reloc-netprobe: iperf3 listening on :5201"
    exit 0
  fi
  if ! pgrep -x iperf3 >/dev/null 2>&1; then
    echo "reloc-netprobe: iperf3 not in process table while waiting for bind" >&2
    exit 1
  fi
  sleep 0.2
done
echo "reloc-netprobe: iperf3 never bound :5201 within 30s" >&2
ss -ltn 2>/dev/null || true
exit 1
`, 2*time.Minute)
	if err != nil {
		logs, _ := k8s.PodLogs(ctx, cs, ns, srv.Name, "iperf-s", 100)
		return 0, fmt.Errorf("iperf3 server not listening on %s: %w (%s; pod logs: %s)",
			serverNode, err, truncate(listenOut, 300), truncate(logs, 400))
	}

	out, err := k8s.HostExec(ctx, cs, cfg, ns, clientNode,
		fmt.Sprintf(`iperf3 -c %q -t 5 -f m | tail -n 5`, serverIP), 2*time.Minute)
	if err != nil {
		logs, _ := k8s.PodLogs(ctx, cs, ns, srv.Name, "iperf-s", 100)
		return 0, fmt.Errorf("iperf: %w (%s; server logs: %s)", err, truncate(out, 300), truncate(logs, 300))
	}
	re := regexp.MustCompile(`([\d.]+)\s+Mbits/sec`)
	matches := re.FindAllStringSubmatch(out, -1)
	if len(matches) == 0 {
		return 0, fmt.Errorf("parse iperf mbps from %q", truncate(out, 400))
	}
	last := matches[len(matches)-1][1]
	return strconv.ParseFloat(last, 64)
}

// startIperfServerPod runs host iperf3 -s in the foreground via nsenter for the
// lifetime of the pod (no -1, no nohup detach). Container stays Running until deleted.
func startIperfServerPod(ctx context.Context, cs *kubernetes.Clientset, namespace, node string) (*corev1.Pod, error) {
	name := fmt.Sprintf("net-iperf-s-%d", time.Now().UnixNano()%1_000_000)
	// timeout keeps a leaked pod from holding :5201 forever if delete fails.
	script := `
set -euo pipefail
nsenter --target 1 --mount --uts --ipc --net --pid -- bash -c '
  set -euo pipefail
  echo "reloc-netprobe: host pid=$$ starting iperf3 -s (foreground)"
  if ! command -v iperf3 >/dev/null 2>&1; then
    echo "reloc-netprobe: iperf3 missing on host" >&2
    exit 1
  fi
  pkill -x iperf3 2>/dev/null || true
  sleep 0.3
  # No -1: accept many connections until the pod is deleted or timeout fires.
  exec timeout 180 iperf3 -s --forceflush
'
`
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": "reloc-netprobe-iperf", "stage0": "true"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			HostPID:       true,
			HostNetwork:   true,
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			}},
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchExpressions: []corev1.NodeSelectorRequirement{{
								Key:      "kubernetes.io/hostname",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{node},
							}},
						}},
					},
				},
			},
			Containers: []corev1.Container{{
				Name:            "iperf-s",
				Image:           k8s.HostExecImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
				Command:         []string{"bash", "-c", script},
			}},
		},
	}
	return cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
}

func boolPtr(v bool) *bool { return &v }

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func must(err error) {
	if err != nil {
		fail(err.Error())
	}
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}
