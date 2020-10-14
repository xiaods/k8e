package executor

import (
	"context"
	"net/http"

	"k8s.io/apiserver/pkg/authentication/authenticator"
)

var (
	executor Executor
)

type Executor interface {
	Kubelet(args []string) error
	KubeProxy(args []string) error
	APIServer(ctx context.Context, etcdReady <-chan struct{}, args []string) (authenticator.Request, http.Handler, error)
	Scheduler(apiReady <-chan struct{}, args []string) error
	ControllerManager(apiReady <-chan struct{}, args []string) error
}

func APIServer(ctx context.Context, etcdReady <-chan struct{}, args []string) (authenticator.Request, http.Handler, error) {
	return executor.APIServer(ctx, etcdReady, args)
}

func Scheduler(apiReady <-chan struct{}, args []string) error {
	return executor.Scheduler(apiReady, args)
}

func ControllerManager(apiReady <-chan struct{}, args []string) error {
	return executor.ControllerManager(apiReady, args)
}

func Kubelet(args []string) error {
	return executor.Kubelet(args)
}
