package sandboxcli

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/urfave/cli"

	"github.com/xiaods/k8e/pkg/sandbox/client"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
)

// withTempStateDir redirects the sandbox data dir (state/certs) to a temp dir.
func withTempStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("K8E_SANDBOX_CERT_DIR", dir)
	return dir
}

func writeState(t *testing.T, tenant string, state *SessionState) {
	t.Helper()
	dir := stateDir(tenant)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

func TestCreatingPlaceholderStale(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	old := time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339)

	cases := []struct {
		name  string
		state *SessionState
		want  bool
	}{
		{"dead pid → stale", &SessionState{Phase: "creating", PID: 999999999, LockedAt: now}, true},
		{"alive pid + fresh → not stale", &SessionState{Phase: "creating", PID: os.Getpid(), LockedAt: now}, false},
		{"alive pid + old → stale (hung creator)", &SessionState{Phase: "creating", PID: os.Getpid(), LockedAt: old}, true},
		{"no pid + old → stale", &SessionState{Phase: "creating", LockedAt: old}, true},
		{"no pid + fresh → not stale", &SessionState{Phase: "creating", LockedAt: now}, false},
		{"no pid + no stamp → stale (broken placeholder)", &SessionState{Phase: "creating"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := creatingPlaceholderStale(c.state); got != c.want {
				t.Fatalf("creatingPlaceholderStale(%+v) = %v, want %v", c.state, got, c.want)
			}
		})
	}
}

// TestResolveSession_ReclaimsStaleCreating reproduces the tenant bug: a
// previous run crashed (or was Ctrl-C'd) after writing a "creating" placeholder
// but before finalizeState. The next run for that tenant used to wait forever;
// now the stale placeholder is reclaimed and the caller proceeds to create.
func TestResolveSession_ReclaimsStaleCreating(t *testing.T) {
	withTempStateDir(t)
	old := time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339)
	writeState(t, "default", &SessionState{Phase: "creating", PID: 999999999, LockedAt: old})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	sid, err := resolveSession(ctx, "default", "")
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if sid != "" {
		t.Fatalf("expected empty sid (caller creates), got %q", sid)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("stale-creating reclaim must be immediate, took %v", elapsed)
	}
}

// TestResolveSession_WaitsForLiveCreator proves a live creator is not trampled:
// resolution waits (bounded by ctx) instead of reclaiming a fresh placeholder.
func TestResolveSession_WaitsForLiveCreator(t *testing.T) {
	withTempStateDir(t)
	now := time.Now().UTC().Format(time.RFC3339)
	writeState(t, "default", &SessionState{Phase: "creating", PID: os.Getpid(), LockedAt: now})

	// Short deadline: resolution must NOT return immediately with a claim; it
	// waits for the creator and only gives up when the context expires.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := resolveSession(ctx, "default", "")
	if elapsed := time.Since(start); elapsed < 300*time.Millisecond {
		t.Fatalf("live-creator wait was trampled after %v", elapsed)
	}
	if err == nil {
		t.Fatal("expected context deadline error after waiting for the live creator")
	}
}

// TestResolveSession_ReusesActiveSession verifies the happy path: an active
// state is returned as-is (the multi-tenant reuse contract).
func TestResolveSession_ReusesActiveSession(t *testing.T) {
	withTempStateDir(t)
	writeState(t, "my-project", &SessionState{SessionID: "sess-1", Phase: "active", TenantID: "my-project"})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	sid, err := resolveSession(ctx, "my-project", "")
	if err != nil {
		t.Fatalf("resolveSession: %v", err)
	}
	if sid != "sess-1" {
		t.Fatalf("expected sess-1, got %q", sid)
	}
}

// TestEnsureSession_ClearStateOnCreateFailure verifies a failed create removes
// the "creating" placeholder so the next run can retry immediately.
func TestEnsureSession_ClearStateOnCreateFailure(t *testing.T) {
	withTempStateDir(t)
	// Stale placeholder from a previous crashed run — ensureSession reclaims
	// it, then the create RPC fails (fake client), and the placeholder must be
	// cleared, not left wedged.
	old := time.Now().UTC().Add(-3 * time.Minute).Format(time.RFC3339)
	writeState(t, "default", &SessionState{Phase: "creating", PID: 999999999, LockedAt: old})

	fake := &failCreatePBClient{}
	realClient := &client.Client{SandboxServiceClient: fake}

	app := cli.NewApp()
	flagSet := flag.NewFlagSet("test", flag.ContinueOnError)
	cctx := cli.NewContext(app, flagSet, nil)

	_, _, err := ensureSession(realClient, cctx)
	if err == nil {
		t.Fatal("expected create failure")
	}
	state, loadErr := loadState("default")
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if state != nil {
		t.Fatalf("placeholder must be cleared after failed create, got %+v", state)
	}
}

// failCreatePBClient satisfies the pb client surface by embedding the interface;
// only CreateSession is reachable in this test and fails with a deadline error.
type failCreatePBClient struct {
	pb.SandboxServiceClient
}

func (c *failCreatePBClient) CreateSession(ctx context.Context, req *pb.CreateSessionRequest, _ ...grpc.CallOption) (*pb.CreateSessionResponse, error) {
	return nil, context.DeadlineExceeded
}
