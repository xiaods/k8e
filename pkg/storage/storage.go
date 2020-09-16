package storage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/etcd"
)

type DB interface {
	InitDB(ctx context.Context) error
	Start(context.Context, *clientaccess.Info) error
	Test(context.Context, *clientaccess.Info) error
}

type Storage struct {
	clientAccessInfo *clientaccess.Info
	db               DB
}

func New(cfg *config.Control) *Storage {
	return &Storage{db: etcd.New(cfg)}
}

func (s *Storage) ShouldBootstrapLoad(cfg *config.Control) (bool, error) {
	if s.db != nil {
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
		s.clientAccessInfo = info
	}

	stamp := s.bootstrapStamp(cfg)
	if _, err := os.Stat(stamp); err == nil {
		logrus.Info("Cluster bootstrap already complete")
		return false, nil
	}

	if s.db != nil { //&& cfg.Token == ""
		return false, fmt.Errorf("K3S_TOKEN is required to join a cluster")
	}

	return true, nil
}

func (s *Storage) bootstrapStamp(cfg *config.Control) string {
	return filepath.Join(cfg.DataDir, "db/joined-"+keyHash(cfg.Token))
}

func keyHash(passphrase string) string {
	d := sha256.New()
	d.Write([]byte(passphrase))
	return hex.EncodeToString(d.Sum(nil)[:])[:12]
}

func (s *Storage) Start(ctx context.Context) (<-chan struct{}, error) {
	var err error
	if err = s.db.InitDB(ctx); err != nil {
		logrus.Error(err)
		return nil, err
	}
	if err = s.start(ctx); err != nil {
		logrus.Error(err)
		return nil, errors.Wrap(err, "start cluster and https")
	}
	//test db start
	ready, err := s.testDB(ctx)
	if err != nil {
		logrus.Error(err)
		return nil, err
	}
	return ready, nil
}

func (s *Storage) start(ctx context.Context) error {
	return s.db.Start(ctx, s.clientAccessInfo)
}

func (s *Storage) testDB(ctx context.Context) (<-chan struct{}, error) {
	result := make(chan struct{})
	if s.db == nil {
		close(result)
		return result, nil
	}

	go func() {
		defer close(result)
		for {
			if err := s.db.Test(ctx, s.clientAccessInfo); err != nil {
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
