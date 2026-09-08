// Command uidprobe validates Stage 0 claim 2.3: UID-pinned TTFS under graceful
// overlap, per docs/measurement-spec.md (Connection: close headline; ClusterIP
// and direct pod-IP both reported).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reloc-disrupt/internal/disrupt"
	"reloc-disrupt/internal/k8s"
	"reloc-disrupt/internal/logevent"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	var (
		kubeconfig = flag.String("kubeconfig", "", "path to kubeconfig")
		namespace  = flag.String("namespace", "reloc-stage0", "namespace")
		suite      = flag.String("suite", "construct", "construct | decomp-repeat")
		outPath    = flag.String("out", "", "JSONL output (defaults per suite)")
		image      = flag.String("image", disrupt.DefaultImage, "uid server image")
		preStop    = flag.Int("prestop-sec", 12, "preStop sleep for graceful overlap")
		grace      = flag.Int("grace-sec", 20, "terminationGracePeriodSeconds")
		workers    = flag.Int("load-workers", 12, "concurrent Connection:close clients (in-cluster)")
		loadSec    = flag.Int("load-sec", 45, "max seconds to collect under load after create")
		repeats    = flag.Int("repeats", 8, "decomp-repeat: number of relocation trials")
	)
	flag.Parse()

	if *outPath == "" {
		switch *suite {
		case "decomp-repeat":
			*outPath = "experiments/results/stage0/uidprobe-decomp-repeat.jsonl"
		default:
			*outPath = "experiments/results/stage0/uidprobe.jsonl"
		}
	}

	ctx := context.Background()
	cs, cfg, err := k8s.ClientsetFromFlags(*kubeconfig)
	must(err)
	must(k8s.EnsureNamespace(ctx, cs, *namespace))

	must(os.MkdirAll(filepath.Dir(*outPath), 0o755))
	w, err := logevent.Create(*outPath)
	must(err)
	defer w.Close()

	allPass := true
	record := func(rec logevent.Record) {
		must(w.Write(rec))
		status := "PASS"
		if !rec.Pass {
			status = "FAIL"
			allPass = false
		}
		fmt.Printf("%s %s\n", status, rec.Check)
		if rec.Error != "" {
			fmt.Printf("  error: %s\n", rec.Error)
		}
	}

	switch *suite {
	case "construct":
		// fall through to original claim-2.3 construct checks below
	case "decomp-repeat":
		runDecompRepeat(ctx, cs, cfg, record, decompRepeatOpts{
			Namespace: *namespace,
			Image:     *image,
			PreStop:   *preStop,
			Grace:     *grace,
			Workers:   *workers,
			Repeats:   *repeats,
		})
		if !allPass {
			os.Exit(1)
		}
		return
	default:
		fail("unknown -suite " + *suite + " (want construct|decomp-repeat)")
	}

	svcName := "uidprobe-svc"
	oldName := "uidprobe-old"
	newName := "uidprobe-new"

	cleanup(ctx, cs, *namespace, svcName, oldName, newName)

	svc := disrupt.UIDService(svcName, *namespace)
	if _, err := cs.CoreV1().Services(*namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		fail(err.Error())
	}
	oldPod := disrupt.UIDServerPod(oldName, *namespace, *image, int32(*preStop), int32(*grace))
	if _, err := cs.CoreV1().Pods(*namespace).Create(ctx, oldPod, metav1.CreateOptions{}); err != nil {
		fail(err.Error())
	}
	old, err := waitPodReady(ctx, cs, *namespace, oldName, 5*time.Minute)
	must(err)
	oldUID := string(old.UID)
	fmt.Printf("old pod ready uid=%s ip=%s\n", oldUID, old.Status.PodIP)

	svcObj, err := cs.CoreV1().Services(*namespace).Get(ctx, svcName, metav1.GetOptions{})
	must(err)
	clusterIP := svcObj.Spec.ClusterIP
	svcURL := fmt.Sprintf("http://%s:%d/", clusterIP, disrupt.Port)

	workersNodes, err := k8s.WorkerNodes(ctx, cs)
	must(err)
	if len(workersNodes) < 1 {
		fail("need a worker for in-cluster load (ClusterIP/pod IP not reachable from Windows host)")
	}
	loadNode := workersNodes[0].Name

	// Baseline: service returns old UID
	baseOut, err := curlUID(ctx, cs, cfg, *namespace, loadNode, svcURL, 1)
	must(err)
	baseUID := firstUID(baseOut)
	pass := baseUID == oldUID
	record(logevent.Record{
		Probe: "uidprobe", Check: "baseline_clusterip_old_uid", Pass: pass,
		Detail: map[string]any{"old_uid": oldUID, "observed": baseUID, "raw": truncate(baseOut, 300)},
		Error:  iff(!pass, fmt.Sprintf("want old uid %s got %q", oldUID, baseUID), ""),
	})

	// Start Connection:close load BEFORE create/delete so overlap is observed.
	// Use a detached context: cancelling early (after pod-IP T1) would kill HostExec
	// mid-flight and drop all ClusterIP samples.
	type loadResult struct {
		samples []disrupt.Sample
		err     error
	}
	loadCh := make(chan loadResult, 1)
	go func() {
		s, e := runClusterLoad(context.Background(), cs, cfg, *namespace, loadNode, svcURL, *workers, *loadSec)
		loadCh <- loadResult{s, e}
	}()
	time.Sleep(2 * time.Second) // let load establish old-UID traffic

	// Create replacement (T0 = API creationTimestamp)
	newPod := disrupt.UIDServerPod(newName, *namespace, *image, int32(*preStop), int32(*grace))
	t0Client := time.Now().UTC()
	created, err := cs.CoreV1().Pods(*namespace).Create(ctx, newPod, metav1.CreateOptions{})
	must(err)
	t0API := created.CreationTimestamp.Time.UTC()
	newUID := string(created.UID)
	fmt.Printf("new pod created uid=%s t0_api=%s\n", newUID, t0API.Format(time.RFC3339Nano))

	// Start graceful deletion of old → overlap window
	gracePeriod := int64(*grace)
	_ = cs.CoreV1().Pods(*namespace).Delete(ctx, oldName, metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
	})
	deleteAt := time.Now().UTC()

	// Start direct pod-IP polling as soon as PodIP exists (not after Ready), so
	// ClusterIP vs pod-IP T1 share the same post-create window for decomposition.
	newIP, err := waitPodIP(ctx, cs, *namespace, newName, 3*time.Minute)
	must(err)
	podURL := fmt.Sprintf("http://%s:%d/", newIP, disrupt.Port)
	fmt.Printf("new pod ip=%s; starting pod-IP poll\n", newIP)

	type pollResult struct {
		samples []disrupt.Sample
		err     error
	}
	podCh := make(chan pollResult, 1)
	go func() {
		s, e := runPodIPPoll(ctx, cs, cfg, *namespace, loadNode, podURL, newUID, 90*time.Second)
		podCh <- pollResult{s, e}
	}()

	if _, err := waitPodReady(ctx, cs, *namespace, newName, 3*time.Minute); err != nil {
		fmt.Printf("warn: wait ready: %v\n", err)
	}

	pr := <-podCh
	podSamples, podErr := pr.samples, pr.err
	if podErr != nil {
		record(logevent.Record{Probe: "uidprobe", Check: "podip_load", Pass: false, Error: podErr.Error(), Detail: map[string]any{"samples": len(podSamples)}})
	}

	fmt.Printf("waiting for ClusterIP load (%ds) to finish...\n", *loadSec)
	lr := <-loadCh
	samples := lr.samples
	if lr.err != nil {
		record(logevent.Record{Probe: "uidprobe", Check: "clusterip_load", Pass: false, Error: lr.err.Error()})
	} else {
		record(logevent.Record{Probe: "uidprobe", Check: "clusterip_load", Pass: true, Detail: map[string]any{"samples": len(samples)}})
	}
	allSamples := append(append([]disrupt.Sample{}, samples...), podSamples...)

	// --- Assertions ---

	// 1) Overlap: old UID responses after DELETE
	oldAfterDelete := disrupt.CountUID(samples, oldUID, deleteAt, deleteAt.Add(time.Duration(*grace)*time.Second))
	pass = oldAfterDelete > 0
	record(logevent.Record{
		Probe: "uidprobe", Check: "overlap_old_uid_after_delete", Pass: pass,
		Detail: map[string]any{"old_uid_hits_after_delete": oldAfterDelete, "delete_at": deleteAt},
		Error:  iff(!pass, "no old-UID 200s after DELETE during grace window", ""),
	})

	// 2) Pinning: first new-UID on ClusterIP
	firstNew, ok := disrupt.FirstSuccessUID(samples, newUID, t0Client)
	pass = ok
	var ttfsClusterAPI, ttfsClusterClient float64
	if ok {
		ttfsClusterAPI = disrupt.FormatDurationMS(firstNew.At.Sub(t0API))
		ttfsClusterClient = disrupt.FormatDurationMS(firstNew.At.Sub(t0Client))
	}
	record(logevent.Record{
		Probe: "uidprobe", Check: "clusterip_first_new_uid", Pass: pass,
		Detail: map[string]any{
			"new_uid": newUID, "t1": firstNew.At, "t0_api": t0API, "t0_client": t0Client,
			"ttfs_clusterip_ms_vs_api_t0":    ttfsClusterAPI,
			"ttfs_clusterip_ms_vs_client_t0": ttfsClusterClient,
			"client":                        "connection_close",
			"path":                          "clusterip",
		},
		Error: iff(!pass, "never saw new UID on ClusterIP under Connection:close load", ""),
	})

	// Old successes before first new (under overlap)
	oldBeforeNew := 0
	if ok {
		oldBeforeNew = disrupt.CountUID(samples, oldUID, deleteAt, firstNew.At)
	}
	pass = ok && oldBeforeNew > 0
	record(logevent.Record{
		Probe: "uidprobe", Check: "pinning_old_before_first_new", Pass: pass,
		Detail: map[string]any{"old_uid_hits_before_first_new": oldBeforeNew},
		Error:  iff(!pass, "first new-UID without prior old-UID hits in overlap (pinning not demonstrated)", ""),
	})

	// 3) Naive bias: first any-200 vs first new-UID
	firstAny, okAny := disrupt.FirstAny200(samples, t0Client)
	naiveMS, pinnedMS := 0.0, 0.0
	if okAny {
		naiveMS = disrupt.FormatDurationMS(firstAny.At.Sub(t0Client))
	}
	if ok {
		pinnedMS = ttfsClusterClient
	}
	biased := ok && okAny && firstAny.UID != newUID && firstAny.At.Before(firstNew.At)
	pass = biased
	record(logevent.Record{
		Probe: "uidprobe", Check: "naive_any200_bias", Pass: pass,
		Detail: map[string]any{
			"first_any_uid": firstAny.UID, "first_any_ms": naiveMS,
			"first_new_uid_ms": pinnedMS, "biased": biased,
			"note": "time-to-first-any-200 should understate disruption vs UID-pinned T1 during overlap",
		},
		Error: iff(!pass, "naive clock not demonstrably earlier/wrong-UID vs UID-pinned clock", ""),
	})

	// 4) Direct pod-IP TTFS
	firstPod, okPod := disrupt.FirstSuccessUID(podSamples, newUID, t0Client)
	pass = okPod
	var ttfsPodAPI, ttfsPodClient float64
	if okPod {
		ttfsPodAPI = disrupt.FormatDurationMS(firstPod.At.Sub(t0API))
		ttfsPodClient = disrupt.FormatDurationMS(firstPod.At.Sub(t0Client))
	}
	record(logevent.Record{
		Probe: "uidprobe", Check: "podip_first_new_uid", Pass: pass,
		Detail: map[string]any{
			"new_uid": newUID, "pod_ip": newIP, "t1": firstPod.At,
			"ttfs_podip_ms_vs_api_t0":    ttfsPodAPI,
			"ttfs_podip_ms_vs_client_t0": ttfsPodClient,
			"client":                    "connection_close",
			"path":                      "podip",
		},
		Error: iff(!pass, "never saw new UID on direct pod IP", ""),
	})

	// 5) Decomposition note (both reported)
	record(logevent.Record{
		Probe: "uidprobe", Check: "ttfs_decomposition_reported", Pass: ok && okPod,
		Detail: map[string]any{
			"ttfs_clusterip_ms_vs_client_t0": ttfsClusterClient,
			"ttfs_podip_ms_vs_client_t0":     ttfsPodClient,
			"endpoint_lag_ms_approx":        ttfsClusterClient - ttfsPodClient,
			"note":                          "ClusterIP includes EndpointSlice lag; pod-IP is readiness-only. Both are first-class per measurement-spec.",
		},
		Error: iff(!(ok && okPod), "need both paths to report decomposition", ""),
	})

	// 6) Header matches Downward API UIDs only (no foreign UIDs under load)
	foreign := 0
	for _, s := range allSamples {
		if s.Status != 200 || s.Err != "" || s.UID == "" {
			continue
		}
		if s.UID != oldUID && s.UID != newUID {
			foreign++
		}
	}
	pass = foreign == 0
	record(logevent.Record{
		Probe: "uidprobe", Check: "header_uids_are_downward_api", Pass: pass,
		Detail: map[string]any{"foreign_uid_responses": foreign, "old_uid": oldUID, "new_uid": newUID},
		Error:  iff(!pass, fmt.Sprintf("%d responses with unexpected X-Pod-Uid", foreign), ""),
	})

	// Distinct UIDs across relocation
	pass = oldUID != newUID && oldUID != "" && newUID != ""
	record(logevent.Record{
		Probe: "uidprobe", Check: "replacement_uid_distinct", Pass: pass,
		Detail: map[string]any{"old_uid": oldUID, "new_uid": newUID},
	})

	cleanup(ctx, cs, *namespace, svcName, oldName, newName)

	if !allPass {
		os.Exit(1)
	}
}

