package cloudprovider

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/rancher/wrangler/v3/pkg/apply"
	appsclient "github.com/rancher/wrangler/v3/pkg/generated/controllers/apps/v1"
	coreclient "github.com/rancher/wrangler/v3/pkg/generated/controllers/core/v1"
	discoveryclient "github.com/rancher/wrangler/v3/pkg/generated/controllers/discovery/v1"
	"github.com/xiaods/k8e/pkg/version"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	cloudprovider "k8s.io/cloud-provider"
)

// Config describes externally-configurable cloud provider configuration.
// This is normally unmarshalled from a JSON config file.
type Config struct {
	NodeEnabled bool `json:"nodeEnabled"`
	Rootless    bool `json:"rootless"`
}

type k8e struct {
	Config

	client   kubernetes.Interface
	recorder record.EventRecorder

	processor      apply.Apply
	daemonsetCache appsclient.DaemonSetCache
	endpointsCache discoveryclient.EndpointSliceCache
	nodeCache      coreclient.NodeCache
	podCache       coreclient.PodCache
	workqueue      workqueue.RateLimitingInterface
}

var _ cloudprovider.Interface = &k8e{}

func init() {
	cloudprovider.RegisterCloudProvider(version.Program, func(config io.Reader) (cloudprovider.Interface, error) {
		var err error
		k := k8e{
			Config: Config{
				NodeEnabled: true,
			},
		}

		if config != nil {
			var bytes []byte
			bytes, err = io.ReadAll(config)
			if err == nil {
				err = json.Unmarshal(bytes, &k.Config)
			}
		}

		if !k.NodeEnabled {
			return nil, fmt.Errorf("all cloud-provider functionality disabled by config")
		}

		return &k, err
	})
}

func (k *k8e) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
}

func (k *k8e) Instances() (cloudprovider.Instances, bool) {
	return nil, false
}

func (k *k8e) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return k, k.NodeEnabled
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
