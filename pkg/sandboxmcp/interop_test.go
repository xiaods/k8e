package sandboxmcp

import (
	"context"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/sandbox/apikey"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
)

type wireBackend struct{ testBackend }

func (backend *wireBackend) PollRun(context.Context, *pb.PollRunRequest, ...grpc.CallOption) (*pb.PollRunResponse, error) {
	return &pb.PollRunResponse{Status: "completed", ExitCode: 0}, nil
}

func TestIndependentPythonClient(t *testing.T) {
	if os.Getenv("MCP_RUN_INTEROP") != "1" {
		t.Skip("set MCP_RUN_INTEROP=1 for networked Python interoperability probe")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required for independent wire interoperability")
	}
	authenticate, err := NewAPIKeyAuthenticator(&secretFixture{records: map[string]apikey.Record{"alice": {Key: "test-token"}}}, "sandbox-apikeys")
	if err != nil {
		t.Fatal(err)
	}
	store := &testStore{records: map[string]Record{}}
	backend := &wireBackend{}
	directory := t.TempDir()
	statePath := filepath.Join(directory, "state.json")
	for _, phase := range []string{"before", "after"} {
		service, err := NewService(backend, store)
		if err != nil {
			t.Fatal(err)
		}
		handler, err := New(Config{Authenticate: authenticate, Tools: service.Tools()})
		if err != nil {
			t.Fatal(err)
		}
		endpoint := httptest.NewTLSServer(handler)
		certificatePath := filepath.Join(directory, "ca.pem")
		if err := os.WriteFile(certificatePath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: endpoint.Certificate().Raw}), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		command := exec.CommandContext(ctx, python, "../../tests/mcp/client_smoke.py", "--endpoint", endpoint.URL, "--ca", certificatePath, "--state", statePath, "--phase", phase)
		command.Env = append(os.Environ(), "MCP_TEST_API_KEY=test-token")
		output, err := command.CombinedOutput()
		cancel()
		endpoint.Close()
		t.Logf("%s", output)
		if err != nil {
			t.Fatalf("%s client: %v", phase, err)
		}
	}
	if backend.creates != 1 || backend.execs != 1 {
		t.Fatalf("replayed backend mutations: create=%d exec=%d", backend.creates, backend.execs)
	}
}
