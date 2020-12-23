package main

import (
	"github.com/xiaods/k8e/pkg/containerd"
	"k8s.io/klog"
)

func main() {
	klog.InitFlags(nil)
	containerd.Main()
}