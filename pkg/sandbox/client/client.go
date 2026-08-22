package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"github.com/xiaods/k8e/pkg/version"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultEndpoint = "127.0.0.1:50051"

// maxCallRecvMsgSize / maxCallSendMsgSize raise the gRPC default 4MiB message
// limit on the client side. Snapshot restore, file reads, and background run
// results routinely exceed 4MiB (see KIP-16 M7); the gateway server already
// raises its own limit to 64MiB.
const maxCallRecvMsgSize = 64 * 1024 * 1024
const maxCallSendMsgSize = 64 * 1024 * 1024

// dialOpts returns the transport + message-size options shared by every client
// dial site in this package.
func dialOpts() []grpc.DialOption {
	return []grpc.DialOption{
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(maxCallRecvMsgSize),
			grpc.MaxCallSendMsgSize(maxCallSendMsgSize),
		),
	}
}

var tlsCandidates = []string{
	// The sandbox gRPC gateway's server cert is signed by the dedicated sandbox
	// CA (KIP-14, /var/lib/k8e/server/tls/sandbox-ca.crt), not the apiserver
	// serving cert. Loopback/local clients — the embedded e2b server and
	// k8e-sandbox-cli local mode — must trust the sandbox CA to complete the
	// mTLS handshake; without it the dial fails with
	// "x509: certificate signed by unknown authority". The apiserver serving
	// certs are kept as fallback trust anchors for legacy paths.
	"/var/lib/k8e/server/tls/sandbox-ca.crt",
	"/var/lib/k8e/server/tls/serving-kube-apiserver.crt",
	"/etc/k8e/tls/serving-kube-apiserver.crt",
}

var kubeconfigCandidates = []string{
	"/etc/k8e/k8e.yaml",
	"/var/lib/k8e/server/cred/admin.kubeconfig",
}

func resolvedKubeconfigCandidates() []string {
	candidates := make([]string, 0, len(kubeconfigCandidates)+2)
	if kc := os.Getenv("KUBECONFIG"); kc != "" {
		candidates = append(candidates, kc)
	}
	if home := os.Getenv("HOME"); home != "" {
		candidates = append(candidates, home+"/.kube/config")
	}
	return append(candidates, kubeconfigCandidates...)
}

// Client wraps a gRPC SandboxServiceClient with its underlying connection.
type Client struct {
	SandboxServiceClient pb.SandboxServiceClient
	conn                 *grpc.ClientConn
}

