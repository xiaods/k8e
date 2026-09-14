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

	"github.com/gofrs/flock"
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
	return NewClientWithOptions(endpoint, apiKey, ConnectOptions{})
}

// ConnectOptions controls explicit authentication. InsecureBootstrap only applies
// when no CA is available; it never bypasses verification of a cached CA.
type ConnectOptions struct {
	CAFile            string
	InsecureBootstrap bool
	ForceLogin        bool
	ResetCerts        bool
}

// NewClientWithOptions serializes credential inspection and mutation across CLI
// processes sharing a cache directory. Ordinary calls reuse valid credentials;
// ForceLogin always authenticates the supplied API key with the server.
func NewClientWithOptions(endpoint, apiKey string, opts ConnectOptions) (*Client, error) {
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("K8E_SANDBOX_APIKEY"))
	}
	if err := validateConnectOptions(endpoint, apiKey, opts); err != nil {
		return nil, err
	}
	if apiKey == "" && (endpoint == "" || isLoopback(endpoint)) {
		if endpoint == "" {
			return NewClient()
		}
		return newLocalClient(endpoint)
	}
	return newRemoteClient(endpoint, apiKey, opts)
}

// validateConnectOptions rejects requests that cannot be satisfied: an explicit
// login or reset needs both an endpoint and an API key.
func validateConnectOptions(endpoint, apiKey string, opts ConnectOptions) error {
	if (opts.ForceLogin || opts.ResetCerts) && (apiKey == "" || endpoint == "") {
		return fmt.Errorf("sandbox client: explicit login/reset requires endpoint and API key")
	}
	return nil
}

// credentialFiles is the mTLS material owned by one credential cache directory.
type credentialFiles struct {
	ca   string
	cert string
	key  string
}

// File names of the mTLS material inside a credential cache directory. They
// are shared by the bootstrap path and every caller that reads the cache, so
// the names are defined once here.
const (
	caFileName     = "ca.crt"
	clientCertFile = "client.crt"
	clientKeyFile  = "client.key"
)

// cacheState is the offline inspection result of the cached credentials: it is
// gathered before any network call is made.
type cacheState struct {
	caMissing bool
	stampErr  error
}

// reusable reports whether cached material may serve this dial as-is, without
// contacting the gateway for a fresh certificate.
func (s cacheState) reusable(opts ConnectOptions) bool {
	return !s.caMissing && s.stampErr == nil && !opts.ForceLogin && !opts.ResetCerts
}

// newRemoteClient resolves credentials for a remote gateway while holding the
// shared credentials lock, reusing valid material or authenticating anew.
func newRemoteClient(endpoint, apiKey string, opts ConnectOptions) (*Client, error) {
	cacheDir, unlock, err := lockCredentialCache()
	if err != nil {
		return nil, err
	}
	defer unlock()

	files := credentialFiles{
		ca:   filepath.Join(cacheDir, caFileName),
		cert: filepath.Join(cacheDir, clientCertFile),
		key:  filepath.Join(cacheDir, clientKeyFile),
	}
	state, err := inspectCredentialCache(cacheDir, endpoint, files)
	if err != nil {
		return nil, err
	}
	if apiKey == "" {
		if err := requireCachedCredentials(endpoint, state); err != nil {
			return nil, err
		}
	}
	if c, reused, err := dialWithCachedCerts(endpoint, files, state, opts); reused {
		return c, err
	}
	if apiKey == "" {
		return nil, fmt.Errorf("sandbox client: cached client certificate for %s is missing or expired; reconnect with --apikey", endpoint)
	}
	return authenticate(endpoint, cacheDir, resolveTrustFile(opts, state, files.ca), apiKey, opts)
}

// lockCredentialCache serializes credential inspection and mutation across
// concurrent CLI processes sharing a cache directory.
func lockCredentialCache() (string, func(), error) {
	cacheDir, err := sandboxCacheDir()
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), loginTimeout)
	defer cancel()
	unlock, err := lockCredentials(ctx, cacheDir)
	if err != nil {
		return "", nil, err
	}
	return cacheDir, unlock, nil
}

// inspectCredentialCache records whether the cached CA exists and whether the
// cached material was issued for the requested endpoint.
func inspectCredentialCache(cacheDir, endpoint string, files credentialFiles) (cacheState, error) {
	state := cacheState{stampErr: checkEndpointStamp(cacheDir, endpoint)}
	_, err := os.Stat(files.ca)
	if err != nil && !os.IsNotExist(err) {
		return cacheState{}, err
	}
	state.caMissing = err != nil
	return state, nil
}

// requireCachedCredentials rejects a keyless dial that the cache cannot serve.
func requireCachedCredentials(endpoint string, state cacheState) error {
	if state.caMissing {
		return fmt.Errorf("sandbox client: no cached CA for %s; run connect or login with --apikey", endpoint)
	}
	if state.stampErr != nil {
		return state.stampErr
	}
	return nil
}

