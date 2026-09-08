package nodeobs

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"reloc-disrupt/internal/k8s"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// DiscardUnpackedAssumption is the containerd GC setting this probe currently
// operates under. Confirmed false on all three Multipass local-VM nodes
// (cp1, worker1, worker2). Re-verify on every new cluster; not a hardcoded eternal fact.
const DiscardUnpackedAssumption = false

// ImageLayer is one manifest layer descriptor.
type ImageLayer struct {
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
	Cached bool   `json:"cached"`
}

// UncachedReport is the Stage 0 uncached-bytes result for one image on one node.
type UncachedReport struct {
	Node                      string       `json:"node"`
	ImageRef                  string       `json:"image_ref"`
	CRIPresent                bool         `json:"cri_present"`
	DiscardUnpackedLayers     *bool        `json:"discard_unpacked_layers"` // nil => key absent
	DiscardUnpackedAssumption bool         `json:"discard_unpacked_assumption"`
	ContainerdVersion         string       `json:"containerd_version,omitempty"`
	Layers                    []ImageLayer `json:"layers"`
	UncachedBytes             int64        `json:"uncached_bytes"`
	TotalLayerBytes           int64        `json:"total_layer_bytes"`
	Notes                     []string     `json:"notes,omitempty"`
}

// ReadDiscardUnpackedLayers greps containerd config on the node.
func ReadDiscardUnpackedLayers(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node string) (*bool, string, error) {
	out, err := k8s.HostExec(ctx, cs, cfg, namespace, node,
		`grep -E '^\s*discard_unpacked_layers' /etc/containerd/config.toml || true; containerd --version 2>/dev/null || true`,
		time.Minute)
	if err != nil {
		return nil, "", err
	}
	ver := ""
	var discard *bool
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "containerd ") || strings.Contains(line, "github.com/containerd/containerd") {
			ver = line
		}
		if strings.Contains(line, "discard_unpacked_layers") {
			if strings.Contains(line, "true") {
				v := true
				discard = &v
			} else if strings.Contains(line, "false") {
				v := false
				discard = &v
			}
		}
	}
	return discard, ver, nil
}

