package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"reloc-disrupt/internal/disrupt"
	"reloc-disrupt/internal/k8s"
	"reloc-disrupt/internal/logevent"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type decompRepeatOpts struct {
	Namespace string
	Image     string
	PreStop   int
	Grace     int
	Workers   int
	Repeats   int
}

type decompTrial struct {
	Trial            int
	ClusterIPMS      float64
	PodIPMS          float64
	DeltaClusterMinusPodIP float64 // >0 means ClusterIP slower (mechanistically expected)
	OK               bool
	Err              string
}

// runDecompRepeat measures ClusterIP vs pod-IP TTFS over N relocations with a
// fair dual poller: both paths share one HostExec process and start together
// once the replacement PodIP exists (avoids the construct-suite sampler-start
// asymmetry that can make ClusterIP look spuriously faster).
func runDecompRepeat(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, record func(logevent.Record), opt decompRepeatOpts) {
	if opt.Repeats < 1 {
		opt.Repeats = 8
	}
	if opt.Workers < 2 {
		opt.Workers = 4
	}
	workersNodes, err := k8s.WorkerNodes(ctx, cs)
	must(err)
	if len(workersNodes) < 1 {
		fail("need a worker for in-cluster dual poll")
	}
	loadNode := workersNodes[0].Name

	svcName := "uidprobe-decomp-svc"
	oldName := "uidprobe-decomp-old"
	newName := "uidprobe-decomp-new"
	cleanup(ctx, cs, opt.Namespace, svcName, oldName, newName)
	// Force-delete leftovers from prior grace periods
	zero := int64(0)
	for _, n := range []string{oldName, newName} {
		_ = cs.CoreV1().Pods(opt.Namespace).Delete(ctx, n, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	}
	time.Sleep(2 * time.Second)

	svc := disrupt.UIDService(svcName, opt.Namespace)
	if _, err := cs.CoreV1().Services(opt.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		fail(err.Error())
	}
	svcObj, err := cs.CoreV1().Services(opt.Namespace).Get(ctx, svcName, metav1.GetOptions{})
	must(err)
	clusterIP := svcObj.Spec.ClusterIP
	svcURL := fmt.Sprintf("http://%s:%d/", clusterIP, disrupt.Port)

	var trials []decompTrial
	cipSlower, pipSlower, tied := 0, 0, 0

	for i := 1; i <= opt.Repeats; i++ {
		fmt.Printf("\n=== decomp trial %d/%d ===\n", i, opt.Repeats)
		cleanup(ctx, cs, opt.Namespace, oldName, newName)
		_ = cs.CoreV1().Pods(opt.Namespace).Delete(ctx, oldName, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		_ = cs.CoreV1().Pods(opt.Namespace).Delete(ctx, newName, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		time.Sleep(1 * time.Second)

		oldPod := disrupt.UIDServerPod(oldName, opt.Namespace, opt.Image, int32(opt.PreStop), int32(opt.Grace))
		if _, err := cs.CoreV1().Pods(opt.Namespace).Create(ctx, oldPod, metav1.CreateOptions{}); err != nil {
			fail(err.Error())
		}
		old, err := waitPodReady(ctx, cs, opt.Namespace, oldName, 5*time.Minute)
		must(err)
		oldUID := string(old.UID)

		newPod := disrupt.UIDServerPod(newName, opt.Namespace, opt.Image, int32(opt.PreStop), int32(opt.Grace))
		created, err := cs.CoreV1().Pods(opt.Namespace).Create(ctx, newPod, metav1.CreateOptions{})
		must(err)
		t0API := created.CreationTimestamp.Time.UTC()
		newUID := string(created.UID)

		gracePeriod := int64(opt.Grace)
		_ = cs.CoreV1().Pods(opt.Namespace).Delete(ctx, oldName, metav1.DeleteOptions{
			GracePeriodSeconds: &gracePeriod,
		})

		newIP, err := waitPodIP(ctx, cs, opt.Namespace, newName, 3*time.Minute)
		must(err)
		podURL := fmt.Sprintf("http://%s:%d/", newIP, disrupt.Port)
		fmt.Printf("trial %d: old=%s new=%s ip=%s t0=%s\n", i, oldUID[:8], newUID[:8], newIP, t0API.Format(time.RFC3339))

		pair, err := runDualPathPoll(ctx, cs, cfg, opt.Namespace, loadNode, svcURL, podURL, newUID, opt.Workers, 90*time.Second)
		tr := decompTrial{Trial: i}
		if err != nil {
			tr.Err = err.Error()
			tr.OK = false
			fmt.Printf("trial %d FAIL: %v\n", i, err)
		} else {
			tr.ClusterIPMS = disrupt.FormatDurationMS(pair.ClusterIPT1.Sub(t0API))
			tr.PodIPMS = disrupt.FormatDurationMS(pair.PodIPT1.Sub(t0API))
			tr.DeltaClusterMinusPodIP = tr.ClusterIPMS - tr.PodIPMS
			tr.OK = !pair.ClusterIPT1.IsZero() && !pair.PodIPT1.IsZero()
			switch {
			case tr.DeltaClusterMinusPodIP > 5:
				cipSlower++ // ClusterIP larger TTFS (mechanistically expected)
			case tr.DeltaClusterMinusPodIP < -5:
				pipSlower++ // pod-IP larger TTFS (backwards vs mechanism)
			default:
				tied++
			}
			fmt.Printf("trial %d: clusterip=%.1fms podip=%.1fms delta(cluster-pod)=%.1fms\n",
				i, tr.ClusterIPMS, tr.PodIPMS, tr.DeltaClusterMinusPodIP)
		}
		trials = append(trials, tr)
		record(logevent.Record{
			Probe: "uidprobe", Check: fmt.Sprintf("decomp_trial_%d", i), Pass: tr.OK,
			Detail: map[string]any{
				"trial": i, "ttfs_clusterip_ms_vs_api_t0": tr.ClusterIPMS,
				"ttfs_podip_ms_vs_api_t0": tr.PodIPMS,
				"delta_cluster_minus_podip_ms": tr.DeltaClusterMinusPodIP,
				"client": "connection_close", "sampler": "dual_poller_same_hostexec",
				"old_uid": oldUID, "new_uid": newUID, "pod_ip": newIP,
			},
			Error: tr.Err,
		})

		// Force-delete so next trial is clean (don't wait out grace)
		_ = cs.CoreV1().Pods(opt.Namespace).Delete(ctx, oldName, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		_ = cs.CoreV1().Pods(opt.Namespace).Delete(ctx, newName, metav1.DeleteOptions{GracePeriodSeconds: &zero})
		time.Sleep(2 * time.Second)
	}

	cleanup(ctx, cs, opt.Namespace, svcName, oldName, newName)

	okN := 0
	for _, t := range trials {
		if t.OK {
			okN++
		}
	}
	// Direction: mechanistically ClusterIP TTFS >= pod-IP TTFS (delta >= 0).
	// "podip_consistently_slower" = pipFaster dominates (backwards finding).
	verdict := "noise"
	note := "ClusterIP-vs-pod-IP delta flips or ties across trials; single-trial magnitudes are illustrative only, not a stable decomposition."
	if okN >= 5 {
		if pipSlower >= okN-1 && cipSlower == 0 {
			verdict = "podip_consistently_slower"
			note = "pod-IP TTFS consistently exceeds ClusterIP TTFS — mechanistically unexpected; investigate ARP/CNI vs kube-proxy race before paper decomposition claim."
		} else if cipSlower >= okN-1 && pipSlower == 0 {
			verdict = "clusterip_consistently_slower_or_equal"
			note = "ClusterIP TTFS consistently >= pod-IP (within noise band) — matches EndpointSlice-after-reachability mechanism."
		} else if pipSlower > cipSlower && pipSlower >= (okN+1)/2 {
			verdict = "podip_leans_slower"
			note = "pod-IP often slower than ClusterIP but not unanimous; treat as lean, not settled mechanism."
		} else if cipSlower > pipSlower && cipSlower >= (okN+1)/2 {
			verdict = "clusterip_leans_slower"
			note = "ClusterIP often slower (expected direction) but not unanimous; magnitudes still noisy."
		}
	}

	record(logevent.Record{
		Probe: "uidprobe", Check: "decomp_repeat_summary", Pass: okN == opt.Repeats,
		Detail: map[string]any{
			"repeats": opt.Repeats, "ok_trials": okN,
			"clusterip_slower_count": cipSlower, // delta > +5ms
			"podip_slower_count":     pipSlower, // delta < -5ms
			"near_tie_count":        tied,      // |delta| <= 5ms
			"verdict":               verdict,
			"note":                  note,
			"trials":                trials,
			"threshold_ms":          5,
		},
		Error: iff(okN != opt.Repeats, fmt.Sprintf("%d/%d trials failed", opt.Repeats-okN, opt.Repeats), ""),
	})
	fmt.Printf("\nSUMMARY verdict=%s clusterip_slower=%d podip_slower=%d tie=%d ok=%d/%d\n%s\n",
		verdict, cipSlower, pipSlower, tied, okN, opt.Repeats, note)
}

type dualPathResult struct {
	ClusterIPT1 time.Time
	PodIPT1     time.Time
}

func runDualPathPoll(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, node, svcURL, podURL, wantUID string, workersPerPath int, timeout time.Duration) (dualPathResult, error) {
	if workersPerPath < 1 {
		workersPerPath = 4
	}
	script := fmt.Sprintf(`
set -euo pipefail
python3 - <<'PY'
import json, time, urllib.request, threading
from datetime import datetime, timezone
CIP = %q
PIP = %q
WANT = %q
WORKERS = %d
deadline = time.time() + %d
t1 = {"clusterip": None, "podip": None}
lock = threading.Lock()

def ts():
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")

def poll(url, path):
    while time.time() < deadline:
        with lock:
            if t1[path] is not None:
                return
            if t1["clusterip"] is not None and t1["podip"] is not None:
                return
        try:
            req = urllib.request.Request(url, headers={"Connection": "close"})
            with urllib.request.urlopen(req, timeout=1) as r:
                uid = r.headers.get("X-Pod-Uid") or ""
                status = r.status
                _ = r.read()
            if status == 200 and uid == WANT:
                with lock:
                    if t1[path] is None:
                        t1[path] = ts()
                return
        except Exception:
            pass
        time.sleep(0.005)

threads = []
for path, url in (("clusterip", CIP), ("podip", PIP)):
    for _ in range(WORKERS):
        t = threading.Thread(target=poll, args=(url, path), daemon=True)
        threads.append(t)
        t.start()
for t in threads:
    t.join(timeout=deadline - time.time() + 5)
print("RESULT_BEGIN")
print(json.dumps({"clusterip_t1": t1["clusterip"], "podip_t1": t1["podip"]}))
print("RESULT_END")
PY
`, svcURL, podURL, wantUID, workersPerPath, int(timeout.Seconds()))

	raw, err := k8s.HostExec(ctx, cs, cfg, ns, node, script, timeout+2*time.Minute)
	if err != nil {
		return dualPathResult{}, fmt.Errorf("dual poll: %w\n%s", err, truncate(raw, 800))
	}
	var payload struct {
		ClusterIPT1 *string `json:"clusterip_t1"`
		PodIPT1     *string `json:"podip_t1"`
	}
	in := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "RESULT_BEGIN" {
			in = true
			continue
		}
		if line == "RESULT_END" {
			break
		}
		if !in || line == "" {
			continue
		}
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			return dualPathResult{}, fmt.Errorf("parse dual result: %w (%q)", err, line)
		}
	}
	if payload.ClusterIPT1 == nil || payload.PodIPT1 == nil {
		return dualPathResult{}, fmt.Errorf("missing T1 (clusterip=%v podip=%v) raw=%s",
			payload.ClusterIPT1 != nil, payload.PodIPT1 != nil, truncate(raw, 400))
	}
	ct, err := time.Parse(time.RFC3339Nano, *payload.ClusterIPT1)
	if err != nil {
		ct, err = time.Parse(time.RFC3339, *payload.ClusterIPT1)
	}
	if err != nil {
		return dualPathResult{}, fmt.Errorf("clusterip t1 parse: %w", err)
	}
	pt, err := time.Parse(time.RFC3339Nano, *payload.PodIPT1)
	if err != nil {
		pt, err = time.Parse(time.RFC3339, *payload.PodIPT1)
	}
	if err != nil {
		return dualPathResult{}, fmt.Errorf("podip t1 parse: %w", err)
	}
	return dualPathResult{ClusterIPT1: ct.UTC(), PodIPT1: pt.UTC()}, nil
}
