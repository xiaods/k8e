//go:build mcp_integration

package sandboxmcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	gateway "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestGatewayWithKubernetesMock(t *testing.T) {
	var submissions atomic.Int32
	var stored string
	var files sync.Mutex
	sandboxd := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		var result any
		switch {
		case request.URL.Path == "/exec/background":
			submissions.Add(1)
			result = map[string]any{"status": "started"}
		case strings.HasPrefix(request.URL.Path, "/exec/background/"):
			result = map[string]any{"status": "completed", "exit_code": 0, "stdout": "mock-ok"}
		case request.URL.Path == "/files/write":
			var body struct{ Content string }
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			files.Lock()
			stored = body.Content
			files.Unlock()
			result = map[string]any{"ok": true}
		case request.URL.Path == "/files/read":
			files.Lock()
			content := stored
			files.Unlock()
			result = map[string]any{"content": base64.StdEncoding.EncodeToString([]byte(content)), "encoding": "base64"}
		case request.URL.Path == "/files/list":
			result = map[string]any{"files": []any{map[string]any{"path": "/workspace/test", "type": "file"}}}
		default:
			result = map[string]any{"status": "ready"}
		}
		_ = json.NewEncoder(writer).Encode(result)
	})}
	listener, err := net.Listen("tcp4", "127.0.0.1:2024")
	if err != nil {
		t.Fatal("run in an isolated container with port 2024 free:", err)
	}
	go sandboxd.Serve(listener)
	t.Cleanup(func() { _ = sandboxd.Close() })

	core := kubefake.NewSimpleClientset(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "mock-node"}, Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")}}},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "warm", Namespace: "sandbox-matrix", Labels: map[string]string{"sandbox.k8e.io/state": "warm", "sandbox.k8e.io/runtime-class": "gvisor"}}, Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "127.0.0.1", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sandbox-apikeys", Namespace: "sandbox-matrix", UID: "mock-secret"}, Data: map[string][]byte{"keys.json": []byte(`{"alice":"alice-key","bob":"bob-key"}`)}},
	)
	installConfigMapCAS(core)
	dynamic := dynfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		{Group: "k8e.sh", Version: "v1alpha1", Resource: "sandboxsessions"}:    "SandboxSessionList",
		{Group: "k8e.sh", Version: "v1alpha1", Resource: "sandboxmatrices"}:    "SandboxMatrixList",
		{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}: "CiliumNetworkPolicyList",
	})
	backend := gateway.NewServer(gateway.ServerConfig{K8s: core, Dyn: dynamic})
	transport := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	pb.RegisterSandboxServiceServer(grpcServer, backend)
	go grpcServer.Serve(transport)
	t.Cleanup(grpcServer.Stop)
	connection, err := grpc.NewClient("passthrough:///mock-gateway", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return transport.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	store, _ := NewKubernetesStore(core.CoreV1().ConfigMaps("mcp-state"))
	authenticate, _ := NewAPIKeyAuthenticator(core.CoreV1().Secrets("sandbox-matrix"), "sandbox-apikeys")
	newHandler := func() *Server {
		service, err := NewService(pb.NewSandboxServiceClient(connection), store)
		if err != nil {
			t.Fatal(err)
		}
		handler, err := New(Config{Authenticate: authenticate, Tools: service.Tools()})
		if err != nil {
			t.Fatal(err)
		}
		return handler
	}
	handler := newHandler()
	call := func(key, name string, arguments map[string]any, rejected bool) map[string]any {
		t.Helper()
		request := newRequest("tools/call", arguments)
		var body map[string]any
		_ = json.NewDecoder(request.Body).Decode(&body)
		body["params"].(map[string]any)["name"] = name
		encoded, _ := json.Marshal(body)
		updated := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(encoded)))
		updated.Header = request.Header
		updated.Header.Set("Mcp-Name", name)
		updated.Header.Set("Authorization", "Bearer "+key)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, updated)
		var envelope struct {
			Result struct {
				IsError bool           `json:"isError"`
				Data    map[string]any `json:"structuredContent"`
			}
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatal(err)
		}
		if response.Code != 200 || envelope.Result.IsError != rejected {
			t.Fatalf("%s: %s", name, response.Body.String())
		}
		return envelope.Result.Data
	}
	created := call("alice-key", "sandbox_create", map[string]any{"operation_id": "create"}, false)
	session := created["session_id"]
	if session == nil {
		t.Fatal(created)
	}
	call("alice-key", "sandbox_get", map[string]any{"session_id": session}, false)
	call("bob-key", "sandbox_get", map[string]any{"session_id": session}, true)
	call("alice-key", "sandbox_write", map[string]any{"session_id": session, "operation_id": "write", "path": "/workspace/test", "content": "payload"}, false)
	read := call("alice-key", "sandbox_read", map[string]any{"session_id": session, "path": "/workspace/test"}, false)
	if read["content"] != base64.StdEncoding.EncodeToString([]byte("payload")) {
		t.Fatal(read)
	}
	call("alice-key", "sandbox_list", map[string]any{"session_id": session}, false)
	execArgs := map[string]any{"session_id": session, "operation_id": "exec", "command": "echo mock-ok"}
	submitted := call("alice-key", "sandbox_exec", execArgs, false)
	if submitted["run_id"] == nil {
		t.Fatal(submitted)
	}
	handler = newHandler()
	replayed := call("alice-key", "sandbox_exec", execArgs, false)
	if replayed["run_id"] != submitted["run_id"] || submissions.Load() != 1 {
		 t.Fatal("duplicate execution after MCP reconstruction")
	}
	call("alice-key", "sandbox_operation", map[string]any{"operation_kind": "sandbox_exec", "operation_id": "exec"}, false)
	call("alice-key", "sandbox_poll", map[string]any{"run_id": submitted["run_id"]}, false)
	call("bob-key", "sandbox_poll", map[string]any{"run_id": submitted["run_id"]}, true)
	call("alice-key", "sandbox_destroy", map[string]any{"session_id": session, "operation_id": "destroy"}, false)
}

func installConfigMapCAS(client *kubefake.Clientset) {
	var lock sync.Mutex
	var version int
	resourceID := corev1.SchemeGroupVersion.WithResource("configmaps")
	client.PrependReactor("*", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		lock.Lock()
		defer lock.Unlock()
		var configMap *corev1.ConfigMap
		switch action.GetVerb() {
		case "create":
			configMap = action.(ktesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		case "update":
			configMap = action.(ktesting.UpdateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		default:
			return false, nil, nil
		}
		previous, err := client.Tracker().Get(resourceID, action.GetNamespace(), configMap.Name)
		if action.GetVerb() == "create" && err == nil {
			return true, nil, apierrors.NewAlreadyExists(resourceID.GroupResource(), configMap.Name)
		}
		if action.GetVerb() == "update" {
			if err != nil {
				return true, nil, err
			}
			if previous.(*corev1.ConfigMap).ResourceVersion != configMap.ResourceVersion {
				return true, nil, apierrors.NewConflict(resourceID.GroupResource(), configMap.Name, fmt.Errorf("stale version"))
			}
		}
		version++
		configMap.ResourceVersion = fmt.Sprint(version)
		if action.GetVerb() == "create" {
			err = client.Tracker().Create(resourceID, configMap, action.GetNamespace())
		} else {
			err = client.Tracker().Update(resourceID, configMap, action.GetNamespace())
		}
		return true, configMap, err
	})
}