// UncachedBytesForImage computes pull-time missing compressed layer bytes on a node
// via ctr content-store queries against the image manifest's layer digests.
//
// Measurement-spec rules:
//   - If CRI reports the image present => uncached_bytes = 0 (kubelet will not pull).
//   - Else sum compressed sizes of layer digests missing from the k8s.io content store.
//
// Operates under DiscardUnpackedAssumption; live config is always read and recorded.
func UncachedBytesForImage(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node, imageRef string) (*UncachedReport, error) {
	discard, ver, err := ReadDiscardUnpackedLayers(ctx, cs, cfg, namespace, node)
	if err != nil {
		return nil, err
	}
	rep := &UncachedReport{
		Node:                      node,
		ImageRef:                  imageRef,
		DiscardUnpackedLayers:     discard,
		DiscardUnpackedAssumption: DiscardUnpackedAssumption,
		ContainerdVersion:         ver,
	}
	switch {
	case discard == nil:
		rep.Notes = append(rep.Notes, fmt.Sprintf("discard_unpacked_layers key absent; version=%q; operating under assumption=%v", ver, DiscardUnpackedAssumption))
	case *discard != DiscardUnpackedAssumption:
		rep.Notes = append(rep.Notes, fmt.Sprintf("WARNING: live discard_unpacked_layers=%v differs from assumption=%v", *discard, DiscardUnpackedAssumption))
	default:
		note, _ := json.Marshal(map[string]any{"discard_unpacked_layers": *discard, "matches_assumption": true})
		rep.Notes = append(rep.Notes, string(note))
	}

	script := fmt.Sprintf(`
set -eu
IMG=%q
NS=k8s.io

if command -v crictl >/dev/null 2>&1 && crictl inspecti "$IMG" >/dev/null 2>&1; then
  echo CRI_PRESENT=1
else
  echo CRI_PRESENT=0
fi

if ! command -v ctr >/dev/null 2>&1; then
  echo ERR=no_ctr
  exit 0
fi
if ! command -v python3 >/dev/null 2>&1; then
  echo ERR=no_python3
  exit 0
fi

export IMG NS
python3 <<'PY'
import json, os, subprocess, sys

img = os.environ["IMG"]
ns = os.environ["NS"]

def run(cmd):
    return subprocess.run(cmd, capture_output=True)

# Locate image name in ctr
ls = run(["ctr", "-n", ns, "images", "ls", "-q"])
refs = [l.decode().strip() for l in ls.stdout.splitlines() if l.strip()]
match = None
for r in refs:
    if r == img or img in r or r in img:
        match = r
        break
print("MATCH=" + (match or "none"))
if not match:
    print("ERR=image_not_on_node")
    sys.exit(0)

# Resolve manifest digest from ctr images ls table
ls2 = run(["ctr", "-n", ns, "images", "ls"])
digest = None
for line in ls2.stdout.decode().splitlines():
    if match.split("@")[0] not in line and match not in line:
        continue
    for part in line.split():
        if part.startswith("sha256:"):
            digest = part
            break
    if digest:
        break
print("MANIFEST_DIGEST=" + str(digest))
if not digest:
    print("ERR=no_manifest_digest")
    sys.exit(0)

got = run(["ctr", "-n", ns, "content", "get", digest])
if got.returncode != 0:
    print("ERR=manifest_get_failed")
    sys.exit(0)
mani = json.loads(got.stdout)
layers = mani.get("layers") or []
if not layers and "manifests" in mani:
    # image index: pick first linux/amd64 or first manifest
    for m in mani["manifests"]:
        plat = m.get("platform") or {}
        if plat.get("os") == "linux" and plat.get("architecture") in ("amd64", "arm64", None):
            d2 = m["digest"]
            got2 = run(["ctr", "-n", ns, "content", "get", d2])
            if got2.returncode == 0:
                mani = json.loads(got2.stdout)
                layers = mani.get("layers") or []
                print("RESOLVED_PLATFORM_MANIFEST=" + d2)
                break
if not layers:
    print("ERR=no_layers")
    sys.exit(0)
# ctr has no "content info". Quiet ls: one digest per line; exact set membership.
cls = run(["ctr", "-n", ns, "content", "ls", "-q"])
if cls.returncode != 0:
    print("ERR=content_ls_failed")
    sys.exit(0)
present = {line.decode().strip() for line in cls.stdout.splitlines() if line.strip()}
print(f"CONTENT_LS_COUNT={len(present)}")
print("CONTENT_PROBE=ctr_content_ls_quiet_exact_digest")
for layer in layers:
    dig = layer["digest"]
    size = int(layer.get("size") or 0)
    cached = 1 if dig in present else 0
    print(f"LAYER\t{dig}\t{size}\t{cached}")
PY
`, imageRef)

	out, err := k8s.HostExec(ctx, cs, cfg, namespace, node, script, 3*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("hostexec: %w\n%s", err, out)
	}
	return parseUncachedOutput(rep, out)
}

func parseUncachedOutput(rep *UncachedReport, out string) (*UncachedReport, error) {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "CRI_PRESENT=1":
			rep.CRIPresent = true
		case line == "CRI_PRESENT=0":
			rep.CRIPresent = false
		case strings.HasPrefix(line, "LAYER\t"):
			parts := strings.Split(line, "\t")
			if len(parts) != 4 {
				continue
			}
			size, _ := strconv.ParseInt(parts[2], 10, 64)
			cached := parts[3] == "1"
			rep.Layers = append(rep.Layers, ImageLayer{Digest: parts[1], Size: size, Cached: cached})
			rep.TotalLayerBytes += size
			if !cached {
				rep.UncachedBytes += size
			}
		case strings.HasPrefix(line, "ERR="),
			strings.HasPrefix(line, "MATCH="),
			strings.HasPrefix(line, "MANIFEST_DIGEST="),
			strings.HasPrefix(line, "RESOLVED_PLATFORM_MANIFEST="),
			strings.HasPrefix(line, "QUERY_MODE="),
			strings.HasPrefix(line, "IMAGE_REF="),
			strings.HasPrefix(line, "CONTENT_NS="),
			strings.HasPrefix(line, "MANIFEST_LAYER_COUNT="),
			strings.HasPrefix(line, "CONTENT_LS_COUNT="),
			strings.HasPrefix(line, "CONTENT_PROBE="),
			strings.HasPrefix(line, "CONTENT_INFO\t"),
			strings.HasPrefix(line, "MANIFEST_OK="):
			rep.Notes = append(rep.Notes, line)
		}
	}
	if rep.CRIPresent {
		rep.UncachedBytes = 0
		rep.Notes = append(rep.Notes, "CRI present => uncached_bytes forced to 0 (kubelet will not pull)")
	}
	return rep, nil
}
