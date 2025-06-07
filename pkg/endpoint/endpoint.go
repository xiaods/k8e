package endpoint

import (
	"crypto/tls"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
)

// ConnectionPoolConfig represents database connection pool configuration
type ConnectionPoolConfig struct {
	MaxIdleConns    int
	MaxOpenConns    int
	ConnMaxLifetime time.Duration
}

// Config represents the configuration for kine endpoint
// This is a local implementation to replace k3s-io/kine dependency
type Config struct {
	GRPCServer           *grpc.Server
	Listener             string
	Endpoint             string
	ConnectionPoolConfig ConnectionPoolConfig
	ServerTLSConfig      TLSConfig
	BackendTLSConfig     TLSConfig
	MetricsRegisterer    prometheus.Registerer
	NotifyInterval       time.Duration
	EmulatedETCDVersion  string
}

// ETCDConfig represents the etcd configuration
// This is a local implementation to replace k3s-io/kine dependency
type ETCDConfig struct {
	Endpoints   []string
	TLSConfig   TLSConfig
	LeaderElect bool
}

// TLSConfig represents TLS configuration
// This is a local implementation to replace k3s-io/kine/pkg/tls
type TLSConfig struct {
	CAFile             string
	CertFile           string
	KeyFile            string
	Cert               string
	Key                string
	InsecureSkipVerify bool
	Config             *tls.Config
}
