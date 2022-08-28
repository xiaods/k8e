package server

import (
	"context"

	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/daemons/config"
)

type Config struct {
	DisableAgent      bool
	ControlConfig     config.Control
	Rootless          bool
	SupervisorPort    int
	StartupHooks      []cmds.StartupHook
	LeaderControllers CustomControllers
	Controllers       CustomControllers
}

type CustomControllers []func(ctx context.Context, sc *Context) error
