package server

import (
	"context"

	"github.com/rancher/helm-controller/pkg/generated/controllers/helm.cattle.io"
	"github.com/rancher/wrangler-api/pkg/generated/controllers/apps"
	"github.com/rancher/wrangler-api/pkg/generated/controllers/batch"
	"github.com/rancher/wrangler-api/pkg/generated/controllers/core"
	"github.com/rancher/wrangler-api/pkg/generated/controllers/rbac"
	"github.com/rancher/wrangler/pkg/apply"
	"github.com/rancher/wrangler/pkg/crd"
	"github.com/rancher/wrangler/pkg/start"
	"github.com/xiaods/k8e/pkg/generated/controllers/k8e.cattle.io"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

type Context struct {
	K8e   *k8e.Factory
	Helm  *helm.Factory
	Batch *batch.Factory
	Apps  *apps.Factory
	Auth  *rbac.Factory
	Core  *core.Factory
	K8s   kubernetes.Interface
	Apply apply.Apply
}

func (c *Context) Start(ctx context.Context) error {
	return start.All(ctx, 5, c.K8e, c.Helm, c.Apps, c.Auth, c.Batch, c.Core)
}

func newContext(ctx context.Context, cfg string) (*Context, error) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", cfg)
	if err != nil {
		return nil, err
	}

	if err := crds(ctx, restConfig); err != nil {
		return nil, err
	}

	k8s := kubernetes.NewForConfigOrDie(restConfig)
	return &Context{
		K8e:   k8e.NewFactoryFromConfigOrDie(restConfig),
		Helm:  helm.NewFactoryFromConfigOrDie(restConfig),
		K8s:   k8s,
		Auth:  rbac.NewFactoryFromConfigOrDie(restConfig),
		Apps:  apps.NewFactoryFromConfigOrDie(restConfig),
		Batch: batch.NewFactoryFromConfigOrDie(restConfig),
		Core:  core.NewFactoryFromConfigOrDie(restConfig),
		Apply: apply.New(k8s, apply.NewClientFactory(restConfig)).WithDynamicLookup(),
	}, nil
}

func crds(ctx context.Context, config *rest.Config) error {
	factory, err := crd.NewFactoryFromClient(config)
	if err != nil {
		return err
	}

	factory.BatchCreateCRDs(ctx, crd.NamespacedTypes(
		"Addon.k8e.cattle.io/v1",
		"HelmChart.helm.cattle.io/v1",
		"HelmChartConfig.helm.cattle.io/v1")...)

	return factory.BatchWait()
}
