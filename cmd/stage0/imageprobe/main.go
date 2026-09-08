// Command imageprobe validates Stage 0 claim 2.5.
//
// Suites:
//
//	plumbing  — discard_unpacked_layers + warm/cold accounting on a single image (default: pause)
//	layer-share — ground-truth L0/L1/L2 images via in-cluster registry (paper divergence claim)
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
	"reloc-disrupt/internal/nodeobs"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func main() {
	var (
		kubeconfig = flag.String("kubeconfig", "", "path to kubeconfig")
		namespace  = flag.String("namespace", "reloc-stage0", "namespace for hostexec pods")
		outPath    = flag.String("out", "experiments/results/stage0/imageprobe.jsonl", "JSONL output")
		suite      = flag.String("suite", "plumbing", "plumbing | layer-share | all")
		image      = flag.String("image", "registry.k8s.io/pause:3.9", "image for plumbing suite")
		allWorkers = flag.Bool("all-workers", true, "plumbing: probe every worker")
		gtFile     = flag.String("ground-truth", "", "path to layer-ground-truth.json (optional; else ConfigMap)")
		gtNS       = flag.String("ground-truth-ns", "reloc-registry", "namespace of layer-ground-truth ConfigMap")
		tol        = flag.Int64("tol-bytes", 0, "absolute byte tolerance for uncached≈size assertions (0 = exact)")
	)
	flag.Parse()

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
	case "plumbing":
		runPlumbing(ctx, cs, cfg, *namespace, *image, *allWorkers, record)
	case "layer-share":
		runLayerShare(ctx, cs, cfg, *namespace, *gtFile, *gtNS, *tol, record)
	case "all":
		runPlumbing(ctx, cs, cfg, *namespace, *image, *allWorkers, record)
		runLayerShare(ctx, cs, cfg, *namespace, *gtFile, *gtNS, *tol, record)
	default:
		fail("unknown -suite " + *suite)
	}

	if !allPass {
		os.Exit(1)
	}
}

func runPlumbing(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, image string, allWorkers bool, record func(logevent.Record)) {
	workers, err := k8s.WorkerNodes(ctx, cs)
	must(err)
	if len(workers) < 1 {
		fail("need at least one worker")
	}
	nodes := []string{workers[0].Name}
	if allWorkers {
		nodes = nodes[:0]
		for _, n := range workers {
			nodes = append(nodes, n.Name)
		}
	}

	fmt.Printf("imageprobe plumbing assumption: discard_unpacked_layers=%v\n", nodeobs.DiscardUnpackedAssumption)

	for _, node := range nodes {
		discard, ver, err := nodeobs.ReadDiscardUnpackedLayers(ctx, cs, cfg, namespace, node)
		detail := map[string]any{"node": node, "version": ver, "assumption": nodeobs.DiscardUnpackedAssumption}
		if discard != nil {
			detail["live_discard_unpacked_layers"] = *discard
		}
		pass := err == nil
		if discard != nil && *discard != nodeobs.DiscardUnpackedAssumption {
			pass = false
			detail["mismatch"] = true
		}
		rec := logevent.Record{Probe: "imageprobe", Check: "discard_unpacked_layers_" + node, Pass: pass, Detail: detail}
		if err != nil {
			rec.Error = err.Error()
			rec.Pass = false
		}
		record(rec)

		rep, err := nodeobs.UncachedBytesForImage(ctx, cs, cfg, namespace, node, image)
		detail = map[string]any{"report": rep}
		pass = err == nil && rep != nil
		if pass && len(rep.Layers) == 0 && !containsNote(rep, "ERR=image_not_on_node") && !rep.CRIPresent {
			pass = false
		}
		if pass && rep.CRIPresent && rep.UncachedBytes != 0 {
			pass = false
			detail["error"] = "CRI present but uncached_bytes != 0"
		}
		if pass && rep.UncachedBytes > rep.TotalLayerBytes {
			pass = false
			detail["error"] = "uncached_bytes > total_layer_bytes"
		}
		rec = logevent.Record{Probe: "imageprobe", Check: "uncached_bytes_" + node, Pass: pass, Detail: detail}
		if err != nil {
			rec.Error = err.Error()
			rec.Pass = false
		}
		record(rec)

		// Plumbing contrast: warm/CRI accounting only — NOT layer-sharing divergence.
		if rep != nil && rep.TotalLayerBytes > 0 {
			record(logevent.Record{
				Probe: "imageprobe",
				Check: "uncached_vs_total_contrast_" + node,
				Pass:  true,
				Detail: map[string]any{
					"uncached_bytes":    rep.UncachedBytes,
					"total_layer_bytes": rep.TotalLayerBytes,
					"diverged":          rep.UncachedBytes != rep.TotalLayerBytes,
					"note":              "plumbing only: CRI-present/warm accounting; does not prove layer-sharing divergence",
				},
			})
		}
	}
}

