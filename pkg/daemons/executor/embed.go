package executor

import (
	"context"
	"net/http"

	"github.com/sirupsen/logrus"
	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/kubernetes/cmd/kube-apiserver/app"
)

func init() {
	executor = Embedded{}
}

type Embedded struct{}

func (Embedded) APIServer(ctx context.Context, etcdReady <-chan struct{}, args []string) (authenticator.Request, http.Handler, error) {
	<-etcdReady
	command := app.NewAPIServerCommand(ctx.Done())
	command.SetArgs(args)

	go func() {
		logrus.Fatalf("apiserver exited: %v", command.Execute())
	}()

	startupConfig := <-app.StartupConfig
	return startupConfig.Authenticator, startupConfig.Handler, nil
}
