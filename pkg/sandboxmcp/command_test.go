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
	set := flag.NewFlagSet("mcp-serve", flag.ContinueOnError)
	values := map[string]string{
		"tls-cert": cert, "tls-key": key,
		"api-key-namespace": "sandbox-matrix", "api-key-secret": "sandbox-apikeys",
		"gateway": "sandbox-grpc-gateway:50051", "gateway-ca": "ca.crt",
		"gateway-cert": "client.crt", "gateway-key": "client.key",
		"state-namespace": "k8e-mcp",
	}
	for name, value := range values {
		set.String(name, value, "")
	}
	return cli.NewContext(cli.NewApp(), set, nil)
}
