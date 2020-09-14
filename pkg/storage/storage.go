package storage

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/etcd"
)

type DB interface {
	InitDB(ctx context.Context) error
	Start(ctx context.Context) error
	Test(context.Context) error
}

type Storage struct {
	db DB
}

func New(cfg *config.Control) *Storage {
	return &Storage{db: etcd.New(cfg)}
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
	return s.db.Start(ctx)
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
			if err := s.db.Test(ctx); err != nil {
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
