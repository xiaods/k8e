package grpc

import (
	"context"
	"crypto/x509"
	"net"
	"os"
	"testing"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// TestCollectServerSANsAdvertiseHostname verifies the AWS/remote-host case: the
// configured advertise hostname (--sandbox-advertise-hostname) is added to the
// server cert SANs — as a DNS name for a domain, or as an IP for a literal
// address — and the legacy K8E_SANDBOX_ADVERTISED_HOSTNAME env var is merged as
// a fallback. The machine hostname is always a DNS SAN.
func TestCollectServerSANsAdvertiseHostname(t *testing.T) {
	t.Setenv("K8E_SANDBOX_ADVERTISED_HOSTNAME", "")

	sans, err := collectServerSANs("ip-10-0-1-5", "sandbox.example.com")
	if err != nil {
		t.Fatalf("collectServerSANs: %v", err)
	}
	if !containsString(sans.dnsNames, "sandbox.example.com") {
		t.Fatalf("advertise hostname %q missing from DNS SANs: %v", "sandbox.example.com", sans.dnsNames)
	}
	if !containsString(sans.dnsNames, "ip-10-0-1-5") {
		t.Fatalf("machine hostname missing from DNS SANs: %v", sans.dnsNames)
	}
}

func TestCollectServerSANsAdvertiseHostnameAsIP(t *testing.T) {
	t.Setenv("K8E_SANDBOX_ADVERTISED_HOSTNAME", "")

	sans, err := collectServerSANs("node", "203.0.113.10")
	if err != nil {
		t.Fatalf("collectServerSANs: %v", err)
	}
	want := net.ParseIP("203.0.113.10")
	if want == nil || !containsIP(sans.ips, want) {
		t.Fatalf("advertise IP %q missing from IP SANs: %v", "203.0.113.10", sans.ips)
	}
	if containsString(sans.dnsNames, "203.0.113.10") {
		t.Fatalf("advertise IP must not be treated as a DNS name: %v", sans.dnsNames)
	}
}

func TestCollectServerSANsEnvVarFallback(t *testing.T) {
	t.Setenv("K8E_SANDBOX_ADVERTISED_HOSTNAME", "legacy.example.com")

	sans, err := collectServerSANs("node", "")
	if err != nil {
		t.Fatalf("collectServerSANs: %v", err)
	}
	if !containsString(sans.dnsNames, "legacy.example.com") {
		t.Fatalf("legacy env-var advertise hostname missing from DNS SANs: %v", sans.dnsNames)
	}
}

func TestCollectServerSANsFlagWinsAndMerges(t *testing.T) {
	t.Setenv("K8E_SANDBOX_ADVERTISED_HOSTNAME", "legacy.example.com")

	sans, err := collectServerSANs("node", "flag.example.com")
	if err != nil {
		t.Fatalf("collectServerSANs: %v", err)
	}
	if !containsString(sans.dnsNames, "flag.example.com") {
		t.Fatalf("flag advertise hostname missing: %v", sans.dnsNames)
	}
	if !containsString(sans.dnsNames, "legacy.example.com") {
		t.Fatalf("env-var advertise hostname should be merged as fallback: %v", sans.dnsNames)
	}
}

func TestSansCoveredBy(t *testing.T) {
	cert := &x509.Certificate{
		DNSNames:    []string{"node", "sandbox.example.com"},
		IPAddresses: []net.IP{net.ParseIP("10.0.0.5")},
	}

	cases := []struct {
		name string
		want serverSANs
		ok   bool
	}{
		{"covered", serverSANs{dnsNames: []string{"sandbox.example.com"}, ips: []net.IP{net.ParseIP("10.0.0.5")}}, true},
		{"missing dns", serverSANs{dnsNames: []string{"other.example.com"}}, false},
		{"missing ip", serverSANs{ips: []net.IP{net.ParseIP("10.0.0.6")}}, false},
		{"empty want", serverSANs{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sansCoveredBy(cert, tc.want); got != tc.ok {
				t.Fatalf("sansCoveredBy(%v) = %v, want %v", tc.want, got, tc.ok)
			}
		})
	}
}

func TestValidateAdvertiseHostname(t *testing.T) {
	cases := []struct {
		name  string
		input string
		ok    bool
	}{
		{"bare dns", "sandbox.example.com", true},
		{"aws ec2 hostname", "ec2-203-0-113-10.compute-1.amazonaws.com", true},
		{"single label", "sandbox", true},
		{"ip-looking string is valid DNS syntax", "203.0.113.10", true}, // IPs route to IP SANs via net.ParseIP upstream
		{"scheme url", "https://sandbox.example.com", false},
		{"scheme http", "http://sandbox.example.com", false},
		{"host and port", "sandbox.example.com:50051", false},
		{"trailing colon", "sandbox.example.com:", false},
		{"path suffix", "sandbox.example.com/foo", false},
		{"userinfo", "user@sandbox.example.com", false},
		{"trailing space is trimmed", "sandbox.example.com ", true}, // normalized before validation
		{"embedded space", "sand box.example.com", false},
		{"empty is unset/valid", "", true}, // empty = no SAN added upstream
		{"leading hyphen label", "-sandbox.example.com", false},
		{"trailing hyphen label", "sandbox-.example.com", false},
		{"underscore", "sand_box.example.com", false},
		{"empty label", "sandbox..example.com", false},
		{"long label", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa.example.com", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := config.ValidateAdvertiseHostname(tc.input)
			if (err == nil) != tc.ok {
				t.Fatalf("config.ValidateAdvertiseHostname(%q) error = %v, want ok=%v", tc.input, err, tc.ok)
			}
		})
	}
}

