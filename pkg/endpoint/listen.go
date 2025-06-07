package endpoint

import (
	"context"

	"github.com/sirupsen/logrus"
)

// Listen starts a simple etcd configuration
// This is a local implementation to replace k3s-io/kine/pkg/endpoint.Listen
func Listen(ctx context.Context, config Config) (ETCDConfig, error) {
	logrus.Infof("Starting local etcd implementation with endpoint: %s", config.Endpoint)

	// Return a simple etcd configuration
	return ETCDConfig{
		Endpoints:   []string{"127.0.0.1:2379"}, // Default etcd endpoint
		LeaderElect: true,
		TLSConfig:   config.ServerTLSConfig,
	}, nil
}
