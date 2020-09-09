package cluster

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/etcd"
)

type Cluster struct {
	etcd *etcd.ETCD
}

func New() *Cluster {
	return &Cluster{etcd: etcd.New()}
}

func (c *Cluster) Start(ctx context.Context) (<-chan struct{}, error) {
	var err error
	if err = c.start(ctx); err != nil {
		return nil, errors.Wrap(err, "start cluster and https")
	}

	return nil, nil
}

func (c *Cluster) start(ctx context.Context) error {
	return c.etcd.Start()
}

func (c *Cluster) testDB(ctx context.Context) (<-chan struct{}, error) {
	result := make(chan struct{})
	if c.etcd == nil {
		close(result)
		return result, nil
	}

	go func() {
		defer close(result)
		for {
			if err := c.etcd.Test(ctx); err != nil {
				logrus.Infof("Failed to test data store connection: %v", err)
			} else {
				logrus.Infof("Data store connection OK")
				return
			}

			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
		}
	}()
	return result, nil
}
