package k8s

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/rest"
)

const HostExecImage = "ubuntu:22.04" // util-linux nsenter; busybox lacks a usable one

// HostExec runs cmd in the node's host mount/pid namespace via a privileged nsenter pod.
// Placement uses required hostname affinity (scheduler path), not spec.nodeName.
func HostExec(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, nodeName string, cmd string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	name := fmt.Sprintf("hostexec-%s-%d", sanitize(nodeName), time.Now().UnixNano()%1_000_000_000)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": "reloc-hostexec", "stage0": "true"},
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
								Values:   []string{nodeName},
							}},
						}},
					},
				},
			},
			Containers: []corev1.Container{{
				Name:            "nsenter",
				Image:           HostExecImage,
				ImagePullPolicy: corev1.PullIfNotPresent,
				SecurityContext: &corev1.SecurityContext{Privileged: boolPtr(true)},
				Command:         []string{"sleep", "600"},
			}},
		},
	}
	created, err := cs.CoreV1().Pods(namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create hostexec pod: %w", err)
	}
	defer func() {
		_ = cs.CoreV1().Pods(namespace).Delete(context.Background(), created.Name, metav1.DeleteOptions{})
	}()

	if err := WaitPodRunning(ctx, cs, namespace, created.Name, timeout); err != nil {
		p, _ := cs.CoreV1().Pods(namespace).Get(context.Background(), created.Name, metav1.GetOptions{})
		phase, reason, msg := "", "", ""
		if p != nil {
			phase = string(p.Status.Phase)
			if len(p.Status.ContainerStatuses) > 0 {
				cs0 := p.Status.ContainerStatuses[0]
				if cs0.State.Waiting != nil {
					reason = cs0.State.Waiting.Reason
					msg = cs0.State.Waiting.Message
				}
			}
		}
		return "", fmt.Errorf("hostexec pod %s not Running on %s: %w (phase=%s waiting=%s msg=%s)", created.Name, nodeName, err, phase, reason, msg)
	}

	// Host is Ubuntu (bash). Use bash -c, not sh/dash: scripts use set -o pipefail.
	full := []string{"nsenter", "--target", "1", "--mount", "--uts", "--ipc", "--net", "--", "bash", "-c", cmd}
	return ExecInPod(ctx, cs, cfg, namespace, created.Name, "nsenter", full)
}

// PodLogs returns recent container logs (stdout+stderr merged by kubelet).
func PodLogs(ctx context.Context, cs *kubernetes.Clientset, namespace, name, container string, tailLines int64) (string, error) {
	opts := &corev1.PodLogOptions{}
	if container != "" {
		opts.Container = container
	}
	if tailLines > 0 {
		opts.TailLines = &tailLines
	}
	stream, err := cs.CoreV1().Pods(namespace).GetLogs(name, opts).Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("pod logs %s/%s: %w", namespace, name, err)
	}
	defer stream.Close()
	b, err := io.ReadAll(stream)
	if err != nil {
		return "", fmt.Errorf("read pod logs %s/%s: %w", namespace, name, err)
	}
	return string(b), nil
}

// ExecInPod runs a command in an existing container and returns combined stdout+stderr.
func ExecInPod(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, pod, container string, command []string) (string, error) {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("spdy: %w", err)
	}
	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	out := stdout.String()
	if stderr.Len() > 0 {
		if out != "" && !strings.HasSuffix(out, "\n") {
			out += "\n"
		}
		out += stderr.String()
	}
	if err != nil {
		return out, fmt.Errorf("exec: %w\noutput: %s", err, out)
	}
	return out, nil
}

func WaitPodRunning(ctx context.Context, cs *kubernetes.Clientset, namespace, name string, timeout time.Duration) error {
	return wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		p, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		switch p.Status.Phase {
		case corev1.PodRunning:
			return true, nil
		case corev1.PodFailed, corev1.PodSucceeded:
			return false, fmt.Errorf("pod %s phase %s", name, p.Status.Phase)
		default:
			return false, nil
		}
	})
}

func WaitPodScheduled(ctx context.Context, cs *kubernetes.Clientset, namespace, name string, timeout time.Duration) (*corev1.Pod, error) {
	var out *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, 300*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		p, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		out = p
		return p.Spec.NodeName != "", nil
	})
	return out, err
}

func WaitPodPhase(ctx context.Context, cs *kubernetes.Clientset, namespace, name string, phase corev1.PodPhase, timeout time.Duration) (*corev1.Pod, error) {
	var out *corev1.Pod
	err := wait.PollUntilContextTimeout(ctx, 300*time.Millisecond, timeout, true, func(ctx context.Context) (bool, error) {
		p, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		out = p
		return p.Status.Phase == phase, nil
	})
	return out, err
}

func EnsureNamespace(ctx context.Context, cs *kubernetes.Clientset, name string) error {
	_, err := cs.CoreV1().Namespaces().Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return nil
	}
	_, err = cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}, metav1.CreateOptions{})
	return err
}

func WorkerNodes(ctx context.Context, cs *kubernetes.Clientset) ([]corev1.Node, error) {
	list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var workers []corev1.Node
	for _, n := range list.Items {
		if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
			continue
		}
		if _, ok := n.Labels["node-role.kubernetes.io/master"]; ok {
			continue
		}
		workers = append(workers, n)
	}
	return workers, nil
}

func PodEvents(ctx context.Context, cs *kubernetes.Clientset, namespace, podName string) ([]corev1.Event, error) {
	list, err := cs.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.name=%s,involvedObject.kind=Pod", podName),
	})
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func sanitize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

func boolPtr(v bool) *bool { return &v }

// DiscardUnread drains an io.Reader (helper for future streaming APIs).
func DiscardUnread(r io.Reader) {
	_, _ = io.Copy(io.Discard, r)
}
