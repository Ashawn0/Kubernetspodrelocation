// Package baseline implements Paper 1 relocation-cost predictors that share one
// interface with the eventual LightGBM model so regret code can call any of them
// uniformly. No Kubernetes API clients — pure offline scoring/fitting.
package baseline

import "fmt"

// Cost is relocation disruption cost in seconds (headline ClusterIP TTFS unless
// a caller substitutes another clock). Same units for all predictors and the oracle.
type Cost = float64

// Trial is one measured relocation under a known target-node feature vector.
// Split is "calibration" | "evaluation" | "" (unset — assign via oracle.Partition).
//
// Pointing at real campaign data later: map CSV/JSONL rows into Trial with
// CellID (e.g. warm_threshold), Replicate, Cost=ttfs_clusterip_sec, and Features
// filled from landed covariates (image present sizes, PSI, …).
type Trial struct {
	ID        string
	CellID    string
	Replicate int
	Split     string
	Cost      Cost
	Features  Features
}

// Features is the target-node / image state a predictor may use.
// FixedCost ignores Features. ImageLocality uses image presence + spread fields
// (total size present — not uncached bytes). LightGBM will use the full set.
type Features struct {
	// UncachedBytes is the paper's primary image covariate (layer-aware).
	// Baselines that ignore it still accept it so one Feature struct feeds all models.
	UncachedBytes int64
	CPUPSIAvg10   float64

	// ImageLocality inputs (match kube-scheduler ImageLocality plugin).
	// Present: only images that already exist on the candidate node.
	PresentImages []ImagePresence
	TotalNodes    int // cluster node count used for spread = NumNodes/TotalNodes
	NumContainers int // init + app containers (and image volumes if counted); for maxThreshold
}

// ImagePresence is one requested image that is present on the candidate node.
// Size is the image size kubelet reports in Node status (bytes), not uncached bytes.
// NumNodes is how many nodes in the cluster currently have that image (spread).
type ImagePresence struct {
	Name     string // normalized ref (optional; for tests/debug)
	Size     int64
	NumNodes int
}

// Predictor is the common surface for fixed-cost, ImageLocality, and LightGBM.
type Predictor interface {
	Name() string
	// Fit learns from calibration (or training) trials only.
	Fit(trials []Trial) error
	// Predict returns predicted relocation cost for the given features.
	Predict(f Features) (Cost, error)
}

// AggregateMean is the default cost aggregator for fixed-cost and cell oracles.
// OPEN DECISION: regret/oracle may later standardize on median instead of mean;
// keep callers on AggregateFn so a one-line swap is enough.
type AggregateFn func(costs []Cost) Cost

func AggregateMean(costs []Cost) Cost {
	if len(costs) == 0 {
		return 0
	}
	var s Cost
	for _, c := range costs {
		s += c
	}
	return s / Cost(len(costs))
}

func AggregateMedian(costs []Cost) Cost {
	if len(costs) == 0 {
		return 0
	}
	cp := append([]Cost(nil), costs...)
	// insertion sort — trial counts are small
	for i := 1; i < len(cp); i++ {
		j := i
		for j > 0 && cp[j] < cp[j-1] {
			cp[j], cp[j-1] = cp[j-1], cp[j]
			j--
		}
	}
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

func costsOf(trials []Trial) []Cost {
	out := make([]Cost, len(trials))
	for i, t := range trials {
		out[i] = t.Cost
	}
	return out
}

func requireTrials(name string, trials []Trial) error {
	if len(trials) == 0 {
		return fmt.Errorf("%s: Fit requires at least one trial", name)
	}
	return nil
}