func runLayerShare(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, gtFile, gtNS string, tol int64, record func(logevent.Record)) {
	workers, err := k8s.WorkerNodes(ctx, cs)
	must(err)
	if len(workers) < 2 {
		fail("layer-share suite needs >= 2 workers")
	}
	w1, w2 := workers[0].Name, workers[1].Name

	gt, err := loadGroundTruth(ctx, cs, gtFile, gtNS)
	must(err)
	l0, l1, l2 := gt.LayerBlobBytes["L0"], gt.LayerBlobBytes["L1"], gt.LayerBlobBytes["L2"]
	fmt.Printf("layer-share ground truth blob bytes: L0=%d L1=%d L2=%d\n", l0, l1, l2)

	regIP, err := registryClusterIP(ctx, cs)
	must(err)
	reg := fmt.Sprintf("%s:5000", regIP)
	prefix := gt.RepoPrefix
	if prefix == "" {
		prefix = "reloc"
	}
	appA := fmt.Sprintf("%s/%s/app-a:v1", reg, prefix)
	appB := fmt.Sprintf("%s/%s/app-b:v1", reg, prefix)
	base := fmt.Sprintf("%s/%s/base:v1", reg, prefix)
	repoA := prefix + "/app-a"
	repoB := prefix + "/app-b"

	detailBase := map[string]any{
		"registry": reg, "app_a": appA, "app_b": appB, "base": base,
		"L0": l0, "L1": l1, "L2": l2, "tol_bytes": tol,
		"file_bytes": gt.FileBytes,
	}

	_, err = nodeobs.RemoveImageAndContent(ctx, cs, cfg, namespace, w1, []string{appA, appB, base}, true)
	clearDigests(ctx, cs, cfg, namespace, w1, gt)
	record(checkRecord("layer_share_worker1_cleared", err == nil, detailBase, err))

	coldA, err := nodeobs.UncachedBytesViaRegistryManifest(ctx, cs, cfg, namespace, w1, reg, repoA, "v1")
	wantColdA := l0 + l1
	pass := err == nil && coldA != nil && !coldA.CRIPresent && nodeobs.ApproxEqual(coldA.UncachedBytes, wantColdA, tol)
	d := copyMap(detailBase)
	d["node"] = w1
	d["uncached"] = coldA
	d["want"] = wantColdA
	rec := checkRecord("layer_share_worker1_cold_app_a", pass, d, err)
	if err == nil && coldA != nil && !pass {
		rec.Error = fmt.Sprintf("uncached(app-a)=%d want %d (L0+L1)", coldA.UncachedBytes, wantColdA)
		rec.Pass = false
	}
	record(rec)

	_, err = nodeobs.PullImagePlainHTTP(ctx, cs, cfg, namespace, w1, appA)
	record(checkRecord("layer_share_worker1_pull_app_a", err == nil, map[string]any{"node": w1, "ref": appA}, err))

	warmA, err := nodeobs.UncachedBytesViaRegistryManifest(ctx, cs, cfg, namespace, w1, reg, repoA, "v1")
	pass = err == nil && warmA != nil && warmA.UncachedBytes == 0
	d = copyMap(detailBase)
	d["node"] = w1
	d["uncached"] = warmA
	rec = checkRecord("layer_share_worker1_warm_app_a_zero", pass, d, err)
	if err == nil && warmA != nil && !pass {
		rec.Error = fmt.Sprintf("uncached(app-a) after pull=%d want 0", warmA.UncachedBytes)
		rec.Pass = false
	}
	record(rec)

	shareB, err := nodeobs.UncachedBytesViaRegistryManifest(ctx, cs, cfg, namespace, w1, reg, repoB, "v1")
	pass = err == nil && shareB != nil && nodeobs.ApproxEqual(shareB.UncachedBytes, l2, tol)
	d = copyMap(detailBase)
	d["node"] = w1
	d["uncached"] = shareB
	d["want_L2_only"] = l2
	d["not_want_L0_plus_L2"] = l0 + l2
	d["gt_L0_digest"] = gt.Digests["L0"]
	d["gt_L2_digest"] = gt.Digests["L2"]
	rec = checkRecord("layer_share_worker1_app_b_shared_L0", pass, d, err)
	if err == nil && shareB != nil && !pass {
		rec.Error = fmt.Sprintf(
			"uncached(app-b)=%d want L2=%d (not L0+L2=%d)\nper-digest probe (not image-ref):\n%s",
			shareB.UncachedBytes, l2, l0+l2, formatLayerProbe(shareB),
		)
		rec.Pass = false
	}
	record(rec)

	totalB := l0 + l2
	uncB := int64(0)
	if shareB != nil {
		uncB = shareB.UncachedBytes
	}
	pass = shareB != nil && totalB != uncB && nodeobs.ApproxEqual(uncB, l2, tol)
	d = copyMap(detailBase)
	d["total_image_layer_bytes_app_b"] = totalB
	d["uncached_app_b"] = uncB
	d["diverged"] = totalB != uncB
	rec = checkRecord("layer_share_worker1_total_ne_uncached_app_b", pass, d, nil)
	if !pass {
		rec.Error = fmt.Sprintf("total=%d uncached=%d (need total≠uncached and uncached≈L2)", totalB, uncB)
		rec.Pass = false
	}
	record(rec)

	_, err = nodeobs.RemoveImageAndContent(ctx, cs, cfg, namespace, w2, []string{appA, appB, base}, true)
	clearDigests(ctx, cs, cfg, namespace, w2, gt)
	record(checkRecord("layer_share_worker2_cleared", err == nil, map[string]any{"node": w2}, err))

	cold2A, err := nodeobs.UncachedBytesViaRegistryManifest(ctx, cs, cfg, namespace, w2, reg, repoA, "v1")
	pass = err == nil && cold2A != nil && nodeobs.ApproxEqual(cold2A.UncachedBytes, l0+l1, tol)
	d = map[string]any{"node": w2, "uncached": cold2A, "want": l0 + l1}
	rec = checkRecord("layer_share_worker2_cold_app_a", pass, d, err)
	if err == nil && cold2A != nil && !pass {
		rec.Error = fmt.Sprintf("uncached(app-a)=%d want %d", cold2A.UncachedBytes, l0+l1)
		rec.Pass = false
	}
	record(rec)

	cold2B, err := nodeobs.UncachedBytesViaRegistryManifest(ctx, cs, cfg, namespace, w2, reg, repoB, "v1")
	pass = err == nil && cold2B != nil && nodeobs.ApproxEqual(cold2B.UncachedBytes, l0+l2, tol)
	d = map[string]any{"node": w2, "uncached": cold2B, "want": l0 + l2}
	rec = checkRecord("layer_share_worker2_cold_app_b", pass, d, err)
	if err == nil && cold2B != nil && !pass {
		rec.Error = fmt.Sprintf("uncached(app-b)=%d want %d", cold2B.UncachedBytes, l0+l2)
		rec.Pass = false
	}
	record(rec)
}

