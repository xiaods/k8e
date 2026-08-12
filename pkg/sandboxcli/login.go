package sandboxcli

import (
	"fmt"
	"os"
	"strings"

	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/sandbox/client"
)

// LoginCommand returns the "login" subcommand for obtaining an mTLS client certificate.
func LoginCommand() cli.Command {
	return cli.Command{
		Name:  "login",
		Usage: "Authenticate to a K8E sandbox gateway and obtain an mTLS client certificate",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "device-name", Usage: "Device name for audit logging (default: hostname)"},
		},
		Action: func(ctx *cli.Context) error {
			deviceFlag := strings.TrimSpace(ctx.String("device-name"))
			resolved, err := ResolveConn(ctx.GlobalString("endpoint"), ctx.GlobalString("apikey"), ctx.GlobalString("profile"), deviceFlag)
			if err != nil {
				return printErrorExit(err.Error(), 1)
			}
			ApplyResolvedConn(resolved)
			endpoint := resolved.Endpoint
			apikey := resolved.APIKey

			if endpoint == "" {
				return printErrorExit("--endpoint is required for login (e.g. sandbox.example.com:50051), or set a profile", 1)
			}
			if apikey == "" {
				return printErrorExit("--apikey is required for login (or K8E_SANDBOX_APIKEY)", 1)
			}

			c, err := client.NewClientWithEndpoint(endpoint, apikey)
			if err != nil {
				return printErrorExit("login failed: "+err.Error(), 1)
			}
			c.Close()

			// Persist endpoint so later CLI calls (and connect --skip-skill re-runs)
			// reuse the same remote gateway without re-passing flags.
			if err := SaveConnectionConfig(&ConnectionConfig{Mode: "remote", Endpoint: endpoint}); err != nil {
				fmt.Fprintf(os.Stderr, "⚠ could not save connection config: %v\n", err)
			}

			fmt.Fprintf(os.Stderr, "✓ Logged in to %s\n", endpoint)
			if resolved.Profile != "" {
				fmt.Fprintf(os.Stderr, "  Profile:  %s\n", resolved.Profile)
			}
			fmt.Fprintf(os.Stderr, "  Credentials stored in ~/.k8e/sandbox/ (override with K8E_SANDBOX_CERT_DIR / profile cert_dir)\n")
			fmt.Fprintf(os.Stderr, "  Certificate valid for 90 days (auto-renewed when <30 days remain)\n")
			fmt.Fprintf(os.Stderr, "  Tip: use 'k8e-sandbox-cli connect' to also install /k8e-sandbox skills\n")
			return nil
		},
	}
}
