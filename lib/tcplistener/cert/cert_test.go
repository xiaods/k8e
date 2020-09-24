package cert

import (
	"crypto/x509"
	"io/ioutil"
	"testing"
)

func Test_CreateCACertKey(t *testing.T) {
	regen, err := CreateCACertKey("etcd-server", "./etcd-ca.crt", "./etcd-ca.key")
	if err != nil {
		t.Fatal(err)
	}
	altNames := &AltNames{
		DNSNames: []string{"localhost"},
	}
	regen, err = CreateClientCertKey(regen, "etcd-server", nil, altNames, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		"./etcd-ca.crt", "./etcd-ca.key", "./etcd-server.crt", "./etcd-server.key")
	if err != nil {
		t.Fatal(err)
	}
}

func Test_Expired(t *testing.T) {
	caBytes, err := ioutil.ReadFile("./etcd-ca.crt")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caBytes)
	getCertExpiredDay("./etcd-server.crt", pool)
}
