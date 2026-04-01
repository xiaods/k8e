package cmds

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	sandboxgrpc "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const (
	defaultTLSDir  = "/var/lib/k8e/server/tls"
	defaultCertFile = defaultTLSDir + "/serving-kube-apiserver.crt"
	defaultKeyFile  = defaultTLSDir + "/serving-kube-apiserver.key"
)

func NewSandboxGatewayCommand(action func(*cli.Context) error) cli.Command {
	return cli.Command{
		Name:   "sandbox-gateway",
		Usage:  "Run the sandbox gRPC gateway",
		Action: action,
		Flags: []cli.Flag{
			cli.StringFlag{Name: "tls-cert", Value: defaultCertFile, EnvVar: "K8E_SANDBOX_CERT"},
			cli.StringFlag{Name: "tls-key", Value: defaultKeyFile, EnvVar: "K8E_SANDBOX_KEY"},
			cli.IntFlag{Name: "grpc-port", Value: 50051, EnvVar: "K8E_SANDBOX_GRPC_PORT"},
		},
	}
}

func SandboxGateway(ctx *cli.Context) error {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return err
	}
	k8s, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return err
	}

	srv := sandboxgrpc.NewServer(k8s, dyn, ctx.String("tls-cert"), ctx.String("tls-key"), ctx.Int("grpc-port"))

	c, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-sig
		logrus.Info("sandbox-gateway shutting down")
		cancel()
	}()

	return srv.Start(c)
}
