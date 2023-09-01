package configfilearg

import (
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/version"
)

var defaultParser = &Parser{
	After:         []string{"server", "agent", "etcd-snapshot:1"},
	ConfigFlags:   []string{"--config", "-c"},
	EnvName:       version.ProgramUpper + "_CONFIG_FILE",
	DefaultConfig: "/etc/" + version.Program + "/config.yaml",
	ValidFlags:    map[string][]cli.Flag{"server": cmds.ServerFlags, "etcd-snapshot": cmds.EtcdSnapshotFlags},
}

func MustParse(args []string) []string {
	result, err := defaultParser.Parse(args)
	if err != nil {
		logrus.Fatal(err)
	}
	return result
}

func MustFindString(args []string, target string) string {
	parser := &Parser{
		OverrideFlags: []string{"--help", "-h", "--version", "-v"},
		EnvName:       version.ProgramUpper + "_CONFIG_FILE",
		DefaultConfig: "/etc/" + version.Program + "/config.yaml",
	}
	result, err := parser.FindString(args, target)
	if err != nil {
		logrus.Fatal(err)
	}
	return result
}
