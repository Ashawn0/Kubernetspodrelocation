// Command trialrunner runs campaign-style disruption trials.
//
//	-mode dryrun    — short timing check (≤4 cells), split=dryrun
//	-mode pilot     — variance estimation: 6 cells × N replicates, split=pilot
//	                  → experiments/results/pilot/ (do not touch live pilot outputs)
//	-mode campaign  — config-driven cal/eval grid (-campaign-config), seeded shuffle,
//	                  split=calibration|evaluation → experiments/results/{calibration,evaluation}/
package main

import (
	"context"
	"encoding/csv"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"reloc-disrupt/internal/disrupt"
	"reloc-disrupt/internal/k8s"
	"reloc-disrupt/internal/logevent"
	"reloc-disrupt/internal/netshape"
	"reloc-disrupt/internal/nodeobs"
	"reloc-disrupt/internal/place"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	deleteGoneTimeout = 15 * time.Second
	// avg10 is a ~10s decaying average; NotFound ≠ clean PSI baseline.
	psiCooldownAvg10Max = 3.0
	psiCooldownTimeout  = 30 * time.Second
	psiCooldownPoll     = 2 * time.Second
	// Dwell after stress Running so avg10 approaches steady state (not mid-ramp).
	stressPSIDwell = 12 * time.Second
)

type cacheKind string

const (
	cacheCold cacheKind = "cold"
	cacheWarm cacheKind = "warm"
)

// cpuPSILevel is the pilot cell label for CPU stress.
type cpuPSILevel string

const (
	cpuNone      cpuPSILevel = "none"
	cpuThreshold cpuPSILevel = "threshold" // 2:1 workers:vCPU
	cpuHigh      cpuPSILevel = "high"      // 3:1 workers:vCPU
)

type trialSpec struct {
	TrialID         string
	CellName        string
	ImageCacheState cacheKind
	CPUPSILevel     cpuPSILevel
	CPURatio        int // 0 = no stress; 2 = threshold; 3 = high
	Replicate       int
	ExecOrder       int
	// Split is assigned before execution for campaign (calibration|evaluation).
	// Pilot/dryrun set this via trialOpts.Split instead (or leave empty).
	Split string
	// Infra is "local_vm" | "aws" for campaign rows; empty for pilot/dryrun.
	Infra string
	// NetworkLevel is 0–3 for AWS cells; -1 when N/A (local-VM / pilot).
	NetworkLevel int
	// HeldOut marks evaluation-only generalization cells (calibration_n forced 0).
	HeldOut bool
	// Dry-run only extras (memory/io):
	ExtraPSI string // "", "memory", "io"
}

func main() {
	var (
		mode           = flag.String("mode", "dryrun", "dryrun | pilot | campaign")
		kubeconfig     = flag.String("kubeconfig", "", "path to kubeconfig")
		namespace      = flag.String("namespace", "reloc-stage0", "namespace")
		outPath        = flag.String("out", "", "output path (dryrun/pilot; campaign uses cal/eval defaults)")
		campaignConfig = flag.String("campaign-config", "", "campaign mode: path to JSON cell/replicate config")
		image          = flag.String("image", "", "full cache-state image ref; empty = registry reloc/app-a:v1")
		imageRepo      = flag.String("image-repo", "reloc/app-a", "repo when -image empty")
		ioPath         = flag.String("io-path", "/mnt/reloc-nvme", "NVMe path for dryrun IO cell")
		skipIO         = flag.Bool("skip-io", true, "dryrun: replace IO cell with warm+cpu")
		preStop        = flag.Int("prestop-sec", 12, "preStop sleep")
		grace          = flag.Int("grace-sec", 20, "terminationGracePeriodSeconds")
		workers        = flag.Int("load-workers", 8, "HostExec Connection:close workers")
		loadSec        = flag.Int("load-sec", 35, "ClusterIP load duration seconds")
		stressSec      = flag.Int("stress-sec", 90, "PSI stress duration (cover disrupt+measure)")
		seedFlag       = flag.Int64("seed", 0, "pilot/campaign PRNG seed (0 = derive from time; always logged)")
		replicates     = flag.Int("replicates", 15, "pilot replicates per cell")
		registryIface  = flag.String("registry-iface", netshape.DefaultRegistryIface, "AWS registry secondary iface (tc target)")
		primaryIface   = flag.String("primary-iface", netshape.DefaultPrimaryIface, "AWS primary CNI iface (never shaped)")
	)
	flag.Parse()

	ctx := context.Background()
	cs, cfg, err := k8s.ClientsetFromFlags(*kubeconfig)
	must(err)
	must(k8s.EnsureNamespace(ctx, cs, *namespace))

	cacheImage, regHost, repo, tag, err := resolveCacheImage(ctx, cs, *image, *imageRepo)
	must(err)

	workersNodes, err := k8s.WorkerNodes(ctx, cs)
	must(err)
	if len(workersNodes) < 1 {
		fail("need >=1 worker")
	}
	must(labelWorkers(ctx, cs, workersNodes))
	target := workersNodes[0].Name
	loadNode := workersNodes[0].Name
	if len(workersNodes) > 1 {
		loadNode = workersNodes[1].Name
	}

	base := trialOpts{
		Namespace:     *namespace,
		Target:        target,
		LoadNode:      loadNode,
		Image:         cacheImage,
		Registry:      regHost,
		Repo:          repo,
		Tag:           tag,
		IOPath:        *ioPath,
		PreStop:       *preStop,
		Grace:         *grace,
		Workers:       *workers,
		LoadSec:       *loadSec,
		StressSec:     *stressSec,
		RegistryIface: *registryIface,
		PrimaryIface:  *primaryIface,
	}

	switch strings.ToLower(*mode) {
	case "dryrun":
		if *outPath == "" {
			*outPath = "experiments/results/stage0/trialrunner-dryrun.jsonl"
		}
		runDryRun(ctx, cs, cfg, base, *outPath, *skipIO)
	case "pilot":
		if *outPath == "" {
			day := time.Now().UTC().Format("20060102")
			*outPath = fmt.Sprintf("experiments/results/pilot/pilot-variance-%s.csv", day)
		}
		runPilot(ctx, cs, cfg, base, *outPath, *seedFlag, *replicates)
	case "campaign":
		if *campaignConfig == "" {
			fail("campaign mode requires -campaign-config path/to/config.json")
		}
		day := time.Now().UTC().Format("20060102")
		calOut := fmt.Sprintf("experiments/results/calibration/campaign-%s.csv", day)
		evalOut := fmt.Sprintf("experiments/results/evaluation/campaign-%s.csv", day)
		runCampaign(ctx, cs, cfg, base, *campaignConfig, calOut, evalOut, *seedFlag)
	default:
		fail("unknown -mode " + *mode + " (want dryrun|pilot|campaign)")
	}
}

