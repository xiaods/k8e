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

// Flag names are declared once and reused by the command definition and every
// accessor, so a rename cannot leave a stale literal behind.
const (
	flagListen          = "listen"
	flagAllowedOrigin   = "allowed-origin"
	flagTLSCert         = "tls-cert"
	flagTLSKey          = "tls-key"
	flagAPIKeyNamespace = "api-key-namespace"
	flagAPIKeySecret    = "api-key-secret"
	flagGateway         = "gateway"
	flagGatewayCA       = "gateway-ca"
	flagGatewayCert     = "gateway-cert"
	flagGatewayKey      = "gateway-key"
	flagStateNamespace  = "state-namespace"
	flagKubeconfig      = "kubeconfig"
	flagBootstrapAPIKey = "bootstrap-api-key"
	flagInsecureBoot    = "insecure-bootstrap"
	flagCertCache       = "cert-cache"
	flagTenant          = "tenant"
)

// Command returns the mcp-serve CLI command and its deployment-facing options.
func Command() cli.Command {
	flags := []cli.Flag{
		cli.StringFlag{Name: flagListen, Value: "127.0.0.1:8443", EnvVar: "K8E_MCP_LISTEN"},
		cli.StringSliceFlag{Name: flagAllowedOrigin, EnvVar: "K8E_MCP_ALLOWED_ORIGINS"},
		cli.StringFlag{Name: flagTLSCert, EnvVar: "K8E_MCP_TLS_CERT"},
		cli.StringFlag{Name: flagTLSKey, EnvVar: "K8E_MCP_TLS_KEY"},
		cli.StringFlag{Name: flagAPIKeyNamespace, Value: "sandbox-matrix", EnvVar: "K8E_MCP_API_KEY_NAMESPACE"},
		cli.StringFlag{Name: flagAPIKeySecret, Value: "sandbox-apikeys", EnvVar: "K8E_MCP_API_KEY_SECRET"},
		cli.StringFlag{Name: flagGateway, EnvVar: "K8E_MCP_GATEWAY"},
		cli.StringFlag{Name: flagGatewayCA, EnvVar: "K8E_MCP_GATEWAY_CA"},
		cli.StringFlag{Name: flagGatewayCert, EnvVar: "K8E_MCP_GATEWAY_CERT"},
		cli.StringFlag{Name: flagGatewayKey, EnvVar: "K8E_MCP_GATEWAY_KEY"},
		cli.StringFlag{Name: flagStateNamespace, EnvVar: "K8E_MCP_STATE_NAMESPACE"},
		// KIP-30 M2: enroll the adapter's own gateway identity from an API key
		// at startup, caching it in a ConfigMap so restarts and rollouts reuse
		// one identity. Sets of flags remain mutually sufficient: either an
		// explicit client pair, or the bootstrap API key.
		cli.StringFlag{Name: flagBootstrapAPIKey, EnvVar: "K8E_MCP_BOOTSTRAP_API_KEY"},
		cli.BoolFlag{Name: flagInsecureBoot, EnvVar: "K8E_MCP_INSECURE_BOOTSTRAP"},
		cli.StringFlag{Name: flagCertCache, Value: defaultCertCacheName, EnvVar: "K8E_MCP_CERT_CACHE"},
		cli.StringFlag{Name: flagTenant, EnvVar: "K8E_MCP_TENANT"},
		cli.StringFlag{Name: flagKubeconfig, EnvVar: "KUBECONFIG"},
	}
	return cli.Command{Name: "mcp-serve", Usage: "Serve MCP using K8E API keys and an explicit mTLS sandbox gateway", Flags: flags, Action: serveCommand}
}

func serveCommand(command *cli.Context) error {
	if err := validateServeOptions(command); err != nil {
		return err
	}
	// Kubernetes connects first: a bootstrap-mode start needs the ConfigMap
	// client for the identity cache before it dials the gateway.
	core, err := connectKubernetes(command)
	if err != nil {
		return err
	}
	connection, err := connectGateway(command, core)
	if err != nil {
		return err
	}
	defer connection.Close()
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
	for _, name := range []string{flagAPIKeyNamespace, flagAPIKeySecret, flagGateway, flagStateNamespace} {
		if command.String(name) == "" {
			return errors.New("required MCP option: --" + name)
		}
	}
	explicitPair := command.String(flagGatewayCert) != "" && command.String(flagGatewayKey) != ""
	if command.String(flagGatewayCert) != "" != (command.String(flagGatewayKey) != "") {
		return errors.New("MCP gateway TLS requires both --" + flagGatewayCert + " and --" + flagGatewayKey)
	}
	if explicitPair && command.String(flagGatewayCA) == "" {
		return errors.New("explicit gateway TLS requires --" + flagGatewayCA)
	}
	if !explicitPair && command.String(flagBootstrapAPIKey) == "" {
		return errors.New("MCP gateway access requires either --" + flagGatewayCert + "/--" + flagGatewayKey + " or --" + flagBootstrapAPIKey)
	}
	if (command.String(flagTLSCert) == "") != (command.String(flagTLSKey) == "") {
		return errors.New("MCP TLS requires both --" + flagTLSCert + " and --" + flagTLSKey)
	}
	return nil
}

