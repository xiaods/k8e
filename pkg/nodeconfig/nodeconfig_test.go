package nodeconfig

import (
	"os"
	"testing"

	"github.com/xiaods/k8e/pkg/version"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var FakeNodeWithNoAnnotation = &corev1.Node{
	TypeMeta: metav1.TypeMeta{
		Kind:       "Node",
		APIVersion: "v1",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "fakeNode-no-annotation",
	},
}

var FakeNodeWithAnnotation = &corev1.Node{
	TypeMeta: metav1.TypeMeta{
		Kind:       "Node",
		APIVersion: "v1",
	},
	ObjectMeta: metav1.ObjectMeta{
		Name: "fakeNode-with-annotation",
		Annotations: map[string]string{
			NodeArgsAnnotation:       `["server"]`,
			NodeEnvAnnotation:        `{"` + version.ProgramUpper + `_NODE_NAME":"fakeNode-with-annotation"}`,
			NodeConfigHashAnnotation: "LNQOAOIMOQIBRMEMACW7LYHXUNPZADF6RFGOSPIHJCOS47UVUJAA====",
		},
	},
}

func assertEqual(t *testing.T, a interface{}, b interface{}) {
	if a != b {
		t.Fatalf("[ %v != %v ]", a, b)
	}
}

func TestSetEmptyNodeConfigAnnotations(t *testing.T) {
	os.Args = []string{version.Program, "server"}
	os.Setenv(version.ProgramUpper+"_NODE_NAME", "fakeNode-no-annotation")
	nodeUpdated, err := SetNodeConfigAnnotations(FakeNodeWithNoAnnotation)
	if err != nil {
		t.Fatalf("Failed to set node config annotation: %v", err)
	}
	assertEqual(t, true, nodeUpdated)

	expectedArgs := `["server"]`
	actualArgs := FakeNodeWithNoAnnotation.Annotations[NodeArgsAnnotation]
	assertEqual(t, expectedArgs, actualArgs)

	expectedEnv := `{"` + version.ProgramUpper + `_NODE_NAME":"fakeNode-no-annotation"}`
	actualEnv := FakeNodeWithNoAnnotation.Annotations[NodeEnvAnnotation]
	assertEqual(t, expectedEnv, actualEnv)

	expectedHash := "GTVBBZB7H52TUK5KNXZDR5HOWTIVI4BHSKVVFYZDPNW4MTTB5MEA===="
	actualHash := FakeNodeWithNoAnnotation.Annotations[NodeConfigHashAnnotation]
	assertEqual(t, expectedHash, actualHash)
}

func TestSetExistingNodeConfigAnnotations(t *testing.T) {
	// adding same config
	os.Args = []string{version.Program, "server"}
	os.Setenv(version.ProgramUpper+"_NODE_NAME", "fakeNode-with-annotation")
	nodeUpdated, err := SetNodeConfigAnnotations(FakeNodeWithAnnotation)
	if err != nil {
		t.Fatalf("Failed to set node config annotation: %v", err)
	}
	assertEqual(t, true, nodeUpdated)
}

func TestSetArgsWithEqual(t *testing.T) {
	os.Args = []string{version.Program, "server", "--write-kubeconfig-mode=777"}
	os.Setenv("K8E_NODE_NAME", "fakeNode-with-no-annotation")
	nodeUpdated, err := SetNodeConfigAnnotations(FakeNodeWithNoAnnotation)
	if err != nil {
		t.Fatalf("Failed to set node config annotation: %v", err)
	}
	assertEqual(t, true, nodeUpdated)
	expectedArgs := `["server","--write-kubeconfig-mode","777"]`
	actualArgs := FakeNodeWithNoAnnotation.Annotations[NodeArgsAnnotation]
	assertEqual(t, expectedArgs, actualArgs)
}
