package cluster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/bootstrap"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/storage"
	"github.com/xiaods/k8e/pkg/version"
)

type Cluster struct {
	s                *storage.Storage
	clientAccessInfo *clientaccess.Info
	cfg              *config.Control
	shouldBootstrap  bool
}

func New(cfg *config.Control) *Cluster {
	c := &Cluster{}
	c.s = storage.New(cfg)
	c.cfg = cfg
	return c
}

func (c *Cluster) initHTTP(ctx context.Context) error {
	h, err := c.s.InitDB(ctx)
	if err != nil {
		return err
	}
	c.cfg.DBInfoHandler = h
	return nil
}

func (c *Cluster) BootstrapLoad(config *config.Control) error {
	shouldBootstrap, err := c.ShouldBootstrapLoad(config)
	if err != nil {
		return err
	}
	c.shouldBootstrap = shouldBootstrap
	if shouldBootstrap {
		return c.bootstrap()
	}
	return nil
}

func (c *Cluster) ShouldBootstrapLoad(cfg *config.Control) (bool, error) {
	if c.s != nil {
		if cfg.JoinURL == "" { //集群server url
			return false, nil
		}

		token, err := clientaccess.NormalizeAndValidateTokenForUser(cfg.JoinURL, cfg.Token, "server")
		if err != nil {
			return false, err
		}

		info, err := clientaccess.ParseAndValidateToken(cfg.JoinURL, token)
		if err != nil {
			return false, err
		}
		c.clientAccessInfo = info
	}

	stamp := c.bootstrapStamp()
	if _, err := os.Stat(stamp); err == nil {
		logrus.Info("Cluster bootstrap already complete")
		return false, nil
	}

	// if s.db != nil && cfg.Token == "" {
	// 	return false, fmt.Errorf("K3S_TOKEN is required to join a cluster")
	// }

	return true, nil
}

func (c *Cluster) bootstrapStamp() string {
	return filepath.Join(c.cfg.DataDir, "db/joined-"+keyHash(c.cfg.Token))
}

func keyHash(passphrase string) string {
	d := sha256.New()
	d.Write([]byte(passphrase))
	return hex.EncodeToString(d.Sum(nil)[:])[:12]
}

func (c *Cluster) bootstrap() error {
	content, err := clientaccess.Get("/v1-"+version.Program+"/server-bootstrap", c.clientAccessInfo)
	if err != nil {
		return err
	}
	runtime := c.cfg.Runtime
	return bootstrap.Read(bytes.NewBuffer(content), &runtime.ControlRuntimeBootstrap)
}

func (c *Cluster) bootstrapped() error {
	if err := os.MkdirAll(filepath.Dir(c.bootstrapStamp()), 0700); err != nil {
		return err
	}

	if _, err := os.Stat(c.bootstrapStamp()); err == nil {
		return nil
	}

	f, err := os.Create(c.bootstrapStamp())
	if err != nil {
		return err
	}

	return f.Close()
}

func (c *Cluster) Start(ctx context.Context) (<-chan struct{}, error) {
	if err := c.initHTTP(ctx); err != nil {
		return nil, err
	}
	ch, err := c.s.Start(ctx, c.clientAccessInfo)
	if err != nil {
		return nil, err
	}
	if c.shouldBootstrap {
		return ch, c.bootstrapped()
	}
	return ch, nil
}
