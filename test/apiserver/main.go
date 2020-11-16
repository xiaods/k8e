package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"path/filepath"

	"github.com/xiaods/k8e/pkg/datadir"
)

func testKubeAPI() error {
	dataDir, err := datadir.LocalHome("", true)
	if err != nil {
		return err
	}
	certFile := filepath.Join(dataDir, "tls", "client-controller.crt")
	keyFile := filepath.Join(dataDir, "tls", "client-controller.key")
	fmt.Println(certFile, keyFile)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		log.Println(err)
		return err
	}
	certBytes, err := ioutil.ReadFile(certFile)
	if err != nil {
		return err
	}
	clientCertPool := x509.NewCertPool()
	ok := clientCertPool.AppendCertsFromPEM(certBytes)
	if !ok {
		return errors.New("failed to parse root certificate")
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:            clientCertPool,
			Certificates:       []tls.Certificate{cert},
			InsecureSkipVerify: true,
		},
	}
	client := &http.Client{Transport: tr}
	resp, err := client.Get("https://127.0.0.1:6443")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	fmt.Println(string(body))
	return nil
}

func main() {
	fmt.Println(testKubeAPI())
}
