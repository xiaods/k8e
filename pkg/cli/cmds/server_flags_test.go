package cmds

import (
	"testing"

	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/netsy"
)

// netsyFlagNames are the datastore flags an operator scripts against.
var netsyFlagNames = []string{
	"netsy", "netsy-binary", "netsy-cluster-id", "netsy-node-id", "netsy-data-dir",
	"netsy-bucket", "netsy-key-prefix", "netsy-storage-provider",
	"netsy-client-port", "netsy-peer-port", "netsy-election-port", "netsy-health-port",
}

// TestNetsyServerFlags pins the CLI contract of the Netsy datastore flags:
// names, defaults and environment variables an operator scripts against.
func TestNetsyServerFlags(t *testing.T) {
	flags := flagsByName(t)
	for _, name := range netsyFlagNames {
		if _, ok := flags[name]; !ok {
			t.Errorf("ServerFlags missing --%s", name)
		}
	}

	assertNetsyDefaults(t, flags)
	assertNetsyEnvVars(t, flags)
}

// flagsByName indexes ServerFlags by flag name.
func flagsByName(t *testing.T) map[string]cli.Flag {
	t.Helper()
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
	return flags
}

// assertNetsyDefaults checks the defaults the Netsy flags ship with.
func assertNetsyDefaults(t *testing.T, flags map[string]cli.Flag) {
	t.Helper()
	for name, want := range map[string]int{
		"netsy-client-port":   netsy.DefaultClientPort,
		"netsy-peer-port":     netsy.DefaultPeerPort,
		"netsy-election-port": netsy.DefaultElectionPort,
		"netsy-health-port":   netsy.DefaultHealthPort,
	} {
		if got := intFlagValue(t, flags[name]); got != want {
			t.Errorf("--%s default = %d, want %d", name, got, want)
		}
	}

	for name, want := range map[string]string{
		"netsy-binary":           netsy.DefaultBinary,
		"netsy-storage-provider": "s3",
	} {
		if got := stringFlagValue(t, flags[name]); got != want {
			t.Errorf("--%s default = %q, want %q", name, got, want)
		}
	}
}

// assertNetsyEnvVars checks every flag is settable by environment for service
// managers.
func assertNetsyEnvVars(t *testing.T, flags map[string]cli.Flag) {
	t.Helper()
	for _, name := range netsyFlagNames {
		f, ok := flags[name]
		if !ok {
			t.Errorf("ServerFlags missing --%s", name)
			continue
		}
		if flagEnvVar(f) == "" {
			t.Errorf("--%s has no EnvVar", name)
		}
	}
}

func intFlagValue(t *testing.T, f cli.Flag) int {
	t.Helper()
	flag, ok := f.(*cli.IntFlag)
	if !ok {
		t.Fatalf("flag %T is not an IntFlag", f)
	}
	return flag.Value
}

func stringFlagValue(t *testing.T, f cli.Flag) string {
	t.Helper()
	flag, ok := f.(*cli.StringFlag)
	if !ok {
		t.Fatalf("flag %T is not a StringFlag", f)
	}
	return flag.Value
}

func flagEnvVar(f cli.Flag) string {
	switch flag := f.(type) {
	case *cli.BoolFlag:
		return flag.EnvVar
	case *cli.StringFlag:
		return flag.EnvVar
	case *cli.IntFlag:
		return flag.EnvVar
	}
	return ""
}