// NewClient auto-discovers the local K8E TLS cert and connects to the sandbox gRPC gateway.
// Override with K8E_SANDBOX_ENDPOINT, K8E_SANDBOX_CERT env vars.
func NewClient() (*Client, error) {
	endpoint := os.Getenv("K8E_SANDBOX_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return newLocalClient(endpoint)
}

func newLocalClient(endpoint string) (*Client, error) {
	creds, err := resolveCreds(endpoint)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: tls: %w", err)
	}
	conn, err := grpc.NewClient(endpoint, append(dialOpts(), grpc.WithTransportCredentials(creds))...)
	if err != nil {
		return nil, dialErr(endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

// Client cert lifetime and lazy-renewal window.
// Server issues 90-day certs; clients renew when fewer than 30 days remain so
// the Login RPC is not paid on every dial (issue #538).
const (
	clientCertRenewalDays = 30
	loginTimeout          = 15 * time.Second
	endpointStampFile     = "endpoint"
)

// NewClientWithEndpoint connects to a remote K8E cluster at endpoint.
// On first use, performs mTLS bootstrap: generates a key pair, logs in with the API key,
// and obtains a short-lived client certificate. Subsequent calls use the cached certificate
// with automatic lazy renewal.
//
// When apiKey is empty, K8E_SANDBOX_APIKEY is used if set (agent/CI convenience).
func NewClientWithEndpoint(endpoint, apiKey string) (*Client, error) {
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("K8E_SANDBOX_APIKEY"))
	}
	if apiKey == "" {
		if endpoint == "" {
			return NewClient()
		}
		if isLoopback(endpoint) {
			return newLocalClient(endpoint)
		}
		return newClientWithCachedCerts(endpoint)
	}
	cacheDir, err := sandboxCacheDir()
	if err != nil {
		return nil, fmt.Errorf("sandbox client: resolve cache dir: %w", err)
	}
	caFile := filepath.Join(cacheDir, "ca.crt")
	certFile := filepath.Join(cacheDir, "client.crt")
	keyFile := filepath.Join(cacheDir, "client.key")

	// Path 1: have CA + valid client cert for this endpoint → direct mTLS
	if _, caErr := os.Stat(caFile); caErr == nil {
		if err := checkEndpointStamp(cacheDir, endpoint); err != nil {
			// Wrong endpoint's material: re-bootstrap with the provided API key.
			return bootstrapInsecure(endpoint, caFile, certFile, keyFile, apiKey)
		}
		st := inspectClientCert(certFile)
		if st.valid {
			// Lazy renewal: single cert parse already decided expiringSoon.
			if st.expiringSoon {
				renewClientCert(endpoint, caFile, certFile, keyFile)
			}
			conn, err := dialMTLS(endpoint, caFile, certFile, keyFile)
			if err != nil {
				return nil, dialErr(endpoint, err)
			}
			return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
		}

		// Path 2: have CA but cert expired/missing → TLS + Login with API key
		return bootstrapWithCA(endpoint, caFile, certFile, keyFile, apiKey)
	}

	// Path 3: no CA → insecure bootstrap, get both CA and client cert
	return bootstrapInsecure(endpoint, caFile, certFile, keyFile, apiKey)
}

// newClientWithCachedCerts reuses the mTLS certificate material stored by a
// previous login/connect for the same remote endpoint.
func newClientWithCachedCerts(endpoint string) (*Client, error) {
	cacheDir, err := sandboxCacheDir()
	if err != nil {
		return nil, fmt.Errorf("sandbox client: resolve cache dir: %w", err)
	}
	caFile := filepath.Join(cacheDir, "ca.crt")
	certFile := filepath.Join(cacheDir, "client.crt")
	keyFile := filepath.Join(cacheDir, "client.key")

	if _, err := os.Stat(caFile); err != nil {
		return nil, fmt.Errorf("sandbox client: no cached CA for %s; run connect or login with --apikey", endpoint)
	}
	if err := checkEndpointStamp(cacheDir, endpoint); err != nil {
		return nil, err
	}
	st := inspectClientCert(certFile)
	if !st.valid {
		return nil, fmt.Errorf("sandbox client: cached client certificate for %s is missing or expired; reconnect with --apikey", endpoint)
	}
	if st.expiringSoon {
		renewClientCert(endpoint, caFile, certFile, keyFile)
	}

	conn, err := dialMTLS(endpoint, caFile, certFile, keyFile)
	if err != nil {
		return nil, dialErr(endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// ── mTLS bootstrap helpers ────────────────────────────────────────────────────

func bootstrapWithCA(endpoint, caFile, certFile, keyFile, apiKey string) (*Client, error) {
	key, err := loadOrGenerateKey(keyFile)
	if err != nil {
		return nil, err
	}
	csr, err := createCSR(key)
	if err != nil {
		return nil, err
	}
	resp, err := callLogin(endpoint, caFile, apiKey, csr)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: login: %w", err)
	}
	if err := atomicWriteFile(certFile, []byte(resp.Cert), 0644); err != nil {
		return nil, fmt.Errorf("sandbox client: save cert: %w", err)
	}
	if err := writeEndpointStamp(filepath.Dir(caFile), endpoint); err != nil {
		return nil, fmt.Errorf("sandbox client: save endpoint stamp: %w", err)
	}

	conn, err := dialMTLS(endpoint, caFile, certFile, keyFile)
	if err != nil {
		return nil, dialErr(endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

func bootstrapInsecure(endpoint, caFile, certFile, keyFile, apiKey string) (*Client, error) {
	key, err := loadOrGenerateKey(keyFile)
	if err != nil {
		return nil, err
	}
	csr, err := createCSR(key)
	if err != nil {
		return nil, err
	}
	// empty caFile → InsecureSkipVerify for bootstrap
	resp, err := callLogin(endpoint, "", apiKey, csr)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: bootstrap login: %w", err)
	}
	if err := atomicWriteFile(caFile, []byte(resp.CaCert), 0644); err != nil {
		return nil, fmt.Errorf("sandbox client: save CA: %w", err)
	}
	if err := atomicWriteFile(certFile, []byte(resp.Cert), 0644); err != nil {
		return nil, fmt.Errorf("sandbox client: save cert: %w", err)
	}
	if err := writeEndpointStamp(filepath.Dir(caFile), endpoint); err != nil {
		return nil, fmt.Errorf("sandbox client: save endpoint stamp: %w", err)
	}

	conn, err := dialMTLS(endpoint, caFile, certFile, keyFile)
	if err != nil {
		return nil, dialErr(endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

// renewClientCert attempts to renew the client certificate via mTLS.
// Failures are silent — the existing (still-valid) cert continues to work.
func renewClientCert(endpoint, caFile, certFile, keyFile string) {
	key, err := loadClientKey(keyFile)
	if err != nil {
		return
	}
	csr, err := createCSR(key)
	if err != nil {
		return
	}
	resp, err := callLoginMTLS(endpoint, caFile, certFile, keyFile, csr)
	if err != nil {
		return
	}
	if err := atomicWriteFile(certFile, []byte(resp.Cert), 0644); err != nil {
		return
	}
	// Backfill endpoint stamp for pre-stamp installs and after successful renew.
	_ = writeEndpointStamp(filepath.Dir(caFile), endpoint)
}

// ── Key & CSR generation ──────────────────────────────────────────────────────

// loadOrGenerateKey reuses an existing client key when present so re-login after
// cert expiry does not pay for a new ECDSA keygen or invalidate other cached material.
func loadOrGenerateKey(keyFile string) (*ecdsa.PrivateKey, error) {
	if key, err := loadClientKey(keyFile); err == nil {
		return key, nil
	}
	return generateAndSaveKey(keyFile)
}

func generateAndSaveKey(keyFile string) (*ecdsa.PrivateKey, error) {
	if err := os.MkdirAll(filepath.Dir(keyFile), 0700); err != nil {
		return nil, fmt.Errorf("sandbox client: create cache dir: %w", err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: generate key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: marshal key: %w", err)
	}
	if err := atomicWriteFile(keyFile, pem.EncodeToMemory(&pem.Block{
		Type: "EC PRIVATE KEY", Bytes: der,
	}), 0600); err != nil {
		return nil, fmt.Errorf("sandbox client: save key: %w", err)
	}
	return key, nil
}

// atomicWriteFile writes data via temp file + rename so concurrent CLI processes
// never observe a half-written cert/key (issue #538).
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// Best-effort cleanup if rename never happens.
	defer os.Remove(tmp) //nolint:errcheck
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func createCSR(key *ecdsa.PrivateKey) (string, error) {
	tmpl := &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "sandbox-client"},
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, tmpl, key)
	if err != nil {
		return "", fmt.Errorf("sandbox client: create CSR: %w", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE REQUEST", Bytes: der,
	})), nil
}

func loadClientKey(keyFile string) (*ecdsa.PrivateKey, error) {
	data, err := os.ReadFile(keyFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid key PEM")
	}
	return x509.ParseECPrivateKey(block.Bytes)
}

// ── Certificate validation ────────────────────────────────────────────────────

// certStatus is the result of a single PEM load+parse for the hot dial path.
type certStatus struct {
	valid        bool
	expiringSoon bool
}

// inspectClientCert loads and parses the client cert once, deciding both
// validity and whether lazy renewal should run. Avoids double disk+parse work
// that the previous certValid + certExpiringSoon pair paid on every dial.
func inspectClientCert(certFile string) certStatus {
	cert, err := loadAndParseCert(certFile)
	if err != nil {
		return certStatus{}
	}
	now := time.Now()
	if !now.After(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return certStatus{}
	}
	renewAfter := cert.NotAfter.Add(-time.Duration(clientCertRenewalDays) * 24 * time.Hour)
	return certStatus{
		valid:        true,
		expiringSoon: now.After(renewAfter) || now.Equal(renewAfter),
	}
}

func loadAndParseCert(certFile string) (*x509.Certificate, error) {
	data, err := os.ReadFile(certFile)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid cert PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}

// ── Connection helpers ────────────────────────────────────────────────────────

// mtlsMaterial holds parsed CA pool + client leaf for one dial (avoids re-reading
// the same PEM files across helper layers on a single connect path).
type mtlsMaterial struct {
	pool       *x509.CertPool
	clientCert tls.Certificate
}

func loadMTLSMaterial(caFile, certFile, keyFile string) (*mtlsMaterial, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse CA cert")
	}
	clientCert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load client cert: %w", err)
	}
	return &mtlsMaterial{pool: pool, clientCert: clientCert}, nil
}

func dialMTLS(endpoint, caFile, certFile, keyFile string) (*grpc.ClientConn, error) {
	mat, err := loadMTLSMaterial(caFile, certFile, keyFile)
	if err != nil {
		return nil, err
	}
	return dialMTLSMaterial(endpoint, mat)
}

func dialMTLSMaterial(endpoint string, mat *mtlsMaterial) (*grpc.ClientConn, error) {
	var creds credentials.TransportCredentials
	if isLoopback(endpoint) {
		creds = credentials.NewTLS(loopbackTLSConfig(mat.pool, mat.clientCert))
	} else {
		creds = credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{mat.clientCert},
			RootCAs:      mat.pool,
			MinVersion:   tls.VersionTLS12,
		})
	}
	return grpc.NewClient(endpoint, append(dialOpts(), grpc.WithTransportCredentials(creds))...)
}

func isLoopback(endpoint string) bool {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}

// loopbackTLSConfig returns a TLS config that verifies the server's certificate
// chain against pool while skipping hostname verification. On loopback the server
// cert's CN/SAN won't match "127.0.0.1", so hostname check is unavoidable.
// The VerifyConnection callback still validates the full cert chain against pool.
func loopbackTLSConfig(pool *x509.CertPool, clientCerts ...tls.Certificate) *tls.Config { // NOSONAR: ssl:S4830 — loopback; full cert chain validated in VerifyConnection
	cfg := &tls.Config{ // NOSONAR
		RootCAs:            pool,
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: true, // NOSONAR: ssl:S4830 — loopback; cert chain validated in VerifyConnection
		VerifyConnection: func(cs tls.ConnectionState) error { // NOSONAR: GO-S1031 — loopback connection uses internal cluster CA; CRL/OCSP infrastructure not applicable
			opts := x509.VerifyOptions{
				Roots:         pool,
				Intermediates: x509.NewCertPool(),
			}
			for _, cert := range cs.PeerCertificates[1:] {
				opts.Intermediates.AddCert(cert)
			}
			_, err := cs.PeerCertificates[0].Verify(opts)
			return err
		},
	}
	if len(clientCerts) > 0 {
		cfg.Certificates = clientCerts
	}
	return cfg
}

func callLogin(endpoint, caFile, apiKey, csr string) (*pb.LoginResponse, error) {
	var creds credentials.TransportCredentials
	if caFile == "" {
		// Bootstrap: no cached CA, server verification is impossible. The
		// connection is authenticated by the API key in gRPC metadata. A MITM
		// that intercepts this single Login call gains only a short-lived client
		// certificate, useless for future mTLS connections that verify the CA.
		//nolint:gosec
		creds = credentials.NewTLS(&tls.Config{ // NOSONAR
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: true, // NOSONAR: ssl:S4830 — bootstrap secured by API key auth
		})
	} else {
		caPEM, err := os.ReadFile(caFile)
		if err != nil {
			return nil, fmt.Errorf("read CA: %w", err)
		}
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caPEM)
		creds = credentials.NewTLS(&tls.Config{
			RootCAs:            pool,
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: isLoopback(endpoint),
		})
	}

	conn, err := grpc.NewClient(endpoint, append(dialOpts(), grpc.WithTransportCredentials(creds))...)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx,
		"authorization", "Bearer "+apiKey,
	)
	return pb.NewSandboxServiceClient(conn).Login(ctx, loginRequest(csr))
}

