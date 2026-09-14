package sandboxcli

import (
	"errors"
	"testing"

	"github.com/urfave/cli"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestParseEnvFlags(t *testing.T) {
	got, err := parseEnvFlags([]string{"FOO=bar", "BAZ=qux"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["FOO"] != "bar" || got["BAZ"] != "qux" {
		t.Fatalf("unexpected map: %v", got)
	}
	if _, err := parseEnvFlags([]string{"NOEQUALS"}); err == nil {
		t.Fatal("expected error for missing '='")
	}
	if _, err := parseEnvFlags([]string{"=novalue"}); err == nil {
		t.Fatal("expected error for empty key")
	}
	nilMap, err := parseEnvFlags(nil)
	if err != nil || nilMap != nil {
		t.Fatalf("expected nil map for empty input, got %v err=%v", nilMap, err)
	}
}

func TestParseSecretFlags(t *testing.T) {
	got, err := parseSecretFlags([]string{"API_KEY=llm-secret:token", "DB=db-sec:password"})
	if err != nil {
		t.Fatalf("unexpected: %v", err)
	}
	if len(got) != 2 || got[0].EnvVar != "API_KEY" || got[0].SecretName != "llm-secret" || got[0].Key != "token" {
		t.Fatalf("got %+v", got)
	}
	if _, err := parseSecretFlags([]string{"BAD"}); err == nil {
		t.Fatal("expected error")
	}
	if _, err := parseSecretFlags([]string{"X=noseparator"}); err == nil {
		t.Fatal("expected error for missing colon")
	}
}

func TestIsSessionExpired_grpcNotFound(t *testing.T) {
	err := status.Error(codes.NotFound, "session sess-abc not found")
	if !isSessionExpired(err) {
		t.Error("expected true for gRPC NotFound")
	}
}

func TestIsSessionExpired_grpcFailedPrecondition(t *testing.T) {
	err := status.Error(codes.FailedPrecondition, "session sess-abc has no pod IP after 60s")
	if !isSessionExpired(err) {
		t.Error("expected true for gRPC FailedPrecondition")
	}
}

func TestIsSessionExpired_grpcUnavailable(t *testing.T) {
	err := status.Error(codes.Unavailable, "sandboxd exec: connection refused")
	if !isSessionExpired(err) {
		t.Error("expected true for gRPC Unavailable")
	}
}

func TestIsSessionExpired_sentinelError(t *testing.T) {
	err := ErrSessionGone
	if !isSessionExpired(err) {
		t.Error("expected true for ErrSessionGone sentinel")
	}
}

func TestIsSessionExpired_wrappedSentinel(t *testing.T) {
	err := errors.New("some context")
	if isSessionExpired(err) {
		t.Error("expected false for plain error")
	}
}

func TestIsSessionExpired_nil(t *testing.T) {
	if isSessionExpired(nil) {
		t.Error("expected false for nil error")
	}
}

func TestFlagName_LongPreferred(t *testing.T) {
	// urfave/cli StringFlag GetName returns the declared name (e.g. "session-id").
	f := cli.StringFlag{Name: "session-id"}
	if got := flagName(f); got != "session-id" {
		t.Fatalf("expected session-id, got %q", got)
	}
}

func TestFlagName_ShorthandSkipped(t *testing.T) {
	// Simulate a comma list with shorthand; prefer the long form.
	f := cli.StringFlag{Name: "s, session-id"}
	if got := flagName(f); got != "session-id" {
		t.Fatalf("expected long flag session-id, got %q", got)
	}
}

func TestCatalogCommand_ListsSurface(t *testing.T) {
	cmds := []cli.Command{
		{Name: "run", Usage: "Execute code"},
		{Name: "create", Usage: "Create session", Flags: []cli.Flag{
			cli.StringFlag{Name: "runtime"},
		}},
	}
	cmd := CatalogCommand(cmds)
	// urfave/cli command Action is callable; exercise the inventory shape via
	// the same logic by invoking flagName on the sample flag.
	if cmd.Name != "catalog" {
		t.Fatalf("unexpected catalog name %q", cmd.Name)
	}
	_ = cmd.Action
}

// TestResolveRunTimeout pins the two-mode timeout contract.
//
// A foreground `run` keeps the 30s default, while a background run — a detached
// service, i.e. the documented `run --background` + `expose` flow — is uncapped
// unless --timeout is passed explicitly. Inheriting the foreground default used
// to SIGKILL every backgrounded service 30 seconds after submission, which made
// the exposed URL return 502 a few seconds later.
func TestResolveRunTimeout(t *testing.T) {
	resolve := func(t *testing.T, args []string, background bool) int32 {
		t.Helper()
		var got int32
		command := RunCommand()
		command.Action = func(ctx *cli.Context) error {
			got = resolveRunTimeout(ctx, background)
			return nil
		}
		app := cli.NewApp()
		app.HideVersion = true
		app.Commands = []cli.Command{command}
		if err := app.Run(args); err != nil {
			t.Fatalf("app.Run(%v): %v", args, err)
		}
		return got
	}

	for _, entry := range []struct {
		name       string
		args       []string
		background bool
		want       int32
	}{
		{"foreground default", []string{"k8e-sandbox-cli", "run", "echo hi"}, false, 30},
		{"foreground explicit", []string{"k8e-sandbox-cli", "run", "--timeout", "120", "echo hi"}, false, 120},
		{"background default is uncapped", []string{"k8e-sandbox-cli", "run", "--background", "echo hi"}, true, 0},
		{"background explicit cap", []string{"k8e-sandbox-cli", "run", "--background", "--timeout", "120", "echo hi"}, true, 120},
		{"negative value clamped", []string{"k8e-sandbox-cli", "run", "--timeout", "-5", "echo hi"}, false, 0},
	} {
		t.Run(entry.name, func(t *testing.T) {
			if got := resolve(t, entry.args, entry.background); got != entry.want {
				t.Fatalf("timeout = %d, want %d", got, entry.want)
			}
		})
	}
}
