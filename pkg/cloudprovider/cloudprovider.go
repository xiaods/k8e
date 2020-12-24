package cloudprovider

import (
	"context"
	"io"

	"github.com/rancher/wrangler-api/pkg/generated/controllers/core"
	coreclient "github.com/rancher/wrangler-api/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/pkg/start"
	"github.com/xiaods/k8e/pkg/version"
	cloudprovider "k8s.io/cloud-provider"
)

type k8e struct {
	NodeCache coreclient.NodeCache
}

func init() {
	cloudprovider.RegisterCloudProvider(version.Program, func(config io.Reader) (cloudprovider.Interface, error) {
		return &k8e{}, nil
	})
}

func (k *k8e) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	coreFactory := core.NewFactoryFromConfigOrDie(clientBuilder.ConfigOrDie("cloud-controller-manager"))

	go start.All(context.Background(), 1, coreFactory)

	k.NodeCache = coreFactory.Core().V1().Node().Cache()
}

func (k *k8e) Instances() (cloudprovider.Instances, bool) {
	return k, true
}

func (k *k8e) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return nil, false
}

func (k *k8e) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return nil, false
}

func (k *k8e) Zones() (cloudprovider.Zones, bool) {
	return nil, false
}

func (k *k8e) Clusters() (cloudprovider.Clusters, bool) {
	return nil, false
}

func (k *k8e) Routes() (cloudprovider.Routes, bool) {
	return nil, false
}

func (k *k8e) ProviderName() string {
	return version.Program
}

func (k *k8e) HasClusterID() bool {
	return false
}