func runClusterLoad(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, node, url string, workers, sec int) ([]disrupt.Sample, error) {
	script := fmt.Sprintf(`
set -euo pipefail
python3 - <<'PY'
import json, time, urllib.request, concurrent.futures, threading
from datetime import datetime, timezone
URL = %q
WORKERS = %d
DURATION = %d
out = []
lock = threading.Lock()
stop = time.time() + DURATION

def ts():
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")

def one(_):
    while time.time() < stop:
        try:
            req = urllib.request.Request(URL, headers={"Connection": "close"})
            with urllib.request.urlopen(req, timeout=2) as r:
                uid = r.headers.get("X-Pod-Uid") or ""
                status = r.status
                _ = r.read()
            err = ""
        except Exception as e:
            uid, status, err = "", 0, str(e)
        with lock:
            out.append({"at": ts(), "uid": uid, "status": status, "path": "clusterip", "err": err})

with concurrent.futures.ThreadPoolExecutor(max_workers=WORKERS) as ex:
    list(ex.map(one, range(WORKERS)))
print("SAMPLES_BEGIN")
for s in out:
    print(json.dumps(s))
print("SAMPLES_END")
PY
`, url, workers, sec)
	raw, err := k8s.HostExec(ctx, cs, cfg, ns, node, script, time.Duration(sec+120)*time.Second)
	if err != nil {
		return nil, fmt.Errorf("cluster load: %w\n%s", err, truncate(raw, 800))
	}
	return parseSamples(raw, "clusterip")
}

