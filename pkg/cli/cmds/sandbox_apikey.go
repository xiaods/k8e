package cmds

import (
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/sandboxcli"
)

func NewSandboxApiKeyCommand() cli.Command {
	cmd := sandboxcli.ApiKeyCommand()
	cmd.Name = "sandbox-api-key"
	cmd.Usage = "Manage sandbox API keys for remote cluster access"
	return cmd
}
