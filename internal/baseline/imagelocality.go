package baseline

import (
	"fmt"
	"math"
	"strings"
)

// ImageLocality mirrors kube-scheduler's ImageLocality score plugin (v1.30 line of
// code: pkg/scheduler/framework/plugins/imagelocality/image_locality.go), then maps
// that score into a relocation-cost prediction comparable to fixed-cost / LightGBM.
//
// Real plugin behavior (not uncached-bytes):
//   - For each requested container image already present on the node, add
//     scaledImageScore = Size * (NumNodes / TotalNodes)  [spread heuristic]
//   - Clamp the sum into [minThreshold, maxContainerThreshold*numContainers]
//     with minThreshold=23MiB, maxContainerThreshold=1000MiB
//   - score = MaxNodeScore * (sum - min) / (max - min), MaxNodeScore=100
//
// Cost adaptation (this package): Fit an affine map
//   cost ≈ a + b * (1 - score/MaxNodeScore)
// on calibration trials so higher locality (images present) predicts lower cost.
// OPEN DECISION: alternative maps (isotonic, piecewise cold/warm table) may replace
// OLS once campaign data exists; Score() itself stays pinned to upstream math.
type ImageLocality struct {
	fitted bool
	a, b   float64 // cost = a + b*(1 - score/maxNodeScore)
}

// Thresholds and MaxNodeScore match kubernetes v1.30 ImageLocality.
const (
	mb                   int64 = 1024 * 1024
	minThreshold         int64 = 23 * mb
	maxContainerThreshold int64 = 1000 * mb
	maxNodeScore         int64 = 100 // framework.MaxNodeScore
)

func NewImageLocality() *ImageLocality {
	return &ImageLocality{}
}

func (p *ImageLocality) Name() string { return "ImageLocality" }

// Score returns the kube-scheduler ImageLocality priority in [0, 100].
func Score(f Features) (int64, error) {
	if f.TotalNodes < 1 {
		return 0, fmt.Errorf("ImageLocality: TotalNodes must be >= 1")
	}
	nCont := f.NumContainers
	if nCont < 1 {
		// At least one container slot for maxThreshold (matches calling calculatePriority
		// with len(init)+len(containers); empty pod is out of scope).
		nCont = 1
	}
	sum := sumImageScores(f)
	return calculatePriority(sum, nCont), nil
}

func sumImageScores(f Features) int64 {
	var sum int64
	for _, img := range f.PresentImages {
		sum += scaledImageScore(img.Size, img.NumNodes, f.TotalNodes)
	}
	return sum
}

func scaledImageScore(size int64, numNodes, totalNodes int) int64 {
	if totalNodes <= 0 || numNodes <= 0 || size <= 0 {
		return 0
	}
	spread := float64(numNodes) / float64(totalNodes)
	return int64(float64(size) * spread)
}

func calculatePriority(sumScores int64, numContainers int) int64 {
	maxThreshold := maxContainerThreshold * int64(numContainers)
	if sumScores < minThreshold {
		sumScores = minThreshold
	} else if sumScores > maxThreshold {
		sumScores = maxThreshold
	}
	return maxNodeScore * (sumScores - minThreshold) / (maxThreshold - minThreshold)
}

// NormalizedImageName mirrors upstream normalizedImageName: append ":latest" when
// no tag is present (last ':' is not after the last '/').
func NormalizedImageName(name string) string {
	if strings.LastIndex(name, ":") <= strings.LastIndex(name, "/") {
		return name + ":latest"
	}
	return name
}

func (p *ImageLocality) Fit(trials []Trial) error {
	if err := requireTrials(p.Name(), trials); err != nil {
		return err
	}
	// OLS: cost = a + b * x, x = 1 - score/100
	var sumX, sumY, sumXX, sumXY float64
	n := 0.0
	for _, t := range trials {
		sc, err := Score(t.Features)
		if err != nil {
			return fmt.Errorf("ImageLocality Fit trial %q: %w", t.ID, err)
		}
		x := 1.0 - float64(sc)/float64(maxNodeScore)
		y := float64(t.Cost)
		sumX += x
		sumY += y
		sumXX += x * x
		sumXY += x * y
		n++
	}
	den := n*sumXX - sumX*sumX
	if math.Abs(den) < 1e-12 {
		// Degenerate (all scores equal): fall back to mean cost, zero slope.
		p.a = sumY / n
		p.b = 0
		p.fitted = true
		return nil
	}
	p.b = (n*sumXY - sumX*sumY) / den
	p.a = (sumY - p.b*sumX) / n
	p.fitted = true
	return nil
}

func (p *ImageLocality) Predict(f Features) (Cost, error) {
	if !p.fitted {
		return 0, errNotFitted(p.Name())
	}
	sc, err := Score(f)
	if err != nil {
		return 0, err
	}
	x := 1.0 - float64(sc)/float64(maxNodeScore)
	return Cost(p.a + p.b*x), nil
}

// Coeff returns the fitted affine map (a, b) for tests/diagnostics.
func (p *ImageLocality) Coeff() (a, b float64) { return p.a, p.b }

func errNotFitted(name string) error {
	return fmt.Errorf("%s: Predict called before Fit", name)
}
