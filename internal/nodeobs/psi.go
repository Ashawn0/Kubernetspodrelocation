package nodeobs

import (
	"bufio"
	"fmt"
	"strconv"
	"strings"
)

// PSISome holds the "some" line from a Linux pressure file.
type PSISome struct {
	Avg10  float64 `json:"avg10"`
	Avg60  float64 `json:"avg60"`
	Avg300 float64 `json:"avg300"`
	Total  uint64  `json:"total_us"`
}

// PSISnapshot is one resource's pressure from one source.
type PSISnapshot struct {
	Source   string  `json:"source"` // proc | cgroup | kubelet
	Resource string  `json:"resource"`
	Some     PSISome `json:"some"`
	Raw      string  `json:"raw,omitempty"`
}

// FeatureGate is fail-loud emission for a target-node PSI covariate.
type FeatureGate struct {
	Resource string   `json:"resource"`
	Emit     bool     `json:"emit"`
	Value    *PSISome `json:"value,omitempty"`
	Reason   string   `json:"reason"`
}

// ParsePSISome parses /proc/pressure/* or cgroup *.pressure content.
func ParsePSISome(raw string) (PSISome, error) {
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "some ") {
			continue
		}
		fields := strings.Fields(line)
		var s PSISome
		for _, f := range fields[1:] {
			k, v, ok := strings.Cut(f, "=")
			if !ok {
				continue
			}
			switch k {
			case "avg10":
				x, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return PSISome{}, err
				}
				s.Avg10 = x
			case "avg60":
				x, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return PSISome{}, err
				}
				s.Avg60 = x
			case "avg300":
				x, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return PSISome{}, err
				}
				s.Avg300 = x
			case "total":
				x, err := strconv.ParseUint(v, 10, 64)
				if err != nil {
					return PSISome{}, err
				}
				s.Total = x
			}
		}
		return s, nil
	}
	return PSISome{}, fmt.Errorf("no 'some' line in PSI output")
}

// IsolationPass reports whether stressed rose and controls stayed flat.
func IsolationPass(stressedDelta, controlMaxDelta uint64, minStressedDelta uint64) (bool, string) {
	if stressedDelta < minStressedDelta {
		return false, fmt.Sprintf("stressed delta %d < min %d (no usable signal)", stressedDelta, minStressedDelta)
	}
	// Control may have tiny baseline drift; require it stay far below stressed.
	if controlMaxDelta*10 > stressedDelta && controlMaxDelta > 1000 {
		return false, fmt.Sprintf("control delta %d not isolated from stressed delta %d", controlMaxDelta, stressedDelta)
	}
	if controlMaxDelta > stressedDelta/2 && controlMaxDelta > 500 {
		return false, fmt.Sprintf("leakage: control delta %d vs stressed %d", controlMaxDelta, stressedDelta)
	}
	return true, "stressed rose; controls flat within tolerance"
}

// RefuseOrEmit implements fail-loud: isolation failure => do not emit feature.
func RefuseOrEmit(resource string, isolated bool, reason string, value PSISome) FeatureGate {
	if !isolated {
		return FeatureGate{
			Resource: resource,
			Emit:     false,
			Value:    nil,
			Reason:   "isolation_failed: " + reason,
		}
	}
	v := value
	return FeatureGate{
		Resource: resource,
		Emit:     true,
		Value:    &v,
		Reason:   "isolation_ok: " + reason,
	}
}
