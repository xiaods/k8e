package grpc

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
)

// Preview labels/markers used on preview Services and Ingresses so session GC
// and DestroySession can revoke every outstanding route by label selector.
const (
	previewLabel      = "sandbox.k8e.io/preview"
	previewLabelValue = "1"
)

// PreviewConfig holds the topology knobs the expose feature consumes. These are
// the gateway config knobs promised by the preview ingress decision (KIP-12
// Part A / issue #484): which ingress class preview Ingresses use, which wildcard
// host preview URLs live under, and the base URL the Ingress controller uses to
// reach the gateway's /preview/verify endpoint.
type PreviewConfig struct {
	// Domain is the wildcard preview host, e.g. "preview.k8e.local". Preview
	// URLs take the form https://<Domain>/p/<sid>/<port>/<token>/.
	Domain string
	// IngressClass is the ingressClassName stamped on preview Ingresses.
	IngressClass string
	// VerifyBaseURL is the base URL the Ingress controller uses to reach the
	// gateway's verify endpoint, e.g. "http://127.0.0.1:50052". The Ingress
	// auth annotation points at <VerifyBaseURL>/preview/verify.
	VerifyBaseURL string
}

// previewToken is the signed payload minted for one exposed port.
type previewToken struct {
	SessionID string `json:"sid"`
	Port      int32  `json:"port"`
	ExpiresAt int64  `json:"exp"` // unix seconds
	Nonce     string `json:"nonce"`
}

// previewEngine mints and verifies preview tokens and parses preview paths.
// The verification key persists at KeyFile so tokens survive gateway restarts.
type previewEngine struct {
	key     []byte
	cfg     PreviewConfig
	k8s     kubernetes.Interface
	dyn     dynamic.Interface
	keyFile string

	mu      sync.Mutex
	revoked map[string]time.Time // "sid|port" → revoked at; port 0 = whole session
}

func newPreviewEngine(k8s kubernetes.Interface, dyn dynamic.Interface, cfg PreviewConfig, keyFile string) (*previewEngine, error) {
	key, err := loadOrCreatePreviewKey(keyFile)
	if err != nil {
		return nil, err
	}
	if cfg.Domain == "" {
		cfg.Domain = "preview.k8e.local"
	}
	if cfg.IngressClass == "" {
		cfg.IngressClass = "nginx"
	}
	return &previewEngine{
		key:     key,
		cfg:     cfg,
		k8s:     k8s,
		dyn:     dyn,
		keyFile: keyFile,
		revoked: make(map[string]time.Time),
	}, nil
}

// loadOrCreatePreviewKey reads the preview HMAC key, generating and persisting
// a fresh one (0600) when the file does not exist yet.
func loadOrCreatePreviewKey(path string) ([]byte, error) {
	if path != "" {
		if data, err := os.ReadFile(path); err == nil && len(data) >= 32 {
			return data, nil
		}
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, fmt.Errorf("preview key: %w", err)
	}
	key := []byte(hex.EncodeToString(buf))
	if path != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, fmt.Errorf("preview key dir: %w", err)
		}
		if err := os.WriteFile(path, key, 0600); err != nil {
			return nil, fmt.Errorf("preview key write: %w", err)
		}
	}
	return key, nil
}

// mintToken signs a preview token with the engine key. Every mint carries a
// fresh random nonce so re-exposing a route issues a genuinely new token even
// when the expiry timestamp is unchanged.
func (e *previewEngine) mintToken(sessionID string, port int32, ttlSeconds int64) (string, error) {
	exp := time.Now().Add(time.Duration(ttlSeconds) * time.Second).Unix()
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	payload, err := json.Marshal(previewToken{
		SessionID: sessionID, Port: port, ExpiresAt: exp,
		Nonce: base64.RawURLEncoding.EncodeToString(nonce),
	})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, e.key)
	mac.Write([]byte(enc))
	return enc + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

// verifyToken checks the HMAC signature and expiry of a preview token.
// Returns the parsed token on success.
func (e *previewEngine) verifyToken(token string) (*previewToken, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, fmt.Errorf("malformed preview token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("malformed preview token payload")
	}
	mac := hmac.New(sha256.New, e.key)
	mac.Write([]byte(parts[0]))
	expect := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expect), []byte(parts[1])) {
		return nil, fmt.Errorf("preview token signature mismatch")
	}
	var tok previewToken
	if err := json.Unmarshal(payload, &tok); err != nil {
		return nil, fmt.Errorf("preview token payload decode: %w", err)
	}
	if tok.SessionID == "" || tok.Port <= 0 || tok.Port > 65535 {
		return nil, fmt.Errorf("preview token invalid fields")
	}
	if time.Now().Unix() >= tok.ExpiresAt {
		return nil, fmt.Errorf("preview token expired")
	}
	return &tok, nil
}