func callLoginMTLS(endpoint, caFile, certFile, keyFile, csr string) (*pb.LoginResponse, error) {
	conn, err := dialMTLS(endpoint, caFile, certFile, keyFile)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()
	return pb.NewSandboxServiceClient(conn).Login(ctx, loginRequest(csr))
}

// loginRequest builds the Login RPC body with CSR plus audit fields.
// Device name priority: K8E_SANDBOX_DEVICE_NAME → hostname (empty if unknown).
func loginRequest(csr string) *pb.LoginRequest {
	return &pb.LoginRequest{
		Csr:           csr,
		DeviceName:    loginDeviceName(),
		ClientVersion: version.Version,
	}
}

func loginDeviceName() string {
	if n := strings.TrimSpace(os.Getenv("K8E_SANDBOX_DEVICE_NAME")); n != "" {
		return n
	}
	host, err := os.Hostname()
	if err != nil {
		return ""
	}
	return host
}

// writeEndpointStamp records which gateway these certs belong to so a later
// dial against a different endpoint does not silently reuse the wrong mTLS material.
func writeEndpointStamp(cacheDir, endpoint string) error {
	return atomicWriteFile(filepath.Join(cacheDir, endpointStampFile), []byte(endpoint+"\n"), 0644)
}

// checkEndpointStamp returns an error when cached material was issued for a
// different endpoint. Missing stamp is tolerated for backward compatibility
// (pre-stamp installs) and is filled in on the next successful bootstrap/renew.
func checkEndpointStamp(cacheDir, endpoint string) error {
	path := filepath.Join(cacheDir, endpointStampFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("sandbox client: read endpoint stamp: %w", err)
	}
	cached := strings.TrimSpace(string(data))
	if cached == "" || cached == endpoint {
		return nil
	}
	return fmt.Errorf("sandbox client: cached certs are for %q, not %q; re-run login/connect with --apikey or use a separate K8E_SANDBOX_CERT_DIR", cached, endpoint)
}

