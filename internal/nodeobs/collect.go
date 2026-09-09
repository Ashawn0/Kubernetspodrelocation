package nodeobs

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"reloc-disrupt/internal/k8s"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

// CollectProcPressure reads /proc/pressure/{cpu,memory,io} on node via host-exec.
func CollectProcPressure(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node string) ([]PSISnapshot, error) {
	cmd := `for r in cpu memory io; do echo "===PROC:$r==="; cat /proc/pressure/$r 2>&1; done`
	out, err := k8s.HostExec(ctx, cs, cfg, namespace, node, cmd, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("host-exec on %s: %w", node, err)
	}
	snaps, err := parseTaggedPSI(out, "proc")
	if err != nil {
		return nil, fmt.Errorf("parse proc PSI on %s: %w\nraw: %s", node, err, truncate(out, 500))
	}
	if len(snaps) == 0 {
		return nil, fmt.Errorf("no proc PSI snapshots on %s; host-exec raw output: %s", node, truncate(out, 500))
	}
	return snaps, nil
}

// CollectCgroupPressure reads root cgroup v2 *.pressure on the node.
func CollectCgroupPressure(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node string) ([]PSISnapshot, error) {
	cmd := `
if [ ! -f /sys/fs/cgroup/cpu.pressure ]; then
  echo "CGROUP_V2_MISSING"
  exit 0
fi
for r in cpu memory io; do
  echo "===CGROUP:$r==="
  cat /sys/fs/cgroup/$r.pressure 2>&1
done
`
	out, err := k8s.HostExec(ctx, cs, cfg, namespace, node, cmd, 2*time.Minute)
	if err != nil {
		return nil, fmt.Errorf("host-exec on %s: %w", node, err)
	}
	if strings.Contains(out, "CGROUP_V2_MISSING") {
		return nil, fmt.Errorf("cgroup v2 pressure files missing on %s", node)
	}
	snaps, err := parseTaggedPSI(out, "cgroup")
	if err != nil {
		return nil, fmt.Errorf("parse cgroup PSI on %s: %w\nraw: %s", node, err, truncate(out, 500))
	}
	if len(snaps) == 0 {
		return nil, fmt.Errorf("no cgroup PSI snapshots on %s; host-exec raw output: %s", node, truncate(out, 500))
	}
	return snaps, nil
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "<empty>"
	}
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func parseTaggedPSI(out, source string) ([]PSISnapshot, error) {
	// Host-exec emits concatenated sections:
	//   ===PROC:cpu===\n<some>\n<full>\n===PROC:memory===\n...
	// Split on full ===LABEL:resource=== markers; do NOT split on bare "==="
	// (that separates the tag from its body and yields zero snapshots).
	prefix := "PROC"
	if source == "cgroup" {
		prefix = "CGROUP"
	}
	re := regexp.MustCompile(`(?m)^===` + prefix + `:([a-z]+)===\s*$`)
	idxs := re.FindAllStringSubmatchIndex(out, -1)
	if len(idxs) == 0 {
		return nil, nil
	}
	var snaps []PSISnapshot
	for i, loc := range idxs {
		res := out[loc[2]:loc[3]] // resource capture
		bodyStart := loc[1]
		bodyEnd := len(out)
		if i+1 < len(idxs) {
			bodyEnd = idxs[i+1][0]
		}
		body := strings.TrimSpace(out[bodyStart:bodyEnd])
		if body == "" {
			return nil, fmt.Errorf("%s %s: empty section body", source, res)
		}
		some, err := ParsePSISome(body)
		if err != nil {
			return nil, fmt.Errorf("%s %s: %w (body=%q)", source, res, err, body)
		}
		snaps = append(snaps, PSISnapshot{Source: source, Resource: res, Some: some, Raw: body})
	}
	return snaps, nil
}

// MemAvailableKiB reads MemAvailable from the node host.
func MemAvailableKiB(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node string) (int64, error) {
	out, err := k8s.HostExec(ctx, cs, cfg, namespace, node, `awk '/MemAvailable:/ {print $2}' /proc/meminfo`, time.Minute)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse MemAvailable %q: %w", out, err)
	}
	return v, nil
}

