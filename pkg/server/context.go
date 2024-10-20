package server

import (
	"context"

	helmcrd "github.com/k3s-io/helm-controller/pkg/crd"
	"github.com/k3s-io/helm-controller/pkg/generated/controllers/helm.cattle.io"
	"github.com/pkg/errors"
	"github.com/rancher/wrangler/v3/pkg/crd"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/apps"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/batch"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/core"
	"github.com/rancher/wrangler/v3/pkg/generated/controllers/rbac"
	"github.com/rancher/wrangler/v3/pkg/start"
	addoncrd "github.com/xiaods/k8e/pkg/crd"
	"github.com/xiaods/k8e/pkg/generated/controllers/k8e.cattle.io"
	"github.com/xiaods/k8e/pkg/util"
	"github.com/xiaods/k8e/pkg/version"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/record"
)

type Context struct {
	K8e   *k8e.Factory
	Helm  *helm.Factory
	Batch *batch.Factory
	Apps  *apps.Factory
	Auth  *rbac.Factory
	Core  *core.Factory
	K8s   kubernetes.Interface
	Event record.EventRecorder
}

func (c *Context) Start(ctx context.Context) error {
	return start.All(ctx, 5, c.K8e, c.Helm, c.Apps, c.Auth, c.Batch, c.Core)
}

func NewContext(ctx context.Context, config *Config, forServer bool) (*Context, error) {
	cfg := config.ControlConfig.Runtime.KubeConfigAdmin
	if forServer {
		cfg = config.ControlConfig.Runtime.KubeConfigSupervisor
	}
	restConfig, err := clientcmd.BuildConfigFromFlags("", cfg)
	if err != nil {
		return nil, err
	}
	restConfig.UserAgent = util.GetUserAgent(version.Program + "-supervisor")

	k8s, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}

	var recorder record.EventRecorder
	if forServer {
		recorder = util.BuildControllerEventRecorder(k8s, version.Program+"-supervisor", metav1.NamespaceAll)
		if err := registerCrds(ctx, config, restConfig); err != nil {
			return nil, errors.Wrap(err, "failed to register CRDs")
		}
	}

	return &Context{
		K8e:   k8e.NewFactoryFromConfigOrDie(restConfig),
		Helm:  helm.NewFactoryFromConfigOrDie(restConfig),
		K8s:   k8s,
		Auth:  rbac.NewFactoryFromConfigOrDie(restConfig),
		Apps:  apps.NewFactoryFromConfigOrDie(restConfig),
		Batch: batch.NewFactoryFromConfigOrDie(restConfig),
		Core:  core.NewFactoryFromConfigOrDie(restConfig),
		Event: recorder,
	}, nil
}

func registerCrds(ctx context.Context, config *Config, restConfig *rest.Config) error {
	factory, err := crd.NewFactoryFromClient(restConfig)
	if err != nil {
		return err
	}

	factory.BatchCreateCRDs(ctx, crds(config)...)

	return factory.BatchWait()
}

func crds(config *Config) []crd.CRD {
	defaultCrds := addoncrd.List()
	if !config.ControlConfig.DisableHelmController {
		defaultCrds = append(defaultCrds, helmcrd.List()...)
	}
	return defaultCrds
}
