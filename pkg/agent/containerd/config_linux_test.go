package containerd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/rancher/wharfie/pkg/registries"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/xiaods/k8e/pkg/agent/templates"
	"github.com/xiaods/k8e/pkg/daemons/config"
)

// stripComments drops TOML comments, so that a statement about what the template
// renders cannot be satisfied or broken by prose alone.
func stripComments(config string) string {
	var directives []string
	for _, line := range strings.Split(config, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			directives = append(directives, line)
		}
	}
	return strings.Join(directives, "\n")
}

// The firecracker branch of the containerd config template is only taken on hosts
// that expose /dev/kvm, so Test_UnitGetHostConfigs never renders it. It is worth
// covering explicitly: the config it used to render made containerd panic before
// it could start a single container.
func Test_UnitContainerdConfigFirecrackerRuntime(t *testing.T) {
	nodeConfig := &config.Node{
		Containerd: config.Containerd{
			Registry: filepath.Join(t.TempDir(), "hosts.d"),
		},
		AgentConfig: config.Agent{
			ImageServiceSocket: "containerd-stargz-grpc.sock",
			Snapshotter:        "stargz",
		},
	}
	containerdConfig := templates.ContainerdConfig{
		NodeConfig:            nodeConfig,
		PrivateRegistryConfig: &registries.Registry{},
		SandboxRuntimes:       templates.SandboxRuntimeConfig{Firecracker: true},
		Program:               "k8e",
	}

	rendered, err := templates.ParseTemplateFromConfig(templates.ContainerdConfigTemplate, containerdConfig)
	require.NoError(t, err, "ParseTemplateFromConfig")

	// Check that the branch under test is actually taken, so that the assertions
	// below cannot pass just because the runtime stopped being rendered at all.
	assert.Contains(t, rendered, `runtimes."firecracker"`)
	assert.Contains(t, rendered, `runtime_type = "aws.firecracker"`)

	// containerd already links in its own devmapper snapshotter plugin, and
	// declaring a second plugin with the same id makes it panic on startup:
	//   panic: io.containerd.snapshotter.v1.devmapper: plugin: id already registered
	// A [proxy_plugins.devmapper] entry must therefore never be rendered.
	directives := stripComments(rendered)
	assert.NotContains(t, directives, "proxy_plugins")
	assert.NotContains(t, directives, "devmapper")
}