// CPUCount returns online CPU count on the node.
func CPUCount(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node string) (int, error) {
	out, err := k8s.HostExec(ctx, cs, cfg, namespace, node, `nproc`, time.Minute)
	if err != nil {
		return 0, err
	}
	v, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, err
	}
	return v, nil
}

// KubeletNodePSI pulls node-level PSI from the kubelet Summary API when present.
// Older kubelets may omit PSI; that is recorded, not fatal for proc/cgroup collection.
func KubeletNodePSI(ctx context.Context, cs *kubernetes.Clientset, node string) (map[string]any, error) {
	result := cs.CoreV1().RESTClient().Get().
		Resource("nodes").
		Name(node).
		SubResource("proxy").
		Suffix("stats/summary").
		Do(ctx)
	raw, err := result.Raw()
	if err != nil {
		return nil, err
	}
	var summary map[string]any
	if err := json.Unmarshal(raw, &summary); err != nil {
		return nil, err
	}
	nodeObj, _ := summary["node"].(map[string]any)
	return nodeObj, nil
}

// StartCPUStress launches a foreground stress-ng pod oversubscribed 2:1 vs nproc
// (Stage 0 isolation default). For other ratios use StartCPUStressRatio.
func StartCPUStress(ctx context.Context, cs *kubernetes.Clientset, namespace, node string, nproc, durationSec int) (*corev1.Pod, int, error) {
	return StartCPUStressRatio(ctx, cs, namespace, node, nproc, durationSec, 2)
}

// StartCPUStressRatio oversubscribes CPU workers at ratio:1 vs nproc
// (threshold≈2, high≈3 for pilot variance cells).
func StartCPUStressRatio(ctx context.Context, cs *kubernetes.Clientset, namespace, node string, nproc, durationSec, ratio int) (*corev1.Pod, int, error) {
	if ratio < 1 {
		ratio = 2
	}
	workers := nproc * ratio
	if workers < 2 {
		workers = 2
	}
	name := fmt.Sprintf("psi-cpu-stress-%d", time.Now().UnixNano()%1_000_000)
	pod := stressPod(name, namespace, node, []string{
		"stress-ng", "--cpu", strconv.Itoa(workers),
		"--timeout", fmt.Sprintf("%ds", durationSec),
		"--metrics-brief",
	})
	created, err := cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	return created, workers, err
}

// StartMemoryStress runs stress-ng in the **host** pid/mount namespace (nsenter),
// matching the manual Multipass validation path. Sizing is past live MemAvailable
// (not a %% of MemTotal): combined = available + margin, split across 2 vm workers.
func StartMemoryStress(ctx context.Context, cs *kubernetes.Clientset, namespace, node string, memAvailableKiB int64, durationSec int) (*corev1.Pod, string, error) {
	// Manual local-VM passes used ~200–500Mi past available combined; use 512Mi margin
	// so demand clearly clears reclaimable cache rather than sitting under "available".
	const marginKiB = int64(512 * 1024)
	combinedKiB := memAvailableKiB + marginKiB
	perKiB := combinedKiB / 2
	if perKiB < 64*1024 {
		perKiB = 64 * 1024
	}
	perM := perKiB / 1024
	vmBytes := fmt.Sprintf("%dM", perM)
	desc := fmt.Sprintf("%s x2 (combined ~%dMi, MemAvailable=%dKiB + margin %dMi)",
		vmBytes, perM*2, memAvailableKiB, marginKiB/1024)

	name := fmt.Sprintf("psi-mem-stress-%d", time.Now().UnixNano()%1_000_000)
	// Install+run stress-ng on the host via nsenter so allocations hit node
	// /proc/pressure/memory the same way the manual multipass runs did.
	// Milestone lines go to container stdout so JSONL can capture install vs run.
	script := fmt.Sprintf(`
set -e
nsenter --target 1 --mount --uts --ipc --net --pid -- bash -c '
  set -e
  echo "reloc-mem-stress: host pid=$$ checking stress-ng"
  if ! command -v stress-ng >/dev/null 2>&1; then
    echo "reloc-mem-stress: installing stress-ng via apt"
    apt-get update -qq
    DEBIAN_FRONTEND=noninteractive apt-get install -y -qq stress-ng
  fi
  command -v stress-ng
  echo "reloc-mem-stress: exec stress-ng --vm 2 --vm-bytes %s --vm-keep --timeout %ds"
  exec stress-ng --vm 2 --vm-bytes %s --vm-keep --timeout %ds --metrics-brief
'
`, vmBytes, durationSec, vmBytes, durationSec)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": "reloc-psiprobe-stress", "stage0": "true", "stress": "memory"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			HostPID:       true,
			HostNetwork:   true,
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			}},
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchExpressions: []corev1.NodeSelectorRequirement{{
								Key:      "kubernetes.io/hostname",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{node},
							}},
						}},
					},
				},
			},
			Containers: []corev1.Container{{
				Name:            "stress",
				Image:           k8s.HostExecImage, // ubuntu: has nsenter; stress-ng installed on host
				ImagePullPolicy: corev1.PullIfNotPresent,
				SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
				Command:         []string{"bash", "-c", script},
			}},
		},
	}
	created, err := cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	return created, desc, err
}

