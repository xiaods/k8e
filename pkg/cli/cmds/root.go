package cmds

import (
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/version"
)

var (
	Debug     bool
	DebugFlag = cli.BoolFlag{
		Name:        "debug",
		Usage:       "(logging) Turn on debug logs",
		Destination: &Debug,
		EnvVar:      version.ProgramUpper + "_DEBUG",
	}
)

var ErrCommandNoArgs = errors.New("this command does not take any arguments")

func init() {
	// hack - force "file,dns" lookup order if go dns is used
	if os.Getenv("RES_OPTIONS") == "" {
		os.Setenv("RES_OPTIONS", " ")
	}
}

func NewApp() *cli.App {
	app := cli.NewApp()
	app.Name = appName
	app.Usage = "Kubernetes, but small and simple"
	app.Version = fmt.Sprintf("%s (%s)", version.Version, version.GitCommit)
	cli.VersionPrinter = func(c *cli.Context) {
		version.PrintK8eASCIIArt()
		fmt.Printf("%s version %s\n", app.Name, app.Version)
		fmt.Printf("go version %s\n", runtime.Version())
	}
	app.Flags = []cli.Flag{
		DebugFlag,
		cli.StringFlag{
			Name:  "data-dir,d",
			Usage: "(data) Folder to hold state default /var/lib/" + version.Program + " or ${HOME}/." + version.Program + " if not root",
		},
	}

	return app
}
