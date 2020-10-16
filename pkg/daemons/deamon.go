package daemons

import (
	"context"
	"net/http"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/master"
)

var D *Daemon

func init() {
	D = &Daemon{}
}

type MasterComponent func(ctx context.Context, cfg *config.Control) error

type NodeComponent func(ctx context.Context, cfg *config.Node) error

type Daemon struct{}

func (d *Daemon) daemon(cfg *config.Control) {
	http.Handle("/db/info", cfg.DBInfoHandler)
	go http.ListenAndServe(":8081", nil)
}

func (d *Daemon) StartMaster(ctx context.Context, cfg *config.Control) error {
	runtime := &config.ControlRuntime{}
	cfg.Runtime = runtime
	err := d.startMaster(ctx, cfg,
		master.Prepare,
		master.ApiServer,
		master.Scheduler,
		master.ControllerManager)
	if err != nil {
		return err
	}
	d.daemon(cfg)
	return nil
}

func (d *Daemon) startMaster(ctx context.Context, cfg *config.Control, funcs ...MasterComponent) error {
	for _, f := range funcs {
		err := f(ctx, cfg)
		if err != nil {
			return err
		}
	}
	return nil
}

func (d *Daemon) StartAgent(ctx context.Context, cfg *config.Node) error {
	return d.startAgent(ctx, cfg)
}

func (d *Daemon) startAgent(ctx context.Context, cfg *config.Node, funcs ...NodeComponent) error {
	for _, f := range funcs {
		err := f(ctx, cfg)
		if err != nil {
			return err
		}
	}
	return nil
}
