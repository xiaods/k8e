package executor

import (
	"context"
	"net/http"

	"github.com/xiaods/k8e/pkg/cli/cmds"
	daemonconfig "github.com/xiaods/k8e/pkg/daemons/config"
	"k8s.io/apiserver/pkg/authentication/authenticator"
)

var (
	executor Executor
)

type Executor interface {
	Bootstrap(ctx context.Context, nodeConfig *daemonconfig.Node, cfg cmds.Agent) error
	Kubelet(ctx context.Context, args []string) error
	APIServerHandlers(ctx context.Context) (authenticator.Request, http.Handler, error)
	APIServer(ctx context.Context, etcdReady <-chan struct{}, args []string) error
	Scheduler(ctx context.Context, apiReady <-chan struct{}, args []string) error
	ControllerManager(ctx context.Context, apiReady <-chan struct{}, args []string) error
	CurrentETCDOptions() (InitialOptions, error)
	CloudControllerManager(ctx context.Context, ccmRBACReady <-chan struct{}, args []string) error
	Containerd(ctx context.Context, node *daemonconfig.Node) error
	Docker(ctx context.Context, node *daemonconfig.Node) error
}

type InitialOptions struct {
	AdvertisePeerURL string `json:"initial-advertise-peer-urls,omitempty"`
	Cluster          string `json:"initial-cluster,omitempty"`
	State            string `json:"initial-cluster-state,omitempty"`
}

func Set(driver Executor) {
	executor = driver
}

func Bootstrap(ctx context.Context, nodeConfig *daemonconfig.Node, cfg cmds.Agent) error {
	return executor.Bootstrap(ctx, nodeConfig, cfg)
}

func Kubelet(ctx context.Context, args []string) error {
	return executor.Kubelet(ctx, args)
}

func APIServerHandlers(ctx context.Context) (authenticator.Request, http.Handler, error) {
	return executor.APIServerHandlers(ctx)
}

func APIServer(ctx context.Context, etcdReady <-chan struct{}, args []string) error {
	return executor.APIServer(ctx, etcdReady, args)
}

func Scheduler(ctx context.Context, apiReady <-chan struct{}, args []string) error {
	return executor.Scheduler(ctx, apiReady, args)
}

func ControllerManager(ctx context.Context, apiReady <-chan struct{}, args []string) error {
	return executor.ControllerManager(ctx, apiReady, args)
}

func CurrentETCDOptions() (InitialOptions, error) {
	return executor.CurrentETCDOptions()
}

func CloudControllerManager(ctx context.Context, ccmRBACReady <-chan struct{}, args []string) error {
	return executor.CloudControllerManager(ctx, ccmRBACReady, args)
}

func Containerd(ctx context.Context, config *daemonconfig.Node) error {
	return executor.Containerd(ctx, config)
}

func Docker(ctx context.Context, config *daemonconfig.Node) error {
	return executor.Docker(ctx, config)
}
