package kubectl

import (
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/kubectl"
)

func Run(ctx *cli.Context) error {
	kubectl.Main()
	return nil
}