func runDryRun(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, base trialOpts, outPath string, skipIO bool) {
	specs := []trialSpec{
		{TrialID: "dry-1", CellName: "cold_none", ImageCacheState: cacheCold, CPUPSILevel: cpuNone, CPURatio: 0, Replicate: 1, ExecOrder: 1},
		{TrialID: "dry-2", CellName: "warm_none", ImageCacheState: cacheWarm, CPUPSILevel: cpuNone, CPURatio: 0, Replicate: 1, ExecOrder: 2},
		{TrialID: "dry-3", CellName: "warm_cpu", ImageCacheState: cacheWarm, CPUPSILevel: cpuThreshold, CPURatio: 2, Replicate: 1, ExecOrder: 3},
		{TrialID: "dry-4", CellName: "cold_memory", ImageCacheState: cacheCold, CPUPSILevel: cpuNone, CPURatio: 0, ExtraPSI: "memory", Replicate: 1, ExecOrder: 4},
	}
	if !skipIO {
		specs[3] = trialSpec{TrialID: "dry-4", CellName: "warm_io", ImageCacheState: cacheWarm, CPUPSILevel: cpuNone, CPURatio: 0, ExtraPSI: "io", Replicate: 1, ExecOrder: 4}
	}

	must(os.MkdirAll(filepath.Dir(outPath), 0o755))
	w, err := logevent.Create(outPath)
	must(err)
	defer w.Close()

	fmt.Printf("trialrunner dry-run: %d trials, target=%s loadNode=%s cache_image=%s\n",
		len(specs), base.Target, base.LoadNode, base.Image)
	totalStart := time.Now()
	for _, sp := range specs {
		opt := base
		opt.Spec = sp
		opt.Split = "dryrun"
		opt.SkipKeepAlive = false
		trialStart := time.Now()
		fmt.Printf("\n=== %s %s ===\n", sp.TrialID, sp.CellName)
		rec, runErr := runOneTrial(ctx, cs, cfg, opt)
		elapsed := time.Since(trialStart).Seconds()
		rec.Detail["wall_clock_trial_sec"] = elapsed
		if runErr != nil {
			rec.Pass = false
			rec.Error = runErr.Error()
			fmt.Printf("FAIL %s (%.1fs): %v\n", sp.CellName, elapsed, runErr)
		} else {
			rec.Pass = true
			fmt.Printf("PASS %s (%.1fs) clusterip=%.3fs podip=%.3fs\n",
				sp.CellName, elapsed,
				num(rec.Detail["ttfs_clusterip_sec"]),
				num(rec.Detail["ttfs_podip_sec"]))
		}
		must(w.Write(rec))
	}
	fmt.Printf("\n=== dry-run total wall time: %.1fs ===\n", time.Since(totalStart).Seconds())
}

