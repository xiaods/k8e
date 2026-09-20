package netsy

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestEnsurePKI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	hosts := []string{"127.0.0.1", "::1", "localhost", "k8e-node"}

	paths, err := EnsurePKI(dir, "k8e", "k8e-node", DefaultClientName, hosts)
	if err != nil {
		t.Fatalf("EnsurePKI() error = %v", err)
	}

	assertPKIFilesExist(t, paths)
	assertCAUsable(t, paths.CA)
	// Every leaf must chain to the CA; the k8e datastore client must carry the
	// `client` role and be usable for a TLS handshake.
	assertKeyPairsLoad(t, paths)
	assertDatastoreClientCert(t, paths)
	assertServerCert(t, paths, hosts)
	// Key files must not be world readable.
	assertFileMode(t, paths.ServerKey, 0600)
}

func assertPKIFilesExist(t *testing.T, paths CertPaths) {
	t.Helper()
	for _, f := range []string{paths.CA, paths.ServerCert, paths.ServerKey,
		paths.PeerClientCert, paths.PeerClientKey, paths.DatastoreCert, paths.DatastoreKey} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("expected %s to exist: %v", f, err)
		}
	}
}

func assertCAUsable(t *testing.T, caFile string) {
	t.Helper()
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("failed to add CA %s to pool", caFile)
	}
}

func assertKeyPairsLoad(t *testing.T, paths CertPaths) {
	t.Helper()
	for _, pair := range [][2]string{
		{paths.ServerCert, paths.ServerKey},
		{paths.PeerClientCert, paths.PeerClientKey},
		{paths.DatastoreCert, paths.DatastoreKey},
	} {
		if _, err := tls.LoadX509KeyPair(pair[0], pair[1]); err != nil {
			t.Errorf("LoadX509KeyPair(%s) error = %v", pair[0], err)
		}
	}
}

func assertDatastoreClientCert(t *testing.T, paths CertPaths) {
	t.Helper()
	leaf, err := loadLeaf(paths.DatastoreCert, paths.DatastoreKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkLeaf(leaf, "k8e", roleClient, DefaultClientName); err != nil {
		t.Errorf("datastore client cert invalid: %v", err)
	}
	if leaf.Subject.CommonName != DefaultClientName {
		t.Errorf("datastore client CN = %q, want %q", leaf.Subject.CommonName, DefaultClientName)
	}
	if len(leaf.ExtKeyUsage) == 0 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Errorf("datastore client ExtKeyUsage = %v, want ClientAuth", leaf.ExtKeyUsage)
	}
}

func assertServerCert(t *testing.T, paths CertPaths, hosts []string) {
	t.Helper()
	server, err := loadLeaf(paths.ServerCert, paths.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, host := range hosts {
		if err := server.VerifyHostname(host); err != nil {
			t.Errorf("server cert does not cover %q: %v", host, err)
		}
	}
	if err := checkLeaf(server, "k8e", rolePeer, "k8e-node"); err != nil {
		t.Errorf("server cert invalid: %v", err)
	}
}

func assertFileMode(t *testing.T, file string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("%s mode = %v, want %v", file, info.Mode().Perm(), want)
	}
}

func TestEnsurePKIReusesExisting(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	hosts := []string{"127.0.0.1", "localhost"}

	first, err := EnsurePKI(dir, "k8e", "k8e", DefaultClientName, hosts)
	if err != nil {
		t.Fatalf("EnsurePKI() error = %v", err)
	}
	caBefore, err := os.ReadFile(first.CA)
	if err != nil {
		t.Fatal(err)
	}

	second, err := EnsurePKI(dir, "k8e", "k8e", DefaultClientName, hosts)
	if err != nil {
		t.Fatalf("EnsurePKI() second call error = %v", err)
	}
	caAfter, err := os.ReadFile(second.CA)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(caBefore, caAfter) {
		t.Error("EnsurePKI() regenerated a valid PKI instead of reusing it")
	}
}

