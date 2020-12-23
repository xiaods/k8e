package ctr

import (
	"github.com/xiaods/k8e/pkg/ctr"
	"github.com/urfave/cli"
)

func Run(ctx *cli.Context) error {
	ctr.Main()
	return nil
}