func runPilot(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, base trialOpts, outPath string, seedFlag int64, replicates int) {
	if replicates < 1 {
		replicates = 15
	}
	seed := seedFlag
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	rng := rand.New(rand.NewSource(seed))

	cells := []struct {
		cache cacheKind
		cpu   cpuPSILevel
		ratio int
	}{
		{cacheCold, cpuNone, 0},
		{cacheCold, cpuThreshold, 2},
		{cacheCold, cpuHigh, 3},
		{cacheWarm, cpuNone, 0},
		{cacheWarm, cpuThreshold, 2},
		{cacheWarm, cpuHigh, 3},
	}

	var specs []trialSpec
	for _, c := range cells {
		cell := fmt.Sprintf("%s_%s", c.cache, c.cpu)
		for r := 1; r <= replicates; r++ {
			specs = append(specs, trialSpec{
				CellName:        cell,
				ImageCacheState: c.cache,
				CPUPSILevel:     c.cpu,
				CPURatio:        c.ratio,
				Replicate:       r,
			})
		}
	}
	if len(specs) != 6*replicates {
		fail(fmt.Sprintf("pilot schedule size %d != %d", len(specs), 6*replicates))
	}

	rng.Shuffle(len(specs), func(i, j int) { specs[i], specs[j] = specs[j], specs[i] })
	for i := range specs {
		specs[i].ExecOrder = i + 1
		specs[i].TrialID = fmt.Sprintf("pilot-%03d-r%d", specs[i].ExecOrder, specs[i].Replicate)
	}

	must(os.MkdirAll(filepath.Dir(outPath), 0o755))
	f, err := os.Create(outPath)
	must(err)
	defer f.Close()
	cw := csv.NewWriter(f)
	header := []string{
		"trial_id", "split", "seed", "exec_order", "replicate",
		"image_cache_state", "cpu_psi_level", "cpu_oversubscribe_ratio",
		"ttfs_clusterip_sec", "ttfs_podip_sec", "ttfs_naive_clusterip_sec",
		"ts_utc", "pass", "error",
		"uncached_bytes", "psi_cpu_avg10", "psi_cpu_avg10_pretrial_baseline",
		"thermal_throttle_events_during_trial", "thermal_throttle_detected",
		"host_cpu_pct_start", "host_cpu_pct_end",
		"target_node", "cache_image",
	}
	must(cw.Write(header))
	cw.Flush()

	fmt.Printf("trialrunner pilot: %d trials (6 cells × %d), seed=%d\n", len(specs), replicates, seed)
	fmt.Printf("output: %s\n", outPath)
	fmt.Printf("target=%s loadNode=%s cache_image=%s\n", base.Target, base.LoadNode, base.Image)
	totalStart := time.Now()

	for _, sp := range specs {
		opt := base
		opt.Spec = sp
		opt.Split = "pilot"
		opt.SkipKeepAlive = true // pilot variance uses headline Connection:close only
		opt.Seed = seed
		trialStart := time.Now()
		fmt.Printf("\n=== [%d/%d] %s cell=%s cache=%s cpu=%s rep=%d ===\n",
			sp.ExecOrder, len(specs), sp.TrialID, sp.CellName, sp.ImageCacheState, sp.CPUPSILevel, sp.Replicate)
		rec, runErr := runOneTrial(ctx, cs, cfg, opt)
		elapsed := time.Since(trialStart).Seconds()
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		pass := runErr == nil
		errStr := ""
		if runErr != nil {
			errStr = runErr.Error()
			fmt.Printf("FAIL (%.1fs): %v\n", elapsed, runErr)
		} else {
			fmt.Printf("PASS (%.1fs) ttfs_cip=%.3fs ttfs_pip=%.3fs naive=%.3fs\n",
				elapsed,
				num(rec.Detail["ttfs_clusterip_sec"]),
				num(rec.Detail["ttfs_podip_sec"]),
				num(rec.Detail["ttfs_naive_clusterip_sec"]))
		}
		unc := ""
		avg10 := ""
		preBaseline := ""
		if landed, ok := rec.Detail["confirmed_landed"].(map[string]any); ok {
			if v, ok := landed["uncached_bytes"]; ok {
				unc = fmt.Sprint(v)
			}
			if v, ok := landed["psi_cpu_avg10"]; ok {
				avg10 = fmt.Sprint(v)
			}
		}
		if v, ok := rec.Detail["psi_cpu_avg10_pretrial_baseline"]; ok {
			preBaseline = fmt.Sprint(v)
		}
		thEvents := "0"
		thDet := "false"
		if v, ok := rec.Detail["thermal_throttle_events_during_trial"]; ok {
			thEvents = fmt.Sprint(v)
		}
		if v, ok := rec.Detail["thermal_throttle_detected"]; ok {
			if b, ok := v.(bool); ok {
				thDet = strconv.FormatBool(b)
			} else {
				thDet = fmt.Sprint(v)
			}
		}
		hostStart := ""
		hostEnd := ""
		if v, ok := rec.Detail["host_cpu_pct_start"]; ok {
			hostStart = fmtNum(v)
		}
		if v, ok := rec.Detail["host_cpu_pct_end"]; ok {
			hostEnd = fmtNum(v)
		}
		row := []string{
			sp.TrialID, "pilot", strconv.FormatInt(seed, 10), strconv.Itoa(sp.ExecOrder), strconv.Itoa(sp.Replicate),
			string(sp.ImageCacheState), string(sp.CPUPSILevel), strconv.Itoa(sp.CPURatio),
			fmtNum(rec.Detail["ttfs_clusterip_sec"]), fmtNum(rec.Detail["ttfs_podip_sec"]), fmtNum(rec.Detail["ttfs_naive_clusterip_sec"]),
			ts, strconv.FormatBool(pass), errStr,
			unc, avg10, preBaseline, thEvents, thDet, hostStart, hostEnd, base.Target, base.Image,
		}
		must(cw.Write(row))
		cw.Flush()
		if err := cw.Error(); err != nil {
			fail(err.Error())
		}
	}
	fmt.Printf("\n=== pilot complete: %d trials, seed=%d, total wall %.1fs ===\n",
		len(specs), seed, time.Since(totalStart).Seconds())
	fmt.Printf("results: %s\n", outPath)
}

