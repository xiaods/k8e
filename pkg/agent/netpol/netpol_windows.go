package netpol

import (
	"context"

	daemonconfig "github.com/xiaods/k8e/pkg/daemons/config"
)

func Run(ctx context.Context, nodeConfig *daemonconfig.Node) error {
	panic("Netpol is not supported on windows ensure to pass --disable-network-policy")
}
