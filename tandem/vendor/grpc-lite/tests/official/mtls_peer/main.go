// A grpc-go peer for native TLS client-certificate interoperability tests.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/interop"
	pb "google.golang.org/grpc/interop/grpc_testing"
)

func run() error {
	server := flag.Bool("server", false, "serve instead of calling EmptyCall")
	address := flag.String("address", "localhost:10000", "peer address")
	caFile := flag.String("ca", "", "trusted peer CA PEM")
	certFile := flag.String("cert", "", "identity certificate PEM")
	keyFile := flag.String("key", "", "identity key PEM")
	flag.Parse()
	pem, err := os.ReadFile(*caFile)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return fmt.Errorf("invalid CA PEM")
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}
	if *certFile != "" {
		identity, err := tls.LoadX509KeyPair(*certFile, *keyFile)
		if err != nil {
			return err
		}
		config.Certificates = []tls.Certificate{identity}
	}
	if *server {
		config.ClientCAs = roots
		config.ClientAuth = tls.RequireAndVerifyClientCert
		listener, err := net.Listen("tcp", *address)
		if err != nil {
			return err
		}
		defer listener.Close()
		s := grpc.NewServer(grpc.Creds(credentials.NewTLS(config)))
		defer s.Stop()
		pb.RegisterTestServiceServer(s, interop.NewTestServer())
		return s.Serve(listener)
	}
	conn, err := grpc.NewClient(*address, grpc.WithTransportCredentials(credentials.NewTLS(config)))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = pb.NewTestServiceClient(conn).EmptyCall(ctx, &pb.Empty{})
	return err
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
