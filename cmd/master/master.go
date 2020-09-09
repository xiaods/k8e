package main

import (
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/cli/master"
)

func main() {
	cmds.RootCmd.AddCommand(cmds.NewMasterCommand(master.Run))
	cmds.Execute()
}
