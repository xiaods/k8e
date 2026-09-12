package kubectl

import (
	"os"

	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/kubectl"
)

// Run executes the embedded kubectl against the cluster's kubeconfig.
func Run(ctx *cli.Context) error {
	// kubectl parses os.Args itself, so hand it only its own arguments.
	// Without this, `k8e kubectl get pods` would reach kubectl as
	// ["kubectl", "kubectl", "get", "pods"] and fail with
	// `unknown command "kubectl" for "kubectl"`.
	os.Args = append([]string{ctx.Command.Name}, ctx.Args()...)
	kubectl.Main()
	return nil
}