func TestCollectServerSANsRejectsMalformed(t *testing.T) {
	t.Setenv("K8E_SANDBOX_ADVERTISED_HOSTNAME", "")

	if _, err := collectServerSANs("node", "https://sandbox.example.com"); err == nil {
		t.Fatalf("malformed URL must fail hard, not be silently omitted")
	}

	if _, err := collectServerSANs("node", "sandbox.example.com:50051"); err == nil {
		t.Fatalf("host:port must fail hard, not be silently omitted")
	}
}

func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/sandbox-server.crt"

	if err := atomicWriteFile(path, []byte("first"), 0644); err != nil {
		t.Fatalf("atomicWriteFile: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "first" {
		t.Fatalf("read back = %q, %v; want first", got, err)
	}

	if err := atomicWriteFile(path, []byte("second"), 0644); err != nil {
		t.Fatalf("atomicWriteFile(second): %v", err)
	}
	got, err = os.ReadFile(path)
	if err != nil || string(got) != "second" {
		t.Fatalf("read back after rotate = %q, %v; want second", got, err)
	}
	// No temp file may be left behind.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}
}

// TestCheckMTLSAuthLoopbackLocalAuth pins the KIP-24 expose chain: the
// embedded e2b server dials the gateway over loopback with CA trust but NO
// client certificate (newLocalClient), relying on the gateway's LocalAuth
// exemption. Without LocalAuth the dial is rejected with Unauthenticated and
// every /k8e/expose/ request 503s ("client certificate required for mTLS").
// Remote (non-loopback) peers must still be rejected without a client cert.
func TestCheckMTLSAuthLoopbackLocalAuth(t *testing.T) {
	loopback := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 45678}
	remote := &net.TCPAddr{IP: net.ParseIP("203.0.113.7"), Port: 45678}
	ctxFrom := func(addr net.Addr) context.Context {
		return peer.NewContext(context.Background(), &peer.Peer{Addr: addr})
	}

	s := &Server{}
	if err := s.checkMTLSAuth(ctxFrom(loopback)); err == nil {
		t.Fatal("loopback without LocalAuth must be rejected")
	} else if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("loopback without LocalAuth: got %v, want Unauthenticated", err)
	}
	if err := s.checkMTLSAuth(ctxFrom(remote)); err == nil {
		t.Fatal("remote without cert must always be rejected")
	}

	s.localAuth = true
	if err := s.checkMTLSAuth(ctxFrom(loopback)); err != nil {
		t.Fatalf("loopback with LocalAuth must pass, got %v", err)
	}
	if err := s.checkMTLSAuth(ctxFrom(remote)); err == nil {
		t.Fatal("remote with LocalAuth set must still require mTLS")
	} else if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("remote with LocalAuth set: got %v, want Unauthenticated", err)
	}
}
