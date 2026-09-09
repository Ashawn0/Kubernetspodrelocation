package evalexport

import (
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"reloc-disrupt/internal/baseline"
)

// LoadTrialsCSV reads a trialrunner (or campaign) results CSV into baseline.Trials.
//
// Required columns (pilot/campaign header):
//
//	trial_id, replicate, image_cache_state, cpu_psi_level, ttfs_clusterip_sec
//
// Optional (filled when present): split, uncached_bytes, psi_cpu_avg10, pass,
// cell_name, infra, network_level, held_out.
//
// CellID defaults to "{image_cache_state}_{cpu_psi_level}" (e.g. warm_none),
// matching campaign local-VM naming. Rows with pass=false are skipped.
//
// ImageLocality PresentImages are not in the CSV; they are synthesized from
// cache state under the Target-vs-LoadNode structural guarantee (see
// featuresFromCacheRow): warm ⇒ NumNodes=1, cold ⇒ empty. TotalNodes defaults to 2.
func LoadTrialsCSV(path string) ([]baseline.Trial, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return DecodeTrialsCSV(f)
}

// DecodeTrialsCSV parses trial rows from r (header required).
func DecodeTrialsCSV(r io.Reader) ([]baseline.Trial, error) {
	cr := csv.NewReader(r)
	cr.ReuseRecord = true
	header, err := cr.Read()
	if err != nil {
		return nil, fmt.Errorf("evalexport csv: read header: %w", err)
	}
	idx := map[string]int{}
	for i, h := range header {
		idx[strings.TrimSpace(h)] = i
	}
	for _, need := range []string{"trial_id", "replicate", "image_cache_state", "cpu_psi_level", "ttfs_clusterip_sec"} {
		if _, ok := idx[need]; !ok {
			return nil, fmt.Errorf("evalexport csv: missing required column %q", need)
		}
	}

	var out []baseline.Trial
	line := 1 // header
	for {
		rec, err := cr.Read()
		if err == io.EOF {
			break
		}
		line++
		if err != nil {
			return nil, fmt.Errorf("evalexport csv: line %d: %w", line, err)
		}
		get := func(col string) string {
			i, ok := idx[col]
			if !ok || i >= len(rec) {
				return ""
			}
			return strings.TrimSpace(rec[i])
		}
		if p := get("pass"); p != "" && !strings.EqualFold(p, "true") {
			continue
		}
		cost, err := strconv.ParseFloat(get("ttfs_clusterip_sec"), 64)
		if err != nil {
			return nil, fmt.Errorf("evalexport csv: line %d ttfs_clusterip_sec: %w", line, err)
		}
		rep, err := strconv.Atoi(get("replicate"))
		if err != nil {
			return nil, fmt.Errorf("evalexport csv: line %d replicate: %w", line, err)
		}
		cache := strings.ToLower(get("image_cache_state"))
		cpu := strings.ToLower(get("cpu_psi_level"))
		cellID := get("cell_name")
		if cellID == "" {
			cellID = cache + "_" + cpu
		}
		var unc int64
		if s := get("uncached_bytes"); s != "" {
			unc, err = strconv.ParseInt(s, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("evalexport csv: line %d uncached_bytes: %w", line, err)
			}
		}
		var psi float64
		if s := get("psi_cpu_avg10"); s != "" {
			psi, err = strconv.ParseFloat(s, 64)
			if err != nil {
				return nil, fmt.Errorf("evalexport csv: line %d psi_cpu_avg10: %w", line, err)
			}
		}
		split := get("split")
		// Pilot CSVs use split=pilot; treat as unset so PartitionByReplicate can assign.
		if split == "pilot" || split == "dryrun" {
			split = ""
		}
		feat := featuresFromCacheRow(cache, unc, psi)
		out = append(out, baseline.Trial{
			ID:        get("trial_id"),
			CellID:    cellID,
			Replicate: rep,
			Split:     split,
			Cost:      baseline.Cost(cost),
			Features:  feat,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("evalexport csv: no usable trial rows")
	}
	return out, nil
}

// featuresFromCacheRow builds Features for offline predictors from CSV columns.
//
// ImageLocality PresentImages / NumNodes — structural guarantee (not an inventory scan):
//
//	trialrunner always forces the app pod onto opt.Target via nodeSelector and runs
//	ClusterIP/PodIP load from opt.LoadNode only. Empirically verified 2026-09-09:
//	worker2 (LoadNode) never hosts reloc/app-a or app-b (crictl images empty for
//	those refs). Therefore the ground-truth image is present on at most the target
//	node: warm ⇒ NumNodesWithImage = 1; cold ⇒ absent (empty PresentImages, score 0).
//	This is exact under that placement contract, not a multi-node approximation.
//	If trialrunner ever allows the app image on LoadNode (or multi-target placement),
//	update this and the placement invariant tests — do not silently keep NumNodes=1.
func featuresFromCacheRow(cache string, uncached int64, psi float64) baseline.Features {
	f := baseline.Features{
		UncachedBytes: uncached,
		CPUPSIAvg10:   psi,
		TotalNodes:    2, // local-VM worker count; spread = NumNodes/TotalNodes
		NumContainers: 1,
	}
	warm := cache == "warm" || uncached == 0
	if warm {
		// Size is illustrative for scoring; absolute bytes largely cancel in the
		// affine ImageLocality cost map Fit. NumNodes=1 is the verified count.
		f.PresentImages = []baseline.ImagePresence{{
			Name:     "reloc/app-a:v1",
			Size:     500 * 1024 * 1024,
			NumNodes: 1, // target only; LoadNode never holds the app image
		}}
	}
	return f
}
