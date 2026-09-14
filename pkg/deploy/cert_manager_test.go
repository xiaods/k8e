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
	content := certManagerEmbedded(t)
	parsed := parseCertManagerManifest(t, content)

	t.Run("deployments", assertCertManagerDeployments(parsed))
	t.Run("crds", assertCertManagerCRDs(parsed))
	t.Run("runtime images", assertCertManagerRuntimeImages(parsed))
	t.Run("airgap coverage", assertCertManagerAirgapCoverage(parsed))
}

// assertCertManagerDeployments returns the sub-test that checks every
// cert-manager controller Deployment is present in the bundle.
func assertCertManagerDeployments(parsed certManagerManifest) func(*testing.T) {
	return func(t *testing.T) {
		t.Helper()
		for _, name := range []string{"cert-manager", "cert-manager-cainjector", "cert-manager-webhook"} {
			if !parsed.deployments[name] {
				t.Errorf("missing deployment %s", name)
			}
		}
	}
}

// assertCertManagerCRDs returns the sub-test that checks the cert-manager API
// is registered through CustomResourceDefinitions.
func assertCertManagerCRDs(parsed certManagerManifest) func(*testing.T) {
	return func(t *testing.T) {
		t.Helper()
		for _, name := range []string{
			"certificates.cert-manager.io",
			"certificaterequests.cert-manager.io",
			"issuers.cert-manager.io",
			"clusterissuers.cert-manager.io",
			"orders.acme.cert-manager.io",
			"challenges.acme.cert-manager.io",
		} {
			if !parsed.crds[name] {
				t.Errorf("missing CRD %s", name)
			}
		}
	}
}

// assertCertManagerRuntimeImages returns the sub-test that checks the bundled
// runtime images, including the ACME HTTP-01 solver.
func assertCertManagerRuntimeImages(parsed certManagerManifest) func(*testing.T) {
	return func(t *testing.T) {
		t.Helper()
		for _, component := range []string{"controller", "cainjector", "webhook", "acmesolver"} {
			image := "quay.io/jetstack/cert-manager-" + component + ":v1.21.2"
			if !parsed.images[image] {
				t.Errorf("missing runtime image %s", image)
			}
		}
	}
}

// assertCertManagerAirgapCoverage returns the sub-test that checks every image
// referenced by the bundle is shipped by the offline archive, at the expected
// version.
func assertCertManagerAirgapCoverage(parsed certManagerManifest) func(*testing.T) {
	return func(t *testing.T) {
		t.Helper()
		packaged := airgapImageSet(t)
		for image := range parsed.images {
			if !packaged[image] {
				t.Errorf("runtime image missing from airgap list: %s", image)
			}
			if !strings.HasSuffix(image, ":v1.21.2") {
				t.Errorf("unexpected cert-manager version: %s", image)
			}
		}
	}
}

// certManagerEmbedded returns the embedded manifest after asserting it still
// matches the on-disk source, so a stale bindata build fails loudly.
func certManagerEmbedded(t *testing.T) []byte {
	t.Helper()
	content := assetBytes(t, "cert-manager.yaml")
	source, err := os.ReadFile("../../manifests/cert-manager.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, source) {
		t.Fatal("cert-manager embedded manifest is stale; run the resource generator")
	}
	return content
}

// airgapImageSet returns the images shipped in the offline archive.
func airgapImageSet(t *testing.T) map[string]bool {
	t.Helper()
	imageList, err := os.ReadFile("../../hack/airgap/image-list.txt")
	if err != nil {
		t.Fatal(err)
	}
	packaged := map[string]bool{}
	for _, image := range strings.Fields(string(imageList)) {
		packaged[image] = true
	}
	return packaged
}

type certManagerManifest struct {
	deployments map[string]bool
	crds        map[string]bool
	images      map[string]bool
}

func parseCertManagerManifest(t *testing.T, content []byte) certManagerManifest {
	t.Helper()
	parsed := certManagerManifest{
		deployments: map[string]bool{},
		crds:        map[string]bool{},
		images:      map[string]bool{},
	}
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
			parsed.crds[obj.GetName()] = true
		case "Deployment":
			parsed.deployments[obj.GetName()] = true
			collectCertManagerImages(t, parsed.images, obj)
		}
	}
	return parsed
}

func collectCertManagerImages(t *testing.T, images map[string]bool, obj unstructured.Unstructured) {
	t.Helper()
	containers, _, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range containers {
		container, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		image, _, _ := unstructured.NestedString(container, "image")
		images[image] = true
		args, _, _ := unstructured.NestedStringSlice(container, "args")
		for _, arg := range args {
			if solver, found := strings.CutPrefix(arg, "--acme-http01-solver-image="); found {
				images[solver] = true
			}
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
