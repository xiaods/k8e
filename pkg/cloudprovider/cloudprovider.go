package cloudprovider

import (
	"encoding/json"
	"io"
	"io/ioutil"

	"github.com/rancher/wrangler/pkg/apply"
	"github.com/rancher/wrangler/pkg/generated/controllers/apps"
	appsclient "github.com/rancher/wrangler/pkg/generated/controllers/apps/v1"
	"github.com/rancher/wrangler/pkg/generated/controllers/core"
	coreclient "github.com/rancher/wrangler/pkg/generated/controllers/core/v1"
	"github.com/rancher/wrangler/pkg/generic"
	"github.com/rancher/wrangler/pkg/start"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/util"
	"github.com/xiaods/k8e/pkg/version"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	cloudprovider "k8s.io/cloud-provider"
)

// Config describes externally-configurable cloud provider configuration.
// This is normally unmarshalled from a JSON config file.
type Config struct {
	LBEnabled   bool   `json:"lbEnabled"`
	LBImage     string `json:"lbImage"`
	LBNamespace string `json:"lbNamespace"`
	Rootless    bool   `json:"rootless"`
}

type k8e struct {
	Config

	client   kubernetes.Interface
	recorder record.EventRecorder

	processor      apply.Apply
	daemonsetCache appsclient.DaemonSetCache
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
				LBEnabled:   true,
				LBImage:     DefaultLBImage,
				LBNamespace: DefaultLBNS,
			},
		}

		if config != nil {
			var bytes []byte
			bytes, err = ioutil.ReadAll(config)
			if err == nil {
				err = json.Unmarshal(bytes, &k.Config)
			}
		}

		return &k, err
	})
}

func (k *k8e) Initialize(clientBuilder cloudprovider.ControllerClientBuilder, stop <-chan struct{}) {
	ctx, _ := wait.ContextForChannel(stop)
	config := clientBuilder.ConfigOrDie(controllerName)
	k.client = kubernetes.NewForConfigOrDie(config)

	if k.LBEnabled {
		// Wrangler controller and caches are only needed if the load balancer controller is enabled.
		k.recorder = util.BuildControllerEventRecorder(k.client, controllerName, meta.NamespaceAll)
		coreFactory := core.NewFactoryFromConfigOrDie(config)
		k.nodeCache = coreFactory.Core().V1().Node().Cache()

		lbCoreFactory := core.NewFactoryFromConfigWithOptionsOrDie(config, &generic.FactoryOptions{Namespace: k.LBNamespace})
		lbAppsFactory := apps.NewFactoryFromConfigWithOptionsOrDie(config, &generic.FactoryOptions{Namespace: k.LBNamespace})

		processor, err := apply.NewForConfig(config)
		if err != nil {
			logrus.Fatalf("Failed to create apply processor for %s: %v", controllerName, err)
		}
		k.processor = processor.WithDynamicLookup().WithCacheTypes(lbAppsFactory.Apps().V1().DaemonSet())
		k.daemonsetCache = lbAppsFactory.Apps().V1().DaemonSet().Cache()
		k.podCache = lbCoreFactory.Core().V1().Pod().Cache()
		k.workqueue = workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())

		if err := k.Register(ctx, coreFactory.Core().V1().Node(), lbCoreFactory.Core().V1().Pod()); err != nil {
			logrus.Fatalf("Failed to register %s handlers: %v", controllerName, err)
		}

		if err := start.All(ctx, 1, coreFactory, lbCoreFactory, lbAppsFactory); err != nil {
			logrus.Fatalf("Failed to start %s controllers: %v", controllerName, err)
		}
	} else {
		// If load-balancer functionality has not been enabled, delete managed daemonsets.
		// This uses the raw kubernetes client, as the controllers are not started when the load balancer controller is disabled.
		if err := k.deleteAllDaemonsets(ctx); err != nil {
			logrus.Fatalf("Failed to clean up %s daemonsets: %v", controllerName, err)
		}
	}
}

func (k *k8e) Instances() (cloudprovider.Instances, bool) {
	return nil, false
}

func (k *k8e) InstancesV2() (cloudprovider.InstancesV2, bool) {
	return k, true
}

func (k *k8e) LoadBalancer() (cloudprovider.LoadBalancer, bool) {
	return k, k.LBEnabled
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
