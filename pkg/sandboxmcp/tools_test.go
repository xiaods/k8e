package sandboxmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
)

type testStore struct {
	sync.Mutex
	records    map[string]Record
	failUpdate bool
}

func (store *testStore) Create(_ context.Context, key string, record Record) (Record, error) {
	store.Lock()
	defer store.Unlock()
	if _, ok := store.records[key]; ok {
		return Record{}, ErrRecordExists
	}
	record.Version = "1"
	store.records[key] = record
	return record, nil
}
func (store *testStore) Get(_ context.Context, key string) (Record, error) {
	store.Lock()
	defer store.Unlock()
	record, ok := store.records[key]
	if !ok {
		return Record{}, ErrRecordMissing
	}
	return record, nil
}
func (store *testStore) Update(_ context.Context, key string, record Record) error {
	store.Lock()
	defer store.Unlock()
	if store.failUpdate {
		return errors.New("storage unavailable")
	}
	previous, ok := store.records[key]
	if !ok || previous.Version != record.Version {
		return errors.New("conflict")
	}
	record.Version = "2"
	store.records[key] = record
	return nil
}

type testBackend struct {
	pb.SandboxServiceClient
	sync.Mutex
	creates, execs, polls int
	lostReply             bool
}

func (backend *testBackend) CreateSession(_ context.Context, request *pb.CreateSessionRequest, _ ...grpc.CallOption) (*pb.CreateSessionResponse, error) {
	backend.Lock()
	defer backend.Unlock()
	backend.creates++
	return &pb.CreateSessionResponse{SessionId: request.SessionId}, nil
}
func (backend *testBackend) Exec(_ context.Context, request *pb.ExecRequest, _ ...grpc.CallOption) (*pb.ExecResponse, error) {
	backend.Lock()
	defer backend.Unlock()
	backend.execs++
	if !request.Background {
		return nil, errors.New("expected background execution")
	}
	if backend.lostReply {
		return nil, errors.New("reply lost after submission")
	}
	return &pb.ExecResponse{RunId: fmt.Sprintf("run-%d", backend.execs), SessionId: request.SessionId, Status: "started"}, nil
}
func (backend *testBackend) PollRun(_ context.Context, _ *pb.PollRunRequest, _ ...grpc.CallOption) (*pb.PollRunResponse, error) {
	backend.Lock()
	defer backend.Unlock()
	backend.polls++
	return &pb.PollRunResponse{Status: "completed", ExitCode: 7}, nil
}

