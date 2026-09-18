package cmds

import (
	"testing"

	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/netsy"
)

// TestNetsyServerFlags pins the CLI contract of the Netsy datastore flags:
// names, defaults and environment variables an operator scripts against.
func TestNetsyServerFlags(t *testing.T) {
	flags := map[string]cli.Flag{}
	for _, f := range ServerFlags {
		switch flag := f.(type) {
		case *cli.BoolFlag:
			flags[flag.Name] = flag
		case *cli.StringFlag:
			flags[flag.Name] = flag
		case *cli.IntFlag:
			flags[flag.Name] = flag
		}
	}

	for _, name := range []string{
		"netsy", "netsy-binary", "netsy-cluster-id", "netsy-node-id", "netsy-data-dir",
		"netsy-bucket", "netsy-key-prefix", "netsy-storage-provider",
		"netsy-client-port", "netsy-peer-port", "netsy-election-port", "netsy-health-port",
	} {
		if _, ok := flags[name]; !ok {
			t.Errorf("ServerFlags missing --%s", name)
		}
	}

	intFlag := func(name string) *cli.IntFlag { return flags[name].(*cli.IntFlag) }
	stringFlag := func(name string) *cli.StringFlag { return flags[name].(*cli.StringFlag) }

	for name, want := range map[string]int{
		"netsy-client-port":   netsy.DefaultClientPort,
		"netsy-peer-port":     netsy.DefaultPeerPort,
		"netsy-election-port": netsy.DefaultElectionPort,
		"netsy-health-port":   netsy.DefaultHealthPort,
	} {
		if got := intFlag(name).Value; got != want {
			t.Errorf("--%s default = %d, want %d", name, got, want)
		}
	}

	if got := stringFlag("netsy-binary").Value; got != netsy.DefaultBinary {
		t.Errorf("--netsy-binary default = %q, want %q", got, netsy.DefaultBinary)
	}
	if got := stringFlag("netsy-storage-provider").Value; got != "s3" {
		t.Errorf("--netsy-storage-provider default = %q, want s3", got)
	}

	// Every flag must be settable by environment for service managers.
	for _, name := range []string{
		"netsy", "netsy-binary", "netsy-cluster-id", "netsy-node-id", "netsy-data-dir",
		"netsy-bucket", "netsy-key-prefix", "netsy-storage-provider",
		"netsy-client-port", "netsy-peer-port", "netsy-election-port", "netsy-health-port",
	} {
		var env string
		switch flag := flags[name].(type) {
		case *cli.BoolFlag:
			env = flag.EnvVar
		case *cli.StringFlag:
			env = flag.EnvVar
		case *cli.IntFlag:
			env = flag.EnvVar
		}
		if env == "" {
			t.Errorf("--%s has no EnvVar", name)
		}
	}
}
