package cloudprovider

import (
	"io"

	"github.com/xiaods/k8e/pkg/version"
	cloudprovider "k8s.io/cloud-provider"
)

type k8e struct {
}

var _ cloudprovider.Interface = &k8e{}

func init() {
	cloudprovider.RegisterCloudProvider(version.Program, func(config io.Reader) (cloudprovider.Interface, error) {
		return &k8e{}, nil
	})
}

func (k *k8e) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
}

func (k *k8e) Instances() (cloudprovider.Instances, bool) {
	return nil, false
}

func (k *k8e) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return k, true
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
