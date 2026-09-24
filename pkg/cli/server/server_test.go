package server

import (
	"strings"
	"testing"

	cmds "github.com/xiaods/k8e/pkg/cli/cmds"
)

// TestTandemDefaultEndpointIsTLS pins the scheme the Tandem backend hands the
// apiserver. Tandem's etcd port requires the apiserver's client certificate, so
// an http:// endpoint makes the apiserver begin a plaintext gRPC handshake at a
// TLS listener. The connection dies with "error reading server preface: EOF",
// which names neither the port nor the cause, and the control plane never comes
// up — the E2E failure this default caused.
func TestTandemDefaultEndpointIsTLS(t *testing.T) {
	for name, tc := range map[string]struct {
		backend  string
		endpoint string
		want     string
	}{
		"tandem gets the loopback default": {backend: "tandem", want: "https://127.0.0.1:2379"},
		"operator endpoint is untouched":   {backend: "tandem", endpoint: "https://10.0.0.5:2379", want: "https://10.0.0.5:2379"},
		"explicit http is respected":       {backend: "tandem", endpoint: "http://127.0.0.1:2379", want: "http://127.0.0.1:2379"},
		"other backends get no default":    {backend: "etcd", want: ""},
		"empty backend is left alone":      {backend: "", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &cmds.Server{DatastoreBackend: tc.backend, DatastoreEndpoint: tc.endpoint}
			if got := tandemDefaultEndpoint(cfg); got != tc.want {
				t.Fatalf("endpoint = %q, want %q", got, tc.want)
			}
			// The one thing that must never happen is a defaulting to
			// plaintext for a port that demands a client certificate.
			if tc.endpoint == "" && tc.backend == "tandem" && !strings.HasPrefix(tandemDefaultEndpoint(cfg), "https://") {
				t.Fatal("the Tandem default endpoint must use https://")
			}
		})
	}
}
