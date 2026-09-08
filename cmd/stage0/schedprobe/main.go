// Command schedprobe validates Stage 0 claim 2.2: nodeSelector/nodeAffinity
// placement goes through the real kube-scheduler binding path; nodeName does not.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reloc-disrupt/internal/k8s"
	"reloc-disrupt/internal/logevent"
	"reloc-disrupt/internal/place"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func main() {
	var (
		kubeconfig = flag.String("kubeconfig", "", "path to kubeconfig")
		namespace  = flag.String("namespace", "reloc-stage0", "namespace for probe pods")
		outPath    = flag.String("out", "experiments/results/stage0/schedprobe.jsonl", "JSONL output")
		timeout    = flag.Duration("timeout", 2*time.Minute, "per-check timeout")
	)
	flag.Parse()

	ctx := context.Background()
	cs, _, err := k8s.ClientsetFromFlags(*kubeconfig)
	must(err)
	must(k8s.EnsureNamespace(ctx, cs, *namespace))

	workers, err := k8s.WorkerNodes(ctx, cs)
	must(err)
	if len(workers) < 1 {
		fail("need at least one worker node")
	}
	target := workers[0]
	must(labelWorkers(ctx, cs, workers))

	must(os.MkdirAll(filepath.Dir(*outPath), 0o755))
	w, err := logevent.Create(*outPath)
	must(err)
	defer w.Close()

	labelKey := place.Stage0LabelKey
	labelVal := target.Name

	checks := []struct {
		name string
		fn   func() (bool, map[string]any, error)
	}{
		{"nodeSelector_scheduled_and_bound", func() (bool, map[string]any, error) {
			return checkSchedulerPath(ctx, cs, *namespace, "sel-"+short(target.Name),
				func(p *corev1.Pod) *corev1.Pod {
					return place.WithNodeSelector(p, labelKey, labelVal)
				}, target.Name, true, *timeout)
		}},
		{"nodeAffinity_scheduled_and_bound", func() (bool, map[string]any, error) {
			return checkSchedulerPath(ctx, cs, *namespace, "aff-"+short(target.Name),
				func(p *corev1.Pod) *corev1.Pod {
					return place.WithRequiredNodeAffinity(p, labelKey, labelVal)
				}, target.Name, true, *timeout)
		}},
		{"nodeName_negative_no_scheduler_event", func() (bool, map[string]any, error) {
			return checkSchedulerPath(ctx, cs, *namespace, "nn-"+short(target.Name),
				func(p *corev1.Pod) *corev1.Pod {
					return place.WithNodeName(p, target.Name)
				}, target.Name, false, *timeout)
		}},
		{"impossible_selector_pending", func() (bool, map[string]any, error) {
			return checkImpossible(ctx, cs, *namespace, "imp-"+short(target.Name), labelKey, *timeout)
		}},
	}

	allPass := true
	for _, c := range checks {
		pass, detail, err := c.fn()
		rec := logevent.Record{Probe: "schedprobe", Check: c.name, Pass: pass, Detail: detail}
		if err != nil {
			rec.Error = err.Error()
			rec.Pass = false
		}
		must(w.Write(rec))
		status := "PASS"
		if !rec.Pass {
			status = "FAIL"
			allPass = false
		}
		fmt.Printf("%s %s\n", status, c.name)
		if rec.Error != "" {
			fmt.Printf("  error: %s\n", rec.Error)
		}
	}
	if !allPass {
		os.Exit(1)
	}
}