func connectGateway(command *cli.Context, core typedcore.CoreV1Interface) (*grpc.ClientConn, error) {
	if command.String(flagGatewayCert) != "" && command.String(flagGatewayKey) != "" {
		return dialExplicitGateway(command)
	}
	return dialEnrolledGateway(command, core)
}

// dialExplicitGateway uses an operator-provided client pair (the pre-M2
// mcp-gateway-tls Secret), verified against the given CA file.
func dialExplicitGateway(command *cli.Context) (*grpc.ClientConn, error) {
	certificate, err := tls.LoadX509KeyPair(command.String(flagGatewayCert), command.String(flagGatewayKey))
	if err != nil {
		return nil, err
	}
	rootPEM, err := os.ReadFile(command.String(flagGatewayCA))
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(rootPEM) {
		return nil, errors.New("gateway CA has no certificates")
	}
	return grpc.NewClient(command.String(flagGateway), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{certificate}})))
}

// dialEnrolledGateway boots the adapter's identity from the identity cache or
// an API-key enrollment, then dials with the resulting mTLS material. A
// --cert-cache value of "" disables the cache: every start enrolls fresh.
func dialEnrolledGateway(command *cli.Context, core typedcore.CoreV1Interface) (*grpc.ClientConn, error) {
	var cache IdentityCache
	if name := command.String(flagCertCache); name != "" {
		bound, err := NewConfigMapIdentityCache(core.ConfigMaps(command.String(flagStateNamespace)), name)
		if err != nil {
			return nil, err
		}
		cache = bound
	}
	ctx, cancel := context.WithTimeout(context.Background(), bootstrapTimeout)
	defer cancel()
	certificate, roots, err := ensureGatewayIdentity(ctx, cache, EnrollmentConfig{
		Endpoint: command.String(flagGateway),
		APIKey:   command.String(flagBootstrapAPIKey),
		CAFile:   command.String(flagGatewayCA),
		Insecure: command.Bool(flagInsecureBoot),
		Device:   "k8e-mcp",
	})
	if err != nil {
		return nil, fmt.Errorf("mcp-serve gateway identity: %w", err)
	}
	return grpc.NewClient(command.String(flagGateway), grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, Certificates: []tls.Certificate{certificate}})))
}

func connectKubernetes(command *cli.Context) (typedcore.CoreV1Interface, error) {
	var kubeConfig *rest.Config
	var err error
	if command.String(flagKubeconfig) == "" {
		kubeConfig, err = rest.InClusterConfig()
	} else {
		kubeConfig, err = clientcmd.BuildConfigFromFlags("", command.String(flagKubeconfig))
	}
	if err != nil {
		return nil, err
	}
	kubeConfig.Timeout = 10 * time.Second
	return typedcore.NewForConfig(kubeConfig)
}

func buildHandler(command *cli.Context, connection *grpc.ClientConn, core typedcore.CoreV1Interface) (*Server, error) {
	store, err := NewKubernetesStore(core.ConfigMaps(command.String(flagStateNamespace)))
	if err != nil {
		return nil, err
	}
	service, err := NewService(pb.NewSandboxServiceClient(connection), store, command.String(flagTenant))
	if err != nil {
		return nil, err
	}
	authenticate, err := NewAPIKeyAuthenticator(core.Secrets(command.String(flagAPIKeyNamespace)), command.String(flagAPIKeySecret))
	if err != nil {
		return nil, err
	}
	return New(Config{Authenticate: authenticate, Tools: service.Tools(), AllowedOrigins: command.StringSlice(flagAllowedOrigin), Ready: readiness(store, pb.NewSandboxServiceClient(connection))})
}

func serveHTTP(command *cli.Context, server *Server) error {
	mux := http.NewServeMux()
	mux.Handle("/mcp", server)
	mux.HandleFunc("/healthz", func(writer http.ResponseWriter, _ *http.Request) { writer.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("/readyz", readinessHandler(server))
	httpServer := &http.Server{Addr: command.String(flagListen), Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 45 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	completed := make(chan error, 1)
	go func() {
		if command.String(flagTLSCert) == "" {
			completed <- httpServer.ListenAndServe()
			return
		}
		completed <- httpServer.ListenAndServeTLS(command.String(flagTLSCert), command.String(flagTLSKey))
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
