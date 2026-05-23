//go:build darwin
// +build darwin

package rootlessports

import (
	"context"

	coreClients "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
)

func Register(ctx context.Context, serviceController coreClients.ServiceController, httpsPort int) error {
	return nil
}
