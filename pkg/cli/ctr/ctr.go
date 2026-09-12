package ctr

import (
	"os"

	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/ctr"
)

func Run(ctx *cli.Context) error {
	// ctr parses os.Args itself, so hand it only its own arguments.
	// Without this, `k8e ctr namespaces ls` would reach ctr with the extra
	// wrapping "ctr" token and be rejected as a bad argument.
	os.Args = append([]string{ctx.Command.Name}, ctx.Args()...)
	ctr.Main()
	return nil
}
