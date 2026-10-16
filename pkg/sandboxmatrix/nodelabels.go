// KIP-30 M3.3: warm-pool node / runtime-class awareness.
//
// A warm pool with RuntimeClass gvisor|kata|firecracker can only run on nodes
// that actually support that runtime (kata/firecracker need /dev/kvm and the
// matching runtime classes installed; gvisor needs neither). Without
// awareness the reconciler schedules warm pods onto every node, the ones on
// unsupported nodes sit Pending forever (RuntimeClass not found), and the
// controller's own recycle pass deletes and recreates them against the same
// nodes in a churn loop — burning reconcile cycles and PVC churn while the
// pool reports NotReady for an operator-visible reason nobody wrote down.
//
// Two mechanisms close the gap:
//
//  1. Node annotation → label mirror. Nodes may annotate
//     sandbox.k8e.io/runtime-classes: gvisor,kata (the set of runtime classes
//     they can serve). Node affinity matches labels, not annotations, so the
//     leader mirrors each class to a boolean label
//     sandbox.k8e.io/runtime-class-<class>: "true" and drops it again when
//     the annotation changes. Warm pods then carry a required nodeAffinity on
//     the pool's class — but only when at least one node is annotated for it,
//     so single-node installs with no annotations keep working exactly as
//     before (the common case; annotating is opt-in).
//
//  2. Unschedulable diagnostics. When a warm pod is Pending with the
//     scheduler reporting Unschedulable, the reconciler records the reason as
//     an annotation on the pod and the pool's NotReady reason on the
//     SandboxMatrix status, instead of silently burning the delete/recreate
//     cycle. A pod whose failure is caused by a node that cannot run the
//     class is retired once; the next create targets a node that can.
package sandboxmatrix

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	// runtimeClassesAnnotation is the operator-facing per-node declaration of
	// which sandbox runtime classes a node can serve.
	runtimeClassesAnnotation = "sandbox.k8e.io/runtime-classes"
	// runtimeClassLabelPrefix + "<class>" is the mirrored boolean label warm
	// pods' nodeAffinity matches on.
	runtimeClassLabelPrefix = "sandbox.k8e.io/runtime-class-"
	// unschedulableReasonAnnotation records why the scheduler could not place
	// a warm pod, so the pool status (and the operator) can see it without
	// reading scheduler events.
	unschedulableReasonAnnotation = "sandbox.k8e.io/unschedulable-reason"
	// nodeLabelSyncInterval bounds how long a stale mirrored label can linger
	// after an annotation change (or a missed watch event).
	nodeLabelSyncInterval = 30 * time.Second
)

// parseRuntimeClasses splits the annotation value ("gvisor,kata") into a
// sorted, deduplicated, sanitized class list. Empty/blank entries are
// dropped; anything that is not a plausible label suffix (alphanumeric plus
// - and .) is dropped too, because it would poison the mirrored label.
func parseRuntimeClasses(value string) []string {
	seen := map[string]bool{}
	var classes []string
	for _, part := range strings.Split(value, ",") {
		class := strings.TrimSpace(part)
		if class == "" || !validRuntimeClassLabel(class) || seen[class] {
			continue
		}
		seen[class] = true
		classes = append(classes, class)
	}
	sort.Strings(classes)
	return classes
}

// validRuntimeClassLabel reports whether class can be a Kubernetes label
// value (used verbatim as sandbox.k8e.io/runtime-class-<class>: "true").
func validRuntimeClassLabel(class string) bool {
	if len(class) > 63 {
		return false
	}
	for _, r := range class {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.':
		default:
			return false
		}
	}
	return class != ""
}

// runtimeClassLabel is the mirrored label key for a runtime class.
func runtimeClassLabel(class string) string {
	return runtimeClassLabelPrefix + class
}