// HostMemoryDiagnostics captures swap state and recent OOM-killer evidence on the node.
// Intended for every memory-isolation JSONL record (pass or fail): no-swap → OOM-not-stall
// is a permanent characteristic of these nodes when it applies.
func HostMemoryDiagnostics(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node string) (swapOut, oomOut string, err error) {
	out, err := k8s.HostExec(ctx, cs, cfg, namespace, node, `
set +e
echo "===SWAPON==="
swapon --show
echo "===SWAPON_END==="
echo "===DMESG_OOM==="
# Prefer -T when available; fall back to plain dmesg. Soft-fail: empty is informative.
if dmesg -T >/tmp/reloc-dmesg.txt 2>/dev/null; then
  :
elif dmesg >/tmp/reloc-dmesg.txt 2>/dev/null; then
  :
else
  echo "dmesg unavailable" >&2
fi
grep -iE 'Out of memory|oom-kill|Killed process|Memory cgroup out of memory' /tmp/reloc-dmesg.txt 2>/dev/null | tail -n 50
echo "===DMESG_OOM_END==="
true
`, 2*time.Minute)
	if err != nil {
		return "", "", err
	}
	swapOut = between(out, "===SWAPON===", "===SWAPON_END===")
	oomOut = between(out, "===DMESG_OOM===", "===DMESG_OOM_END===")
	return strings.TrimSpace(swapOut), strings.TrimSpace(oomOut), nil
}

func between(s, start, end string) string {
	i := strings.Index(s, start)
	if i < 0 {
		return ""
	}
	s = s[i+len(start):]
	j := strings.Index(s, end)
	if j < 0 {
		return strings.TrimSpace(s)
	}
	return strings.TrimSpace(s[:j])
}

// WaitHostStressNG polls the host until pgrep finds stress-ng (functional readiness).
// PodRunning alone is insufficient: the container may still be apt-installing.
// Uses one HostExec that loops on the host so readiness does not burn the PSI sample window.
func WaitHostStressNG(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, node string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	sec := int(timeout.Seconds())
	if sec < 5 {
		sec = 5
	}
	cmd := fmt.Sprintf(`
set -euo pipefail
END=$((SECONDS+%d))
while (( SECONDS < END )); do
  if out=$(pgrep -a stress-ng 2>/dev/null); then
    echo "$out"
    exit 0
  fi
  sleep 0.5
done
echo "reloc-mem-stress: stress-ng not in process table after %ds" >&2
exit 1
`, sec, sec)
	out, err := k8s.HostExec(ctx, cs, cfg, namespace, node, cmd, timeout+30*time.Second)
	if err != nil {
		return strings.TrimSpace(out), fmt.Errorf("wait stress-ng on %s: %w", node, err)
	}
	return strings.TrimSpace(out), nil
}

