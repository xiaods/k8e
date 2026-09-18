package netsy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// dialTimeout bounds one connection attempt against the Netsy client API.
const dialTimeout = 5 * time.Second

// clientTLS builds the etcd v3 client TLS configuration from the PKI k8e
// generated. It is the client side of the --etcd-cafile/--etcd-certfile/
// --etcd-keyfile triple handed to kube-apiserver, so the readiness probe speaks
// the same mTLS identity as the datastore clients do.
func clientTLS(certs CertPaths) (*tls.Config, error) {
	pair, err := tls.LoadX509KeyPair(certs.DatastoreCert, certs.DatastoreKey)
	if err != nil {
		return nil, fmt.Errorf("failed to load netsy datastore client certificate: %w", err)
	}
	caPEM, err := os.ReadFile(certs.CA)
	if err != nil {
		return nil, fmt.Errorf("failed to read netsy CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("failed to parse netsy CA %s", certs.CA)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}

// waitReady blocks until the Netsy client API reports an elected Primary, i.e.
// until the datastore accepts writes. A Netsy node can be healthy before the
// elector has picked a Primary, and a write sent in that window fails with
// "this node is not accepting writes (state: replica)"; kube-apiserver must not
// be pointed at the datastore before it can serve Kubernetes' transaction
// writes.
func (p *Process) waitReady(ctx context.Context, certs CertPaths) error {
	tlsConfig, err := clientTLS(certs)
	if err != nil {
		return err
	}
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{p.endpoint},
		DialTimeout: dialTimeout,
		TLS:         tlsConfig,
	})
	if err != nil {
		return fmt.Errorf("failed to create netsy client: %w", err)
	}
	defer client.Close()

	deadline := time.Now().Add(p.readyAfter)
	for {
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for a writable netsy datastore at %s\n%s", p.readyAfter, p.endpoint, p.logs.String())
		}
		// Bound every probe so the loop can still observe a process exit or a
		// context cancellation on an unreachable endpoint.
		probeCtx, cancel := context.WithTimeout(ctx, dialTimeout)
		status, err := client.Status(probeCtx, p.endpoint)
		cancel()
		if err == nil && status.Leader != 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-p.done:
			return fmt.Errorf("netsy exited before becoming ready: %v\n%s", p.waitErr, p.logs.String())
		case <-time.After(p.poll):
		}
	}
}
