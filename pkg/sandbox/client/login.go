package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"errors"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

// This file is the out-of-process entry into the mTLS bootstrap handshake:
// everything else in the package assumes the caller caches credentials in
// this package's on-disk directory. KIP-30 M2 (the k8e-mcp adapter) caches
// the issued identity in a Kubernetes ConfigMap instead and drives the same
// Login RPC through these exports, so client-side and in-cluster enrollment
// cannot drift apart.

// LoginOptions controls one bootstrap Login RPC for callers that do not use
// this package's on-disk credential cache.
type LoginOptions struct {
	// APIKey authorizes enrollment and is sent as an Authorization: Bearer
	// header. One Login call consumes it once; the key is never wrapped into
	// any returned material.
	APIKey string
	// CAPEM pins the gateway's serving CA. When empty, the system roots are
	// used, which cannot verify the sandbox CA — the handshake then fails
	// unless Insecure is set.
	CAPEM []byte
	// Insecure explicitly opts into trust-on-first-use: it sends the API key
	// over a TLS connection whose server certificate is not verified first.
	// Mirrors the CLI's --insecure-bootstrap; a warning belongs on whoever
	// sets it, not here.
	Insecure bool
	// DeviceName is the audit label the server records in its issued-certificate
	// ledger. Empty falls back to the K8E_SANDBOX_DEVICE_NAME env / hostname,
	// same as the CLI.
	DeviceName string
}

// Login submits csr to the gateway and returns the signed client certificate
// and the sandbox CA. The response must be validated with
// ValidateIssuedCertificate before the material is trusted, exactly like the
// CLI path.
//
// On the loopback endpoints the gateway's serving certificate cannot match
// "127.0.0.1", so hostname verification is traded for full chain validation
// against CAPEM (see loopbackTLSConfig); an attacker without the CA still
// cannot complete the handshake.
func Login(ctx context.Context, endpoint, csr string, opts LoginOptions) (*pb.LoginResponse, error) {
	if opts.APIKey == "" {
		return nil, errors.New("sandbox client: login requires an API key")
	}
	if csr == "" {
		return nil, errors.New("sandbox client: login requires a CSR")
	}
	cfg, err := loginTLSConfigFromBytes(endpoint, opts.CAPEM, opts.Insecure)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(endpoint, append(dialOpts(), grpc.WithTransportCredentials(credentials.NewTLS(cfg)))...)
	if err != nil {
		return nil, dialErr(endpoint, err)
	}
	defer conn.Close()
	requestCtx := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+opts.APIKey)
	return pb.NewSandboxServiceClient(conn).Login(requestCtx, loginRequestWithDevice(csr, opts.DeviceName))
}

// CreateCSR returns a PEM-encoded certificate request for key, the exact
// request shape every K8E client submits.
func CreateCSR(key *ecdsa.PrivateKey) (string, error) { return createCSR(key) }

// ValidateIssuedCertificate verifies the Login response before it is cached:
// the issued certificate must pair with the caller's private key and chain to
// the returned CA for client authentication.
func ValidateIssuedCertificate(resp *pb.LoginResponse, keyPEM []byte) error {
	return validateIssuedCertificate(resp, keyPEM)
}

// loginTLSConfigFromBytes is loginTLSConfig with the trust anchor in memory
// instead of a file path, so callers never have to stage a temp file.
func loginTLSConfigFromBytes(endpoint string, caPEM []byte, insecure bool) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if len(caPEM) == 0 {
		// System roots cannot verify the sandbox CA, so the caller must have
		// opted into trust-on-first-use to reach the API-key exchange at all.
		cfg.InsecureSkipVerify = insecure //nolint:gosec // explicit bootstrap opt-in
		return cfg, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("sandbox client: parse CA certificate")
	}
	if isLoopback(endpoint) {
		return loopbackTLSConfig(pool), nil
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// loginRequestWithDevice is loginRequest with an explicit device-name label
// for non-CLI callers (the adapter reports "k8e-mcp"); empty keeps the
// env/hostname default so both paths report the same shape.
func loginRequestWithDevice(csr, device string) *pb.LoginRequest {
	request := loginRequest(csr)
	if device != "" {
		request.DeviceName = device
	}
	return request
}