// StartIOStress writes continuously under ioPath on the host (nsenter), for
// IO PSI isolation against a dedicated device (AWS: /mnt/reloc-nvme on instance-store NVMe).
// Do not point this at the root EBS volume.
func StartIOStress(ctx context.Context, cs *kubernetes.Clientset, namespace, node, ioPath string, durationSec int) (*corev1.Pod, string, error) {
	if ioPath == "" {
		ioPath = "/mnt/reloc-nvme"
	}
	name := fmt.Sprintf("psi-io-stress-%d", time.Now().UnixNano()%1_000_000)
	desc := fmt.Sprintf("dd write loop under %s for %ds (host nsenter)", ioPath, durationSec)
	script := fmt.Sprintf(`
set -euo pipefail
nsenter --target 1 --mount --uts --ipc --net --pid -- bash -c '
  set -euo pipefail
  IOPATH=%q
  if [[ ! -d "$IOPATH" ]]; then
    echo "IO stress path missing: $IOPATH" >&2
    exit 1
  fi
  if ! findmnt -n "$IOPATH" >/dev/null 2>&1; then
    echo "IO stress path not a mountpoint (refusing root/EBS fallthrough): $IOPATH" >&2
    exit 1
  fi
  SRC=$(findmnt -n -o SOURCE "$IOPATH" || true)
  ROOTSRC=$(findmnt -n -o SOURCE / || true)
  if [[ -n "$ROOTSRC" && "$SRC" == "$ROOTSRC" ]]; then
    echo "refusing: io path is on root device source=$SRC" >&2
    exit 1
  fi
  END=$((SECONDS+%d))
  i=0
  while (( SECONDS < END )); do
    dd if=/dev/zero of="$IOPATH/reloc-io-stress.$i" bs=1M count=64 oflag=direct conv=fdatasync status=none || true
    rm -f "$IOPATH/reloc-io-stress.$i"
    i=$((i+1))
  done
'
`, ioPath, durationSec)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": "reloc-psiprobe-stress", "stage0": "true"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			HostPID:       true,
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			}},
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchExpressions: []corev1.NodeSelectorRequirement{{
								Key:      "kubernetes.io/hostname",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{node},
							}},
						}},
					},
				},
			},
			Containers: []corev1.Container{{
				Name:            "stress",
				Image:           k8s.HostExecImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
				Command:         []string{"bash", "-c", script},
			}},
		},
	}
	created, err := cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	return created, desc, err
}

func stressPod(name, namespace, node string, args []string) *corev1.Pod {
	grace0 := int64(0)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": "reloc-psiprobe-stress", "stage0": "true"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			TerminationGracePeriodSeconds: &grace0, // stress-ng: no shutdown work; avoid 30s bleed into next trial
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			}},
			// Required affinity by hostname — scheduler path, never nodeName.
			Affinity: &corev1.Affinity{
				NodeAffinity: &corev1.NodeAffinity{
					RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
						NodeSelectorTerms: []corev1.NodeSelectorTerm{{
							MatchExpressions: []corev1.NodeSelectorRequirement{{
								Key:      "kubernetes.io/hostname",
								Operator: corev1.NodeSelectorOpIn,
								Values:   []string{node},
							}},
						}},
					},
				},
			},
			Containers: []corev1.Container{{
				Name:            "stress",
				Image:           "ghcr.io/colinianking/stress-ng:latest",
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         args,
				SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
			}},
		},
	}
}

func boolPtr(v bool) *bool { return &v }

// FindSome returns the some totals for a resource from proc snapshots.
func FindSome(snaps []PSISnapshot, resource string) (PSISome, bool) {
	for _, s := range snaps {
		if s.Resource == resource {
			return s.Some, true
		}
	}
	return PSISome{}, false
}
