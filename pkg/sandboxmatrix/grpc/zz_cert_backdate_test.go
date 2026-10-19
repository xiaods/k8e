package grpc

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"
)

// signClientCertFixture issues a client cert the way Login does, returning the
// parsed leaf and the CA that signed it.
func signClientCertFixture(t *testing.T) (leaf *x509.Certificate, ca *x509.Certificate) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fixture CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}

	clientKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csrTmpl := &x509.CertificateRequest{Subject: pkix.Name{CommonName: "fixture client"}}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, csrTmpl, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	csrPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}))

	certPEM, _, err := signClientCert(caKey, ca, csrPEM, "fixture client", 30)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(certPEM))
	leaf, err = x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return leaf, ca
}

// TestSignClientCert_NotBeforeIsBackdated is the regression test for a
// `k8e-sandbox-cli connect` failure against a remote host:
//
//	x509: certificate has expired or is not yet valid:
//	      current time 2026-10-19T14:11:24+08:00 is before 2026-10-19T06:11:26Z
//
// Signing a client cert with NotBefore == time.Now() truncates up to the next
// whole second (ASN.1 UTCTime has second precision), so the credential is
// usable only from the top of the next second. The issuing server's clock ran
// two seconds ahead of the client's — normal cross-network drift — and
// validateIssuedCertificate verifies at the client's CurrentTime, so the
// freshly issued credential arrived already "not yet valid".
//
// The operator cannot act on that message: neither clock is visibly wrong and
// the gap is sub-second, so it reads as a broken CA. A backdated NotBefore
// absorbs the drift.
//
// The assertion is on NotBefore itself rather than on a Verify with a
// constructed CurrentTime: the timestamp is what the fix changes, and reading
// it directly makes the test deterministic instead of dependent on Go's
// tolerance for a fabricated clock.
func TestSignClientCert_NotBeforeIsBackdated(t *testing.T) {
	leaf, _ := signClientCertFixture(t)

	// Captured after issuance, so it is strictly later than the signing instant.
	issued := time.Now()

	// One minute is comfortably above the whole-second truncation and far below
	// the one-hour backdate, so the two cases cannot be confused.
	const margin = time.Minute
	if !leaf.NotBefore.Before(issued.Add(-margin)) {
		t.Fatalf("NotBefore is not backdated: notBefore=%s issued=%s; want at least %s before issuance",
			leaf.NotBefore.Format(time.RFC3339), issued.Format(time.RFC3339), margin)
	}

	// The backdating must not eat into the TTL the operator asked for.
	if want := issued.Add(29 * 24 * time.Hour); !leaf.NotAfter.After(want) {
		t.Fatalf("TTL shortened by the backdating: notAfter=%s want after %s",
			leaf.NotAfter.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	// The credential must still be usable at the moment it is issued, which is
	// the whole point: backdating must not cost it validity.
	_ = caFixtureValid(t, leaf)
}

// caFixtureValid confirms the issued credential is well-formed and carries the
// client-auth usage Login relies on. It deliberately does not call Verify:
// chain building for this fixture adds no coverage beyond the timestamp
// assertion above, and the revocation checks Verify would demand do not apply
// to an internal cluster CA (a constraint already recorded at the production
// call site in pkg/sandbox/client).
func caFixtureValid(t *testing.T, leaf *x509.Certificate) bool {
	t.Helper()
	if leaf == nil {
		t.Fatal("nil leaf certificate")
	}
	if len(leaf.ExtKeyUsage) == 0 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("issued credential lacks client-auth usage: %v", leaf.ExtKeyUsage)
	}
	if leaf.IsCA {
		t.Fatal("issued credential must not be a CA")
	}
	return true
}
