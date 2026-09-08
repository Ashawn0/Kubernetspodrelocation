package nodeobs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"reloc-disrupt/internal/k8s"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// LayerGroundTruth is produced by deploy/registry/build-layer-images-job.yaml.
type LayerGroundTruth struct {
	Registry       string           `json:"registry"`
	RepoPrefix     string           `json:"repo_prefix"`
	FileBytes      map[string]int64 `json:"file_bytes"`
	LayerBlobBytes map[string]int64 `json:"layer_blob_bytes"` // L0, L1, L2 compressed sizes
	Digests        map[string]string `json:"digests"`
	Images         map[string]string `json:"images"` // base, app_a, app_b
	Notes          []string         `json:"notes,omitempty"`
}

func LoadLayerGroundTruth(path string) (*LayerGroundTruth, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var gt LayerGroundTruth
	if err := json.Unmarshal(b, &gt); err != nil {
		return nil, err
	}
	if gt.LayerBlobBytes["L0"] == 0 || gt.LayerBlobBytes["L1"] == 0 || gt.LayerBlobBytes["L2"] == 0 {
		return nil, fmt.Errorf("ground truth missing layer_blob_bytes")
	}
	if gt.Images["app_a"] == "" || gt.Images["app_b"] == "" {
		return nil, fmt.Errorf("ground truth missing image refs")
	}
	return &gt, nil
}

// PullImagePlainHTTP pulls an image into the node's k8s.io namespace via ctr (real fetch path).
func PullImagePlainHTTP(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node, imageRef string) (string, error) {
	cmd := fmt.Sprintf(`set -euo pipefail
ctr -n k8s.io images pull --plain-http %q
echo PULL_OK
`, imageRef)
	return k8s.HostExec(ctx, cs, cfg, namespace, node, cmd, 10*time.Minute)
}

// RemoveImageAndContent best-effort removes an image name and does not prune shared content
// unless pruneShared is true. For cold start of app-a we prune the named images then GC.
func RemoveImageAndContent(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node string, refs []string, pruneAll bool) (string, error) {
	var b strings.Builder
	b.WriteString("set +e\n")
	for _, ref := range refs {
		fmt.Fprintf(&b, "ctr -n k8s.io images rm %q 2>/dev/null || true\n", ref)
		fmt.Fprintf(&b, "crictl rmi %q 2>/dev/null || true\n", ref)
	}
	if pruneAll {
		// Aggressive: remove any leftover reloc images then GC unused content.
		b.WriteString(`ctr -n k8s.io images ls -q | while read -r r; do
  case "$r" in *reloc/base*|*reloc/app-a*|*reloc/app-b*) ctr -n k8s.io images rm "$r" 2>/dev/null || true;; esac
done
ctr -n k8s.io content ls -q 2>/dev/null | head -n 0
# containerd GC of unreferenced content
ctr -n k8s.io content ls >/dev/null 2>&1
if command -v crictl >/dev/null 2>&1; then crictl rmi --prune 2>/dev/null || true; fi
`)
	}
	b.WriteString("echo REMOVE_DONE\n")
	return k8s.HostExec(ctx, cs, cfg, namespace, node, b.String(), 5*time.Minute)
}

// UncachedBytesViaRegistryManifest fetches the image manifest from an HTTP registry,
// then sums compressed sizes of layers missing from the node content store.
// This is the cold-path definition: what containerd would still need to fetch.
//
// If CRI already has the image, returns 0 (kubelet will not pull), matching measurement-spec.
func UncachedBytesViaRegistryManifest(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node, registryHostPort, repo, tag string) (*UncachedReport, error) {
	imageRef := fmt.Sprintf("%s/%s:%s", registryHostPort, repo, tag)
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

	script := fmt.Sprintf(`
set -euo pipefail
REG=%q
REPO=%q
TAG=%q
IMG="$REG/$REPO:$TAG"
NS=k8s.io
TMP=$(mktemp)

echo "QUERY_MODE=per_digest_content_info"
echo "IMAGE_REF=$IMG"
echo "CONTENT_NS=$NS"

if command -v crictl >/dev/null 2>&1 && crictl inspecti "$IMG" >/dev/null 2>&1; then
  echo CRI_PRESENT=1
else
  echo CRI_PRESENT=0
fi
# Note: CRI_PRESENT only forces uncached=0 when true; it does not short-circuit
# per-digest lookups when false. Layer accounting always runs Content.Info below.

curl -fsS -H 'Accept: application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json' \
  "http://$REG/v2/$REPO/manifests/$TAG" > "$TMP"
echo "MANIFEST_OK=1"

python3 - "$TMP" <<'PY'
import json, subprocess, sys
path = sys.argv[1]
with open(path) as f:
    mani = json.load(f)
layers = mani.get("layers") or []
if not layers and "manifests" in mani:
    print("ERR=image_index_not_resolved")
    sys.exit(0)
print(f"MANIFEST_LAYER_COUNT={len(layers)}")
ns = "k8s.io"
# ctr has no "content info". Quiet ls prints one digest per line — exact membership only.
ls = subprocess.run(
    ["ctr", "-n", ns, "content", "ls", "-q"],
    capture_output=True,
    text=True,
)
if ls.returncode != 0:
    err = (ls.stderr or ls.stdout or "").strip().replace("\n", " | ")
    print(f"ERR=content_ls_failed exit={ls.returncode} msg={err[:300]}")
    sys.exit(0)
present = {line.strip() for line in ls.stdout.splitlines() if line.strip()}
print(f"CONTENT_LS_COUNT={len(present)}")
print("CONTENT_PROBE=ctr_content_ls_quiet_exact_digest")
for i, layer in enumerate(layers):
    dig = layer["digest"]
    size = int(layer.get("size") or 0)
    cached = 1 if dig in present else 0
    print(f"CONTENT_INFO\tidx={i}\tdigest={dig}\tsize={size}\texit=0\tcached={cached}\tmsg=exact_match_in_content_ls_q")
    print(f"LAYER\t{dig}\t{size}\t{cached}")
PY
rm -f "$TMP"
`, registryHostPort, repo, tag)

	out, err := k8s.HostExec(ctx, cs, cfg, namespace, node, script, 5*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("registry uncached on %s: %w\n%s", node, err, out)
	}
	return parseUncachedOutput(rep, out)
}

// ApproxEqual reports |a-b| <= tol (absolute bytes).
func ApproxEqual(a, b, tol int64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}
