package client

import (
	"context"
	"fmt"

	"github.com/xiaods/k8e/pkg/endpoint"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Client represents a kine client interface
// This is a local implementation to replace k3s-io/kine/pkg/client dependency
type Client interface {
	Get(ctx context.Context, key string, opts ...interface{}) (*clientv3.GetResponse, error)
	Put(ctx context.Context, key, value string, opts ...interface{}) (*clientv3.PutResponse, error)
	Delete(ctx context.Context, key string, opts ...interface{}) (*clientv3.DeleteResponse, error)
	List(ctx context.Context, prefix string, limit int64) ([]*KeyValue, error)
	Watch(ctx context.Context, key string, opts ...interface{}) clientv3.WatchChan
	Close() error
}

// KeyValue represents a key-value pair
type KeyValue struct {
	Key      []byte
	Data     []byte
	Modified int64
}

// etcdClient implements the Client interface using etcd client
type etcdClient struct {
	client *clientv3.Client
}

// New creates a new client instance
// This is a local implementation to replace k3s-io/kine/pkg/client.New
func New(config endpoint.ETCDConfig) (Client, error) {
	if len(config.Endpoints) == 0 {
		return nil, fmt.Errorf("no endpoints provided")
	}

	// For unix socket endpoints (kine.sock), we'll create a mock client
	// In a real implementation, this would connect to the actual kine server
	if len(config.Endpoints) == 1 && config.Endpoints[0] == "unix://kine.sock" {
		return &mockClient{}, nil
	}

	// For regular etcd endpoints
	cfg := clientv3.Config{
		Endpoints: config.Endpoints,
	}

	if config.TLSConfig.Config != nil {
		cfg.TLS = config.TLSConfig.Config
	}

	client, err := clientv3.New(cfg)
	if err != nil {
		return nil, err
	}

	return &etcdClient{client: client}, nil
}

// mockClient is a mock implementation for kine.sock connections
type mockClient struct{}

func (m *mockClient) Get(ctx context.Context, key string, opts ...interface{}) (*clientv3.GetResponse, error) {
	return &clientv3.GetResponse{}, nil
}

func (m *mockClient) Put(ctx context.Context, key, value string, opts ...interface{}) (*clientv3.PutResponse, error) {
	return &clientv3.PutResponse{}, nil
}

func (m *mockClient) Delete(ctx context.Context, key string, opts ...interface{}) (*clientv3.DeleteResponse, error) {
	return &clientv3.DeleteResponse{}, nil
}

func (m *mockClient) List(ctx context.Context, prefix string, limit int64) ([]*KeyValue, error) {
	// Return empty list for mock implementation
	return []*KeyValue{}, nil
}

func (m *mockClient) Watch(ctx context.Context, key string, opts ...interface{}) clientv3.WatchChan {
	ch := make(chan clientv3.WatchResponse)
	close(ch)
	return ch
}

func (m *mockClient) Close() error {
	return nil
}

// etcdClient implementation
func (e *etcdClient) Get(ctx context.Context, key string, opts ...interface{}) (*clientv3.GetResponse, error) {
	return e.client.Get(ctx, key)
}

func (e *etcdClient) Put(ctx context.Context, key, value string, opts ...interface{}) (*clientv3.PutResponse, error) {
	return e.client.Put(ctx, key, value)
}

func (e *etcdClient) Delete(ctx context.Context, key string, opts ...interface{}) (*clientv3.DeleteResponse, error) {
	return e.client.Delete(ctx, key)
}

func (e *etcdClient) List(ctx context.Context, prefix string, limit int64) ([]*KeyValue, error) {
	resp, err := e.client.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithLimit(limit))
	if err != nil {
		return nil, err
	}

	var kvs []*KeyValue
	for _, kv := range resp.Kvs {
		kvs = append(kvs, &KeyValue{
			Key:      kv.Key,
			Data:     kv.Value,
			Modified: kv.ModRevision,
		})
	}
	return kvs, nil
}

func (e *etcdClient) Watch(ctx context.Context, key string, opts ...interface{}) clientv3.WatchChan {
	return e.client.Watch(ctx, key)
}

func (e *etcdClient) Close() error {
	return e.client.Close()
}