func clearDigests(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node string, gt *nodeobs.LayerGroundTruth) {
	var digs []string
	for _, k := range []string{"L0", "L1", "L2"} {
		if d := gt.Digests[k]; d != "" {
			digs = append(digs, d)
		}
	}
	if len(digs) == 0 {
		return
	}
	var b strings.Builder
	b.WriteString("set +e\n")
	for _, d := range digs {
		fmt.Fprintf(&b, "ctr -n k8s.io content rm %q 2>/dev/null || true\n", d)
	}
	b.WriteString("echo DIGESTS_CLEARED\n")
	_, _ = k8s.HostExec(ctx, cs, cfg, namespace, node, b.String(), 2*time.Minute)
}

func loadGroundTruth(ctx context.Context, cs *kubernetes.Clientset, gtFile, gtNS string) (*nodeobs.LayerGroundTruth, error) {
	if gtFile != "" {
		return nodeobs.LoadLayerGroundTruth(gtFile)
	}
	cm, err := cs.CoreV1().ConfigMaps(gtNS).Get(ctx, "layer-ground-truth", metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("ConfigMap %s/layer-ground-truth: %w (run build-layer-images Job, or pass -ground-truth=)", gtNS, err)
	}
	raw, ok := cm.Data["layer-ground-truth.json"]
	if !ok || raw == "" {
		return nil, fmt.Errorf("ConfigMap missing layer-ground-truth.json")
	}
	tmp, err := os.CreateTemp("", "layer-gt-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(raw); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	_ = tmp.Close()
	return nodeobs.LoadLayerGroundTruth(tmp.Name())
}

func registryClusterIP(ctx context.Context, cs *kubernetes.Clientset) (string, error) {
	svc, err := cs.CoreV1().Services("reloc-registry").Get(ctx, "registry", metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("service reloc-registry/registry: %w", err)
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		return "", fmt.Errorf("registry Service has no ClusterIP")
	}
	return svc.Spec.ClusterIP, nil
}

func checkRecord(name string, pass bool, detail map[string]any, err error) logevent.Record {
	rec := logevent.Record{Probe: "imageprobe", Check: name, Pass: pass, Detail: detail}
	if err != nil {
		rec.Error = err.Error()
		rec.Pass = false
	}
	return rec
}

func copyMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+4)
	for k, v := range in {
		out[k] = v
	}
	return out
}

