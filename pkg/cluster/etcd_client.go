package cluster

import (
	"context"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// BootstrapValue represents a key-value pair from the datastore
type BootstrapValue struct {
	Key      string
	Data     []byte
	Modified int64
}

// EtcdStorageClient wraps etcd client to provide kine-compatible interface
type EtcdStorageClient struct {
	client *clientv3.Client
}

// NewEtcdStorageClient creates a new etcd storage client
func NewEtcdStorageClient(config *clientv3.Config) (*EtcdStorageClient, error) {
	client, err := clientv3.New(*config)
	if err != nil {
		return nil, err
	}
	return &EtcdStorageClient{client: client}, nil
}

// Close closes the etcd client connection
func (e *EtcdStorageClient) Close() error {
	return e.client.Close()
}

// Get retrieves a value by key
func (e *EtcdStorageClient) Get(ctx context.Context, key string) (*BootstrapValue, error) {
	resp, err := e.client.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	if len(resp.Kvs) == 0 {
		return nil, nil
	}
	kv := resp.Kvs[0]
	return &BootstrapValue{
		Key:      string(kv.Key),
		Data:     kv.Value,
		Modified: kv.ModRevision,
	}, nil
}

// List retrieves all keys with the given prefix
func (e *EtcdStorageClient) List(ctx context.Context, prefix string, limit int64) ([]BootstrapValue, error) {
	opts := []clientv3.OpOption{clientv3.WithPrefix()}
	if limit > 0 {
		opts = append(opts, clientv3.WithLimit(limit))
	}
	resp, err := e.client.Get(ctx, prefix, opts...)
	if err != nil {
		return nil, err
	}
	var values []BootstrapValue
	for _, kv := range resp.Kvs {
		values = append(values, BootstrapValue{
			Key:      string(kv.Key),
			Data:     kv.Value,
			Modified: kv.ModRevision,
		})
	}
	return values, nil
}

// Create creates a new key-value pair
func (e *EtcdStorageClient) Create(ctx context.Context, key string, data []byte) error {
	// Use a transaction to ensure the key doesn't exist
	txn := e.client.Txn(ctx)
	resp, err := txn.If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, string(data))).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return &KeyExistsError{Key: key}
	}
	return nil
}

// Update updates an existing key-value pair
func (e *EtcdStorageClient) Update(ctx context.Context, key string, revision int64, data []byte) error {
	// Use a transaction to ensure the revision matches
	txn := e.client.Txn(ctx)
	resp, err := txn.If(clientv3.Compare(clientv3.ModRevision(key), "=", revision)).
		Then(clientv3.OpPut(key, string(data))).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return &RevisionMismatchError{Key: key, Expected: revision}
	}
	return nil
}

// Delete deletes a key-value pair
func (e *EtcdStorageClient) Delete(ctx context.Context, key string, revision int64) error {
	// Use a transaction to ensure the revision matches
	txn := e.client.Txn(ctx)
	resp, err := txn.If(clientv3.Compare(clientv3.ModRevision(key), "=", revision)).
		Then(clientv3.OpDelete(key)).
		Commit()
	if err != nil {
		return err
	}
	if !resp.Succeeded {
		return &RevisionMismatchError{Key: key, Expected: revision}
	}
	return nil
}

// KeyExistsError is returned when trying to create a key that already exists
type KeyExistsError struct {
	Key string
}

func (e *KeyExistsError) Error() string {
	return "key exists"
}

// RevisionMismatchError is returned when the revision doesn't match
type RevisionMismatchError struct {
	Key      string
	Expected int64
}

func (e *RevisionMismatchError) Error() string {
	return "revision mismatch"
}
