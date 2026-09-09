package sandboxmcp

import (
	"flag"
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