func runPodIPPoll(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, node, url, wantUID string, timeout time.Duration) ([]disrupt.Sample, error) {
	script := fmt.Sprintf(`
set -euo pipefail
python3 - <<'PY'
import json, time, urllib.request
from datetime import datetime, timezone
URL = %q
WANT = %q
deadline = time.time() + %d
out = []

def ts():
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")

while time.time() < deadline:
    try:
        req = urllib.request.Request(URL, headers={"Connection": "close"})
        with urllib.request.urlopen(req, timeout=2) as r:
            uid = r.headers.get("X-Pod-Uid") or ""
            status = r.status
            _ = r.read()
        err = ""
    except Exception as e:
        uid, status, err = "", 0, str(e)
    out.append({"at": ts(), "uid": uid, "status": status, "path": "podip", "err": err})
    if status == 200 and uid == WANT:
        break
    time.sleep(0.02)
print("SAMPLES_BEGIN")
for s in out:
    print(json.dumps(s))
print("SAMPLES_END")
PY
`, url, wantUID, int(timeout.Seconds()))
	raw, err := k8s.HostExec(ctx, cs, cfg, ns, node, script, timeout+2*time.Minute)
	if err != nil {
		return parseSamples(raw, "podip")
	}
	return parseSamples(raw, "podip")
}