// ConnErrorHint returns an actionable recovery hint for a gateway
// connection/TLS failure (empty string when there is none). Exported so the
// connect flow — whose lazy handshake surfaces these errors after the dial —
// can attach the same guidance dialErr puts on direct dials.
func ConnErrorHint(err error) string {
	msg := err.Error()
	cacheHint, _ := sandboxCacheDir()
	if cacheHint == "" {
		cacheHint = "~/.k8e/sandbox"
	}
	switch {
	case strings.Contains(msg, "certificate signed by unknown authority"),
		strings.Contains(msg, "ECDSA verification failure"),
		strings.Contains(msg, "x509: certificate"),
		strings.Contains(msg, "certificate is not standards compliant"):
		return fmt.Sprintf("the cached CA (%s/ca.crt) does not match this gateway — the server was reinstalled or its CA rotated. Re-run connect with --reset-certs, or manually: rm -rf %s", cacheHint, cacheHint)
	case strings.Contains(msg, "certificate required"), strings.Contains(msg, "bad certificate"):
		return "client certificate rejected — re-run login/connect with --apikey"
	default:
		return ""
	}
}

func dialErr(endpoint string, err error) error {
	msg := err.Error()
	// Friendly recovery hints for the most common remote-TLS failures (issue #538).
	switch {
	case strings.Contains(msg, "x509: certificate signed by unknown authority"),
		strings.Contains(msg, "certificate is not standards compliant"),
		strings.Contains(msg, "x509:"):
		cacheHint, _ := sandboxCacheDir()
		if cacheHint == "" {
			cacheHint = "~/.k8e/sandbox"
		}
		return fmt.Errorf("sandbox client: dial %s: %w\n  hint: TLS trust failed — remove %s/ca.crt and re-run login/connect with --apikey", endpoint, err, cacheHint)
	case strings.Contains(msg, "certificate required"),
		strings.Contains(msg, "bad certificate"):
		return fmt.Errorf("sandbox client: dial %s: %w\n  hint: client cert rejected — re-run login/connect with --apikey", endpoint, err)
	case strings.Contains(msg, "EOF"), strings.Contains(msg, "connection reset"), strings.Contains(msg, "connection closed"):
		// EOF during the TLS handshake almost always means the gateway closed
		// the connection: it is not running on the endpoint, the port is
		// firewalled, or the cached CA/client cert belongs to a previous CA
		// generation (server CA rotated). All three resolve by re-bootstrapping.
		cacheHint, _ := sandboxCacheDir()
		if cacheHint == "" {
			cacheHint = "~/.k8e/sandbox"
		}
		return fmt.Errorf("sandbox client: dial %s: %w\n  hint: gateway closed the TLS handshake (EOF) — verify the gateway is running and reachable, then re-run connect/login with --apikey (remove %s/ca.crt if the server CA rotated)", endpoint, err, cacheHint)
	default:
		return fmt.Errorf("sandbox client: dial %s: %w", endpoint, err)
	}
}

