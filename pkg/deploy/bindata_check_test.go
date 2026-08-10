package deploy

import (
	"bytes"
	"testing"
)

func TestCiliumYamlContainsDNSProxyTemplate(t *testing.T) {
	b, err := ciliumYamlBytes()
	if err != nil {
		t.Fatalf("ciliumYamlBytes: %v", err)
	}
	if !bytes.Contains(b, []byte("dnsProxy")) {
		t.Fatalf("dnsProxy missing from embedded cilium.yaml:\n%s", b)
	}
	if !bytes.Contains(b, []byte("CILIUM_DNS_PROXY_ENABLED")) {
		t.Fatalf("CILIUM_DNS_PROXY_ENABLED template missing from embedded cilium.yaml:\n%s", b)
	}
}