func invoke(t *testing.T, service *Service, owner, name string, input any) (CallResult, error) {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range service.Tools() {
		if tool.Name == name {
			return tool.Call(context.Background(), Principal{ID: owner}, raw)
		}
	}
	t.Fatalf("missing tool %s", name)
	return CallResult{}, nil
}
func resultObject(t *testing.T, result CallResult) map[string]any {
	t.Helper()
	raw, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var output map[string]any
	if err = json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	return output
}
func serviceFixture(t *testing.T) (*Service, *testStore, *testBackend, string) {
	t.Helper()
	store := &testStore{records: map[string]Record{}}
	backend := &testBackend{}
	service, err := NewService(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	result, err := invoke(t, service, "alice", "sandbox_create", map[string]any{"operation_id": "create-1"})
	if err != nil {
		t.Fatal(err)
	}
	return service, store, backend, resultObject(t, result)["session_id"].(string)
}

func TestSharedStoreDeduplicatesAcrossReplicas(t *testing.T) {
	service, store, backend, sessionID := serviceFixture(t)
	other, _ := NewService(backend, store)
	var group sync.WaitGroup
	for index := 0; index < 32; index++ {
		group.Add(1)
		go func(index int) {
			defer group.Done()
			selected := service
			if index%2 == 0 {
				selected = other
			}
			_, err := invoke(t, selected, "alice", "sandbox_exec", map[string]any{"session_id": sessionID, "operation_id": "exec-1", "command": "echo hello"})
			if err != nil {
				t.Error(err)
			}
		}(index)
	}
	group.Wait()
	if backend.execs != 1 {
		t.Fatalf("executed %d times", backend.execs)
	}
	result, err := invoke(t, other, "alice", "sandbox_operation", map[string]any{"operation_kind": "sandbox_exec", "operation_id": "exec-1"})
	if err != nil {
		t.Fatal(err)
	}
	if resultObject(t, result)["run_id"] != "run-1" {
		t.Fatal(result)
	}
	_, err = invoke(t, other, "alice", "sandbox_exec", map[string]any{"session_id": sessionID, "operation_id": "exec-1", "command": "different"})
	var invalid *InvalidParams
	if !errors.As(err, &invalid) {
		t.Fatalf("expected idempotency conflict: %v", err)
	}
}

func TestOwnershipPrecedesEveryBackendOperation(t *testing.T) {
	service, _, backend, sessionID := serviceFixture(t)
	for _, entry := range []struct {
		name string
		args map[string]any
	}{
		{"sandbox_get", map[string]any{"session_id": sessionID}},
		{"sandbox_exec", map[string]any{"session_id": sessionID, "operation_id": "x", "command": "ls"}},
		{"sandbox_destroy", map[string]any{"session_id": sessionID, "operation_id": "x"}},
		{"sandbox_read", map[string]any{"session_id": sessionID, "path": "/workspace/a"}},
		{"sandbox_write", map[string]any{"session_id": sessionID, "path": "/workspace/a", "operation_id": "x", "content": ""}},
		{"sandbox_list", map[string]any{"session_id": sessionID}},
	} {
		if _, err := invoke(t, service, "bob", entry.name, entry.args); err == nil {
			t.Errorf("%s allowed another owner", entry.name)
		}
	}
	_, err := invoke(t, service, "alice", "sandbox_get", map[string]any{"session_id": "legacy-unowned"})
	if err == nil {
		t.Fatal("legacy resource accepted")
	}
	_, err = invoke(t, service, "alice", "sandbox_exec", map[string]any{"session_id": sessionID, "operation_id": "x", "command": "ls"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = invoke(t, service, "bob", "sandbox_poll", map[string]any{"run_id": "run-1"}); err == nil {
		t.Fatal("cross-owner polling accepted")
	}
	if _, err = invoke(t, service, "bob", "sandbox_operation", map[string]any{"operation_id": "x", "operation_kind": "sandbox_exec"}); err == nil {
		t.Fatal("cross-owner operation accepted")
	}
	if backend.polls != 0 {
		t.Fatal("unauthorized poll reached backend")
	}
	result, err := invoke(t, service, "alice", "sandbox_poll", map[string]any{"run_id": "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if resultObject(t, result)["exit_code"] != float64(7) {
		t.Fatal(result)
	}
}

func TestUnknownSubmissionNeverRunsAgain(t *testing.T) {
	for _, failure := range []string{"backend", "store"} {
		t.Run(failure, func(t *testing.T) {
			service, store, backend, sessionID := serviceFixture(t)
			if failure == "backend" {
				backend.lostReply = true
			} else {
				store.failUpdate = true
			}
			args := map[string]any{"session_id": sessionID, "operation_id": "uncertain", "command": "side-effect"}
			result, err := invoke(t, service, "alice", "sandbox_exec", args)
			if err != nil {
				t.Fatal(err)
			}
			if resultObject(t, result)["status"] != "unknown" {
				t.Fatal(result)
			}
			store.failUpdate = false
			backend.lostReply = false
			restarted, _ := NewService(backend, store)
			result, err = invoke(t, restarted, "alice", "sandbox_exec", args)
			if err != nil {
				t.Fatal(err)
			}
			if backend.execs != 1 || resultObject(t, result)["status"] != "unknown" {
				t.Fatalf("unknown operation resubmitted: %d", backend.execs)
			}
		})
	}
}

func TestRejectForgedAndOutOfRangeArguments(t *testing.T) {
	service, _, backend, sessionID := serviceFixture(t)
	for _, extra := range []map[string]any{{"owner": "alice"}, {"tenant_id": "alice"}, {"endpoint": "attacker"}, {"timeout": 0}, {"timeout": 3601}, {"timeout": nil}} {
		args := map[string]any{"session_id": sessionID, "operation_id": "x", "command": "ls"}
		for key, value := range extra {
			args[key] = value
		}
		if _, err := invoke(t, service, "alice", "sandbox_exec", args); err == nil {
			t.Fatalf("accepted %v", extra)
		}
	}
	if backend.execs != 0 {
		t.Fatal("invalid args executed")
	}
}
