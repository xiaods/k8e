package cmds

import (
	"github.com/urfave/cli"
)

func NewCheckConfigCommand(action func(*cli.Context) error) cli.Command {
	return cli.Command{
		Name:            "check-config",
		Usage:           "Run config check",
		SkipFlagParsing: true,
		SkipArgReorder:  true,
		Action:          action,
	}
}

func NewInitOSConfigCommand(action func(*cli.Context) error) cli.Command {
	return cli.Command{
		Name:            "init-os-config",
		Usage:           "Initialize OS configuration",
		SkipFlagParsing: true,
		SkipArgReorder:  true,
		Action:          action,
	}
}
