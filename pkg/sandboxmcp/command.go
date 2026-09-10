package sandboxmcp

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/urfave/cli"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// Command returns the mcp-serve CLI command and its deployment-facing options.
func Command() cli.Command {
	flags := []cli.Flag{
		cli.StringFlag{Name: "listen", Value: "127.0.0.1:8443", EnvVar: "K8E_MCP_LISTEN"},
		cli.StringSliceFlag{Name: "allowed-origin", EnvVar: "K8E_MCP_ALLOWED_ORIGINS"},
		cli.StringFlag{Name: "tls-cert", EnvVar: "K8E_MCP_TLS_CERT"},
		cli.StringFlag{Name: "tls-key", EnvVar: "K8E_MCP_TLS_KEY"},
		cli.StringFlag{Name: "api-key-namespace", Value: "sandbox-matrix", EnvVar: "K8E_MCP_API_KEY_NAMESPACE"},
		cli.StringFlag{Name: "api-key-secret", Value: "sandbox-apikeys", EnvVar: "K8E_MCP_API_KEY_SECRET"},
		cli.StringFlag{Name: "gateway", EnvVar: "K8E_MCP_GATEWAY"},
		cli.StringFlag{Name: "gateway-ca", EnvVar: "K8E_MCP_GATEWAY_CA"},
		cli.StringFlag{Name: "gateway-cert", EnvVar: "K8E_MCP_GATEWAY_CERT"},
		cli.StringFlag{Name: "gateway-key", EnvVar: "K8E_MCP_GATEWAY_KEY"},
		cli.StringFlag{Name: "state-namespace", EnvVar: "K8E_MCP_STATE_NAMESPACE"},
		cli.StringFlag{Name: "kubeconfig", EnvVar: "KUBECONFIG"},
	}
	return cli.Command{Name: "mcp-serve", Usage: "Serve MCP using K8E API keys and an explicit mTLS sandbox gateway", Flags: flags, Action: serveCommand}
}

func serveCommand(command *cli.Context) error {
	if err := validateServeOptions(command); err != nil {
		return err
	}
	connection, err := connectGateway(command)
	if err != nil {
		return err
	}
	defer connection.Close()
	core, err := connectKubernetes(command)
	if err != nil {
		return err
	}
	handler, err := buildHandler(command, connection, core)
	if err != nil {
		return err
	}
	return serveHTTP(command, handler)
}

// readiness verifies the two dependencies every tool call needs: the durable
// ConfigMap state store and the sandbox gRPC gateway. It issues a cheap gateway
// RPC so a socket that is open but unusable (TLS/credentials rejected) is not
// reported ready. A pod that cannot reach its dependencies leaves Service
// endpoints instead of accepting calls that would fail.
func readiness(store RecordStore, client pb.SandboxServiceClient) func(context.Context) error {
	return func(ctx context.Context) error {
		if _, err := store.Get(ctx, "mcp-readiness-probe"); err != nil && !errors.Is(err, ErrRecordMissing) {
			return fmt.Errorf("state store unavailable: %w", err)
		}
		// Any application-level error proves the gateway answered; only transport
		// and credential failures mean the adapter cannot serve tool calls.
		_, err := client.GetSession(ctx, &pb.GetSessionRequest{})
		switch status.Code(err) {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unauthenticated:
			return fmt.Errorf("sandbox gateway unavailable: %w", err)
		}
		return nil
	}
}

// readinessHandler serves probe requests against the configured readiness
// check. A handler without a check always reports ready.
func readinessHandler(server *Server) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
		defer cancel()
		if err := server.Ready(ctx); err != nil {
			http.Error(writer, "MCP dependencies unavailable", http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
	}
}

func validateServeOptions(command *cli.Context) error {
	for _, name := range []string{"api-key-namespace", "api-key-secret", "gateway", "gateway-ca", "gateway-cert", "gateway-key", "state-namespace"} {
		if command.String(name) == "" {
			return errors.New("required MCP option: --" + name)
		}
	}
	if (command.String("tls-cert") == "") != (command.String("tls-key") == "") {
		return errors.New("MCP TLS requires both --tls-cert and --tls-key")
	}
	return nil
}

func connectGateway(command *cli.Context) (*grpc.ClientConn, error) {
	certificate, err := tls.LoadX509KeyPair(command.String("gateway-cert"), command.String("gateway-key"))
	if err != nil {
		return nil, err
	}
	rootPEM, err := os.ReadFile(command.String("gateway-ca"))
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		return nil, errors.New("gateway CA has no certificates")
	}
	return grpc.NewClient(command.String("gateway"), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{certificate}})))
}

func connectKubernetes(command *cli.Context) (typedcore.CoreV1Interface, error) {
	var kubeConfig *rest.Config
	var err error
	if command.String("kubeconfig") == "" {
		kubeConfig, err = rest.InClusterConfig()
	} else {
		kubeConfig, err = clientcmd.BuildConfigFromFlags("", command.String("kubeconfig"))
	}
	if err != nil {
		return nil, err
	}
	kubeConfig.Timeout = 10 * time.Second
	return typedcore.NewForConfig(kubeConfig)
}

func buildHandler(command *cli.Context, connection *grpc.ClientConn, core typedcore.CoreV1Interface) (*Server, error) {
	store, err := NewKubernetesStore(core.ConfigMaps(command.String("state-namespace")))
	if err != nil {
		return nil, err
	}
	service, err := NewService(pb.NewSandboxServiceClient(connection), store)
	if err != nil {
		return nil, err
	}
	authenticate, err := NewAPIKeyAuthenticator(core.Secrets(command.String("api-key-namespace")), command.String("api-key-secret"))
	if err != nil {
		return nil, err
	}
	return New(Config{Authenticate: authenticate, Tools: service.Tools(), AllowedOrigins: command.StringSlice("allowed-origin"), Ready: readiness(store, pb.NewSandboxServiceClient(connection))})
}

func serveHTTP(command *cli.Context, server *Server) error {
	mux := http.NewServeMux()
	mux.Handle("/mcp", server)
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/readyz", readinessHandler(server))
	httpServer := &http.Server{Addr: command.String("listen"), Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	completed := make(chan error, 1)
	go func() {
		if command.String("tls-cert") == "" {
			completed <- httpServer.ListenAndServe()
			return
		}
		completed <- httpServer.ListenAndServeTLS(command.String("tls-cert"), command.String("tls-key"))
	}()
	select {
	case err := <-completed:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 35*time.Second)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			_ = httpServer.Close()
			return err
		}
		return nil
	}
}
