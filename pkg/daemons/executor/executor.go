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
	APIServer(ctx context.Context, etcdReady <-chan struct{}, args []string) (authenticator.Request, http.Handler, error)
}

func APIServer(ctx context.Context, etcdReady <-chan struct{}, args []string) (authenticator.Request, http.Handler, error) {
	return executor.APIServer(ctx, etcdReady, args)
}
