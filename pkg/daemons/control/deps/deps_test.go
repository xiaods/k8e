package deps

import (
	"net"
	"reflect"
	"testing"

	certutil "github.com/rancher/dynamiclistener/cert"
	"github.com/xiaods/k8e/pkg/daemons/config"
)

// A Raft peer verifies the listener's certificate against the address it
// dialled, so the SAN has to be the advertised address — a name borrowed from
// another service fails the handshake. Without an advertise address the
// listener stays on loopback, which is the single-node and same-host case.
func Test_UnitTandemRaftAltNames(t *testing.T) {
	tests := []struct {
		name      string
		advertise string
		wantIPs   []string
		wantDNS   []string
	}{
		{name: "advertised IP", advertise: "10.0.0.1", wantIPs: []string{"10.0.0.1"}},
		{name: "advertised name", advertise: "node-a.example", wantDNS: []string{"node-a.example"}},
		{name: "no advertise address", advertise: "", wantIPs: []string{"127.0.0.1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tandemRaftAltNames(&config.Control{AdvertiseIP: tt.advertise})
			var ips []string
			for _, ip := range got.IPs {
				ips = append(ips, ip.String())
			}
			if !reflect.DeepEqual(ips, tt.wantIPs) {
				t.Errorf("IPs = %v, want %v", ips, tt.wantIPs)
			}
			if !reflect.DeepEqual(got.DNSNames, tt.wantDNS) {
				t.Errorf("DNSNames = %v, want %v", got.DNSNames, tt.wantDNS)
			}
		})
	}
}

func Test_UnitAddSANs(t *testing.T) {
	type args struct {
		altNames *certutil.AltNames
		sans     []string
	}
	tests := []struct {
		name string
		args args
		want certutil.AltNames
	}{
		{
			name: "One IP, One DNS",
			args: args{
				altNames: &certutil.AltNames{},
				sans:     []string{"192.168.205.10", "192.168.205.10.nip.io"},
			},
			want: certutil.AltNames{
				IPs:      []net.IP{net.ParseIP("192.168.205.10")},
				DNSNames: []string{"192.168.205.10.nip.io"},
			},
		},
		{
			name: "Two IP, No DNS",
			args: args{
				altNames: &certutil.AltNames{},
				sans:     []string{"192.168.205.10", "10.168.21.15"},
			},
			want: certutil.AltNames{
				IPs: []net.IP{net.ParseIP("192.168.205.10"), net.ParseIP("10.168.21.15")},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addSANs(tt.args.altNames, tt.args.sans)
			if !reflect.DeepEqual(*tt.args.altNames, tt.want) {
				t.Errorf("addSANs() = %v, want %v", *tt.args.altNames, tt.want)
			}
		})
	}
}
