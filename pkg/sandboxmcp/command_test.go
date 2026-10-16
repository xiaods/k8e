package sandboxmcp

import (
	"context"
	"errors"
	"flag"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/urfave/cli"
)

func TestValidateServeOptionsAllowsGatewayTerminatedTLS(t *testing.T) {
	if err := validateServeOptions(testServeContext(t, "", "")); err != nil {
		t.Fatalf("gateway-terminated TLS should allow an internal HTTP listener: %v", err)
	}
	if err := validateServeOptions(testServeContext(t, "tls.crt", "tls.key")); err != nil {
		t.Fatalf("direct TLS should remain supported: %v", err)
	}
}

func TestValidateServeOptionsRejectsIncompleteTLS(t *testing.T) {
	for _, files := range [][2]string{{"tls.crt", ""}, {"", "tls.key"}} {
		err := validateServeOptions(testServeContext(t, files[0], files[1]))
		if err == nil || !strings.Contains(err.Error(), "both --tls-cert and --tls-key") {
			t.Fatalf("cert=%q key=%q: %v", files[0], files[1], err)
		}
	}
}

func TestValidateServeOptionsAcceptsEnrollmentMode(t *testing.T) {
	noGatewayPair := map[string]string{"gateway-cert": "", "gateway-key": ""}
	if err := validateServeOptions(testServeContextWith(t, "", "", mergeStrings(noGatewayPair, map[string]string{"bootstrap-api-key": "e2b-key"}))); err != nil {
		t.Fatalf("bootstrap API key should satisfy gateway access: %v", err)
	}
	if err := validateServeOptions(testServeContextWith(t, "", "", noGatewayPair)); err == nil ||
		!strings.Contains(err.Error(), "bootstrap-api-key") {
		t.Fatalf("neither gateway material nor bootstrap key must fail: %v", err)
	}
	// An explicit client pair needs the CA file — the enrollment path is the
	// only one that may discover a trust anchor from a warm cache.
	explicit := mergeStrings(noGatewayPair, map[string]string{"gateway-cert": "client.crt", "gateway-key": "client.key", "gateway-ca": ""})
	if err := validateServeOptions(testServeContextWith(t, "", "", explicit)); err == nil {
		t.Fatal("explicit gateway pair without --gateway-ca must fail")
	}
}

func mergeStrings(a, b map[string]string) map[string]string {
	merged := make(map[string]string, len(a)+len(b))
	for name, value := range a {
		merged[name] = value
	}
	for name, value := range b {
		merged[name] = value
	}
	return merged
}

func TestReadinessReflectsDependencies(t *testing.T) {
	authenticate := func(*http.Request) (Principal, error) { return Principal{ID: "a"}, nil }
	healthy := false
	server, err := New(Config{Authenticate: authenticate, Ready: func(context.Context) error {
		if healthy {
			return nil
		}
		return errors.New("downstream down")
	}})
	if err != nil {
		t.Fatal(err)
	}
	probe := readinessHandler(server)
	response := httptest.NewRecorder()
	probe(response, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy dependency reported ready: %d", response.Code)
	}
	healthy = true
	response = httptest.NewRecorder()
	probe(response, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	if response.Code != http.StatusNoContent {
		t.Fatalf("healthy dependency reported unready: %d", response.Code)
	}
	plain, err := New(Config{Authenticate: authenticate})
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	readinessHandler(plain)(response, httptest.NewRequest(http.MethodGet, "/readyz", http.NoBody))
	if response.Code != http.StatusNoContent {
		t.Fatalf("default readiness probe must report ready: %d", response.Code)
	}
}

func testServeContext(t *testing.T, cert, key string) *cli.Context {
	t.Helper()
	return testServeContextWith(t, cert, key, nil)
}

func testServeContextWith(t *testing.T, cert, key string, overrides map[string]string) *cli.Context {
	t.Helper()
	set := flag.NewFlagSet("mcp-serve", flag.ContinueOnError)
	values := map[string]string{
		"tls-cert": cert, "tls-key": key,
		"api-key-namespace": "sandbox-matrix", "api-key-secret": "sandbox-apikeys",
		"gateway": "sandbox-grpc-gateway:50051", "gateway-ca": "ca.crt",
		"gateway-cert": "client.crt", "gateway-key": "client.key",
		"state-namespace": "k8e-mcp", "cert-cache": defaultCertCacheName,
		"bootstrap-api-key": "", "insecure-bootstrap": "false", "tenant": "",
	}
	for name, value := range overrides {
		values[name] = value
	}
	for name, value := range values {
		if name == "insecure-bootstrap" {
			set.Bool(name, value == "true", "")
			continue
		}
		set.String(name, value, "")
	}
	return cli.NewContext(cli.NewApp(), set, nil)
}