// dialWithCachedCerts serves the dial from valid cached material, renewing the
// certificate lazily when it nears expiry. The second result reports whether
// the cached path applied; when it does not, the caller authenticates anew.
func dialWithCachedCerts(endpoint string, files credentialFiles, state cacheState, opts ConnectOptions) (*Client, bool, error) {
	if !state.reusable(opts) {
		return nil, false, nil
	}
	st := inspectClientCert(files.cert)
	if !st.valid {
		return nil, false, nil
	}
	if st.expiringSoon {
		renewClientCert(endpoint, files.ca, files.cert, files.key)
	}
	conn, err := dialMTLS(endpoint, files.ca, files.cert, files.key)
	if err != nil {
		return nil, true, dialErr(endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, true, nil
}

// resolveTrustFile picks the CA that verifies the gateway during bootstrap: an
// explicit --ca-file always wins, otherwise the cached CA is reused unless it
// belongs to another endpoint or the caller asked to reset credentials.
func resolveTrustFile(opts ConnectOptions, state cacheState, caFile string) string {
	if opts.CAFile != "" || opts.ResetCerts || state.caMissing || state.stampErr != nil {
		return opts.CAFile
	}
	return caFile
}

func lockCredentials(ctx context.Context, dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(dir, "credentials.lock"))
	locked, err := lock.TryLockContext(ctx, 25*time.Millisecond)
	if err != nil || !locked {
		_ = lock.Close()
		if err == nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("sandbox client: wait for credentials lock: %w", err)
	}
	return func() { _ = lock.Close() }, nil
}

func (c *Client) Close() error { return c.conn.Close() }

// ── mTLS bootstrap helpers ────────────────────────────────────────────────────

// authenticate prepares and validates the complete response before replacing any
// credentials. The caller holds credentials.lock through publication and loading.
func authenticate(endpoint, cacheDir, trustFile, apiKey string, opts ConnectOptions) (*Client, error) {
	keyFile := filepath.Join(cacheDir, clientKeyFile)
	key, err := loadClientKey(keyFile)
	if err != nil || opts.ResetCerts {
		if err != nil && !os.IsNotExist(err) && !opts.ResetCerts {
			return nil, fmt.Errorf("sandbox client: read private key: %w", err)
		}
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
	}
	csr, err := createCSR(key)
	if err != nil {
		return nil, err
	}
	resp, err := callLogin(endpoint, trustFile, apiKey, csr, opts.InsecureBootstrap)
	if err != nil {
		return nil, fmt.Errorf("sandbox client: login (verify server trust with --ca-file): %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
	if err := validateIssuedCertificate(resp, keyPEM); err != nil {
		return nil, err
	}
	if err := publishCredentials(cacheDir, []credentialFile{
		{clientKeyFile, keyPEM, 0600},
		{caFileName, []byte(resp.CaCert), 0644},
		{clientCertFile, []byte(resp.Cert), 0644},
		{endpointStampFile, []byte(endpoint + "\n"), 0644},
	}, os.Rename); err != nil {
		return nil, err
	}

	conn, err := dialMTLS(endpoint, filepath.Join(cacheDir, caFileName), filepath.Join(cacheDir, clientCertFile), keyFile)
	if err != nil {
		return nil, dialErr(endpoint, err)
	}
	return &Client{SandboxServiceClient: pb.NewSandboxServiceClient(conn), conn: conn}, nil
}

func validateIssuedCertificate(resp *pb.LoginResponse, keyPEM []byte) error {
	pair, err := tls.X509KeyPair([]byte(resp.GetCert()), keyPEM)
	if err != nil {
		return fmt.Errorf("sandbox client: invalid issued certificate: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(resp.GetCaCert())) {
		return fmt.Errorf("sandbox client: invalid issued CA")
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return err
		}
		intermediates.AddCert(cert)
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	return err
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
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil || validateIssuedCertificate(resp, keyPEM) != nil {
		return
	}
	if err := atomicWriteFile(certFile, []byte(resp.Cert), 0644); err != nil {
		return
	}
	// Backfill endpoint stamp for pre-stamp installs and after successful renew.
	_ = writeEndpointStamp(filepath.Dir(caFile), endpoint)
}

// ── Key & CSR generation ──────────────────────────────────────────────────────

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

func loginTLSConfig(endpoint, caFile string, insecure bool) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if caFile == "" {
		// System roots are the default. An explicit opt-in is required to send
		// the API key without authenticating the server during initial bootstrap.
		cfg.InsecureSkipVerify = insecure //nolint:gosec // explicit bootstrap opt-in
		return cfg, nil
	}
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("parse CA certificate: %s", caFile)
	}
	if isLoopback(endpoint) {
		return loopbackTLSConfig(pool), nil
	}
	cfg.RootCAs = pool
	return cfg, nil
}

func callLogin(endpoint, caFile, apiKey, csr string, insecure bool) (*pb.LoginResponse, error) {
	cfg, err := loginTLSConfig(endpoint, caFile, insecure)
	if err != nil {
		return nil, err
	}
	creds := credentials.NewTLS(cfg)

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
		return fmt.Sprintf("TLS verification failed using %s/ca.crt; verify the endpoint and obtain the correct CA through a trusted channel, then run connect --reset-certs --apikey <key> --ca-file <trusted-ca>", cacheHint)
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
		return fmt.Errorf("sandbox client: dial %s: %w\n  hint: TLS trust failed — verify %s/ca.crt and reconnect with --reset-certs --apikey <key> --ca-file <trusted-ca>", endpoint, err, cacheHint)
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
		return fmt.Errorf("sandbox client: dial %s: %w\n  hint: gateway closed the TLS handshake (EOF) — verify the gateway is running and reachable, then verify %s/ca.crt; if the CA rotated, use connect --reset-certs --apikey <key> --ca-file <trusted-ca>", endpoint, err, cacheHint)
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
