package server

import (
	"net"
	"testing"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

// TestAdvertiseIP_NeverLoopback is the invariant test for KIP-21: no input
// combination may produce a loopback (or otherwise non-routable) advertise
// IP, because Kubernetes does not allow Endpoints to use 127.0.0.1/::1 and
// the KIP-18 Gateway API ingress would silently have no healthy backends.
func TestAdvertiseIP_NeverLoopback(t *testing.T) {
	cases := []struct {
		name      string
		advertise string
		bind      string
	}{
		{"pure defaults", "", ""},
		{"bind loopback", "", "127.0.0.1"},
		{"bind any", "", "0.0.0.0"},
		{"bind v6 any", "", "::"},
		{"advertise loopback", "127.0.0.1", ""},
		{"advertise loopback v6", "::1", ""},
		{"advertise unspecified", "0.0.0.0", ""},
		{"advertise link-local", "169.254.1.1", ""},
		{"advertise multicast", "224.0.0.1", ""},
		{"advertise loopback bind non-loopback", "127.0.0.1", "10.0.0.5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Control{AdvertiseIP: tc.advertise, APIServerBindAddress: tc.bind}
			got := advertiseIP(cfg)
			if got == "" {
				return // no default-route interface on this host — caller skips staging
			}
			if !isRoutableAdvertiseIP(got) {
				t.Fatalf("advertiseIP(advertise=%q, bind=%q) = %q: not a routable unicast address", tc.advertise, tc.bind, got)
			}
			if ip := net.ParseIP(got); ip != nil && ip.IsLoopback() {
				t.Fatalf("advertiseIP(advertise=%q, bind=%q) returned loopback %q", tc.advertise, tc.bind, got)
			}
		})
	}
}

func TestAdvertiseIP_ExplicitWins(t *testing.T) {
	cfg := &config.Control{AdvertiseIP: "192.168.1.50", APIServerBindAddress: "0.0.0.0"}
	if got := advertiseIP(cfg); got != "192.168.1.50" {
		t.Fatalf("explicit --advertise-address should win, got %q", got)
	}
}

func TestAdvertiseIP_BindFallback(t *testing.T) {
	cfg := &config.Control{APIServerBindAddress: "10.0.0.5"}
	if got := advertiseIP(cfg); got != "10.0.0.5" {
		t.Fatalf("non-loopback --bind-address should be used, got %q", got)
	}
}

func TestAdvertiseIP_RejectsNonRoutableExplicit(t *testing.T) {
	// An explicit loopback --advertise-address must be rejected (never
	// written into Endpoints); resolution falls through to the bind address.
	cfg := &config.Control{AdvertiseIP: "127.0.0.1", APIServerBindAddress: "10.0.0.5"}
	if got := advertiseIP(cfg); got != "10.0.0.5" {
		t.Fatalf("loopback advertise rejected and bind used instead, got %q", got)
	}
}

func TestIsRoutableAdvertiseIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		// loopback — rejected
		{"127.0.0.1", false},
		{"127.0.0.9", false},
		{"::1", false},
		// unspecified — rejected
		{"0.0.0.0", false},
		{"::", false},
		// link-local — rejected
		{"169.254.1.1", false},
		{"169.254.0.0", false},
		{"fe80::1", false},
		// multicast — rejected
		{"224.0.0.1", false},
		{"239.255.255.250", false},
		{"ff02::1", false},
		// unicast — accepted
		{"192.168.1.1", true},
		{"10.0.0.1", true},
		{"172.16.0.1", true},
		{"8.8.8.8", true},
		{"2001:db8::1", true},
		// malformed — rejected
		{"", false},
		{"not-an-ip", false},
		{"127.0.0.1:6443", false},
	}
	for _, tc := range cases {
		if got := isRoutableAdvertiseIP(tc.ip); got != tc.want {
			t.Errorf("isRoutableAdvertiseIP(%q) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}
