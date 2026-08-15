package cmds

import (
	"github.com/urfave/cli"
)

// E2BServerConfig holds the `k8e e2b-server` configuration.
type E2BServerConfig struct {
	Listen              string
	Endpoint            string
	APIKey              string
	NodeID              string
	SigningSecret       string
	DefaultCPUs         int
	DefaultMemoryMB     int
	DefaultDiskMB       int
	AllowedRuntimeClasses cli.StringSlice
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
			Usage:       "(e2b) API key to authenticate to the gateway and accept as the E2B API key (K8E_SANDBOX_APIKEY)",
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
			Name:  "runtime",
			Usage: "(e2b) Allowed runtime-class templateIDs (repeatable; default gvisor,kata,firecracker)",
			Value: &E2BServer.AllowedRuntimeClasses,
			EnvVar: "K8E_E2B_RUNTIMES",
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