func TestEnsurePKIRegeneratesOnMismatch(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	hosts := []string{"127.0.0.1"}

	first, err := EnsurePKI(dir, "cluster-a", "node-a", DefaultClientName, hosts)
	if err != nil {
		t.Fatalf("EnsurePKI() error = %v", err)
	}
	caBefore, err := os.ReadFile(first.CA)
	if err != nil {
		t.Fatal(err)
	}

	// A different cluster/node must not silently reuse the old certificates.
	second, err := EnsurePKI(dir, "cluster-b", "node-b", DefaultClientName, hosts)
	if err != nil {
		t.Fatalf("EnsurePKI() error = %v", err)
	}
	caAfter, err := os.ReadFile(second.CA)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(caBefore, caAfter) {
		t.Error("EnsurePKI() reused a PKI issued for a different cluster")
	}
	leaf, err := loadLeaf(second.DatastoreCert, second.DatastoreKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkLeaf(leaf, "cluster-b", roleClient, DefaultClientName); err != nil {
		t.Errorf("regenerated datastore cert invalid: %v", err)
	}
}

func TestEnsurePKIFailsOnUnwritableDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores directory permissions")
	}
	parent := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(parent, 0500); err != nil {
		t.Fatal(err)
	}
	// Restore owner write so t.TempDir cleanup can remove the read-only dir.
	t.Cleanup(func() { _ = os.Chmod(parent, 0600) })

	dir := filepath.Join(parent, "tls")
	if _, err := EnsurePKI(dir, "k8e", "k8e", DefaultClientName, []string{"127.0.0.1"}); err == nil {
		t.Fatal("EnsurePKI() error = nil, want permission error")
	}
}

func TestValidPKIRejectsCorruptCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	paths, err := EnsurePKI(dir, "k8e", "k8e", DefaultClientName, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.CA, []byte("not a certificate"), 0644); err != nil {
		t.Fatal(err)
	}
	if validPKI(paths, "k8e", "k8e", []string{"127.0.0.1"}) {
		t.Error("validPKI() = true, want false for a corrupt CA")
	}
	if _, err := EnsurePKI(dir, "k8e", "k8e", DefaultClientName, []string{"127.0.0.1"}); err != nil {
		t.Fatalf("EnsurePKI() failed to recover from corrupt CA: %v", err)
	}
}

func TestCheckLeafRejectsWrongRoleAndCluster(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	paths, err := EnsurePKI(dir, "k8e", "k8e", DefaultClientName, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	client, err := loadLeaf(paths.DatastoreCert, paths.DatastoreKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkLeaf(client, "k8e", rolePeer, DefaultClientName); err == nil {
		t.Error("checkLeaf() accepted a client cert in the peer role")
	}
	if err := checkLeaf(client, "other", roleClient, DefaultClientName); err == nil {
		t.Error("checkLeaf() accepted a cert from another cluster")
	}
	if err := checkLeaf(client, "k8e", roleClient, "other"); err == nil {
		t.Error("checkLeaf() accepted a cert for another identity")
	}
}

func TestValidPKIRejectsIncompleteOrWrongPKI(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, p CertPaths)
	}{
		{name: "missing files", mutate: func(_ *testing.T, p CertPaths) { _ = os.Remove(p.CA) }},
		{name: "server cert missing", mutate: func(_ *testing.T, p CertPaths) { _ = os.Remove(p.ServerCert) }},
		{name: "server key missing", mutate: func(_ *testing.T, p CertPaths) { _ = os.Remove(p.ServerKey) }},
		{name: "peer cert missing", mutate: func(_ *testing.T, p CertPaths) { _ = os.Remove(p.PeerClientCert) }},
		{name: "datastore cert missing", mutate: func(_ *testing.T, p CertPaths) { _ = os.Remove(p.DatastoreCert) }},
		{name: "server cert does not cover host", mutate: func(t *testing.T, p CertPaths) {
			newPaths, err := EnsurePKI(filepath.Join(t.TempDir(), "tls"), "k8e", "k8e", DefaultClientName, []string{"10.0.0.9"})
			if err != nil {
				t.Fatal(err)
			}
			copyFile(t, newPaths.ServerCert, p.ServerCert)
			copyFile(t, newPaths.ServerKey, p.ServerKey)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := EnsurePKI(filepath.Join(t.TempDir(), "tls"), "k8e", "k8e", DefaultClientName, []string{"127.0.0.1"})
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(t, p)
			if validPKI(p, "k8e", "k8e", []string{"127.0.0.1"}) {
				t.Errorf("validPKI() = true for %s, want false", tt.name)
			}
		})
	}
}

