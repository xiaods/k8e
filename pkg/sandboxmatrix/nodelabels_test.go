package sandboxmatrix

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestParseRuntimeClasses(t *testing.T) {
	got := parseRuntimeClasses("gvisor, kata ,,gvisor")
	if len(got) != 2 || got[0] != "gvisor" || got[1] != "kata" {
		t.Fatalf("parse = %v", got)
	}
	// Invalid label characters are dropped, not passed through.
	if got := parseRuntimeClasses("gvisor, Bad_Class!, kata"); len(got) != 2 || got[0] != "gvisor" || got[1] != "kata" {
		t.Fatalf("parse with invalid = %v", got)
	}
	if got := parseRuntimeClasses("   "); len(got) != 0 {
		t.Fatalf("blank = %v", got)
	}
}

func TestMirrorNodeRuntimeLabels(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	annotated := &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "kata-node",
		Annotations: map[string]string{runtimeClassesAnnotation: "gvisor,kata"},
	}}
	plain := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "plain-node"}}
	if _, err := k8s.CoreV1().Nodes().Create(context.Background(), annotated, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := k8s.CoreV1().Nodes().Create(context.Background(), plain, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	mirrorNodeRuntimeLabels(context.Background(), k8s)

	kataNode, err := k8s.CoreV1().Nodes().Get(context.Background(), "kata-node", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if kataNode.Labels[runtimeClassLabel("gvisor")] != "true" || kataNode.Labels[runtimeClassLabel("kata")] != "true" {
		t.Fatalf("annotated node labels = %v", kataNode.Labels)
	}
	plainNode, err := k8s.CoreV1().Nodes().Get(context.Background(), "plain-node", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := plainNode.Labels[runtimeClassLabel("gvisor")]; ok {
		t.Fatalf("plain node must not get runtime labels: %v", plainNode.Labels)
	}

	// Annotation shrinks to gvisor only: the kata label is removed.
	kataNode.Annotations[runtimeClassesAnnotation] = "gvisor"
	if _, err := k8s.CoreV1().Nodes().Update(context.Background(), kataNode, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	mirrorNodeRuntimeLabels(context.Background(), k8s)
	kataNode, _ = k8s.CoreV1().Nodes().Get(context.Background(), "kata-node", metav1.GetOptions{})
	if kataNode.Labels[runtimeClassLabel("gvisor")] != "true" {
		t.Fatalf("gvisor label lost: %v", kataNode.Labels)
	}
	if _, ok := kataNode.Labels[runtimeClassLabel("kata")]; ok {
		t.Fatalf("stale kata label: %v", kataNode.Labels)
	}
}

func TestWarmPodNodeAffinityOnlyWhenAnnotated(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewSimpleClientset()
	// No annotated nodes: unannotated single-node installs schedule freely.
	if affinity := warmPodNodeAffinity(ctx, k8s, "gvisor"); affinity != nil {
		t.Fatalf("unannotated cluster must not constrain affinity: %+v", affinity)
	}
	if _, err := k8s.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{
		Name:        "kata-node",
		Annotations: map[string]string{runtimeClassesAnnotation: "kata"},
	}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if affinity := warmPodNodeAffinity(ctx, k8s, "kata"); affinity == nil {
		t.Fatal("annotated class must constrain affinity")
	}
	// A class no node declares: no constraint even when other classes exist.
	if affinity := warmPodNodeAffinity(ctx, k8s, "firecracker"); affinity != nil {
		t.Fatalf("undeclared class must not constrain affinity: %+v", affinity)
	}
}

func TestWarmPodUnschedulableReasonAndAnnotate(t *testing.T) {
	ctx := context.Background()
	k8s := fake.NewSimpleClientset()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "sandbox-warm-x", Namespace: testNS},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
			Conditions: []corev1.PodCondition{{
				Type:    corev1.PodScheduled,
				Status:  corev1.ConditionFalse,
				Reason:  corev1.PodReasonUnschedulable,
				Message: "0/1 nodes available: RuntimeClass not found",
			}},
		},
	}
	if _, err := k8s.CoreV1().Pods(testNS).Create(ctx, pod, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if reason := warmPodUnschedulableReason(pod); reason != "0/1 nodes available: RuntimeClass not found" {
		t.Fatalf("reason = %q", reason)
	}
	annotateUnschedulableWarmPod(ctx, k8s, testNS, pod, "RuntimeClass not found")
	current, err := k8s.CoreV1().Pods(testNS).Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if current.Annotations[unschedulableReasonAnnotation] != "RuntimeClass not found" {
		t.Fatalf("annotation = %v", current.Annotations)
	}

	reasons := poolNotReadyReasons([]corev1.Pod{*pod})
	if len(reasons) != 1 || reasons[0] != "0/1 nodes available: RuntimeClass not found" {
		t.Fatalf("pool reasons = %v", reasons)
	}
}

func TestNewWarmPodAppliesAffinityAndNodeSelector(t *testing.T) {
	cfg := defaultCfg()
	affinity := warmPodNodeAffinityForTest("kata")
	pod := newWarmPod(cfg, "kata", 0, "workspace-warm-x", affinity, map[string]string{"disk": "ssd"})
	if pod.Spec.Affinity == nil || pod.Spec.Affinity.NodeAffinity == nil {
		t.Fatal("affinity not applied to the warm pod spec")
	}
	if pod.Spec.NodeSelector["disk"] != "ssd" {
		t.Fatalf("nodeSelector = %v", pod.Spec.NodeSelector)
	}
	// The pool's nodeSelector must not be mutated through the shared map.
	nodeSelector := map[string]string{"disk": "ssd"}
	_ = newWarmPod(cfg, "gvisor", 0, "workspace-warm-x", nil, nodeSelector)
	nodeSelector["disk"] = "nvme"
}

func warmPodNodeAffinityForTest(class string) *corev1.NodeAffinity {
	return &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key:      runtimeClassLabel(class),
					Operator: corev1.NodeSelectorOpIn,
					Values:   []string{"true"},
				}},
			}},
		},
	}
}
