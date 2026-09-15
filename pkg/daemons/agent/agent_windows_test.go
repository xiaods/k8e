//go:build windows
// +build windows

package agent

import (
	"testing"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

func TestCheckRuntimeEndpoint(t *testing.T) {
	tests := []struct {
		name string
		cfg  *config.Agent
		want string
	}{
		{
			name: "Runtime endpoint unaltered",
			cfg:  &config.Agent{RuntimeSocket: "npipe:////./pipe/containerd-containerd"},
			want: "npipe:////./pipe/containerd-containerd",
		},
		{
			name: "Runtime endpoint altered",
			cfg:  &config.Agent{RuntimeSocket: "//./pipe/containerd-containerd"},
			want: "npipe:////./pipe/containerd-containerd",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newKubeletSettings()
			applyPlatformKubeletSettings(s, tt.cfg)
			if got := s.config.ContainerRuntimeEndpoint; got != tt.want {
				t.Errorf("input %q: containerRuntimeEndpoint = %q, want %q", tt.cfg.RuntimeSocket, got, tt.want)
			}
		})
	}
}
