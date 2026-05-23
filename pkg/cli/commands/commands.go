package commands

import (
	"github.com/xiaods/k8e/pkg/cli/agent"
	"github.com/xiaods/k8e/pkg/cli/cert"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/cli/completion"
	"github.com/xiaods/k8e/pkg/cli/crictl"
	"github.com/xiaods/k8e/pkg/cli/ctr"
	"github.com/xiaods/k8e/pkg/cli/etcdsnapshot"
	"github.com/xiaods/k8e/pkg/cli/kubectl"
	"github.com/xiaods/k8e/pkg/cli/secretsencrypt"
	"github.com/xiaods/k8e/pkg/cli/server"
	"github.com/xiaods/k8e/pkg/cli/token"
)

// Funcs returns the AppCommandFuncs populated with all CLI command handlers.
func Funcs() cmds.AppCommandFuncs {
	return cmds.AppCommandFuncs{
		Server:  server.Run,
		Agent:   agent.Run,
		Kubectl: kubectl.Run,
		CRICTL:  crictl.Run,
		Ctr:     ctr.Run,
		Token: cmds.TokenCommandFuncs{
			Create: token.Create, Delete: token.Delete,
			Generate: token.Generate, List: token.List, Rotate: token.Rotate,
		},
		EtcdSnapshot: cmds.EtcdSnapshotCommandFuncs{
			Delete: etcdsnapshot.Delete, List: etcdsnapshot.List,
			Prune: etcdsnapshot.Prune, Save: etcdsnapshot.Save,
		},
		SecretsEncrypt: cmds.SecretsEncryptCommandFuncs{
			Status: secretsencrypt.Status, Enable: secretsencrypt.Enable,
			Disable: secretsencrypt.Disable, Prepare: secretsencrypt.Prepare,
			Rotate: secretsencrypt.Rotate, Reencrypt: secretsencrypt.Reencrypt,
			RotateKeys: secretsencrypt.RotateKeys,
		},
		Cert: cmds.CertCommandFuncs{
			Check: cert.Check, Rotate: cert.Rotate, RotateCA: cert.RotateCA,
		},
		Completion: completion.Run,
	}
}
