package main

import (
	"os"

	bindata "github.com/go-bindata/go-bindata"
	controllergen "github.com/rancher/wrangler/v3/pkg/controller-gen"
	"github.com/rancher/wrangler/v3/pkg/controller-gen/args"
	"github.com/sirupsen/logrus"
	v1 "github.com/xiaods/k8e/pkg/apis/k8e.cattle.io/v1"
)

var (
	basePackage = "github.com/xiaods/k8e/types"
)

func main() {
	os.Unsetenv("GOPATH")
	bc := &bindata.Config{
		Input: []bindata.InputConfig{
			{
				Path:      "build/data",
				Recursive: true,
			},
		},
		Package:    "data",
		NoCompress: true,
		NoMemCopy:  true,
		NoMetadata: true,
		Output:     "pkg/data/zz_generated_bindata.go",
	}
	if err := bindata.Translate(bc); err != nil {
		logrus.Fatal(err)
	}

	bc = &bindata.Config{
		Input: []bindata.InputConfig{
			{
				Path:      "manifests",
				Recursive: true,
			},
		},
		Package:    "deploy",
		NoMetadata: true,
		Prefix:     "manifests/",
		Output:     "pkg/deploy/zz_generated_bindata.go",
		Tags:       "!no_stage",
	}
	if err := bindata.Translate(bc); err != nil {
		logrus.Fatal(err)
	}

	bc = &bindata.Config{
		Input: []bindata.InputConfig{
			{
				Path:      "build/static",
				Recursive: true,
			},
		},
		Package:    "static",
		NoMetadata: true,
		Prefix:     "build/static/",
		Output:     "pkg/static/zz_generated_bindata.go",
		Tags:       "!no_stage",
	}
	if err := bindata.Translate(bc); err != nil {
		logrus.Fatal(err)
	}

	controllergen.Run(args.Options{
		OutputPackage: "github.com/xiaods/k8e/pkg/generated",
		Boilerplate:   "hack/boilerplate.go.txt",
		Groups: map[string]args.Group{
			"k8e.cattle.io": {
				Types: []interface{}{
					v1.Addon{},
					v1.ETCDSnapshotFile{},
				},
				GenerateTypes:   true,
				GenerateClients: true,
			},
		},
	})
}
