package main

import (
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/cli/master"
)

func main() {
	// app := cmds.NewApp()

	// if err := app.Run(configfilearg.MustParse(os.Args)); err != nil {
	// 	logrus.Fatal(err)
	// }
	cmds.RootCmd.AddCommand(cmds.NewMasterCommand(master.Run))
	cmds.Execute()
}
