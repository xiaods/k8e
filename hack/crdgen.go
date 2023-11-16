package main

import (
	"os"

	"github.com/rancher/wrangler/pkg/crd"
	k8ecrd "github.com/xiaods/k8e/pkg/crd"
	_ "github.com/xiaods/k8e/pkg/generated/controllers/k8e.cattle.io/v1"
)

func main() {
	crd.Print(os.Stdout, k8ecrd.List())
}
