package main

import (
	"fmt"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// GetLocal reads a user's KUBECONFIG file and returns a Client interface, a REST interface, and current namespace
func GetLocal() (*kubernetes.Clientset, *rest.Config, string, error) {
	var err error

	clientConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{ClusterInfo: clientcmdapi.Cluster{Server: ""}})

	namespace, _, err := clientConfig.Namespace()
	if err != nil {
		return nil, nil, "", err
	}

	config, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, nil, "", err
	}

	client, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, "", err
	}
	return client, config, namespace, nil
}

func main() {

	client, config, ns, _ := GetLocal()

	fmt.Printf("client: %+v\n, config: %+v\n, namespace: %+v", client, config, ns)

}
