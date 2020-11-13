package daemons

import (
	"context"
	"net/http"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/daemons/agent"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/server"
	"github.com/xiaods/k8e/pkg/version"
)

var D *Daemon

func init() {
	D = &Daemon{}
}

type ServerComponent func(ctx context.Context, cfg *config.Control) error

type NodeComponent func(ctx context.Context, cfg *config.Node) error

type Daemon struct{}

func (d *Daemon) daemon(ctx context.Context, cfg *config.Control) {
	// http.Handle("/db/info", cfg.DBInfoHandler) //用于获取etcd集群信息

	// http.ListenAndServe(":8081", nil)
	server := http.Server{}
	server.Addr = ":8081"
	server.Handler = router(cfg)
	go func() {
		logrus.Fatalf("server stopped: %v", server.ListenAndServe())
	}()
	go func() {
		<-ctx.Done()
		server.Shutdown(context.Background())
	}()
}

func router(cfg *config.Control) http.Handler {
	prefix := "/v1-" + version.Program
	router := mux.NewRouter()
	router.Path("/db/info").Handler(cfg.DBInfoHandler)
	router.Path(prefix + "/client-ca.crt").Handler(fileHandler(cfg.Runtime.ClientCA))
	router.Path(prefix + "/server-ca.crt").Handler(fileHandler(cfg.Runtime.ServerCA))
	router.Path(prefix + "/client-kubelet.crt").Handler(clientKubeletCert(cfg, cfg.Runtime.ClientKubeletKey))
	return router
}

func (d *Daemon) StartServer(ctx context.Context, cfg *config.Control) error {
	runtime := &config.ControlRuntime{}
	cfg.Runtime = runtime
	err := d.startServer(ctx, cfg,
		server.Prepare,
		server.ApiServer,
		server.Scheduler,
		server.ControllerManager,
		server.StartWrangler,
		server.Kubectl)
	if err != nil {
		return err
	}
	d.daemon(ctx, cfg)
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
