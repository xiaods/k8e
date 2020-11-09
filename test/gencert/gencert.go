package main

import (
	"crypto/x509"
	"path/filepath"

	"github.com/xiaods/k8e/lib/tcplistener/cert"
	"github.com/xiaods/k8e/pkg/datadir"
)

//Create client certificate
func genClientCert() error {
	dataDir, err := datadir.LocalHome("", true)
	if err != nil {
		return err
	}

	clientCA := filepath.Join(dataDir, "tls", "client-ca.crt")
	ClientCAKey := filepath.Join(dataDir, "tls", "client-ca.key")

	certFile := filepath.Join(dataDir, "tls", "client-admin.crt")
	keyFile := filepath.Join(dataDir, "tls", "client-admin.key")
	_, err = cert.CreateClientCertKey(true, "system:admin",
		[]string{"system:masters"},
		nil,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		clientCA,
		ClientCAKey,
		certFile,
		keyFile)

	return err

}

func main() {

}