// runCampaign executes a config-driven cal/eval schedule using the same
// runOneTrial path as pilot (teardown, PSI cooldown, dwell, thermal, host CPU).
func runCampaign(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, base trialOpts, configPath, calOut, evalOut string, seedFlag int64) {
	ccfg, err := loadCampaignConfig(configPath)
	must(err)
	seed := seedFlag
	if seed == 0 {
		seed = time.Now().UnixNano()
	}
	baseSpecs, err := buildCampaignSchedule(ccfg)
	must(err)
	specs := shuffleCampaignSchedule(baseSpecs, seed)

	must(os.MkdirAll(filepath.Dir(calOut), 0o755))
	must(os.MkdirAll(filepath.Dir(evalOut), 0o755))
	calF, err := os.Create(calOut)
	must(err)
	defer calF.Close()
	evalF, err := os.Create(evalOut)
	must(err)
	defer evalF.Close()
	calW := csv.NewWriter(calF)
	evalW := csv.NewWriter(evalF)
	header := campaignCSVHeader()
	must(calW.Write(header))
	must(evalW.Write(header))
	calW.Flush()
	evalW.Flush()

	nCal, nEval := 0, 0
	for _, sp := range specs {
		switch sp.Split {
		case "calibration":
			nCal++
		case "evaluation":
			nEval++
		}
	}
	fmt.Printf("trialrunner campaign: %d trials (cal=%d eval=%d), seed=%d\n", len(specs), nCal, nEval, seed)
	fmt.Printf("config: %s\n", configPath)
	fmt.Printf("calibration out: %s\n", calOut)
	fmt.Printf("evaluation out:  %s\n", evalOut)
	fmt.Printf("target=%s loadNode=%s cache_image=%s\n", base.Target, base.LoadNode, base.Image)
	totalStart := time.Now()

	for _, sp := range specs {
		opt := base
		opt.Spec = sp
		opt.Split = sp.Split // calibration | evaluation — never "pilot"
		opt.SkipKeepAlive = true
		opt.Seed = seed
		trialStart := time.Now()
		fmt.Printf("\n=== [%d/%d] %s cell=%s split=%s infra=%s cache=%s cpu=%s net=%s rep=%d held_out=%v ===\n",
			sp.ExecOrder, len(specs), sp.TrialID, sp.CellName, sp.Split, sp.Infra,
			sp.ImageCacheState, sp.CPUPSILevel, networkLevelCSV(sp), sp.Replicate, sp.HeldOut)
		rec, runErr := runOneTrial(ctx, cs, cfg, opt)
		elapsed := time.Since(trialStart).Seconds()
		ts := time.Now().UTC().Format(time.RFC3339Nano)
		pass := runErr == nil
		errStr := ""
		if runErr != nil {
			errStr = runErr.Error()
			fmt.Printf("FAIL (%.1fs): %v\n", elapsed, runErr)
		} else {
			fmt.Printf("PASS (%.1fs) ttfs_cip=%.3fs ttfs_pip=%.3fs naive=%.3fs\n",
				elapsed,
				num(rec.Detail["ttfs_clusterip_sec"]),
				num(rec.Detail["ttfs_podip_sec"]),
				num(rec.Detail["ttfs_naive_clusterip_sec"]))
		}
		row := campaignCSVRow(sp, seed, base, rec, pass, errStr, ts)
		var w *csv.Writer
		switch sp.Split {
		case "calibration":
			w = calW
		case "evaluation":
			w = evalW
		default:
			fail("campaign trial missing split: " + sp.TrialID)
		}
		must(w.Write(row))
		w.Flush()
		if err := w.Error(); err != nil {
			fail(err.Error())
		}
	}
	fmt.Printf("\n=== campaign complete: %d trials, seed=%d, total wall %.1fs ===\n",
		len(specs), seed, time.Since(totalStart).Seconds())
	fmt.Printf("calibration: %s\n", calOut)
	fmt.Printf("evaluation:  %s\n", evalOut)
}

func campaignCSVHeader() []string {
	return []string{
		"trial_id", "split", "seed", "exec_order", "replicate",
		"infra", "cell_name", "held_out",
		"image_cache_state", "cpu_psi_level", "cpu_oversubscribe_ratio", "network_level",
		"ttfs_clusterip_sec", "ttfs_podip_sec", "ttfs_naive_clusterip_sec",
		"ts_utc", "pass", "error",
		"uncached_bytes", "psi_cpu_avg10", "psi_cpu_avg10_pretrial_baseline",
		"thermal_throttle_events_during_trial", "thermal_throttle_detected",
		"host_cpu_pct_start", "host_cpu_pct_end",
		"target_node", "cache_image",
	}
}

func networkLevelCSV(sp trialSpec) string {
	if sp.Infra != infraAWS || sp.NetworkLevel < 0 {
		return ""
	}
	return strconv.Itoa(sp.NetworkLevel)
}

func campaignCSVRow(sp trialSpec, seed int64, base trialOpts, rec logevent.Record, pass bool, errStr, ts string) []string {
	unc := ""
	avg10 := ""
	preBaseline := ""
	if landed, ok := rec.Detail["confirmed_landed"].(map[string]any); ok {
		if v, ok := landed["uncached_bytes"]; ok {
			unc = fmt.Sprint(v)
		}
		if v, ok := landed["psi_cpu_avg10"]; ok {
			avg10 = fmt.Sprint(v)
		}
	}
	if v, ok := rec.Detail["psi_cpu_avg10_pretrial_baseline"]; ok {
		preBaseline = fmt.Sprint(v)
	}
	thEvents := "0"
	thDet := "false"
	if v, ok := rec.Detail["thermal_throttle_events_during_trial"]; ok {
		thEvents = fmt.Sprint(v)
	}
	if v, ok := rec.Detail["thermal_throttle_detected"]; ok {
		if b, ok := v.(bool); ok {
			thDet = strconv.FormatBool(b)
		} else {
			thDet = fmt.Sprint(v)
		}
	}
	hostStart := ""
	hostEnd := ""
	if v, ok := rec.Detail["host_cpu_pct_start"]; ok {
		hostStart = fmtNum(v)
	}
	if v, ok := rec.Detail["host_cpu_pct_end"]; ok {
		hostEnd = fmtNum(v)
	}
	return []string{
		sp.TrialID, sp.Split, strconv.FormatInt(seed, 10), strconv.Itoa(sp.ExecOrder), strconv.Itoa(sp.Replicate),
		sp.Infra, sp.CellName, strconv.FormatBool(sp.HeldOut),
		string(sp.ImageCacheState), string(sp.CPUPSILevel), strconv.Itoa(sp.CPURatio), networkLevelCSV(sp),
		fmtNum(rec.Detail["ttfs_clusterip_sec"]), fmtNum(rec.Detail["ttfs_podip_sec"]), fmtNum(rec.Detail["ttfs_naive_clusterip_sec"]),
		ts, strconv.FormatBool(pass), errStr,
		unc, avg10, preBaseline, thEvents, thDet, hostStart, hostEnd, base.Target, base.Image,
	}
}

