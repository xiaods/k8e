package kubectl

import (
	"github.com/xiaods/k8e/pkg/kubectl"
	"github.com/urfave/cli"
)

func Run(ctx *cli.Context) error {
	kubectl.Main()
	return nil
}
