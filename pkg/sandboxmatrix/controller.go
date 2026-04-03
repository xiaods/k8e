// Package sandboxmatrix implements the Agentic AI Sandbox Matrix controller.
package sandboxmatrix

import (
	"context"
	"fmt"
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

	"github.com/xiaods/k8e/pkg/daemons/config"
	sandboxgrpc "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc"
)

var warmPoolGVR = schema.GroupVersionResource{Group: "k8e.cattle.io", Version: "v1alpha1", Resource: "sandboxwarmpools"}

const tlsDir = "/var/lib/k8e/server/tls"

// Register starts the SandboxMatrix controller and gRPC gateway.
func Register(ctx context.Context, k8s kubernetes.Interface, kubeconfig string, cfg config.SandboxConfig) error {
	// Apply defaults
	if cfg.DefaultRuntime == "" {
		cfg.DefaultRuntime = "gvisor"
	}
	if cfg.DefaultImage == "" {
		cfg.DefaultImage = "ghcr.io/xiaods/k8e-sandbox:latest"
	}
	if cfg.DefaultCPU == "" {
		cfg.DefaultCPU = "500m"
	}
	if cfg.DefaultMemory == "" {
		cfg.DefaultMemory = "512Mi"
	}
	if cfg.GRPCPort == 0 {
		cfg.GRPCPort = 50051
	}
	if cfg.Namespace == "" {
		cfg.Namespace = "sandbox-matrix"
	}

	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return err
	}

	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return err
	}

	go runWarmPoolReconciler(ctx, k8s, dyn, cfg)

	orch := sandboxgrpc.NewOrchestrator(k8s, dyn)
	go runGCLoop(ctx, orch, cfg.Namespace)

	srv := sandboxgrpc.NewServer(k8s, dyn,
		tlsDir+"/serving-kube-apiserver.crt",
		tlsDir+"/serving-kube-apiserver.key",
		cfg.GRPCPort,
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

	logrus.Infof("sandbox-matrix: controller started (runtime=%s namespace=%s grpc-port=%d)",
		cfg.DefaultRuntime, cfg.Namespace, cfg.GRPCPort)
	return nil
}

func runWarmPoolReconciler(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcileWarmPools(ctx, k8s, dyn, cfg)
		}
	}
}

func reconcileWarmPools(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig) {
	pools, err := dyn.Resource(warmPoolGVR).Namespace(cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for _, pool := range pools.Items {
		specMap, _ := pool.Object["spec"].(map[string]interface{})
		size, _ := specMap["size"].(int64)
		runtimeClass, _ := specMap["runtimeClass"].(string)
		if runtimeClass == "" {
			runtimeClass = cfg.DefaultRuntime
		}

		pods, err := k8s.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "sandbox.k8e.io/state=warm",
		})
		if err != nil {
			continue
		}

		for i := int64(len(pods.Items)); i < size; i++ {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					GenerateName: "sandbox-warm-",
					Namespace:    cfg.Namespace,
					Labels:       map[string]string{"sandbox.k8e.io/state": "warm"},
				},
				Spec: warmPodSpec(runtimeClass, cfg),
			}
			k8s.CoreV1().Pods(cfg.Namespace).Create(ctx, pod, metav1.CreateOptions{})
		}
	}
	updateSandboxMatrixStatus(ctx, k8s, dyn, cfg)
}

func updateSandboxMatrixStatus(ctx context.Context, k8s kubernetes.Interface, dyn dynamic.Interface, cfg config.SandboxConfig) {
	matrixGVR := schema.GroupVersionResource{Group: "k8e.cattle.io", Version: "v1alpha1", Resource: "sandboxmatrices"}
	matrices, err := dyn.Resource(matrixGVR).Namespace(cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil || len(matrices.Items) == 0 {
		return
	}

	warmPods, _ := k8s.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "sandbox.k8e.io/state=warm"})
	activePods, _ := k8s.CoreV1().Pods(cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "sandbox.k8e.io/state=active"})

	readyWarm := 0
	for i := range warmPods.Items {
		if warmPods.Items[i].Status.Phase == corev1.PodRunning {
			readyWarm++
		}
	}

	matrix := matrices.Items[0].DeepCopy()
	if matrix.Object["status"] == nil {
		matrix.Object["status"] = map[string]interface{}{}
	}
	status := matrix.Object["status"].(map[string]interface{})
	status["readyWarmCount"] = int64(readyWarm)
	status["activeSessions"] = int64(len(activePods.Items))
	dyn.Resource(matrixGVR).Namespace(cfg.Namespace).UpdateStatus(ctx, matrix, metav1.UpdateOptions{}) //nolint:errcheck
}

func warmPodSpec(runtimeClass string, cfg config.SandboxConfig) corev1.PodSpec {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "sandbox",
			Image: cfg.DefaultImage,
			Ports: []corev1.ContainerPort{{ContainerPort: int32(sandboxgrpc.SandboxdPort)}},
			Resources: corev1.ResourceRequirements{
				Limits: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse(cfg.DefaultCPU),
					corev1.ResourceMemory: resource.MustParse(cfg.DefaultMemory),
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

// sandboxdPortAddr returns the gRPC listen address for the given port.
func sandboxdPortAddr(port int) string {
	return fmt.Sprintf("127.0.0.1:%d", port)
}

func runGCLoop(ctx context.Context, orch *sandboxgrpc.Orchestrator, namespace string) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			gcExpiredSessions(ctx, orch, namespace)
		}
	}
}

func gcExpiredSessions(ctx context.Context, orch *sandboxgrpc.Orchestrator, namespace string) {
	sessions, err := orch.ListActiveSessions(ctx, namespace)
	if err != nil {
		return
	}
	now := time.Now()
	for _, s := range sessions {
		if s.Status.ExpiresAt != nil && s.Status.ExpiresAt.Time.Before(now) {
			logrus.Infof("sandbox-matrix: GC session %s (expired at %s)", s.Name, s.Status.ExpiresAt.Time)
			if err := orch.DestroySession(ctx, s.Name); err != nil {
				logrus.Warnf("sandbox-matrix: GC destroy %s: %v", s.Name, err)
			}
		}
	}
}
