// Package sandboxmatrix implements the Agentic AI Sandbox Matrix controller.
package sandboxmatrix

import (
	"context"
	"os"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	sandboxgrpc "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc"
)

var warmPoolGVR = schema.GroupVersionResource{Group: "k8e.cattle.io", Version: "v1alpha1", Resource: "sandboxwarmpools"}

const tlsDir = "/var/lib/k8e/server/tls"

// Register starts the SandboxMatrix controller and gRPC gateway.
func Register(ctx context.Context, k8s kubernetes.Interface, kubeconfig string) error {
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return err
	}

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	go runWarmPoolReconciler(ctx, k8s, dyn)

	srv := sandboxgrpc.NewServer(k8s, dyn,
		tlsDir+"/serving-kube-apiserver.crt",
		tlsDir+"/serving-kube-apiserver.key",
	)
	go func() {
		if err := srv.Start(ctx); err != nil {
			logrus.Errorf("sandbox gRPC gateway: %v", err)
		}
	}()

	if _, err := os.Stat("/dev/kvm"); err == nil {
		logrus.Info("sandbox-matrix: /dev/kvm detected, Firecracker RuntimeClass enabled")
	} else {
		logrus.Info("sandbox-matrix: /dev/kvm not found, Firecracker RuntimeClass skipped")
	}

	logrus.Info("sandbox-matrix: controller started")
	return nil
}

func runWarmPoolReconciler(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileWarmPools(ctx, k8s, dyn)
		}
	}
}

func reconcileWarmPools(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface) {
	pools, err := dyn.Resource(warmPoolGVR).Namespace("sandbox-matrix").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, pool := range pools.Items {
		specMap, _ := pool.Object["spec"].(map[string]interface{})
		size, _ := specMap["size"].(int64)
		runtimeClass, _ := specMap["runtimeClass"].(string)

		pods, err := k8s.CoreV1().Pods("sandbox-matrix").List(ctx, metav1.ListOptions{
			LabelSelector: "sandbox.k8e.io/state=warm",
		})
		if err != nil {
			continue
		}

		for i := int64(len(pods.Items)); i < size; i++ {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "sandbox-warm-",
					Namespace:    "sandbox-matrix",
					Labels:       map[string]string{"sandbox.k8e.io/state": "warm"},
				},
				Spec: warmPodSpec(runtimeClass),
			}
			k8s.CoreV1().Pods("sandbox-matrix").Create(ctx, pod, metav1.CreateOptions{})
		}
	}
}

func warmPodSpec(runtimeClass string) corev1.PodSpec {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "sandbox",
			Image: "ghcr.io/xiaods/k8e-sandbox:latest",
			Ports: []corev1.ContainerPort{{ContainerPort: 2024}},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("4"),
					corev1.ResourceMemory: resource.MustParse("4Gi"),
				},
			},
		}},
		RestartPolicy: corev1.RestartPolicyNever,
	}
	if runtimeClass != "" {
		spec.RuntimeClassName = &runtimeClass
	}
	return spec
}