type trialOpts struct {
	Namespace     string
	Target        string
	LoadNode      string
	Image         string
	Registry      string
	Repo          string
	Tag           string
	IOPath        string
	PreStop       int
	Grace         int
	Workers       int
	LoadSec       int
	StressSec     int
	Split         string
	Spec          trialSpec
	Seed          int64
	SkipKeepAlive bool
	RegistryIface string // AWS secondary ENI (tc target)
	PrimaryIface  string // AWS primary CNI (never shaped)
}

func runOneTrial(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, opt trialOpts) (rec logevent.Record, err error) {
	trialWallStart := time.Now()
	sp := opt.Spec
	detail := map[string]any{
		"trial_id":                sp.TrialID,
		"image_cache_state":       string(sp.ImageCacheState),
		"cpu_psi_level":           string(sp.CPUPSILevel),
		"cpu_oversubscribe_ratio": sp.CPURatio,
		"replicate":               sp.Replicate,
		"exec_order":              sp.ExecOrder,
		"target_node":             opt.Target,
		"image":                   opt.Image,
		"cache_image_note":        "reloc ground-truth image; not pause/sandbox",
	}
	if sp.Infra != "" {
		detail["infra"] = sp.Infra
		detail["held_out"] = sp.HeldOut
		if sp.Infra == infraAWS && sp.NetworkLevel >= 0 {
			detail["network_level"] = sp.NetworkLevel
		}
	}
	if opt.Seed != 0 {
		detail["seed"] = opt.Seed
	}
	rec = logevent.Record{
		Split:  opt.Split,
		Probe:  "trialrunner",
		Check:  sp.CellName,
		Detail: detail,
	}
	defer func() {
		// Early exits: still record end-of-attempt host covariates.
		if _, ok := detail["host_cpu_pct_end"]; !ok {
			attachHostCPUEnd(detail)
		}
		if _, ok := detail["thermal_throttle_events_during_trial"]; !ok {
			attachThermalThrottle(detail, trialWallStart, time.Now())
		}
	}()

	// Host CPU at trial start (informational; never gates the trial).
	attachHostCPUStart(detail)

	suffix := fmt.Sprintf("%d-%d", sp.ExecOrder, time.Now().UnixNano()%1_000_000)
	svcName := fmt.Sprintf("tr-svc-%s", suffix)
	oldName := fmt.Sprintf("tr-old-%s", suffix)
	newName := fmt.Sprintf("tr-new-%s", suffix)
	if len(svcName) > 63 {
		svcName = svcName[:63]
	}
	if len(oldName) > 63 {
		oldName = oldName[:63]
	}
	if len(newName) > 63 {
		newName = newName[:63]
	}
	if err = cleanup(ctx, cs, opt.Namespace, svcName, oldName, newName); err != nil {
		return rec, err
	}

	var stressPod string
	shapeApplied := false
	defer func() {
		bg := context.Background()
		if stressPod != "" {
			if e := deletePodGone(bg, cs, opt.Namespace, stressPod, deleteGoneTimeout); e != nil && err == nil {
				err = e
			}
		}
		// Network shaping is node-level tc state, not a pod — confirm clear before
		// the next trial (same discipline as deletePodGone / PSI cooldown).
		if shapeApplied {
			regIF := opt.RegistryIface
			if regIF == "" {
				regIF = netshape.DefaultRegistryIface
			}
			last, timedOut, e := netshape.ClearAndWait(bg, cs, cfg, opt.Namespace, opt.Target, regIF, time.Second, 30*time.Second)
			detail["netshape_clear_last_qdisc"] = strings.TrimSpace(last)
			detail["netshape_clear_timed_out"] = timedOut
			if e != nil && err == nil {
				err = e
			}
		}
		if e := cleanup(bg, cs, opt.Namespace, svcName, oldName, newName); e != nil && err == nil {
			err = e
		}
	}()

	// After prior-trial teardown (NotFound), wait for avg10 decay before setup.
	lastAvg, timedOut, coolErr := waitCPUAvg10Below(ctx, cs, cfg, opt.Namespace, opt.Target, psiCooldownAvg10Max, psiCooldownPoll, psiCooldownTimeout)
	if coolErr != nil {
		return rec, fmt.Errorf("psi cooldown: %w", coolErr)
	}
	detail["psi_cpu_avg10_cooldown_last"] = lastAvg
	detail["psi_cpu_avg10_cooldown_timed_out"] = timedOut

	baseSnaps, err := nodeobs.CollectProcPressure(ctx, cs, cfg, opt.Namespace, opt.Target)
	if err != nil {
		return rec, fmt.Errorf("pretrial CollectProcPressure: %w", err)
	}
	if cpu, ok := nodeobs.FindSome(baseSnaps, "cpu"); ok {
		detail["psi_cpu_avg10_pretrial_baseline"] = cpu.Avg10
	}

	setupStart := time.Now()

	// AWS campaign cells: apply registry-iface shaping (§7 levels) before cache
	// prep so cold pulls observe the impairment. LIVE UNVERIFIED until next AWS
	// provision — see internal/netshape package comment.
	if sp.Infra == infraAWS && sp.NetworkLevel >= 0 {
		regIF := opt.RegistryIface
		priIF := opt.PrimaryIface
		if regIF == "" {
			regIF = netshape.DefaultRegistryIface
		}
		if priIF == "" {
			priIF = netshape.DefaultPrimaryIface
		}
		prof, out, shapeErr := netshape.EnsureLevel(ctx, cs, cfg, opt.Namespace, opt.Target, regIF, priIF, sp.NetworkLevel)
		if shapeErr != nil {
			return rec, fmt.Errorf("netshape level %d: %w", sp.NetworkLevel, shapeErr)
		}
		shapeApplied = true
		detail["netshape_level"] = prof.Level
		detail["netshape_name"] = prof.Name
		detail["netshape_delay_ms"] = prof.DelayMS
		detail["netshape_rate_mbit"] = prof.RateMbit
		detail["netshape_registry_iface"] = regIF
		detail["netshape_primary_iface"] = priIF
		detail["netshape_apply_out"] = strings.TrimSpace(out)
		detail["netshape_live_verified"] = false // no AWS cluster tonight
		detail["netshape_intent"] = prof.IntentNote
	}

	switch sp.ImageCacheState {
	case cacheCold:
		if _, err := nodeobs.RemoveImageAndContent(ctx, cs, cfg, opt.Namespace, opt.Target, []string{opt.Image}, true); err != nil {
			return rec, fmt.Errorf("cold RemoveImageAndContent: %w", err)
		}
	case cacheWarm:
		if _, err := nodeobs.PullImagePlainHTTP(ctx, cs, cfg, opt.Namespace, opt.Target, opt.Image); err != nil {
			return rec, fmt.Errorf("warm PullImagePlainHTTP: %w", err)
		}
	default:
		return rec, fmt.Errorf("unknown image_cache_state %q", sp.ImageCacheState)
	}

	if sp.CPURatio > 0 {
		nproc, err := nodeobs.CPUCount(ctx, cs, cfg, opt.Namespace, opt.Target)
		if err != nil {
			return rec, err
		}
		pod, nWorkers, err := nodeobs.StartCPUStressRatio(ctx, cs, opt.Namespace, opt.Target, nproc, opt.StressSec, sp.CPURatio)
		if err != nil {
			return rec, fmt.Errorf("StartCPUStressRatio(%d): %w", sp.CPURatio, err)
		}
		stressPod = pod.Name
		detail["stress_cpu_workers"] = nWorkers
		detail["stress_cpu_nproc"] = nproc
		if err := k8s.WaitPodRunning(ctx, cs, opt.Namespace, stressPod, 3*time.Minute); err != nil {
			return rec, fmt.Errorf("cpu stress running: %w", err)
		}
		time.Sleep(stressPSIDwell)
	}

	switch sp.ExtraPSI {
	case "":
	case "memory":
		avail, err := nodeobs.MemAvailableKiB(ctx, cs, cfg, opt.Namespace, opt.Target)
		if err != nil {
			return rec, err
		}
		pod, vmDesc, err := nodeobs.StartMemoryStress(ctx, cs, opt.Namespace, opt.Target, avail, opt.StressSec)
		if err != nil {
			return rec, fmt.Errorf("StartMemoryStress: %w", err)
		}
		stressPod = pod.Name
		detail["stress_vm_bytes"] = vmDesc
		if err := k8s.WaitPodRunning(ctx, cs, opt.Namespace, stressPod, 3*time.Minute); err != nil {
			return rec, fmt.Errorf("mem stress running: %w", err)
		}
		pgrep, err := nodeobs.WaitHostStressNG(ctx, cs, cfg, opt.Namespace, opt.Target, 3*time.Minute)
		if err != nil {
			return rec, fmt.Errorf("WaitHostStressNG: %w", err)
		}
		detail["stress_ng_pgrep"] = pgrep
		time.Sleep(stressPSIDwell)
	case "io":
		pod, desc, err := nodeobs.StartIOStress(ctx, cs, opt.Namespace, opt.Target, opt.IOPath, opt.StressSec)
		if err != nil {
			return rec, fmt.Errorf("StartIOStress: %w", err)
		}
		stressPod = pod.Name
		detail["stress_io"] = desc
		if err := k8s.WaitPodRunning(ctx, cs, opt.Namespace, stressPod, 3*time.Minute); err != nil {
			return rec, fmt.Errorf("io stress running: %w", err)
		}
		time.Sleep(stressPSIDwell)
	default:
		return rec, fmt.Errorf("unknown ExtraPSI %q", sp.ExtraPSI)
	}

	unc, err := nodeobs.UncachedBytesViaRegistryManifest(ctx, cs, cfg, opt.Namespace, opt.Target, opt.Registry, opt.Repo, opt.Tag)
	if err != nil {
		return rec, fmt.Errorf("UncachedBytesViaRegistryManifest: %w", err)
	}
	psiSnaps, err := nodeobs.CollectProcPressure(ctx, cs, cfg, opt.Namespace, opt.Target)
	if err != nil {
		return rec, fmt.Errorf("CollectProcPressure: %w", err)
	}
	landed := map[string]any{
		"uncached_bytes":    unc.UncachedBytes,
		"total_layer_bytes": unc.TotalLayerBytes,
		"cri_present":       unc.CRIPresent,
		"psi_proc":          psiSnaps,
	}
	if cpu, ok := nodeobs.FindSome(psiSnaps, "cpu"); ok {
		landed["psi_cpu_some_total_us"] = cpu.Total
		landed["psi_cpu_avg10"] = cpu.Avg10
	}
	detail["confirmed_landed"] = landed
	detail["setup_duration_sec"] = time.Since(setupStart).Seconds()

	disruptStart := time.Now()
	svc := disrupt.UIDService(svcName, opt.Namespace)
	if _, err := cs.CoreV1().Services(opt.Namespace).Create(ctx, svc, metav1.CreateOptions{}); err != nil {
		return rec, fmt.Errorf("create service: %w", err)
	}
	oldPod := disrupt.UIDServerPod(oldName, opt.Namespace, disrupt.DefaultImage, int32(opt.PreStop), int32(opt.Grace))
	grace0 := int64(0)
	oldPod.Spec.TerminationGracePeriodSeconds = &grace0 // cleanup is SIGKILL; disrupt Delete still passes GracePeriodSeconds
	if _, err := cs.CoreV1().Pods(opt.Namespace).Create(ctx, oldPod, metav1.CreateOptions{}); err != nil {
		return rec, fmt.Errorf("create old pod: %w", err)
	}
	old, err := waitPodReady(ctx, cs, opt.Namespace, oldName, 5*time.Minute)
	if err != nil {
		return rec, fmt.Errorf("old ready: %w", err)
	}
	oldUID := string(old.UID)

	svcObj, err := cs.CoreV1().Services(opt.Namespace).Get(ctx, svcName, metav1.GetOptions{})
	if err != nil {
		return rec, err
	}
	svcURL := fmt.Sprintf("http://%s:%d/", svcObj.Spec.ClusterIP, disrupt.Port)

	type loadResult struct {
		samples []disrupt.Sample
		err     error
	}
	loadCh := make(chan loadResult, 1)
	go func() {
		s, e := disrupt.RunClusterLoad(context.Background(), cs, cfg, opt.Namespace, opt.LoadNode, svcURL, opt.Workers, opt.LoadSec)
		loadCh <- loadResult{s, e}
	}()
	time.Sleep(2 * time.Second)

	newPod := disrupt.UIDServerPod(newName, opt.Namespace, disrupt.DefaultImage, int32(opt.PreStop), int32(opt.Grace))
	newPod.Spec.TerminationGracePeriodSeconds = &grace0
	newPod = place.WithNodeSelector(newPod, place.Stage0LabelKey, opt.Target)
	t0Client := time.Now().UTC()
	created, err := cs.CoreV1().Pods(opt.Namespace).Create(ctx, newPod, metav1.CreateOptions{})
	if err != nil {
		return rec, fmt.Errorf("create new pod: %w", err)
	}
	t0API := created.CreationTimestamp.Time.UTC()
	newUID := string(created.UID)
	detail["placement"] = map[string]any{
		"method": "nodeSelector", "key": place.Stage0LabelKey, "value": opt.Target,
		"new_uid": newUID, "old_uid": oldUID,
	}
	detail["t0_api"] = t0API.Format(time.RFC3339Nano)

	gracePeriod := int64(opt.Grace)
	_ = cs.CoreV1().Pods(opt.Namespace).Delete(ctx, oldName, metav1.DeleteOptions{GracePeriodSeconds: &gracePeriod})

	newIP, err := waitPodIP(ctx, cs, opt.Namespace, newName, 3*time.Minute)
	if err != nil {
		<-loadCh
		return rec, fmt.Errorf("wait pod IP: %w", err)
	}
	placed, err := cs.CoreV1().Pods(opt.Namespace).Get(ctx, newName, metav1.GetOptions{})
	if err == nil {
		detail["placement"].(map[string]any)["actual_node"] = placed.Spec.NodeName
		if placed.Spec.NodeName != opt.Target {
			<-loadCh
			return rec, fmt.Errorf("forced placement missed: want %s got %s", opt.Target, placed.Spec.NodeName)
		}
	}

	podURL := fmt.Sprintf("http://%s:%d/", newIP, disrupt.Port)
	type pollResult struct {
		samples []disrupt.Sample
		err     error
	}
	podCh := make(chan pollResult, 1)
	go func() {
		s, e := disrupt.RunPodIPPoll(ctx, cs, cfg, opt.Namespace, opt.LoadNode, podURL, newUID, 90*time.Second)
		podCh <- pollResult{s, e}
	}()
	_, _ = waitPodReady(ctx, cs, opt.Namespace, newName, 3*time.Minute)
	detail["disrupt_duration_sec"] = time.Since(disruptStart).Seconds()

	measureStart := time.Now()
	pr := <-podCh
	lr := <-loadCh
	if lr.err != nil {
		return rec, fmt.Errorf("RunClusterLoad: %w", lr.err)
	}
	if pr.err != nil && len(pr.samples) == 0 {
		return rec, fmt.Errorf("RunPodIPPoll: %w", pr.err)
	}

	// UID-pinned TTFS (headline) vs API T0 — measurement-spec.
	firstCIP, okCIP := disrupt.FirstSuccessUID(lr.samples, newUID, t0Client)
	firstPIP, okPIP := disrupt.FirstSuccessUID(pr.samples, newUID, t0Client)
	var ttfsCIPSec, ttfsPIPSec float64
	if okCIP {
		ttfsCIPSec = firstCIP.At.Sub(t0API).Seconds()
	}
	if okPIP {
		ttfsPIPSec = firstPIP.At.Sub(t0API).Seconds()
	}
	detail["ttfs_clusterip_sec"] = ttfsCIPSec
	detail["ttfs_podip_sec"] = ttfsPIPSec
	detail["ttfs_clusterip_ok"] = okCIP
	detail["ttfs_podip_ok"] = okPIP
	detail["client_headline"] = "connection_close"

	// Naive clock: first any-200 on ClusterIP (bias check from Stage 0).
	if firstAny, okAny := disrupt.FirstAny200(lr.samples, t0Client); okAny {
		detail["ttfs_naive_clusterip_sec"] = firstAny.At.Sub(t0API).Seconds()
		detail["ttfs_naive_uid"] = firstAny.UID
	}

	if !opt.SkipKeepAlive {
		kaSample, kaErr := disrupt.RunKeepAliveFirstUID(ctx, cs, cfg, opt.Namespace, opt.LoadNode, svcURL, newUID, 45*time.Second)
		if kaErr != nil {
			detail["keepalive_ttfs_clusterip_sec"] = nil
			detail["keepalive_error"] = kaErr.Error()
		} else {
			detail["keepalive_ttfs_clusterip_sec"] = kaSample.At.Sub(t0API).Seconds()
		}
	}

	detail["measure_duration_sec"] = time.Since(measureStart).Seconds()
	detail["ts_utc"] = time.Now().UTC().Format(time.RFC3339Nano)

	// Host covariates (record-only): thermal events + host CPU bookends.
	attachHostCPUEnd(detail)
	attachThermalThrottle(detail, trialWallStart, time.Now())

	if !okCIP || !okPIP {
		return rec, fmt.Errorf("TTFS incomplete: clusterip_ok=%v podip_ok=%v", okCIP, okPIP)
	}
	return rec, nil
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

func resolveCacheImage(ctx context.Context, cs *kubernetes.Clientset, imageFlag, imageRepo string) (fullRef, regHost, repo, tag string, err error) {
	tag = "v1"
	repo = imageRepo
	if repo == "" {
		repo = "reloc/app-a"
	}
	if imageFlag != "" {
		fullRef = imageFlag
		regHost, repo, tag, ok := splitRegistryRef(imageFlag)
		if ok {
			return fullRef, regHost, repo, tag, nil
		}
		return "", "", "", "", fmt.Errorf("-image %q must look like host:port/repo:tag (in-cluster registry)", imageFlag)
	}
	ip, err := registryClusterIP(ctx, cs)
	if err != nil {
		return "", "", "", "", fmt.Errorf("resolve cache image (need reloc-registry/registry or pass -image): %w", err)
	}
	regHost = ip + ":5000"
	fullRef = fmt.Sprintf("%s/%s:%s", regHost, repo, tag)
	return fullRef, regHost, repo, tag, nil
}

func splitRegistryRef(ref string) (hostPort, repo, tag string, ok bool) {
	i := strings.IndexByte(ref, '/')
	if i <= 0 || i >= len(ref)-1 {
		return "", "", "", false
	}
	hostPort = ref[:i]
	if !strings.Contains(hostPort, ":") {
		return "", "", "", false
	}
	rest := ref[i+1:]
	j := strings.LastIndexByte(rest, ':')
	if j <= 0 {
		return "", "", "", false
	}
	return hostPort, rest[:j], rest[j+1:], true
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

func cleanup(ctx context.Context, cs *kubernetes.Clientset, ns string, names ...string) error {
	for _, n := range names {
		if err := deletePodGone(ctx, cs, ns, n, deleteGoneTimeout); err != nil {
			return err
		}
		if err := deleteServiceGone(ctx, cs, ns, n, deleteGoneTimeout); err != nil {
			return err
		}
	}
	return nil
}

// waitCPUAvg10Below polls host CPU PSI avg10 until it falls below maxAvg10 or timeout.
// On timeout it logs a warning and returns timedOut=true (caller should proceed).
func waitCPUAvg10Below(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, node string, maxAvg10 float64, poll, timeout time.Duration) (last float64, timedOut bool, err error) {
	deadline := time.Now().Add(timeout)
	for {
		snaps, snapErr := nodeobs.CollectProcPressure(ctx, cs, cfg, ns, node)
		if snapErr != nil {
			return last, false, snapErr
		}
		if cpu, ok := nodeobs.FindSome(snaps, "cpu"); ok {
			last = cpu.Avg10
			if last < maxAvg10 {
				return last, false, nil
			}
		}
		if time.Now().After(deadline) {
			fmt.Printf("WARN: CPU PSI avg10 cooldown timed out after %s (last=%.2f, want < %.1f); proceeding\n",
				timeout, last, maxAvg10)
			return last, true, nil
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return last, false, ctx.Err()
		case <-timer.C:
		}
	}
}

// deletePodGone force-deletes a pod (grace 0) and blocks until Get returns NotFound.
func deletePodGone(ctx context.Context, cs *kubernetes.Clientset, ns, name string, timeout time.Duration) error {
	zero := int64(0)
	err := cs.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete pod %s/%s: %w", ns, name, err)
	}
	err = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		_, getErr := cs.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("pod %s/%s still present after force-delete (>%s): %w", ns, name, timeout, err)
	}
	return nil
}

func deleteServiceGone(ctx context.Context, cs *kubernetes.Clientset, ns, name string, timeout time.Duration) error {
	zero := int64(0)
	err := cs.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &zero})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("delete service %s/%s: %w", ns, name, err)
	}
	err = wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		_, getErr := cs.CoreV1().Services(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("service %s/%s still present after delete (>%s): %w", ns, name, timeout, err)
	}
	return nil
}

func num(v any) float64 {
	switch t := v.(type) {
	case float64:
		return t
	case float32:
		return float64(t)
	case int:
		return float64(t)
	default:
		return 0
	}
}

func fmtNum(v any) string {
	if v == nil {
		return ""
	}
	return strconv.FormatFloat(num(v), 'f', 6, 64)
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
