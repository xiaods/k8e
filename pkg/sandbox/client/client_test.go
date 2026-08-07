package client

import (
	"strings"
	"testing"
)

func TestNewClientWithEndpointRemoteWithoutKeyRequiresCachedCerts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	_, err := NewClientWithEndpoint("sandbox.example.com:50051", "")
	if err == nil {
		t.Fatal("expected cached-cert error")
	}
	if !strings.Contains(err.Error(), "cached") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewClientWithEndpointLoopbackWithoutKeyUsesLocalDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	c, err := NewClientWithEndpoint("127.0.0.1:50051", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
}