// runNodeRuntimeLabelLoop mirrors node annotations to labels until ctx is
// cancelled. Leader-gated (same Lease as the rest): the mirror is a write on
// every node, and two writers would fight over the same label.
func runNodeRuntimeLabelLoop(ctx context.Context, k8s kubernetes.Interface) {
	ticker := time.NewTicker(nodeLabelSyncInterval)
	defer ticker.Stop()
	for {
		mirrorNodeRuntimeLabels(ctx, k8s)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// mirrorNodeRuntimeLabels reconciles the mirrored labels on every node:
// present for each annotated class, absent for classes the node no longer
// declares. Exported for tests.
func mirrorNodeRuntimeLabels(ctx context.Context, k8s kubernetes.Interface) {
	nodes, err := k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		logrus.Debugf("sandbox-nodelabels: list nodes: %v", err)
		return
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		syncNodeRuntimeLabels(ctx, k8s, node)
	}
}

func syncNodeRuntimeLabels(ctx context.Context, k8s kubernetes.Interface, node *corev1.Node) {
	desired := parseRuntimeClasses(node.Annotations[runtimeClassesAnnotation])
	desiredSet := map[string]bool{}
	for _, class := range desired {
		desiredSet[class] = true
	}

	labels := map[string]string{}
	if node.Labels != nil {
		for k, v := range node.Labels {
			labels[k] = v
		}
	}
	changed := false
	// Drop labels for classes no longer annotated.
	for key := range labels {
		if !strings.HasPrefix(key, runtimeClassLabelPrefix) {
			continue
		}
		class := strings.TrimPrefix(key, runtimeClassLabelPrefix)
		if !desiredSet[class] {
			delete(labels, key)
			changed = true
		}
	}
	// Add labels for annotated classes.
	for _, class := range desired {
		key := runtimeClassLabel(class)
		if labels[key] != "true" {
			labels[key] = "true"
			changed = true
		}
	}
	if !changed {
		return
	}
	updated := node.DeepCopy()
	updated.Labels = labels
	if _, err := k8s.CoreV1().Nodes().Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		logrus.Warnf("sandbox-nodelabels: update %s: %v", node.Name, err)
	}
}

// nodesSupportingClass returns the nodes that can serve runtimeClass: every
// node when no node carries the annotation (single-node default), otherwise
// only the annotated ones. The second return reports whether any node
// declared the class explicitly.
func nodesSupportingClass(ctx context.Context, k8s kubernetes.Interface, runtimeClass string) (candidateNames []string, anyAnnotated bool, err error) {
	if runtimeClass == "" {
		return nil, false, nil
	}
	nodes, err := k8s.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, false, err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if nodeAnnotatesClass(node, runtimeClass) {
			candidateNames = append(candidateNames, node.Name)
			anyAnnotated = true
		}
	}
	return candidateNames, anyAnnotated, nil
}

func nodeAnnotatesClass(node *corev1.Node, runtimeClass string) bool {
	for _, class := range parseRuntimeClasses(node.Annotations[runtimeClassesAnnotation]) {
		if class == runtimeClass {
			return true
		}
	}
	return false
}

// warmPodNodeAffinity returns a required nodeAffinity for the pool's runtime
// class, or nil when no node is annotated for it (unannotated single-node
// installs must keep scheduling freely).
func warmPodNodeAffinity(ctx context.Context, k8s kubernetes.Interface, runtimeClass string) *corev1.NodeAffinity {
	_, anyAnnotated, err := nodesSupportingClass(ctx, k8s, runtimeClass)
	if err != nil || !anyAnnotated {
		return nil
	}
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      runtimeClassLabel(runtimeClass),
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"true"},
				}},
			}},
		},
	}
}

// warmPodUnschedulableReason extracts the scheduler's failure reason from a
// Pending warm pod, or "" when it is not (yet) unschedulable.
func warmPodUnschedulableReason(pod *corev1.Pod) string {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse {
			if condition.Reason == corev1.PodReasonUnschedulable {
				return condition.Message
			}
		}
	}
	return ""
}

// annotateUnschedulableWarmPod records the scheduler's reason on the pod so
// the pool status can surface it (KIP-30 M3.3). Best-effort: a failed
// annotation never blocks the reconcile.
func annotateUnschedulableWarmPod(ctx context.Context, k8s kubernetes.Interface, namespace string, pod *corev1.Pod, reason string) {
	if pod.Annotations[unschedulableReasonAnnotation] == reason {
		return
	}
	updated := pod.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[unschedulableReasonAnnotation] = reason
	if _, err := k8s.CoreV1().Pods(namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		logrus.Debugf("sandbox-nodelabels: annotate %s: %v", pod.Name, err)
	}
}

// poolNotReadyReasons summarizes why warm pods cannot serve: the distinct
// unschedulable reasons seen on Pending warm pods.
func poolNotReadyReasons(pods []corev1.Pod) []string {
	seen := map[string]bool{}
	var reasons []string
	for i := range pods {
		pod := &pods[i]
		if pod.Status.Phase != corev1.PodPending {
			continue
		}
		reason := warmPodUnschedulableReason(pod)
		if reason == "" || seen[reason] {
			continue
		}
		seen[reason] = true
		reasons = append(reasons, reason)
	}
	return reasons
}
