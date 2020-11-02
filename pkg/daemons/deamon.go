package daemons

import (
	"context"
	"net/http"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/daemons/agent"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/server"
)

var D *Daemon

func init() {
	D = &Daemon{}
}

type ServerComponent func(ctx context.Context, cfg *config.Control) error

type NodeComponent func(ctx context.Context, cfg *config.Node) error

type Daemon struct{}

func (d *Daemon) daemon(cfg *config.Control) {
	http.Handle("/db/info", cfg.DBInfoHandler)
	go http.ListenAndServe(":8081", nil)
}

func (d *Daemon) StartServer(ctx context.Context, cfg *config.Control) error {
	runtime := &config.ControlRuntime{}
	cfg.Runtime = runtime
	err := d.startServer(ctx, cfg,
		server.Prepare,
		server.ApiServer,
		server.Scheduler,
		server.ControllerManager)
	if err != nil {
		return err
	}
	d.daemon(cfg)
	return nil
}

func (d *Daemon) startServer(ctx context.Context, cfg *config.Control, funcs ...ServerComponent) error {
	for _, f := range funcs {
		err := f(ctx, cfg)
		if err != nil {
			logrus.Error(err)
			return err
		}
	}
	return nil
}

func (d *Daemon) StartAgent(ctx context.Context, cfg *config.Node) error {
	return d.startAgent(ctx, cfg,
		agent.Prepare,
		agent.Containerd,
		agent.Kubelet,
		agent.KubeProxy,
		agent.NetWorkCNI)
}

func (d *Daemon) startAgent(ctx context.Context, cfg *config.Node, funcs ...NodeComponent) error {
	for _, f := range funcs {
		err := f(ctx, cfg)
		if err != nil {
			logrus.Error("start agent module fail", err)
			return err
		}
	}
	return nil
}