func checkSchedulerPath(
	ctx context.Context,
	cs *kubernetes.Clientset,
	ns, name string,
	mutate func(*corev1.Pod) *corev1.Pod,
	wantNode string,
	expectSchedulerEvent bool,
	timeout time.Duration,
) (bool, map[string]any, error) {
	_ = cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
	base := place.ProbePod(name, ns)
	pod := mutate(base)
	created, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return false, nil, err
	}
	defer func() {
		_ = cs.CoreV1().Pods(ns).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
	}()

	detail := map[string]any{
		"pod":                     created.Name,
		"expect_scheduler_event":  expectSchedulerEvent,
		"want_node":               wantNode,
		"created_with_node_name":  pod.Spec.NodeName,
		"created_with_selector":   pod.Spec.NodeSelector,
		"binding_evidence":        "pod.spec.nodeName transition + Event reason=Scheduled reportingController/source=default-scheduler (Binding subresource is what DefaultBinder POSTs; without API audit we treat Scheduled from default-scheduler as binding-path evidence)",
	}

	deadline := time.Now().Add(timeout)
	var final *corev1.Pod
	for time.Now().Before(deadline) {
		p, err := cs.CoreV1().Pods(ns).Get(ctx, created.Name, metav1.GetOptions{})
		if err != nil {
			return false, detail, err
		}
		final = p
		if expectSchedulerEvent {
			if p.Spec.NodeName != "" && (p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodPending) {
				break
			}
		} else {
			// nodeName path: kubelet may start without scheduler
			if p.Status.Phase == corev1.PodRunning || p.Spec.NodeName != "" {
				time.Sleep(2 * time.Second) // allow events to land
				break
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	if final == nil {
		return false, detail, fmt.Errorf("pod vanished")
	}
	detail["final_node"] = final.Spec.NodeName
	detail["final_phase"] = string(final.Status.Phase)

	events, err := k8s.PodEvents(ctx, cs, ns, created.Name)
	if err != nil {
		return false, detail, err
	}
	var scheduledFromScheduler []string
	for _, ev := range events {
		src := ev.Source.Component
		if src == "" {
			src = ev.ReportingController
		}
		detail["events"] = appendEvent(detail["events"], map[string]any{
			"reason":  ev.Reason,
			"message": ev.Message,
			"source":  src,
			"type":    ev.Type,
		})
		if ev.Reason == "Scheduled" && (src == "default-scheduler" || strings.Contains(src, "scheduler")) {
			scheduledFromScheduler = append(scheduledFromScheduler, src+": "+ev.Message)
		}
	}
	detail["scheduled_events_from_scheduler"] = scheduledFromScheduler

	if expectSchedulerEvent {
		if final.Spec.NodeName != wantNode {
			return false, detail, fmt.Errorf("landed on %q want %q", final.Spec.NodeName, wantNode)
		}
		if len(scheduledFromScheduler) == 0 {
			return false, detail, fmt.Errorf("missing Scheduled event from default-scheduler (binding path not evidenced)")
		}
		return true, detail, nil
	}

	// Negative control: must NOT have scheduler Scheduled event.
	if len(scheduledFromScheduler) > 0 {
		return false, detail, fmt.Errorf("nodeName path unexpectedly has scheduler Scheduled events: %v", scheduledFromScheduler)
	}
	if final.Spec.NodeName != wantNode {
		return false, detail, fmt.Errorf("nodeName pod not on %q (got %q)", wantNode, final.Spec.NodeName)
	}
	return true, detail, nil
}

func checkImpossible(ctx context.Context, cs *kubernetes.Clientset, ns, name, labelKey string, timeout time.Duration) (bool, map[string]any, error) {
	_ = cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{})
	pod := place.WithNodeSelector(place.ProbePod(name, ns), labelKey, "no-such-node-label-value")
	created, err := cs.CoreV1().Pods(ns).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return false, nil, err
	}
	defer func() {
		_ = cs.CoreV1().Pods(ns).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
	}()

	// Wait long enough for scheduler to emit FailedScheduling / remain Pending.
	time.Sleep(8 * time.Second)
	p, err := cs.CoreV1().Pods(ns).Get(ctx, created.Name, metav1.GetOptions{})
	if err != nil {
		return false, nil, err
	}
	detail := map[string]any{
		"pod":   p.Name,
		"phase": string(p.Status.Phase),
		"node":  p.Spec.NodeName,
	}
	events, err := k8s.PodEvents(ctx, cs, ns, created.Name)
	if err != nil {
		return false, detail, err
	}
	unsched := false
	for _, ev := range events {
		src := ev.Source.Component
		if src == "" {
			src = ev.ReportingController
		}
		detail["events"] = appendEvent(detail["events"], map[string]any{
			"reason":  ev.Reason,
			"message": ev.Message,
			"source":  src,
		})
		if ev.Reason == "FailedScheduling" || strings.Contains(strings.ToLower(ev.Message), "affinity") || strings.Contains(ev.Message, "node selector") || strings.Contains(ev.Message, "didn't match") {
			unsched = true
		}
	}
	detail["unschedulable_evidenced"] = unsched

	if p.Spec.NodeName != "" {
		return false, detail, fmt.Errorf("impossible selector was bound to %q", p.Spec.NodeName)
	}
	if p.Status.Phase != corev1.PodPending {
		return false, detail, fmt.Errorf("want Pending, got %s", p.Status.Phase)
	}
	if !unsched {
		// Still pass Pending+empty nodeName; note weak event evidence.
		detail["note"] = "Pending with empty nodeName; FailedScheduling event not seen within wait window"
	}
	_ = timeout
	return true, detail, nil
}

func labelWorkers(ctx context.Context, cs *kubernetes.Clientset, workers []corev1.Node) error {
	for _, n := range workers {
		node, err := cs.CoreV1().Nodes().Get(ctx, n.Name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels[place.Stage0LabelKey] = n.Name
		if _, err := cs.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func appendEvent(existing any, ev map[string]any) []any {
	var list []any
	if existing != nil {
		list, _ = existing.([]any)
	}
	return append(list, ev)
}

func short(s string) string {
	s = strings.ToLower(s)
	if len(s) > 20 {
		return s[:20]
	}
	return s
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