func parseSamples(raw, defaultPath string) ([]disrupt.Sample, error) {
	var out []disrupt.Sample
	in := false
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "SAMPLES_BEGIN" {
			in = true
			continue
		}
		if line == "SAMPLES_END" {
			break
		}
		if !in || line == "" {
			continue
		}
		var row struct {
			At     string `json:"at"`
			UID    string `json:"uid"`
			Status int    `json:"status"`
			Path   string `json:"path"`
			Err    string `json:"err"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, row.At)
		if err != nil {
			t, err = time.Parse(time.RFC3339, row.At)
		}
		if err != nil {
			// python format may be odd; try best-effort
			t = time.Now().UTC()
		}
		path := row.Path
		if path == "" {
			path = defaultPath
		}
		out = append(out, disrupt.Sample{At: t.UTC(), UID: row.UID, Status: row.Status, Path: path, Err: row.Err})
	}
	return out, nil
}

func curlUID(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, node, url string, n int) (string, error) {
	script := fmt.Sprintf(`set -euo pipefail
for i in $(seq 1 %d); do
  curl -fsS -D - --http1.1 -H 'Connection: close' %q -o /tmp/uidbody.txt | tee /tmp/uidheaders.txt
  echo
done
`, n, url)
	return k8s.HostExec(ctx, cs, cfg, ns, node, script, 2*time.Minute)
}

func firstUID(curlOut string) string {
	for _, line := range strings.Split(curlOut, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(line), "x-pod-uid:") {
			return strings.TrimSpace(line[len("x-pod-uid:"):])
		}
		if strings.HasPrefix(line, "X-Pod-Uid:") {
			return strings.TrimSpace(line[len("X-Pod-Uid:"):])
		}
	}
	return ""
}

func waitPodIP(ctx context.Context, cs *kubernetes.Clientset, ns, name string, timeout time.Duration) (string, error) {
	var ip string
	err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if p.Status.PodIP == "" {
			return false, nil
		}
		ip = p.Status.PodIP
		return true, nil
	})
	return ip, err
}

func waitPodReady(ctx context.Context, cs *kubernetes.Clientset, ns, name string, timeout time.Duration) (*corev1.Pod, error) {
	var out *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		out = p
		if p.Status.Phase != corev1.PodRunning || p.Status.PodIP == "" {
			return false, nil
		}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				return true, nil
			}
		}
		return false, nil
	})
	return out, err
}

func cleanup(ctx context.Context, cs *kubernetes.Clientset, ns string, names ...string) {
	for _, n := range names {
		_ = cs.CoreV1().Pods(ns).Delete(ctx, n, metav1.DeleteOptions{})
		_ = cs.CoreV1().Services(ns).Delete(ctx, n, metav1.DeleteOptions{})
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func iff(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
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