// previewPathRe matches /p/<sid>/<port>/<token>/... — the path layout the
// Ingress routes on. The token is the segment after the port.
var previewPathRe = regexp.MustCompile(`^/p/([^/]+)/([0-9]+)/([^/]+)/?`)

// parsePreviewPath extracts sid, port and token from a preview request path.
func parsePreviewPath(path string) (sid string, port int32, token string, ok bool) {
	m := previewPathRe.FindStringSubmatch(path)
	if m == nil {
		return "", 0, "", false
	}
	p, err := strconv.ParseInt(m[2], 10, 32)
	if err != nil || p <= 0 || p > 65535 {
		return "", 0, "", false
	}
	return m[1], int32(p), m[3], true
}

// RevokePort records an explicit revocation for a (session, port) pair.
func (e *previewEngine) RevokePort(sessionID string, port int32) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revoked[previewRevKey(sessionID, port)] = time.Now()
}

// RevokeSession records revocation for every port of a session. Ports are
// unknown at GC time, so the session-level marker is checked alongside the
// per-port marker in IsRevoked.
func (e *previewEngine) RevokeSession(sessionID string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revoked[previewRevKey(sessionID, 0)] = time.Now()
}

// IsRevoked reports whether a (session, port) pair was explicitly revoked.
func (e *previewEngine) IsRevoked(sessionID string, port int32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.revoked[previewRevKey(sessionID, port)]; ok {
		return true
	}
	_, ok := e.revoked[previewRevKey(sessionID, 0)]
	return ok
}

func previewRevKey(sessionID string, port int32) string {
	return sessionID + "|" + strconv.FormatInt(int64(port), 10)
}

// authorizePreview is the full verify decision used by the /preview/verify
// handler: signature + expiry + explicit revocation + session still Active +
// the port still published on the session.
func (e *previewEngine) authorizePreview(token string) (sid string, port int32, err error) {
	tok, err := e.verifyToken(token)
	if err != nil {
		return "", 0, err
	}
	if e.IsRevoked(tok.SessionID, tok.Port) {
		return tok.SessionID, tok.Port, fmt.Errorf("preview route revoked")
	}
	session, err := sessionFromDynamic(e.dyn, tok.SessionID)
	if err != nil {
		return tok.SessionID, tok.Port, fmt.Errorf("session not active: %v", err)
	}
	if session.Status.Phase != sandboxv1.SandboxPhaseActive {
		return tok.SessionID, tok.Port, fmt.Errorf("session not active (phase %s)", session.Status.Phase)
	}
	if session.Status.ExpiresAt != nil && !session.Status.ExpiresAt.Time.After(time.Now()) {
		return tok.SessionID, tok.Port, fmt.Errorf("session expired")
	}
	found := false
	for _, ep := range session.Status.ExposedPorts {
		if ep.Port == tok.Port {
			found = true
			break
		}
	}
	if !found {
		return tok.SessionID, tok.Port, fmt.Errorf("port %d no longer published", tok.Port)
	}
	// The Service selects the session pod by label; a live pod is required.
	pods, err := e.k8s.CoreV1().Pods(sandboxNS).List(context.Background(), metav1.ListOptions{
		LabelSelector: labelSessionID + "=" + tok.SessionID,
	})
	if err != nil || len(pods.Items) == 0 {
		return tok.SessionID, tok.Port, fmt.Errorf("session pod no longer exists")
	}
	return tok.SessionID, tok.Port, nil
}

// sessionFromDynamic reads a SandboxSession CRD via the dynamic client.
func sessionFromDynamic(dyn dynamic.Interface, sessionID string) (*sandboxv1.SandboxSession, error) {
	u, err := dyn.Resource(sessionGVR).Namespace(sandboxNS).Get(context.Background(), sessionID, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return unstructuredToSession(u)
}

// ServePreviewVerify implements the Ingress external-auth callback. nginx
// auth_request passes the original request URI in the X-Original-URI header.
// 200 → allow, 403 → deny.
func (e *previewEngine) ServePreviewVerify(w http.ResponseWriter, r *http.Request) {
	uri := r.Header.Get("X-Original-URI")
	if uri == "" {
		uri = r.URL.RequestURI()
	}
	sid, port, token, ok := parsePreviewPath(uri)
	if !ok {
		logrus.Debugf("preview verify: unparseable uri %q", uri)
		http.Error(w, "invalid preview path", http.StatusForbidden)
		return
	}
	if _, _, err := e.authorizePreview(token); err != nil {
		logrus.Debugf("preview verify: deny sid=%s port=%d: %v", sid, port, err)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
}
