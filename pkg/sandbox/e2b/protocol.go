// Package e2b implements an E2B-compatible HTTP server for K8E sandboxes
// (KIP-18). It speaks the E2B wire protocol — control plane REST under
// /e2b/api, the envd Connect-RPC surface under /e2b/envd, and signed file
// URLs at /files — and translates every call into the k8e sandbox gRPC
// gateway (sandbox.v1.SandboxService). The official `e2b` SDK (JS and
// Python, unmodified) connects by changing two URLs and using
// `e2b_<K8E_SANDBOX_APIKEY>` as its API key.
//
// The protocol layer is modeled on Dormice's measured E2B compatibility
// implementation (https://github.com/BitMiracle-AI/Dormice): faithful by
// default, honest about what is not supported (machine-readable
// `unimplemented` errors instead of silent partial behavior).
package e2b

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EnvdVersion is the envd version this compat layer claims. Deliberately
// 0.6.1: at or above 0.5.7 the SDK uploads via octet-stream (we support it),
// below 0.6.2 it refuses file-metadata options client-side (we do not
// support xattr metadata) — the SDK blocks what we cannot do instead of us
// accepting data and silently dropping it.
const EnvdVersion = "0.6.1"

// DefaultTimeoutSeconds is E2B's default sandbox TTL.
const DefaultTimeoutSeconds = 300

// NeverTimeout is the SDK's NEVER_TIMEOUT sentinel (-1): no idle reclamation,
// no endAt in views. Mirrors CubeSandbox's timeout=-1 semantics.
const NeverTimeout = -1

// maxTimeoutSeconds bounds timeout/connect bodies (30 days).
const maxTimeoutSeconds = 30 * 24 * 60 * 60

// Connect streaming envelope flags (the Connect protocol's flag byte).
const (
	FlagMessage   = 0x00
	FlagEndStream = 0x02
)

// E2bError is an error on the E2B surface. Two sub-dialects, both verified
// against the official SDK: the control plane's `code` is the NUMERIC status
// (the JS SDK literally checks error.code === 404), the envd Connect RPC's
// `code` is the protocol string ('not_found', 'unimplemented', ...).
type E2bError struct {
	StatusCode int
	Code       any // numeric for the control plane, string for the envd dialect
	Message    string
}

func (e *E2bError) Error() string { return e.Message }

// apiError builds a control-plane error whose code mirrors the numeric
// status, per E2B's openapi.
func apiError(status int, message string) *E2bError {
	return &E2bError{StatusCode: status, Code: status, Message: message}
}

// connectStatus maps Connect RPC error codes to HTTP statuses (unary
// mapping). Only the codes this layer actually emits.
var connectStatus = map[string]int{
	"invalid_argument":   400,
	"unauthenticated":    401,
	"not_found":          404,
	"already_exists":     409,
	"resource_exhausted": 429,
	"internal":           500,
	"unimplemented":      501,
	"unavailable":        502,
}

// connectError builds an envd-dialect error (string code).
func connectError(code, message string) *E2bError {
	status := connectStatus[code]
	if status == 0 {
		status = 500
	}
	return &E2bError{StatusCode: status, Code: code, Message: message}
}

// envelope builds one Connect stream frame: 1 flag byte + 4-byte big-endian
// payload length + JSON payload. The JSON codec because the SDK is configured
// with useBinaryFormat: false — no protobuf wire format anywhere.
func envelope(flags int, payload any) []byte {
	body, err := json.Marshal(payload)
	if err != nil {
		// All payloads here are marshalable plain maps; an error would be a
		// programming bug — surface it as an explicit end-stream error frame.
		body, _ = json.Marshal(map[string]any{
			"error": map[string]string{"code": "internal", "message": err.Error()},
		})
	}
	head := make([]byte, 5)
	head[0] = byte(flags)
	binary.BigEndian.PutUint32(head[1:], uint32(len(body)))
	return append(head, body...)
}

