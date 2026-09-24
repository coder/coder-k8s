package controller

import (
	"maps"

	corev1 "k8s.io/api/core/v1"
)

// kubectlRestartedAtAnnotation is the pod template annotation that
// `kubectl rollout restart` sets to trigger a new rollout.
const kubectlRestartedAtAnnotation = "kubectl.kubernetes.io/restartedAt"

// withPreservedRolloutRestart returns desired with the kubectl rollout restart
// annotation copied verbatim from current, when current has it. Controllers
// replace the whole pod template on every reconcile; without this, they drop
// the annotation and immediately undo `kubectl rollout restart`. No other
// annotation is preserved, and the value is never generated or changed.
func withPreservedRolloutRestart(current, desired corev1.PodTemplateSpec) corev1.PodTemplateSpec {
	restartedAt, ok := current.Annotations[kubectlRestartedAtAnnotation]
	if !ok {
		return desired
	}

	annotations := maps.Clone(desired.Annotations)
	if annotations == nil {
		annotations = make(map[string]string, 1)
	}
	annotations[kubectlRestartedAtAnnotation] = restartedAt
	desired.Annotations = annotations

	return desired
}