// ── Local auto-discovery ──────────────────────────────────────────────────────

// sandboxCacheDir resolves the directory for CA + client cert material.
// Priority: K8E_SANDBOX_CERT_DIR → ~/.k8e/sandbox.
func sandboxCacheDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("K8E_SANDBOX_CERT_DIR")); dir != "" {
		return filepath.Clean(dir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".k8e", "sandbox"), nil
}

func resolveCreds(endpoint string) (credentials.TransportCredentials, error) {
	skipVerify := isLoopback(endpoint)
	if creds, ok := resolveCredsFromEnv(skipVerify); ok {
		return creds, nil
	}
	if creds, ok := resolveCredsFromTLSFiles(skipVerify); ok {
		return creds, nil
	}
	if creds, ok := resolveCredsFromKubeconfig(); ok {
		return creds, nil
	}
	return resolveCredsFallback(skipVerify), nil
}

func resolveCredsFromEnv(skipVerify bool) (credentials.TransportCredentials, bool) {
	cert := os.Getenv("K8E_SANDBOX_CERT")
	if cert == "" {
		return nil, false
	}
	if key := os.Getenv("K8E_SANDBOX_KEY"); key != "" {
		tlsCert, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return nil, false
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if skipVerify {
			return credentials.NewTLS(loopbackTLSConfig(pool, tlsCert)), true
		}
		return credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{tlsCert},
			RootCAs:      pool,
			MinVersion:   tls.VersionTLS12,
		}), true
	}
	if creds, err := credentials.NewClientTLSFromFile(cert, ""); err == nil {
		return creds, true
	}
	return nil, false
}

