package config

import (
	"fmt"
	"net"
	"regexp"
	"strings"
)

// dnsLabelRe matches one RFC 1123 hostname label: 1-63 chars, letters/digits/
// hyphens, never starting or ending with a hyphen.
var dnsLabelRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// ValidateAdvertiseHostname validates a value supplied for the sandbox gateway's
// external advertise hostname (--sandbox-advertise-hostname /
// K8E_SANDBOX_ADVERTISED_HOSTNAME). It must be a bare IP (routed to IP SANs) or
// a bare RFC 1123 DNS name (routed to DNS SANs). URL schemes, host:port, paths,
// userinfo, whitespace, and malformed DNS labels are rejected so an operator
// cannot silently produce a gateway certificate that never verifies for the
// intended endpoint. An empty value is valid (means "unset").
func ValidateAdvertiseHostname(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	if net.ParseIP(name) != nil {
		return nil // IP — routed to IP SANs upstream
	}
	if len(name) > 253 {
		return fmt.Errorf("%q is too long for a DNS name (max 253 chars)", name)
	}
	if strings.ContainsAny(name, " \t\n\r/\\:@?#%") {
		return fmt.Errorf("%q is not a bare DNS name or IP (no scheme, port, path, or whitespace allowed; e.g. sandbox.example.com or 203.0.113.10)", name)
	}
	if !validDNSLabels(name) {
		return fmt.Errorf("%q is not a valid DNS name", name)
	}
	return nil
}

// validDNSLabels reports whether every dot-separated label of name is a valid
// RFC 1123 hostname label (see dnsLabelRe).
func validDNSLabels(name string) bool {
	for _, label := range strings.Split(name, ".") {
		if !dnsLabelRe.MatchString(label) {
			return false
		}
	}
	return true
}
