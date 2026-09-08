package place

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const Stage0LabelKey = "reloc-disrupt.stage0/node"

// ProbePod is a tiny pause pod used only for placement-path checks.
func ProbePod(name, namespace string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    map[string]string{"app": "reloc-schedprobe", "stage0": "true"},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{{
				Name:            "pause",
				Image:           "registry.k8s.io/pause:3.9",
				ImagePullPolicy: corev1.PullIfNotPresent,
			}},
			Tolerations: []corev1.Toleration{{
				Operator: corev1.TolerationOpExists,
			}},
		},
	}
}

// WithNodeSelector forces placement through the scheduler Filter path.
func WithNodeSelector(p *corev1.Pod, key, value string) *corev1.Pod {
	out := p.DeepCopy()
	out.Spec.NodeSelector = map[string]string{key: value}
	out.Spec.NodeName = ""
	return out
}

// WithRequiredNodeAffinity forces placement via requiredDuringSchedulingIgnoredDuringExecution.
func WithRequiredNodeAffinity(p *corev1.Pod, key, value string) *corev1.Pod {
	out := p.DeepCopy()
	out.Spec.NodeName = ""
	out.Spec.Affinity = &corev1.Affinity{
		NodeAffinity: &corev1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{
					MatchExpressions: []corev1.NodeSelectorRequirement{{
						Key:      key,
						Operator: corev1.NodeSelectorOpIn,
						Values:   []string{value},
					}},
				}},
			},
		},
	}
	return out
}

// WithNodeName is the Stage 0 negative control: bypasses the scheduler.
// Never use this for campaign placement.
func WithNodeName(p *corev1.Pod, nodeName string) *corev1.Pod {
	out := p.DeepCopy()
	out.Spec.NodeName = nodeName
	out.Spec.NodeSelector = nil
	out.Spec.Affinity = nil
	return out
}
