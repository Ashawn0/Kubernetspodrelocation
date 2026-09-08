package disrupt

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

const (
	AppLabel     = "reloc-uidprobe"
	Port         = 8080
	DefaultImage = "python:3.12-alpine" // Stage 0 default; no custom image push required
)

// UIDServerPod returns a pod that serves X-Pod-Uid from Downward API.
// preStopSleepSec creates graceful overlap after DELETE.
func UIDServerPod(name, namespace, image string, preStopSleepSec, graceSec int32) *corev1.Pod {
	if image == "" {
		image = DefaultImage
	}
	if preStopSleepSec <= 0 {
		preStopSleepSec = 12
	}
	if graceSec <= preStopSleepSec {
		graceSec = preStopSleepSec + 5
	}
	cmd := []string{"python3", "-c", uidServerPython(Port)}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": AppLabel, "stage0": "true"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyAlways,
			TerminationGracePeriodSeconds: int64Ptr(int64(graceSec)),
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			}},
			Containers: []corev1.Container{{
				Name:            "uidserver",
				Image:           image,
				ImagePullPolicy: corev1.PullIfNotPresent,
				Command:         cmd,
				Ports: []corev1.ContainerPort{{
					Name: "http", ContainerPort: Port,
				}},
				Env: []corev1.EnvVar{{
					Name: "POD_UID",
					ValueFrom: &corev1.EnvVarSource{
						FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"},
					},
				}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{Path: "/", Port: intstr.FromInt(Port)},
					},
					InitialDelaySeconds: 1,
					PeriodSeconds:       1,
				},
				Lifecycle: &corev1.Lifecycle{
					PreStop: &corev1.LifecycleHandler{
						Exec: &corev1.ExecAction{
							Command: []string{"sleep", fmt.Sprintf("%d", preStopSleepSec)},
						},
					},
				},
			}},
		},
	}
}

// UIDService is a ClusterIP Service selecting uidprobe pods.
func UIDService(name, namespace string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": AppLabel, "stage0": "true"},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": AppLabel},
			Ports: []corev1.ServicePort{{
				Name: "http", Port: Port, TargetPort: intstr.FromInt(Port),
			}},
			Type: corev1.ServiceTypeClusterIP,
		},
	}
}

func uidServerPython(port int) string {
	return fmt.Sprintf(`
import os
from http.server import BaseHTTPRequestHandler, HTTPServer
UID = os.environ.get("POD_UID", "")
class H(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200)
        self.send_header("X-Pod-Uid", UID)
        self.send_header("Content-Type", "text/plain")
        self.end_headers()
        self.wfile.write(("ok uid=%%s\n" %% UID).encode())
    def log_message(self, *args):
        pass
HTTPServer(("", %d), H).serve_forever()
`, port)
}

func int64Ptr(v int64) *int64 { return &v }
