package daemons

import (
	"context"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

type ComponentRun func(ctx context.Context, cfg *config.Control, ready <-chan struct{}) error

type Daemon struct {
}

func (d *Daemon) StartMaster() {}

func (d *Daemon) StartAgent() {}
