//go:build !no_stage

package deploy

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/util/yaml"
)

func TestCertManagerBundle(t *testing.T) {
	content := assetBytes(t, "cert-manager.yaml")
	source, err := os.ReadFile("../../manifests/cert-manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, source) {
		t.Fatal("cert-manager embedded manifest is stale; run the resource generator")
	}
	imageList, err := os.ReadFile("../../hack/airgap/image-list.txt")
	if err != nil {
		t.Fatal(err)
	}
	packaged := map[string]bool{}
	for _, image := range strings.Fields(string(imageList)) {
		packaged[image] = true
	}
	images := map[string]bool{}
	deployments := map[string]bool{}
	crds := map[string]bool{}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(content), 4096)
	for {
		var obj unstructured.Unstructured
		if err := decoder.Decode(&obj); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		switch obj.GetKind() {
		case "Issuer", "ClusterIssuer", "Certificate", "CertificateRequest":
			t.Fatalf("default install must not request certificates: %s/%s", obj.GetKind(), obj.GetName())
		case "CustomResourceDefinition":
			crds[obj.GetName()] = true
		case "Deployment":
			deployments[obj.GetName()] = true
			containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range containers {
				container := item.(map[string]interface{})
				image, _, _ := unstructured.NestedString(container, "image")
				images[image] = true
				args, _, _ := unstructured.NestedStringSlice(container, "args")
				for _, arg := range args {
					if image, found := strings.CutPrefix(arg, "--acme-http01-solver-image="); found {
						images[image] = true
					}
				}
			}
		}
	}
	for _, name := range []string{"cert-manager", "cert-manager-cainjector", "cert-manager-webhook"} {
		if !deployments[name] {
			t.Errorf("missing deployment %s", name)
		}
	}
	for _, name := range []string{"certificates.cert-manager.io", "certificaterequests.cert-manager.io", "issuers.cert-manager.io", "clusterissuers.cert-manager.io", "orders.acme.cert-manager.io", "challenges.acme.cert-manager.io"} {
		if !crds[name] {
			t.Errorf("missing CRD %s", name)
		}
	}
	for _, component := range []string{"controller", "cainjector", "webhook", "acmesolver"} {
		image := "quay.io/jetstack/cert-manager-" + component + ":v1.21.2"
		if !images[image] {
			t.Errorf("missing runtime image %s", image)
		}
	}
	for image := range images {
		if !packaged[image] {
			t.Errorf("runtime image missing from airgap list: %s", image)
		}
		if !strings.HasSuffix(image, ":v1.21.2") {
			t.Errorf("unexpected cert-manager version: %s", image)
		}
	}
}

func TestCertManagerDefaultAndDisable(t *testing.T) {
	dir := t.TempDir()
	if err := Stage(dir, nil, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cert-manager.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default install did not stage cert-manager: %v", err)
	}
	disables := map[string]bool{"cert-manager": true}
	if !shouldDisableFile(dir, path, disables) {
		t.Fatal("--disable=cert-manager must disable the Addon")
	}
	if err := Stage(dir, nil, disables); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled bundle still staged: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "coredns.yaml")); err != nil {
		t.Fatalf("disabling cert-manager affected another bundle: %v", err)
	}
}
