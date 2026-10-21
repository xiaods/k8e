package e2bserver

import (
	"testing"

	"github.com/urfave/cli"

	"github.com/xiaods/k8e/pkg/cli/cmds"
)

// TestE2BServerStateStoreFlags pins the CLI surface of the CRD-backed state
// store: the standalone e2b-server must be able to reach the cluster, and
// the in-memory registry must stay an explicit opt-in.
func TestE2BServerStateStoreFlags(t *testing.T) {
	flags := map[string]bool{}
	for _, f := range cmds.E2BServerFlags {
		name := f.GetName()
		if name == "" {
			continue
		}
		flags[name] = true
	}

	for _, name := range []string{"kubeconfig", "sandbox-namespace", "in-memory-state"} {
		if !flags[name] {
			t.Fatalf("the %q flag is missing from the e2b-server command", name)
		}
	}

	// The in-memory store must default to off: it loses the E2B bookkeeping
	// on restart and is not shared between nodes.
	if cmds.E2BServer.InMemoryState {
		t.Fatalf("the in-memory state store must not be the default")
	}
}

// TestE2BServerStateStoreFlagsParse checks the flags actually reach the
// config, including the env-var form.
func TestE2BServerStateStoreFlagsParse(t *testing.T) {
	// Save and restore the shared config: it is a package-level var.
	origKubeconfig, origNamespace, origInMemory := cmds.E2BServer.Kubeconfig, cmds.E2BServer.Namespace, cmds.E2BServer.InMemoryState
	t.Cleanup(func() {
		cmds.E2BServer.Kubeconfig, cmds.E2BServer.Namespace, cmds.E2BServer.InMemoryState = origKubeconfig, origNamespace, origInMemory
	})

	t.Setenv("K8E_E2B_KUBECONFIG", "/tmp/kubeconfig-from-env")
	t.Setenv("K8E_E2B_SANDBOX_NAMESPACE", "sandbox-ns-from-env")
	t.Setenv("K8E_E2B_IN_MEMORY_STATE", "true")

	app := cli.NewApp()
	app.Commands = []cli.Command{cmds.NewE2BServerCommand(func(*cli.Context) error { return nil })}
	if err := app.Run([]string{"k8e", "e2b-server"}); err != nil {
		t.Fatalf("run e2b-server: %v", err)
	}

	if cmds.E2BServer.Kubeconfig != "/tmp/kubeconfig-from-env" {
		t.Fatalf("Kubeconfig = %q, want the env value", cmds.E2BServer.Kubeconfig)
	}
	if cmds.E2BServer.Namespace != "sandbox-ns-from-env" {
		t.Fatalf("Namespace = %q, want the env value", cmds.E2BServer.Namespace)
	}
	if !cmds.E2BServer.InMemoryState {
		t.Fatalf("InMemoryState = false, want true from the env var")
	}
}
