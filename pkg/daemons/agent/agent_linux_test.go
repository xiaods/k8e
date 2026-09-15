//go:build linux
// +build linux

package agent

import (
	"strings"
	"testing"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

func TestApplyCgroupArgsOmitsRemovedDevicePluginsGate(t *testing.T) {
	argsMap := map[string]string{}
	applyCgroupArgs(argsMap, &config.Agent{}, map[string]bool{"cpu": true, "pids": true}, "/kubelet", "/runtime")

	if fg := argsMap["feature-gates"]; strings.Contains(fg, "DevicePlugins") {
		t.Fatalf("feature-gates %q still sets removed DevicePlugins gate", fg)
	}
	if got := argsMap["kubelet-cgroups"]; got != "/kubelet" {
		t.Fatalf("kubelet-cgroups = %q, want /kubelet", got)
	}
	if got := argsMap["runtime-cgroups"]; got != "/runtime" {
		t.Fatalf("runtime-cgroups = %q, want /runtime", got)
	}
}
