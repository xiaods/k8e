package sandboxcli

import (
	"fmt"
	"os"

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
			endpoint := ctx.GlobalString("endpoint")
			apikey := ctx.GlobalString("apikey")

			if endpoint == "" {
				return printErrorExit("--endpoint is required for login (e.g. sandbox.example.com:50051)", 1)
			}
			if apikey == "" {
				return printErrorExit("--apikey is required for login", 1)
			}

			c, err := client.NewClientWithEndpoint(endpoint, apikey)
			if err != nil {
				return printErrorExit("login failed: "+err.Error(), 1)
			}
			c.Close()

			fmt.Fprintf(os.Stderr, "✓ Logged in to %s\n", endpoint)
			fmt.Fprintf(os.Stderr, "  Credentials stored in ~/.k8e/sandbox/\n")
			fmt.Fprintf(os.Stderr, "  Certificate valid for 30 days (auto-renewed on use)\n")
			return nil
		},
	}
}
