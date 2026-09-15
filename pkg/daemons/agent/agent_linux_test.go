//go:build linux
// +build linux

package agent

import (
	"strings"
	"testing"
)

func TestApplyCgroupArgsOmitsRemovedDevicePluginsGate(t *testing.T) {
	s := newKubeletSettings()
	applyCgroupArgs(s, map[string]bool{"cpu": true, "pids": true}, "/kubelet", "/runtime")

	for name := range s.gates {
		if strings.Contains(name, "DevicePlugins") {
			t.Fatalf("kubelet still requests the removed DevicePlugins gate: %#v", s.gates)
		}
	}
	if got := s.config.KubeletCgroups; got != "/kubelet" {
		t.Fatalf("kubeletCgroups = %q, want /kubelet", got)
	}
	if got := s.flags["runtime-cgroups"]; got != "/runtime" {
		t.Fatalf("runtime-cgroups = %q, want /runtime", got)
	}
}

func TestApplyCgroupArgsMovesCPUQuotaIntoConfig(t *testing.T) {
	s := newKubeletSettings()
	applyCgroupArgs(s, map[string]bool{"cpu": false, "pids": true}, "", "")

	if s.config.CPUCFSQuota == nil || *s.config.CPUCFSQuota {
		t.Fatalf("cpuCFSQuota = %v, want false", s.config.CPUCFSQuota)
	}
	if _, ok := s.flags["cpu-cfs-quota"]; ok {
		t.Fatalf("cpu-cfs-quota has a KubeletConfiguration field and must not stay a flag: %#v", s.flags)
	}
}
