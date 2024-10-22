//go:build linux && !no_embedded_executor
// +build linux,!no_embedded_executor

package executor

import (
	// registering k8e cloud provider
	_ "github.com/xiaods/k8e/pkg/cloudprovider"
)

