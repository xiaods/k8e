package cmds

import (
	"github.com/urfave/cli"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

// E2BServerConfig holds the `k8e e2b-server` configuration.
type E2BServerConfig struct {
	Listen                string
	Endpoint              string
	APIKey                string
	NodeID                string
	SigningSecret         string
	DefaultCPUs           int
	DefaultMemoryMB       int
	DefaultDiskMB         int
	AllowedRuntimeClasses cli.StringSlice
	// Kubeconfig is the kubeconfig used to reach the cluster for the
	// CRD-backed E2B state store. Empty means the default resolution.
	Kubeconfig string
	// Namespace holds the SandboxSession CRDs the state store reads and
	// writes. Empty means DefaultSandboxNamespace.
	Namespace string
	// InMemoryState allows the in-memory E2B state store as a fallback when
	// the cluster is unreachable. Off by default: the in-memory store loses
	// the E2B bookkeeping on restart and is not shared between nodes.
	InMemoryState bool
}

var (
	// E2BServer is the runtime config for `k8e e2b-server`.
	E2BServer E2BServerConfig

	// E2BServerFlags are the flags for `k8e e2b-server`.
	E2BServerFlags = []cli.Flag{
		cli.StringFlag{
			Name:        "listen",
			Usage:       "(e2b) HTTP listen address",
			Value:       "127.0.0.1:3676",
			Destination: &E2BServer.Listen,
			EnvVar:      "K8E_E2B_LISTEN",
		},
		cli.StringFlag{
			Name:        "endpoint",
			Usage:       "(e2b) Sandbox gRPC gateway endpoint (default: local auto-discovery)",
			Destination: &E2BServer.Endpoint,
			EnvVar:      "K8E_SANDBOX_ENDPOINT",
		},
		cli.StringFlag{
			Name:        "apikey",
			Usage:       "(e2b) The single API key for this deployment (K8E_SANDBOX_APIKEY): it is accepted from e2b SDK clients (as \"e2b_\"+key) and used to log in to the sandbox gRPC gateway, so the same key must be provisioned in the sandbox-apikeys Secret. Generate it with `k8e sandbox-apikey create <name>` — keys are 64 hex characters, which is exactly what official e2b SDKs require (client-side validateApiKey /^e2b_[0-9a-f]+$/) — then pass \"e2b_\"+key to the SDK; the server strips the prefix",
			Destination: &E2BServer.APIKey,
			EnvVar:      "K8E_SANDBOX_APIKEY",
		},
		cli.StringFlag{
			Name:        "node-id",
			Usage:       "(e2b) Value reported as clientID in session views",
			Value:       "k8e",
			Destination: &E2BServer.NodeID,
			EnvVar:      "K8E_E2B_NODE_ID",
		},
		cli.StringFlag{
			Name:        "signing-secret",
			Usage:       "(e2b) Secret keying envd access tokens and signed file URLs (K8E_E2B_SIGNING_SECRET; falls back to the server sandbox CA key)",
			Destination: &E2BServer.SigningSecret,
			EnvVar:      "K8E_E2B_SIGNING_SECRET",
		},
		cli.IntFlag{
			Name:        "default-cpus",
			Usage:       "(e2b) CPU count reported in info views",
			Value:       1,
			Destination: &E2BServer.DefaultCPUs,
			EnvVar:      "K8E_E2B_DEFAULT_CPUS",
		},
		cli.IntFlag{
			Name:        "default-memory",
			Usage:       "(e2b) Memory in MiB reported in info views",
			Value:       512,
			Destination: &E2BServer.DefaultMemoryMB,
			EnvVar:      "K8E_E2B_DEFAULT_MEMORY",
		},
		cli.IntFlag{
			Name:        "default-disk",
			Usage:       "(e2b) Disk size in MiB reported in info views",
			Value:       10 * 1024,
			Destination: &E2BServer.DefaultDiskMB,
			EnvVar:      "K8E_E2B_DEFAULT_DISK",
		},
		cli.StringSliceFlag{
			Name:   "runtime",
			Usage:  "(e2b) Allowed runtime-class templateIDs (repeatable; default gvisor,kata,firecracker)",
			Value:  &E2BServer.AllowedRuntimeClasses,
			EnvVar: "K8E_E2B_RUNTIMES",
		},
		cli.StringFlag{
			Name:        "kubeconfig",
			Usage:       "(e2b) Kubeconfig used to reach the cluster for the CRD-backed E2B state store (default: the in-cluster config, then the standard kubeconfig resolution)",
			Destination: &E2BServer.Kubeconfig,
			EnvVar:      "K8E_E2B_KUBECONFIG",
		},
		cli.StringFlag{
			Name:        "sandbox-namespace",
			Usage:       "(e2b) Namespace holding the SandboxSession CRDs used by the E2B state store",
			Value:       config.DefaultSandboxNamespace,
			Destination: &E2BServer.Namespace,
			EnvVar:      "K8E_E2B_SANDBOX_NAMESPACE",
		},
		cli.BoolFlag{
			Name:        "in-memory-state",
			Usage:       "(e2b) Allow the in-memory E2B state store as a fallback when the cluster is unreachable. The E2B bookkeeping (auto-pause, the metadata.name index, the E2B deadline) is then lost on restart and is not shared between nodes",
			Destination: &E2BServer.InMemoryState,
			EnvVar:      "K8E_E2B_IN_MEMORY_STATE",
		},
	}
)

// NewE2BServerCommand returns the `k8e e2b-server` command.
func NewE2BServerCommand(action func(*cli.Context) error) cli.Command {
	return cli.Command{
		Name:      "e2b-server",
		Usage:     "Run the E2B-compatible sandbox HTTP server (KIP-18)",
		UsageText: appName + " e2b-server [OPTIONS]",
		Action:    action,
		Flags:     E2BServerFlags,
	}
}
