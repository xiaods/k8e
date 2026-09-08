package sandboxmcp

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/clientcmd"
)

func TestKubernetesRestartRecovery(t *testing.T) {
	configPath := os.Getenv("MCP_TEST_KUBECONFIG")
	if configPath == "" {
		t.Skip("set MCP_TEST_KUBECONFIG to run against a real Kubernetes API")
	}
	core := recoveryCoreClient(t, configPath)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	phase := os.Getenv("MCP_TEST_PHASE")
	if phase == "" {
		runRecoveryCoordinator(t, ctx, core)
		return
	}
	runRecoveryChild(t, ctx, core, phase, os.Getenv("MCP_TEST_NAMESPACE"))
}

func recoveryCoreClient(t *testing.T, configPath string) typedcore.CoreV1Interface {
	t.Helper()
	config, err := clientcmd.BuildConfigFromFlags("", configPath)
	if err != nil {
		t.Fatal(err)
	}
	config.Timeout = 10 * time.Second
	core, err := typedcore.NewForConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	return core
}

func runRecoveryCoordinator(t *testing.T, ctx context.Context, core typedcore.CoreV1Interface) {
	t.Helper()
	namespace, err := core.Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "mcp-recovery-test-"}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { deleteRecoveryNamespace(t, core, namespace.Name) })
	verifyConfigMapCAS(t, ctx, core.ConfigMaps(namespace.Name))
	runRecoveryProcess(t, ctx, "submit", namespace.Name)
	restartRecoveryContainer(t, ctx, core, namespace.Name)
	runRecoveryProcess(t, ctx, "recover", namespace.Name)
}

func deleteRecoveryNamespace(t *testing.T, core typedcore.CoreV1Interface, namespace string) {
	cleanup, done := context.WithTimeout(context.Background(), 15*time.Second)
	defer done()
	if err := core.Namespaces().Delete(cleanup, namespace, metav1.DeleteOptions{}); err != nil {
		t.Errorf("cleanup namespace %s: %v", namespace, err)
	}
}

func verifyConfigMapCAS(t *testing.T, ctx context.Context, maps typedcore.ConfigMapInterface) {
	t.Helper()
	store, _ := NewKubernetesStore(maps)
	record, err := store.Create(ctx, "cas-probe", Record{Owner: "probe", State: "unknown"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, "cas-probe", Record{Owner: "probe", State: "unknown"}); !errors.Is(err, ErrRecordExists) {
		t.Fatalf("atomic admission: %v", err)
	}
	updated := record
	updated.State = "admitted"
	if err := store.Update(ctx, "cas-probe", updated); err != nil {
		t.Fatal(err)
	}
	record.State = "complete"
	if err := store.Update(ctx, "cas-probe", record); !apierrors.IsConflict(err) {
		t.Fatalf("stale update must conflict: %v", err)
	}
}

func runRecoveryProcess(t *testing.T, ctx context.Context, phase, namespace string) {
	t.Helper()
	process := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestKubernetesRestartRecovery$", "-test.v")
	process.Env = append(os.Environ(), "MCP_TEST_PHASE="+phase, "MCP_TEST_NAMESPACE="+namespace)
	output, err := process.CombinedOutput()
	t.Logf("%s process: %s", phase, output)
	if err != nil {
		t.Fatalf("%s: %v", phase, err)
	}
}

func restartRecoveryContainer(t *testing.T, ctx context.Context, core typedcore.CoreV1Interface, namespace string) {
	t.Helper()
	container := os.Getenv("MCP_TEST_RESTART_CONTAINER")
	if container == "" {
		return
	}
	restart := exec.CommandContext(ctx, "docker", "restart", container)
	if output, err := restart.CombinedOutput(); err != nil {
		t.Fatalf("restart K8E: %v: %s", err, output)
	}
	for {
		if _, err := core.Namespaces().Get(ctx, namespace, metav1.GetOptions{}); err == nil {
			t.Log("K8E container restarted; persisted test namespace recovered")
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("K8E API did not recover after restart")
		case <-time.After(time.Second):
		}
	}
}

func runRecoveryChild(t *testing.T, ctx context.Context, core typedcore.CoreV1Interface, phase, namespace string) {
	t.Helper()
	if phase != "submit" && phase != "recover" {
		t.Fatal("invalid test phase")
	}
	if namespace == "" {
		t.Fatal("missing test namespace")
	}
	store, _ := NewKubernetesStore(core.ConfigMaps(namespace))
	backend := &testBackend{}
	service, err := NewService(backend, store)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := submitRecoveryOperations(t, ctx, service, backend)
	if phase == "recover" {
		assertRecoveredOperations(t, ctx, service, backend)
	} else if backend.creates != 1 || backend.execs != 2 || sessionID == "" {
		t.Fatal("submission did not reach backend")
	}
}

func submitRecoveryOperations(t *testing.T, ctx context.Context, service *Service, backend *testBackend) string {
	t.Helper()
	created, err := invoke(t, service, "alice", "sandbox_create", map[string]any{"operation_id": "create"})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := resultObject(t, created)["session_id"].(string)
	result, err := invoke(t, service, "alice", "sandbox_exec", map[string]any{"session_id": sessionID, "operation_id": "completed", "command": "echo ok"})
	if err != nil || resultObject(t, result)["run_id"] != "run-1" {
		t.Fatalf("completed recovery: %v %v", result, err)
	}
	backend.lostReply = true
	result, err = invoke(t, service, "alice", "sandbox_exec", map[string]any{"session_id": sessionID, "operation_id": "unknown", "command": "echo uncertain"})
	if err != nil || resultObject(t, result)["status"] != "unknown" {
		t.Fatalf("unknown recovery: %v %v", result, err)
	}
	return sessionID
}

func assertRecoveredOperations(t *testing.T, ctx context.Context, service *Service, backend *testBackend) {
	t.Helper()
	if backend.creates != 0 || backend.execs != 0 {
		t.Fatal("restart replayed a backend mutation")
	}
	if _, err := invoke(t, service, "bob", "sandbox_poll", map[string]any{"run_id": "run-1"}); err == nil {
		t.Fatal("ownership lost on restart")
	}
	if _, err := invoke(t, service, "alice", "sandbox_poll", map[string]any{"run_id": "run-1"}); err != nil {
		t.Fatal(err)
	}
}