func containsNote(rep *nodeobs.UncachedReport, prefix string) bool {
	if rep == nil {
		return false
	}
	for _, n := range rep.Notes {
		if strings.HasPrefix(n, prefix) {
			return true
		}
	}
	return false
}

func formatLayerProbe(rep *nodeobs.UncachedReport) string {
	if rep == nil {
		return "<nil report>"
	}
	var b strings.Builder
	for _, n := range rep.Notes {
		if strings.HasPrefix(n, "QUERY_MODE=") ||
			strings.HasPrefix(n, "IMAGE_REF=") ||
			strings.HasPrefix(n, "CONTENT_NS=") ||
			strings.HasPrefix(n, "CRI_PRESENT") ||
			strings.HasPrefix(n, "MANIFEST_LAYER_COUNT=") ||
			strings.HasPrefix(n, "CONTENT_INFO\t") {
			b.WriteString("  ")
			b.WriteString(n)
			b.WriteByte('\n')
		}
	}
	for _, layer := range rep.Layers {
		fmt.Fprintf(&b, "  LAYER digest=%s size=%d cached=%v\n", layer.Digest, layer.Size, layer.Cached)
	}
	if b.Len() == 0 {
		return "  <no CONTENT_INFO lines — parser saw no per-digest probe output>"
	}
	return b.String()
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
