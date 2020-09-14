package master

import (
	"context"
	"os"
	"path/filepath"

	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/storage"
)

func StartMaster(ctx context.Context, cfg *cmds.MasterConfig) error {
	var err error
	if err = master(ctx, cfg); err != nil {
		return err
	}
	return nil
}

func master(ctx context.Context, cfg *cmds.MasterConfig) error {
	var err error
	if err = prepare(ctx); err != nil {
		return err
	}

	return err
}

func prepare(ctx context.Context) error {
	c := storage.New()
	c.Start(ctx)
	//	e := etcd.New()
	//	e.Start()
	return nil
}

func apiServer(ctx context.Context, cfg *cmds.MasterConfig) {
	argsMap := make(map[string]string)
	certDir := filepath.Join(cfg.DataDir, "tls", "temporary-certs")
	os.MkdirAll(certDir, 0700)
	argsMap["cert-dir"] = certDir        //存放 TLS 证书的目录。如果提供了 --tls-cert-file 和 --tls-private-key-file 选项，该标志将被忽略。（默认值 "/var/run/kubernetes"）
	argsMap["allow-privileged"] = "true" // 如果为 true, 将允许特权容器
	argsMap[""] = ""

}
