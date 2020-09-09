package master

import (
	"context"

	"github.com/xiaods/k8e/pkg/cluster"
)

func StartMaster(ctx context.Context) error {
	var err error
	if err = master(ctx); err != nil {
		return err
	}
	return nil
}

func master(ctx context.Context) error {
	var err error
	if err = prepare(ctx); err != nil {
		return err
	}
	return err
}

func prepare(ctx context.Context) error {
	c := cluster.New()
	c.Start(ctx)
	//	e := etcd.New()
	//	e.Start()
	return nil
}

func apiServer() {

}
