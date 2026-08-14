// Package e2bserver implements the `k8e e2b-server` command: the
// E2B-compatible HTTP front door for the k8e sandbox gateway (KIP-18).
package e2bserver

import (
	"context"

	"github.com/pkg/errors"
	"github.com/rancher/wrangler/v3/pkg/signals"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/sandbox/client"
	sandboxe2b "github.com/xiaods/k8e/pkg/sandbox/e2b"
)

// Run starts the E2B server and blocks until a signal arrives.
func Run(app *cli.Context) error {
	cfg := cmds.E2BServer

	ctx := signals.SetupSignalContext()

	var gw sandboxe2b.Gateway
	if cfg.Endpoint != "" {
		c, err := client.NewClientWithEndpoint(cfg.Endpoint, cfg.APIKey)
		if err != nil {
			return errors.Wrap(err, "e2b-server: connect to sandbox gateway")
		}
		defer c.Close()
		gw = sandboxe2b.GatewayFromClient(c)
	} else {
		c, err := client.NewClient()
		if err != nil {
			return errors.Wrap(err, "e2b-server: local sandbox gateway not reachable")
		}
		defer c.Close()
		gw = sandboxe2b.GatewayFromClient(c)
	}

	srv := sandboxe2b.NewServer(sandboxe2b.Config{
		Listen:              cfg.Listen,
		Endpoint:            cfg.Endpoint,
		APIKey:              cfg.APIKey,
		NodeID:              cfg.NodeID,
		SigningSecret:       cfg.SigningSecret,
		DefaultCPUs:         cfg.DefaultCPUs,
		DefaultMemoryMB:     cfg.DefaultMemoryMB,
		DefaultDiskMB:       cfg.DefaultDiskMB,
		AllowedRuntimeClasses: cfg.AllowedRuntimeClasses,
	}, gw)

	if err := srv.Start(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logrus.Fatalf("e2b-server: %v", err)
	}
	return nil
}
