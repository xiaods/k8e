package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaods/k8e/pkg/daemons/config"
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"
	"sigs.k8s.io/yaml"
)

func TestWriteDropInRendersKubeletConfiguration(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kubelet.conf.d")

	settings := newKubeletSettings()
	commonKubeletSettings(settings, &config.Agent{
		ClusterDomain:    "cluster.local",
		KubeletConfigDir: dir,
		PodManifests:     filepath.Join(t.TempDir(), "pod-manifests"),
	})
	if err := settings.writeDropIn(dir); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, kubeletDropInFileName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	parsed := &kubeletconfigv1beta1.KubeletConfiguration{}
	if err := yaml.Unmarshal(data, parsed); err != nil {
		t.Fatalf("drop-in %s is not a valid KubeletConfiguration: %v", path, err)
	}
	if parsed.APIVersion != kubeletconfigv1beta1.SchemeGroupVersion.String() || parsed.Kind != "KubeletConfiguration" {
		t.Fatalf("drop-in declares %s/%s, want %s/KubeletConfiguration",
			parsed.APIVersion, parsed.Kind, kubeletconfigv1beta1.SchemeGroupVersion.String())
	}
	if parsed.ClusterDomain != "cluster.local" {
		t.Fatalf("clusterDomain = %q, want cluster.local", parsed.ClusterDomain)
	}
	if parsed.FailSwapOn == nil || *parsed.FailSwapOn {
		t.Fatalf("failSwapOn = %v, want false", parsed.FailSwapOn)
	}
	if want := "5%"; parsed.EvictionHard["imagefs.available"] != want {
		t.Fatalf("evictionHard[imagefs.available] = %q, want %q", parsed.EvictionHard["imagefs.available"], want)
	}

	if got := settings.flags[kubeletDropInDirFlag]; got != dir {
		t.Fatalf("--%s = %q, want %q", kubeletDropInDirFlag, got, dir)
	}
}

// A gate upstream removed from a Kubernetes release must not reach kubelet:
// kubelet rejects the whole command line with "unrecognized feature gate" and the
// node never comes up.
func TestUnknownFeatureGatesAreDropped(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "kubelet.conf.d")

	s := newKubeletSettings()
	s.addFeatureGate("DefinitelyNotAKubeletFeatureGate", true)

	if gates := s.featureGates(); len(gates) != 0 {
		t.Fatalf("unregistered gate survived filtering: %#v", gates)
	}

	if err := s.writeDropIn(dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, kubeletDropInFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "DefinitelyNotAKubeletFeatureGate") {
		t.Fatalf("drop-in still carries an unregistered gate:\n%s", data)
	}
}
