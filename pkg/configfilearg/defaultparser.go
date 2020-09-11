package configfilearg

import (
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/version"
)

func MustParse(args []string) []string {
	parser := &Parser{
		After:         []string{"server", "agent"},
		FlagNames:     []string{"--config", "-c"},
		EnvName:       version.ProgramUpper + "_CONFIG_FILE",
		DefaultConfig: "/etc/xiaods/" + version.Program + "/config.yaml",
	}
	result, err := parser.Parse(args)
	if err != nil {
		logrus.Fatal(err)
	}
	return result
}
