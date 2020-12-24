package cmds

import (
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/version"
)

var (
	// ConfigFlag is here to show to the user, but the actually processing is done by configfileargs before
	// call urfave
	ConfigFlag = cli.StringFlag{
		Name:   "config,c",
		Usage:  "(config) Load configuration from `FILE`",
		EnvVar: version.ProgramUpper + "_CONFIG_FILE",
		Value:  "/etc/k8e/" + version.Program + "/config.yaml",
	}
)
