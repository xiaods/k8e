package crictl

import (
	"os"

	"github.com/urfave/cli"
	"sigs.k8s.io/cri-tools/cmd/crictl"
)

func Run(ctx *cli.Context) error {
	// crictl parses os.Args itself, so hand it only its own arguments.
	// Without this, `k8e crictl ps` would reach crictl as
	// ["crictl", "crictl", "ps"] and be rejected as a bad argument.
	os.Args = append([]string{ctx.Command.Name}, ctx.Args()...)
	crictl.Main()
	return nil
}
