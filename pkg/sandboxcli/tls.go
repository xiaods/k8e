// KIP-30 M3.4: `tls export-ca` — write the sandbox CA where MCP/E2B clients
// can trust the IP-only gateway.
//
// An IP-only install has no DNS name to match the gateway certificate, so
// clients (curl, the e2b SDK, MCP clients with custom TLS) must trust the
// sandbox CA explicitly — NODE_EXTRA_CA_CERTS, --cacert, or a system store.
// The CA lives at /var/lib/k8e/server/tls/sandbox-ca.crt on the server host;
// this command fetches it (locally, or from the cluster Secret when the CLI
// runs with a kubeconfig) and writes ~/.k8e/sandbox-ca.crt (or stdout), so the
// IP-only bootstrap becomes two commands: start MCP, export-ca.
package sandboxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/urfave/cli"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const (
	// sandboxCAFile is where the k8e server writes the sandbox CA (same path
	// the gRPC gateway itself trusts, pkg/sandboxmatrix tlsDir).
	sandboxCAFile = "/var/lib/k8e/server/tls/sandbox-ca.crt"
	// exportedCAFileName is the default destination under ~/.k8e.
	exportedCAFileName = "sandbox-ca.crt"
)

// TLSCommand groups the certificate-distribution helpers (KIP-30 M3.4;
// KIP-28's /.well-known distribution is the future non-`/.well-known`
// fallback this complements).
func TLSCommand() cli.Command {
	return cli.Command{
		Name:  "tls",
		Usage: "Sandbox TLS helpers (export the CA for client trust)",
		Subcommands: []cli.Command{
			tlsExportCACommand(),
		},
	}
}

func tlsExportCACommand() cli.Command {
	return cli.Command{
		Name:      "export-ca",
		Usage:     "Write the sandbox CA to ~/.k8e/sandbox-ca.crt (or --output / -)",
		ArgsUsage: " ",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:  "output",
				Usage: "Destination path; '-' writes to stdout. Defaults to ~/.k8e/sandbox-ca.crt",
			},
		},
		Action: func(ctx *cli.Context) error {
			caPEM, err := loadSandboxCA()
			if err != nil {
				return printErrorExit(err.Error(), 2)
			}
			output := ctx.String("output")
			if output == "-" {
				fmt.Print(string(caPEM))
				return nil
			}
			if output == "" {
				dir, err := dataDir()
				if err != nil {
					return printErrorExit("resolve home dir: "+err.Error(), 2)
				}
				output = filepath.Join(dir, exportedCAFileName)
			}
			if err := os.WriteFile(output, caPEM, 0644); err != nil {
				return printErrorExit("write CA: "+err.Error(), 2)
			}
			printJSON(map[string]any{"ok": true, "ca": output})
			return nil
		},
	}
}

// loadSandboxCA reads the sandbox CA from the local server TLS directory, or
// — when this CLI runs off-server with a kubeconfig — from the
// sandbox-matrix/sandbox-ca Secret if the cluster exposes one, falling back to
// the documented on-server path.
func loadSandboxCA() ([]byte, error) {
	if data, err := os.ReadFile(sandboxCAFile); err == nil && len(data) > 0 {
		return data, nil
	}
	if caPEM, err := loadSandboxCAFromCluster(); err == nil && len(caPEM) > 0 {
		return caPEM, nil
	}
	return nil, fmt.Errorf("sandbox CA not found: read %s (or run on the server host)", sandboxCAFile)
}

// loadSandboxCAFromCluster reads the CA from the cluster's
// sandbox-matrix/sandbox-ca ConfigMap when present. The CA is not secret — it
// exists to be distributed — so a ConfigMap is the natural carrier for
// off-server clients.
func loadSandboxCAFromCluster() ([]byte, error) {
	kubeconfig := resolveKubeconfigPath()
	if kubeconfig == "" {
		return nil, fmt.Errorf("no kubeconfig")
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	k8s, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	configMap, err := k8s.CoreV1().ConfigMaps(sandboxNamespace()).Get(context.Background(), "sandbox-ca", metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if caPEM, ok := configMap.Data["ca.crt"]; ok && caPEM != "" {
		return []byte(caPEM), nil
	}
	return nil, fmt.Errorf("sandbox-ca ConfigMap has no ca.crt")
}

// resolveKubeconfigPath mirrors the client's kubeconfig discovery order.
func resolveKubeconfigPath() string {
	if path := os.Getenv("KUBECONFIG"); path != "" {
		return path
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidate := filepath.Join(home, ".kube", "config")
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}
