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

// signClientCertFixture issues a client cert the way Login does, returns both
// the parsed leaf and the CA that signed it.
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
		IsCA:                   true,
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

// TestSignClientCert_NotBeforeSurvivesClockSkew is the regression test for a
// `k8e-sandbox-cli connect` failure against a remote host:
//
//	x509: certificate has expired or is not yet valid:
//	      current time 2026-10-19T14:11:24+08:00 is before 2026-10-19T06:11:26Z
//
// Signing a client cert with NotBefore == time.Now() truncates up to the next
// whole second (ASN.1 UTCTime has second precision), so the credential is valid
// only from the top of the next second. The issuing server's clock was two
// seconds ahead of the client's, and validateIssuedCertificate verifies at the
// client's CurrentTime, so the fresh credential was "not yet valid" the instant
// it arrived. Because the error names a clock skew the operator cannot see or
// fix, it reads as a broken CA rather than a sub-second tolerance bug.
//
// A backdated NotBefore covers skew in both directions; a client up to an hour
// behind the server must be accepted, and the configured TTL must survive.
func TestSignClientCert_NotBeforeSurvivesClockSkew(t *testing.T) {
	leaf, ca := signClientCertFixture(t)
	pool := x509.NewCertPool()
	pool.AddCert(ca)

	// A client whose clock lags the server must still accept the credential.
	// real skew, which is what broke the observed handshake.
	const lag = 5 * time.Minute
	now := time.Now().Add(-lag)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("client clock %s behind the server rejects the credential: %v", lag, err)
	}

	// And a clock ahead of the server is equally fine.
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:       pool,
		CurrentTime: time.Now().Add(lag),
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Fatalf("client clock %s ahead of the server rejects the credential: %v", lag, err)
	}

	// The backdating must not eat into the TTL the operator asked for.
	if want := time.Now().Add(29 * 24 * time.Hour); !leaf.NotAfter.After(want) {
		t.Fatalf("TTL shortened by the backdating: notAfter=%s want after %s",
			leaf.NotAfter.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}
