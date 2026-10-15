package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStageAdvertiseIPSubstitution verifies the staged output of the
// EndpointSlice bridge: the %{ADVERTISE_IP}% template is substituted into
// both EndpointSlice addresses and the resulting manifest carries the
// routable (non-loopback) address.
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
	for _, want := range []string{"10.1.2.3", "port: 50051", "port: 3676", "addressType: IPv4"} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("staged e2b-gateway.yaml missing %q", want)
		}
	}
	if strings.Contains(string(b), "127.0.0.1") {
		t.Fatalf("staged e2b-gateway.yaml contains loopback")
	}
}

// TestStageSkipsE2BGatewayWhenAdvertiseUnresolvable verifies that when no
// routable advertise IP exists, e2b-gateway.yaml is skipped while other
// manifests still stage.
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

// TestStageSkipsRemovesStaleCopy verifies the fail-closed transition: a
// manifest left on disk by an earlier successful run is REMOVED when it
// becomes skip-listed (the deploy watcher would otherwise re-apply the
// stale file).
func TestStageSkipsRemovesStaleCopy(t *testing.T) {
	dir := t.TempDir()

	// First run stages the manifest (advertise IP resolvable).
	if err := Stage(dir, map[string]string{"%{ADVERTISE_IP}%": "10.1.2.3"}, nil); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "sandbox-matrix", "e2b-gateway.yaml")
	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("first stage should create e2b-gateway.yaml: %v", err)
	}

	// Second run: advertise IP unresolvable -> manifest skip-listed. The
	// stale copy must be removed so the watcher cannot re-apply it.
	if err := Stage(dir, map[string]string{}, map[string]bool{
		"sandbox-matrix/e2b-gateway.yaml": true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("skip-listed manifest must be removed from disk, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "coredns.yaml")); err != nil {
		t.Fatalf("other manifests must still be staged after skip: %v", err)
	}
}

// TestStageSkipsSandboxMCPDefault verifies the opt-in manifest set for the
// MCP adapter: applying manifests/sandbox-mcp by default lays down a
// k8e-mcp Deployment whose pods can never become ready (missing
// mcp-gateway-tls Secret and the unpublished k8e-mcp:local image), so the
// server default-skips the whole directory and hack/init-sandbox-mcp.sh
// applies the manifest directly once the prerequisites exist. Skipping must
// also remove copies written by earlier releases.
func TestStageSkipsSandboxMCPDefault(t *testing.T) {
	dir := t.TempDir()

	// A copy staged by an earlier release must not survive the skip.
	stale := filepath.Join(dir, "sandbox-mcp", "deployment.yaml")
	if err := os.MkdirAll(filepath.Dir(stale), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("stale"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := Stage(dir, map[string]string{}, map[string]bool{"sandbox-mcp": true, "sandbox-mcp/deployment.yaml": true}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatalf("sandbox-mcp/deployment.yaml must not be staged, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "sandbox-mcp", "Dockerfile")); !os.IsNotExist(err) {
		t.Fatalf("sandbox-mcp/Dockerfile must not be staged, stat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "coredns.yaml")); err != nil {
		t.Fatalf("other manifests must still stage: %v", err)
	}
}

// TestStageSkipsRemovalToleratesMissingCopy verifies the removal is a
// no-op (and not an error) when the skip-listed manifest was never staged.
func TestStageSkipsRemovalToleratesMissingCopy(t *testing.T) {
	dir := t.TempDir()
	if err := Stage(dir, map[string]string{}, map[string]bool{
		"sandbox-matrix/e2b-gateway.yaml": true,
	}); err != nil {
		t.Fatalf("staging with skip on a fresh dir must not error: %v", err)
	}
}

// TestStageSkipRemovalFailureFailsStaging verifies that when the stale copy
// cannot be removed, Stage fails loudly instead of silently leaving the file
// for the deploy watcher to re-apply (Greptile P1 follow-up on PR #550). A
// non-empty directory at the manifest path makes os.Remove fail with
// ENOTEMPTY deterministically on every platform.
func TestStageSkipRemovalFailureFailsStaging(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "sandbox-matrix", "e2b-gateway.yaml")
	if err := os.MkdirAll(filepath.Join(stale, "sub"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := Stage(dir, map[string]string{}, map[string]bool{
		"sandbox-matrix/e2b-gateway.yaml": true,
	}); err == nil {
		t.Fatalf("Stage must fail when the stale copy cannot be removed")
	}
}