func copyFile(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestEnsurePKIRecoversFromMixedCAGeneration(t *testing.T) {
	hosts := []string{"127.0.0.1"}
	dir := filepath.Join(t.TempDir(), "tls")
	first, err := EnsurePKI(dir, "k8e", "k8e", DefaultClientName, hosts)
	if err != nil {
		t.Fatalf("EnsurePKI() error = %v", err)
	}
	caBefore, err := os.ReadFile(first.CA)
	if err != nil {
		t.Fatal(err)
	}

	// A regeneration interrupted after the new CA was published leaves a new CA
	// next to the previous leaves. They still parse, still cover the hosts and
	// still carry the right roles, so only the signature check can reject them.
	other, err := EnsurePKI(filepath.Join(t.TempDir(), "tls"), "k8e", "k8e", DefaultClientName, hosts)
	if err != nil {
		t.Fatal(err)
	}
	copyFile(t, other.CA, first.CA)
	if validPKI(first, "k8e", "k8e", hosts) {
		t.Error("validPKI() = true for a new CA with the previous leaves")
	}

	// EnsurePKI must publish a consistent generation instead of reusing it.
	second, err := EnsurePKI(dir, "k8e", "k8e", DefaultClientName, hosts)
	if err != nil {
		t.Fatalf("EnsurePKI() failed to recover from a mixed generation: %v", err)
	}
	caAfter, err := os.ReadFile(second.CA)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(caBefore, caAfter) {
		t.Error("EnsurePKI() reused the mixed CA/leaf generation")
	}
	if !validPKI(second, "k8e", "k8e", hosts) {
		t.Error("EnsurePKI() did not publish a consistent PKI after the interrupted generation")
	}
}

func TestEnsurePKIRenewsInsideRenewalWindow(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	hosts := []string{"127.0.0.1"}
	old := leafValidity
	t.Cleanup(func() { leafValidity = old })
	// Leaves that expire inside the renewal window are complete and consistent,
	// so only the window can reject them.
	leafValidity = certRenewBefore - 24*time.Hour

	first, err := EnsurePKI(dir, "k8e", "k8e", DefaultClientName, hosts)
	if err != nil {
		t.Fatalf("EnsurePKI() error = %v", err)
	}
	caBefore, err := os.ReadFile(first.CA)
	if err != nil {
		t.Fatal(err)
	}
	if validPKI(first, "k8e", "k8e", hosts) {
		t.Error("validPKI() = true for certificates inside the renewal window")
	}

	second, err := EnsurePKI(dir, "k8e", "k8e", DefaultClientName, hosts)
	if err != nil {
		t.Fatalf("EnsurePKI() error = %v", err)
	}
	caAfter, err := os.ReadFile(second.CA)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(caBefore, caAfter) {
		t.Error("EnsurePKI() reused certificates that expire inside the renewal window")
	}
}

func TestLoadLeafErrors(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "bad.crt")
	keyFile := filepath.Join(dir, "bad.key")
	if err := os.WriteFile(certFile, []byte("not a cert"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte("not a key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadLeaf(certFile, keyFile); err == nil {
		t.Error("loadLeaf() = nil, want parse error")
	}
}

func TestCheckLeafURIFiltering(t *testing.T) {
	other := &x509.Certificate{
		Subject: pkix.Name{Organization: []string{"k8e"}},
		URIs: []*url.URL{
			{Scheme: "spiffe", Host: "k8e", Path: "/client/k8e"},
			{Scheme: "netsy", Host: "other", Path: "/client/k8e"},
		},
	}
	if err := checkLeaf(other, "k8e", roleClient, DefaultClientName); err == nil {
		t.Error("checkLeaf() accepted a cert with only foreign URI SANs")
	}
	noOrg := &x509.Certificate{URIs: []*url.URL{{Scheme: "netsy", Host: "k8e", Path: "/client/k8e"}}}
	if err := checkLeaf(noOrg, "k8e", roleClient, DefaultClientName); err == nil {
		t.Error("checkLeaf() accepted a cert with no organization")
	}
}

func TestAllowsUsage(t *testing.T) {
	clientOnly := &x509.Certificate{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	if allowsUsage(clientOnly, x509.ExtKeyUsageServerAuth) {
		t.Error("allowsUsage() = true for a client-auth leaf used for server auth")
	}
	if !allowsUsage(clientOnly, x509.ExtKeyUsageClientAuth) {
		t.Error("allowsUsage() = false for the usage of the leaf")
	}
	// Go treats a leaf without extended key usages as unrestricted.
	if !allowsUsage(&x509.Certificate{}, x509.ExtKeyUsageServerAuth) {
		t.Error("allowsUsage() = false for a leaf without extended key usages")
	}
	any := &x509.Certificate{ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}}
	if !allowsUsage(any, x509.ExtKeyUsageServerAuth) {
		t.Error("allowsUsage() = false for a leaf with the any usage")
	}
}
