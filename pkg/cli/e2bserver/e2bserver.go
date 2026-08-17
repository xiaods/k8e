// Package e2bserver implements the `k8e e2b-server` command: the
// E2B-compatible HTTP front door for the k8e sandbox gateway (KIP-18).
package e2bserver

import (
	"context"
	"os"

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

	// Warn — do not fail — on a key the official e2b SDKs can never present:
	// their validateApiKey (/^e2b_[0-9a-f]+$/) runs client-side before any
	// request. A legacy `k8e-…` key remains valid for the gRPC gateway login
	// (the sandbox-apikeys Secret accepts it), so blocking startup would
	// break existing deployments; the warning tells operators to rotate to a
	// hex key (k8e sandbox-apikey create) if they need SDK access.
	if err := sandboxe2b.ValidateE2BAPIKey(cfg.APIKey); err != nil {
		logrus.Warnf("e2b-server: %v", err)
		logrus.Warnf("e2b-server: official e2b SDK clients will not be able to authenticate; generate a hex key with `k8e sandbox-apikey create <name>` and set --apikey to it")
	}

	ctx := signals.SetupSignalContext()

	var gw sandboxe2b.Gateway
	if cfg.Endpoint != "" {
		// One key serves every role: it is accepted from e2b SDK clients (as
		// "e2b_"+key) and used for the gRPC gateway login. The gateway's
		// sandbox-apikeys Secret stores the bare token, so the login
		// credential must be the normalized key — passing the configured
		// "e2b_<hex>" verbatim would fail the Secret's exact lookup.
		c, err := client.NewClientWithEndpoint(cfg.Endpoint, resolveGatewayLoginKey(cfg.APIKey))
		if err != nil {
			return errors.Wrapf(err, "e2b-server: connect to sandbox gateway (ensure the key is provisioned in the sandbox-apikeys Secret: `k8e sandbox-apikey create <name>`)")
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

// resolveGatewayLoginKey returns the bare token used for the sandbox gRPC
// gateway mTLS login. The sandbox-apikeys Secret stores bare tokens, so the
// SDK-facing key (which operators may configure with the `e2b_` prefix) is
// normalized before use; the K8E_SANDBOX_APIKEY env is consulted when the
// flag is empty (matching client.NewClientWithEndpoint's fallback).
func resolveGatewayLoginKey(configured string) string {
	key := sandboxe2b.NormalizeE2BAPIKey(configured)
	if key == "" {
		key = sandboxe2b.NormalizeE2BAPIKey(os.Getenv("K8E_SANDBOX_APIKEY"))
	}
	return key
}
