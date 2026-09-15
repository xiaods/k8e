//go:build linux || darwin

package cmds

import "testing"

// k3s v1.37.0+k3s1 ships rancher/mirrored-pause:3.10.2 as the kubelet/containerd
// sandbox image. Keep k8e's default on that tag.
func TestDefaultPauseImageTracksK3s137(t *testing.T) {
	const want = "rancher/mirrored-pause:3.10.2"
	if DefaultPauseImage != want {
		t.Fatalf("DefaultPauseImage = %q, want %q", DefaultPauseImage, want)
	}
}
