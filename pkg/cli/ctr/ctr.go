package ctr

import (
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/ctr"
)

func Run(ctx *cli.Context) error {
	ctr.Main()
	return nil
}
