package cluster

import (
	"context"
	ctls "crypto/tls"
	"crypto/x509"
	"os"
	"time"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/executor"
	"github.com/xiaods/k8e/pkg/endpoint"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// NativeStorageClient 原生etcd存储客户端
type NativeStorageClient struct {
	client *clientv3.Client
	config *config.Control
}

// Value 存储值结构体，兼容kine接口
type Value struct {
	Key      string
	Value    []byte
	Data     []byte // 兼容kine接口
	Revision int64
	Modified int64 // 兼容kine接口
}

// NewNativeStorageClient 创建原生etcd存储客户端
func NewNativeStorageClient(cfg *config.Control) (*NativeStorageClient, error) {
	clientConfig := clientv3.Config{
		Endpoints:   cfg.Runtime.EtcdConfig.Endpoints,
		DialTimeout: 5 * time.Second,
	}

	// 配置TLS
	if cfg.Runtime.EtcdConfig.TLSConfig.CertFile != "" || cfg.Runtime.EtcdConfig.TLSConfig.KeyFile != "" || cfg.Runtime.EtcdConfig.TLSConfig.CAFile != "" {
		tlsConfig, err := loadTLSConfigFromKine(cfg.Runtime.EtcdConfig.TLSConfig)
		if err != nil {
			return nil, err
		}
		clientConfig.TLS = tlsConfig
	}

	client, err := clientv3.New(clientConfig)
	if err != nil {
		return nil, err
	}

	return &NativeStorageClient{
		client: client,
		config: cfg,
	}, nil
}

func loadTLSConfigFromKine(kineTLSConfig endpoint.TLSConfig) (*ctls.Config, error) {
	// Basic TLS Config setup
	config := &ctls.Config{}

	// Load client cert
	if kineTLSConfig.CertFile != "" && kineTLSConfig.KeyFile != "" {
		cert, err := ctls.LoadX509KeyPair(kineTLSConfig.CertFile, kineTLSConfig.KeyFile)
		if err != nil {
			return nil, err
		}
		config.Certificates = []ctls.Certificate{cert}
	}

	// Load CA cert
	if kineTLSConfig.CAFile != "" {
		caCert, err := os.ReadFile(kineTLSConfig.CAFile)
		if err != nil {
			return nil, err
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		config.RootCAs = caCertPool
	}

	return config, nil
}

// Get 获取键值
func (n *NativeStorageClient) Get(ctx context.Context, key string) (*Value, error) {
	resp, err := n.client.Get(ctx, key)
	if err != nil {
		return nil, err
	}

	if len(resp.Kvs) == 0 {
		return nil, nil
	}

	return &Value{
		Key:      string(resp.Kvs[0].Key),
		Value:    resp.Kvs[0].Value,
		Data:     resp.Kvs[0].Value, // 兼容kine接口
		Revision: resp.Kvs[0].ModRevision,
		Modified: resp.Kvs[0].ModRevision, // 兼容kine接口
	}, nil
}

// Put 设置键值
func (n *NativeStorageClient) Put(ctx context.Context, key, value string) error {
	_, err := n.client.Put(ctx, key, value)
	return err
}

// Delete 删除键
func (n *NativeStorageClient) Delete(ctx context.Context, key string) error {
	_, err := n.client.Delete(ctx, key)
	return err
}

// List 列出指定前缀的所有键值对
func (n *NativeStorageClient) List(ctx context.Context, prefix string, limit int64) ([]Value, error) {
	opts := []clientv3.OpOption{clientv3.WithPrefix()}
	if limit > 0 {
		opts = append(opts, clientv3.WithLimit(limit))
	}

	resp, err := n.client.Get(ctx, prefix, opts...)
	if err != nil {
		return nil, err
	}

	var values []Value
	for _, kv := range resp.Kvs {
		values = append(values, Value{
			Key:      string(kv.Key),
			Value:    kv.Value,
			Data:     kv.Value, // 兼容kine接口
			Revision: kv.ModRevision,
			Modified: kv.ModRevision, // 兼容kine接口
		})
	}

	return values, nil
}

// Watch 监听键变化
func (n *NativeStorageClient) Watch(ctx context.Context, key string, revision int64) clientv3.WatchChan {
	opts := []clientv3.OpOption{}
	if revision > 0 {
		opts = append(opts, clientv3.WithRev(revision))
	}
	return n.client.Watch(ctx, key, opts...)
}

// Close 关闭客户端连接
func (n *NativeStorageClient) Close() error {
	return n.client.Close()
}

// loadTLSConfig 从 ServerTrust 配置加载 TLS 配置
func loadTLSConfig(serverTrust executor.ServerTrust) (*ctls.Config, error) {
	tlsConfig := &ctls.Config{}

	if serverTrust.CertFile != "" && serverTrust.KeyFile != "" {
		cert, err := ctls.LoadX509KeyPair(serverTrust.CertFile, serverTrust.KeyFile)
		if err != nil {
			return nil, err
		}
		tlsConfig.Certificates = []ctls.Certificate{cert}
	}

	if serverTrust.TrustedCAFile != "" {
		caCert, err := os.ReadFile(serverTrust.TrustedCAFile)
		if err != nil {
			return nil, err
		}
		caCertPool := x509.NewCertPool()
		caCertPool.AppendCertsFromPEM(caCert)
		tlsConfig.RootCAs = caCertPool
	}

	if serverTrust.ClientCertAuth {
		tlsConfig.ClientAuth = ctls.RequireAndVerifyClientCert
	}

	return tlsConfig, nil
}

// Create 创建键值（如果不存在）
func (n *NativeStorageClient) Create(ctx context.Context, key, value string, ttl int64) error {
	txn := n.client.Txn(ctx)
	txn = txn.If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
	txn = txn.Then(clientv3.OpPut(key, value))
	_, err := txn.Commit()
	return err
}

// Update 更新键值（如果存在）
func (n *NativeStorageClient) Update(ctx context.Context, key, value string, revision int64) error {
	txn := n.client.Txn(ctx)
	txn = txn.If(clientv3.Compare(clientv3.ModRevision(key), "=", revision))
	txn = txn.Then(clientv3.OpPut(key, value))
	_, err := txn.Commit()
	return err
}
