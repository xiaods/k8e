package etcdstorage

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"time"

	certutil "github.com/rancher/dynamiclistener/cert"
	"github.com/xiaods/k8e/pkg/daemons/config"
	clientv3 "go.etcd.io/etcd/client/v3"
)

var ErrNotFound = errors.New("key not found")

// Value represents a key-value pair from etcd.
type Value struct {
	Key      []byte
	Data     []byte
	Modified int64
}

// Client provides storage-oriented operations on etcd.
type Client interface {
	List(ctx context.Context, key string, rev int) ([]Value, error)
	Get(ctx context.Context, key string) (Value, error)
	Create(ctx context.Context, key string, value []byte) error
	Update(ctx context.Context, key string, revision int64, value []byte) error
	Delete(ctx context.Context, key string, revision int64) error
	Close() error
}

type client struct {
	c *clientv3.Client
}

// New creates a new etcd storage client from the given ETCDConfig.
func New(cfg config.ETCDConfig) (Client, error) {
	tlsConfig, err := tlsConfigFromFiles(cfg.TLSConfig)
	if err != nil {
		return nil, err
	}

	c, err := clientv3.New(clientv3.Config{
		Endpoints:   cfg.Endpoints,
		DialTimeout: 5 * time.Second,
		TLS:         tlsConfig,
	})
	if err != nil {
		return nil, err
	}

	return &client{c: c}, nil
}

func (c *client) List(ctx context.Context, key string, rev int) ([]Value, error) {
	resp, err := c.c.Get(ctx, key, clientv3.WithPrefix(), clientv3.WithRev(int64(rev)))
	if err != nil {
		return nil, err
	}

	var vals []Value
	for _, kv := range resp.Kvs {
		vals = append(vals, Value{
			Key:      kv.Key,
			Data:     kv.Value,
			Modified: kv.ModRevision,
		})
	}

	return vals, nil
}

func (c *client) Get(ctx context.Context, key string) (Value, error) {
	resp, err := c.c.Get(ctx, key)
	if err != nil {
		return Value{}, err
	}

	if len(resp.Kvs) == 1 {
		return Value{
			Key:      resp.Kvs[0].Key,
			Data:     resp.Kvs[0].Value,
			Modified: resp.Kvs[0].ModRevision,
		}, nil
	}

	return Value{}, ErrNotFound
}

func (c *client) Create(ctx context.Context, key string, value []byte) error {
	resp, err := c.c.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, string(value))).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return fmt.Errorf("key exists")
	}
	return nil
}

func (c *client) Update(ctx context.Context, key string, revision int64, value []byte) error {
	resp, err := c.c.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", revision)).
		Then(clientv3.OpPut(key, string(value))).
		Else(clientv3.OpGet(key)).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return fmt.Errorf("revision %d doesnt match", revision)
	}
	return nil
}

func (c *client) Delete(ctx context.Context, key string, revision int64) error {
	resp, err := c.c.Txn(ctx).
		If(clientv3.Compare(clientv3.ModRevision(key), "=", revision)).
		Then(clientv3.OpDelete(key)).
		Else(clientv3.OpGet(key)).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return fmt.Errorf("revision %d doesnt match", revision)
	}
	return nil
}

func (c *client) Close() error {
	return c.c.Close()
}

// tlsConfigFromFiles creates a *tls.Config from certificate file paths.
// Returns nil if no certificate files are configured.
func tlsConfigFromFiles(cfg config.TLSConfig) (*tls.Config, error) {
	if cfg.CertFile == "" && cfg.KeyFile == "" && cfg.CAFile == "" {
		return nil, nil
	}

	var certificates []tls.Certificate
	if cfg.CertFile != "" && cfg.KeyFile != "" {
		clientCert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		certificates = append(certificates, clientCert)
	}

	var rootCAs *x509.CertPool
	if cfg.CAFile != "" {
		pool, err := certutil.NewPool(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		rootCAs = pool
	}

	return &tls.Config{
		RootCAs:      rootCAs,
		Certificates: certificates,
	}, nil
}