func resolveCredsFromTLSFiles(skipVerify bool) (credentials.TransportCredentials, bool) {
	for _, path := range tlsCandidates {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if skipVerify {
			certPEM, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			pool := x509.NewCertPool()
			pool.AppendCertsFromPEM(certPEM)
			return credentials.NewTLS(loopbackTLSConfig(pool)), true
		}
		if creds, err := credentials.NewClientTLSFromFile(path, ""); err == nil {
			return creds, true
		}
		return nil, false
	}
	return nil, false
}

func resolveCredsFromKubeconfig() (credentials.TransportCredentials, bool) {
	for _, kc := range resolvedKubeconfigCandidates() {
		if creds, err := credsFromKubeconfig(kc); err == nil {
			return creds, true
		}
	}
	return nil, false
}

func resolveCredsFallback(skipVerify bool) credentials.TransportCredentials {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if skipVerify {
		return credentials.NewTLS(loopbackTLSConfig(pool))
	}
	return credentials.NewTLS(&tls.Config{
		RootCAs:    pool,
		MinVersion: tls.VersionTLS12,
	})
}

func credsFromKubeconfig(path string) (credentials.TransportCredentials, error) {
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return nil, err
	}
	for _, cluster := range cfg.Clusters {
		var caData []byte
		if len(cluster.CertificateAuthorityData) > 0 {
			caData = cluster.CertificateAuthorityData
		} else if cluster.CertificateAuthority != "" {
			caData, err = os.ReadFile(cluster.CertificateAuthority)
			if err != nil {
				continue
			}
		}
		if len(caData) == 0 {
			continue
		}
		pool := x509.NewCertPool()
		if pool.AppendCertsFromPEM(caData) {
			return credentials.NewTLS(&tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}), nil
		}
	}
	return nil, fmt.Errorf("no valid CA found in %s", path)
}
