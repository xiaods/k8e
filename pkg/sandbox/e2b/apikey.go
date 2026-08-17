package e2b

import (
	"fmt"
	"regexp"
	"strings"
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
