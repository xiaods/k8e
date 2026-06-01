package cmds

import (
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/sandboxcli"
)

// AppCommandFuncs holds the handler functions for all CLI commands.
type AppCommandFuncs struct {
	Server         func(*cli.Context) error
	Agent          func(*cli.Context) error
	Kubectl        func(*cli.Context) error
	CRICTL         func(*cli.Context) error
	Ctr            func(*cli.Context) error
	Token          TokenCommandFuncs
	EtcdSnapshot   EtcdSnapshotCommandFuncs
	SecretsEncrypt SecretsEncryptCommandFuncs
	Cert           CertCommandFuncs
	Completion     func(*cli.Context) error
}

type TokenCommandFuncs struct {
	Create   func(*cli.Context) error
	Delete   func(*cli.Context) error
	Generate func(*cli.Context) error
	List     func(*cli.Context) error
	Rotate   func(*cli.Context) error
}

type EtcdSnapshotCommandFuncs struct {
	Delete func(*cli.Context) error
	List   func(*cli.Context) error
	Prune  func(*cli.Context) error
	Save   func(*cli.Context) error
}

type SecretsEncryptCommandFuncs struct {
	Status     func(*cli.Context) error
	Enable     func(*cli.Context) error
	Disable    func(*cli.Context) error
	Prepare    func(*cli.Context) error
	Rotate     func(*cli.Context) error
	Reencrypt  func(*cli.Context) error
	RotateKeys func(*cli.Context) error
}

type CertCommandFuncs struct {
	Check    func(*cli.Context) error
	Rotate   func(*cli.Context) error
	RotateCA func(*cli.Context) error
}

// NewAppCommands returns the full command list for the k8e CLI.
func NewAppCommands(f AppCommandFuncs) []cli.Command {
	return []cli.Command{
		NewServerCommand(f.Server),
		NewAgentCommand(f.Agent),
		NewKubectlCommand(f.Kubectl),
		NewCRICTL(f.CRICTL),
		NewCtrCommand(f.Ctr),
		NewTokenCommands(
			f.Token.Create, f.Token.Delete, f.Token.Generate, f.Token.List, f.Token.Rotate,
		),
		NewEtcdSnapshotCommands(
			f.EtcdSnapshot.Delete, f.EtcdSnapshot.List, f.EtcdSnapshot.Prune, f.EtcdSnapshot.Save,
		),
		NewSecretsEncryptCommands(
			f.SecretsEncrypt.Status, f.SecretsEncrypt.Enable, f.SecretsEncrypt.Disable,
			f.SecretsEncrypt.Prepare, f.SecretsEncrypt.Rotate, f.SecretsEncrypt.Reencrypt,
			f.SecretsEncrypt.RotateKeys,
		),
		NewCertCommands(
			f.Cert.Check, f.Cert.Rotate, f.Cert.RotateCA,
		),
		NewCompletionCommand(f.Completion),
		NewSandboxApiKeyCommand(),
	}
}

// NewSandboxApiKeyCommand returns the api-key command renamed for the server binary.
func NewSandboxApiKeyCommand() cli.Command {
	cmd := sandboxcli.ApiKeyCommand()
	cmd.Name = "sandbox-apikey"
	return cmd
}
