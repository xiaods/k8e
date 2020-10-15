package cluster

import (
	"context"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/storage"
)

type Cluster struct {
	s   *storage.Storage
	cfg *config.Control
}

func New(cfg *config.Control) *Cluster {
	c := &Cluster{}
	c.s = storage.New(cfg)
	c.cfg = cfg
	return c
}

func (c *Cluster) initHttp(ctx context.Context) error {
	h, err := c.s.InitDB(ctx)
	if err != nil {
		return err
	}
	c.cfg.DBInfoHandler = h
	return nil
}

func (c *Cluster) BootstrapLoad(config *config.Control) error {
	if _, err := c.s.ShouldBootstrapLoad(config); err != nil {
		return err
	}
	return nil
}

func (c *Cluster) Start(ctx context.Context) (<-chan struct{}, error) {
	var err error
	if err = c.initHttp(ctx); err != nil {
		return nil, err
	}
	return c.s.Start(ctx)
}
