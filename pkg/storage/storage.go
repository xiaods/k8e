package storage

import (
	"context"
	"net/http"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/etcd"
)

type DB interface {
	InitDB(ctx context.Context) (http.Handler, error)
	Start(context.Context, *clientaccess.Info) error
	Test(context.Context, *clientaccess.Info) error
}

type Storage struct {
	db DB
}

func New(cfg *config.Control) *Storage {
	return &Storage{db: etcd.New(cfg)}
}

func (s *Storage) InitDB(ctx context.Context) (http.Handler, error) {
	return s.db.InitDB(ctx)
}

func (s *Storage) Start(ctx context.Context, info *clientaccess.Info) (<-chan struct{}, error) {
	var err error
	if err = s.start(ctx, info); err != nil {
		logrus.Error(err)
		return nil, errors.Wrap(err, "start cluster and https")
	}
	//test db start
	ready, err := s.testDB(ctx, info)
	if err != nil {
		logrus.Error(err)
		return nil, err
	}
	return ready, nil
}

func (s *Storage) start(ctx context.Context, info *clientaccess.Info) error {
	return s.db.Start(ctx, info)
}

func (s *Storage) testDB(ctx context.Context, info *clientaccess.Info) (<-chan struct{}, error) {
	result := make(chan struct{})
	if s.db == nil {
		close(result)
		return result, nil
	}

	go func() {
		defer close(result)
		for {
			if err := s.db.Test(ctx, info); err != nil {
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