// parseEnvelopes splits a raw payload into its enveloped frames.
func parseEnvelopes(payload []byte) ([]frame, error) {
	var frames []frame
	for offset := 0; offset+5 <= len(payload); {
		flags := int(payload[offset])
		length := int(binary.BigEndian.Uint32(payload[offset+1:]))
		if length < 0 || offset+5+length > len(payload) {
			return nil, fmt.Errorf("truncated connect envelope at offset %d", offset)
		}
		var body map[string]any
		if length > 0 {
			if err := json.Unmarshal(payload[offset+5:offset+5+length], &body); err != nil {
				return nil, fmt.Errorf("connect envelope is not JSON: %w", err)
			}
		}
		frames = append(frames, frame{flags: flags, json: body})
		offset += 5 + length
	}
	return frames, nil
}

type frame struct {
	flags int
	json  map[string]any
}

// readFirstMessage extracts the first enveloped message of a streaming
// request body — a Start/Connect/WatchDir request carries exactly one.
func readFirstMessage(payload []byte) (map[string]any, error) {
	frames, err := parseEnvelopes(payload)
	if err != nil {
		return nil, connectError("invalid_argument", err.Error())
	}
	if len(frames) == 0 {
		return nil, connectError("invalid_argument", "truncated connect envelope")
	}
	return frames[0].json, nil
}

// --- envd access tokens ---------------------------------------------------

// mintEnvdToken derives the per-sandbox envd access token:
// HMAC(signingSecret, "envd:"+sandboxID). Stateless — the server verifies
// without a lookup, and the SDK treats the value as opaque (it just echoes
// what create returned). Keyed by the signing secret, never the API key, so
// the two credentials rotate independently.
func mintEnvdToken(signingSecret, sandboxID string) string {
	mac := hmac.New(sha256.New, []byte(signingSecret))
	mac.Write([]byte("envd:" + sandboxID))
	return fmt.Sprintf("%x", mac.Sum(nil))
}

// verifyEnvdToken checks a presented token constant-time against the minted
// value (both sides hashed so timingSafeEqual sees equal lengths).
func verifyEnvdToken(signingSecret, sandboxID, presented string) bool {
	expected := mintEnvdToken(signingSecret, sandboxID)
	return hmac.Equal([]byte(sha256Hex(expected)), []byte(sha256Hex(presented)))
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return fmt.Sprintf("%x", sum)
}

// --- signed file URLs -----------------------------------------------------

// fileSignature is E2B's file-URL signature, pinned against the official
// SDK's getSignature: "v1_" + base64(sha256(path:operation:username:token
// [:expiration])) — standard base64 alphabet, '=' padding stripped. path and
// username are empty strings when absent; expiration is absolute unix
// seconds, present in the material exactly when the URL carries
// signature_expiration.
func fileSignature(envdAccessToken string, material signatureMaterial) string {
	parts := []string{material.path, material.operation, material.username, envdAccessToken}
	if material.expirationUnix != nil {
		parts = append(parts, strconv.FormatInt(*material.expirationUnix, 10))
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, ":")))
	return "v1_" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}

type signatureMaterial struct {
	path           string
	operation      string // "read" | "write"
	username       string
	expirationUnix *int64
}

// signatureMatches reports whether a presented signature is the one the
// sandbox's token would sign (HMAC only — expiration is judged separately so
// a valid-but-expired signature can still identify its sandbox).
func signatureMatches(presented, sandboxID, signingSecret string, material signatureMaterial) bool {
	if presented == "" {
		return false
	}
	expected := fileSignature(mintEnvdToken(signingSecret, sandboxID), material)
	return hmac.Equal([]byte(sha256Hex(expected)), []byte(sha256Hex(presented)))
}

// checkSignatureExpiration applies the expiration half of the check.
func checkSignatureExpiration(material signatureMaterial) *E2bError {
	if material.expirationUnix != nil && *material.expirationUnix < unixNow() {
		return connectError("unauthenticated", "signature is already expired")
	}
	return nil
}

// --- small helpers --------------------------------------------------------

// apiKeyFromHeader strips the SDK's `e2b_` prefix. The prefix is the SDK's
// convention, not a secret; the bare token is what gets compared.
func stripE2BPrefix(presented string) string {
	if strings.HasPrefix(presented, "e2b_") {
		return presented[len("e2b_"):]
	}
	return presented
}

// shellQuote single-quotes a string for /bin/sh, escaping embedded quotes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// randomID returns a short random hex id (crypto-strength).
func randomID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
