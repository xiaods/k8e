package e2b

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/xiaods/k8e/pkg/sandbox/apikey"
)

// e2bAPIKeyPattern mirrors the official `e2b` SDK's client-side validation
// (dist/index.js: `const API_KEY_PATTERN = /^e2b_[0-9a-f]+$/`): the SDK
// throws AuthenticationError before any request unless the configured
// apiKey is "e2b_" followed by hex characters. K8E's server accepts any
// bare token, but to be usable from the unmodified official SDK the bare
// token must be hex-only.
var e2bAPIKeyPattern = regexp.MustCompile(`^[0-9a-f]+$`)

// NormalizeE2BAPIKey canonicalizes a configured E2B API key to its bare
// token: whitespace is trimmed and the SDK's `e2b_` prefix (a client
// convention, not part of the secret) is stripped. The result is what the
// server compares against the token presented in Authorization/X-API-KEY
// headers (credentialFromHeaders strips the same prefix). Configuring the
// key with or without the `e2b_` prefix is therefore equivalent.
func NormalizeE2BAPIKey(key string) string {
	key = strings.TrimSpace(key)
	return strings.TrimPrefix(key, "e2b_")
}

// ValidateE2BAPIKey reports whether a configured E2B API key can be used
// with the official e2b SDKs. Empty keys pass (unconfigured — the caller
// decides whether that is acceptable). Any non-empty key whose bare token
// is not hex-only is rejected: even though K8E's server would accept it,
// the SDK's validateApiKey (/^e2b_[0-9a-f]+$/) runs client-side before any
// request, so such a key can never authenticate from an unmodified SDK.
func ValidateE2BAPIKey(key string) error {
	bare := NormalizeE2BAPIKey(key)
	if bare == "" || e2bAPIKeyPattern.MatchString(bare) {
		return nil
	}
	return fmt.Errorf("E2B API key %q is not compatible with the official e2b SDK: "+
		"SDK clients require the key to be \"e2b_\" followed by hex characters "+
		"(validateApiKey: /^e2b_[0-9a-f]+$/), but the bare token %q contains non-hex characters; "+
		"generate a compatible key with `openssl rand -hex 32` and configure it "+
		"without the e2b_ prefix (the server strips it; the SDK prepends it)", key, bare)
}

// SDKAPIKey returns the official-SDK form of a bare token: "e2b_" + hex.
// The SDK's validateApiKey (/^e2b_[0-9a-f]+$/) rejects a bare hex key
// before any request, so this is what operators must pass as apiKey.
func SDKAPIKey(bare string) string {
	bare = NormalizeE2BAPIKey(bare)
	if bare == "" {
		return ""
	}
	return "e2b_" + bare
}

// SecretKeySet is a parsed snapshot of the sandbox-apikeys Secret. It retains
// each record's expiry so callers can re-evaluate expiration against the
// current time even when a later Secret read/parse fails — an expired
// credential must not stay authenticated just because the Secret became
// unreadable or corrupt.
type SecretKeySet struct {
	records map[string]apikey.Record
}

// ParseSecretKeys parses a sandbox-apikeys keys.json payload (v2 records or
// legacy flat map) into a SecretKeySet, retaining expiry information.
func ParseSecretKeys(data []byte) (SecretKeySet, error) {
	records, err := apikey.Parse(data)
	if err != nil {
		return SecretKeySet{}, err
	}
	return SecretKeySet{records: records}, nil
}

// Active returns the bare tokens still valid at now, normalized the same way
// as a configured --e2b-apikey (trim + strip "e2b_" + dedupe), so a Secret
// created by `k8e sandbox-apikey create` can authenticate official e2b SDK
// clients (which present "e2b_"+token). Expired keys are dropped.
func (s SecretKeySet) Active(now time.Time) []string {
	secrets := apikey.ActiveSecrets(s.records, now)
	keys := make([]string, 0, len(secrets))
	for _, secret := range secrets {
		keys = append(keys, secret)
	}
	return normalizeKeyList(keys)
}

// normalizeKeyList trims, strips the SDK prefix, and de-duplicates while
// preserving first-seen order.
func normalizeKeyList(keys []string) []string {
	seen := make(map[string]struct{}, len(keys))
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		k = NormalizeE2BAPIKey(k)
		if k == "" {
			continue
		}
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, k)
	}
	return out
}
