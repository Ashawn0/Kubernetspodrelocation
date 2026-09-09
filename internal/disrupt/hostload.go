package disrupt

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"reloc-disrupt/internal/k8s"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// RunClusterLoad runs Connection:close HTTP load from inside the cluster via
// HostExec (operator machines cannot reach ClusterIP/pod IPs). Returns Sample
// rows parsed from the remote Python collector — same pattern as uidprobe.
func RunClusterLoad(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, node, url string, workers, sec int) ([]Sample, error) {
	if workers < 1 {
		workers = 8
	}
	if sec < 1 {
		sec = 30
	}
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
		return nil, fmt.Errorf("cluster load: %w\n%s", err, truncateHost(raw, 800))
	}
	return ParseHostSamples(raw, "clusterip")
}

// RunPodIPPoll polls a pod IP until wantUID succeeds or timeout (HostExec).
func RunPodIPPoll(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, node, url, wantUID string, timeout time.Duration) ([]Sample, error) {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
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
		samples, _ := ParseHostSamples(raw, "podip")
		return samples, fmt.Errorf("podip poll: %w\n%s", err, truncateHost(raw, 400))
	}
	return ParseHostSamples(raw, "podip")
}

// RunKeepAliveFirstUID is the sensitivity cell: HTTP keep-alive from HostExec
// until first 200 with wantUID (not headline Connection:close).
func RunKeepAliveFirstUID(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, ns, node, url, wantUID string, timeout time.Duration) (Sample, error) {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	script := fmt.Sprintf(`
set -euo pipefail
python3 - <<'PY'
import json, time, http.client
from datetime import datetime, timezone
from urllib.parse import urlparse
URL = %q
WANT = %q
deadline = time.time() + %d
u = urlparse(URL)
host, port = u.hostname, u.port or 80
path = u.path or "/"
conn = http.client.HTTPConnection(host, port, timeout=2)

def ts():
    return datetime.now(timezone.utc).isoformat().replace("+00:00", "Z")

out = None
while time.time() < deadline:
    try:
        conn.request("GET", path, headers={"Connection": "keep-alive"})
        r = conn.getresponse()
        uid = r.getheader("X-Pod-Uid") or ""
        status = r.status
        _ = r.read()
        err = ""
    except Exception as e:
        uid, status, err = "", 0, str(e)
        try:
            conn.close()
        except Exception:
            pass
        conn = http.client.HTTPConnection(host, port, timeout=2)
    row = {"at": ts(), "uid": uid, "status": status, "path": "clusterip_keepalive", "err": err}
    if status == 200 and uid == WANT:
        out = row
        break
    time.sleep(0.02)
print("SAMPLES_BEGIN")
if out:
    print(json.dumps(out))
print("SAMPLES_END")
PY
`, url, wantUID, int(timeout.Seconds()))
	raw, err := k8s.HostExec(ctx, cs, cfg, ns, node, script, timeout+2*time.Minute)
	if err != nil {
		return Sample{}, fmt.Errorf("keepalive poll: %w\n%s", err, truncateHost(raw, 400))
	}
	samples, err := ParseHostSamples(raw, "clusterip_keepalive")
	if err != nil {
		return Sample{}, err
	}
	if len(samples) == 0 {
		return Sample{}, fmt.Errorf("keepalive: no sample for uid %s", wantUID)
	}
	return samples[0], nil
}

// ParseHostSamples parses SAMPLES_BEGIN/END JSON lines from HostExec stdout.
func ParseHostSamples(raw, defaultPath string) ([]Sample, error) {
	var out []Sample
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
			t = time.Now().UTC()
		}
		path := row.Path
		if path == "" {
			path = defaultPath
		}
		out = append(out, Sample{At: t.UTC(), UID: row.UID, Status: row.Status, Path: path, Err: row.Err})
	}
	return out, nil
}

func truncateHost(s string, n int) string {
	s = strings.TrimSpace(s)
	if n <= 0 || len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
