package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStageAdvertiseIPSubstitution verifies the KIP-21 staged output: the
// %{ADVERTISE_IP}% template is substituted into the e2b-gateway.yaml
// Endpoints and the resulting manifest carries the routable (non-loopback)
// address.
func TestStageAdvertiseIPSubstitution(t *testing.T) {
	dir := t.TempDir()
	vars := map[string]string{
		"%{ADVERTISE_IP}%": "10.1.2.3",
	}
	if err := Stage(dir, vars, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "sandbox-matrix", "e2b-gateway.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "%{ADVERTISE_IP}%") {
		t.Fatalf("template %%%%{ADVERTISE_IP}%%%% not substituted")
	}
	for _, want := range []string{"ip: 10.1.2.3", "port: 50051", "port: 3676"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("staged e2b-gateway.yaml missing %q", want)
		}
	}
	if strings.Contains(string(b), "127.0.0.1") {
		t.Fatalf("staged e2b-gateway.yaml contains loopback")
	}
}

// TestStageSkipsE2BGatewayWhenAdvertiseUnresolvable verifies that when no
// routable advertise IP exists, e2b-gateway.yaml is skipped (KIP-21 hard
// failure instead of writing a broken manifest) while other manifests still
// stage.
func TestStageSkipsE2BGatewayWhenAdvertiseUnresolvable(t *testing.T) {
	dir := t.TempDir()
	skips := map[string]bool{
		"sandbox-matrix/e2b-gateway.yaml": true,
	}
	if err := Stage(dir, map[string]string{}, skips); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sandbox-matrix", "e2b-gateway.yaml")); !os.IsNotExist(err) {
		t.Fatalf("e2b-gateway.yaml should be skipped, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "coredns.yaml")); err != nil {
		t.Fatalf("other manifests must still stage: %v", err)
	}
}
