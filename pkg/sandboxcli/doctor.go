package sandboxcli

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/urfave/cli"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// doctorCheck is one configuration check with a PASS/FAIL verdict and, on
// failure, the exact command or edit that fixes it.
type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

type doctorReport struct {
	OK       bool          `json:"ok"`
	Problems int           `json:"problems"`
	Checks   []doctorCheck `json:"checks"`
}

// dshProfilePaths lists every dsh profile's package.json under $DSH_HOME/profiles.
func dshProfilePaths() []string {
	profilesDir := filepath.Join(dshHome(), "profiles")
	entries, err := os.ReadDir(profilesDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(profilesDir, e.Name(), "package.json")
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// dshProfileBundles returns the bundles registered in one dsh profile.
func dshProfileBundles(profilePkg string) []string {
	data, err := os.ReadFile(profilePkg)
	if err != nil {
		return nil
	}
	var parsed struct {
		Dsh struct {
			Profile struct {
				Bundles []string `json:"bundles"`
			} `json:"profile"`
		} `json:"dsh"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	return parsed.Dsh.Profile.Bundles
}

// dshBundleInstalledVersion returns the installed bundle version from the
// profile's node_modules, or "" when missing.
func dshBundleInstalledVersion(profilePkg string) string {
	dir := filepath.Join(filepath.Dir(profilePkg), "node_modules", "@k8e-sandbox", "dsh-k8e-sandbox-bundle")
	data, err := os.ReadFile(filepath.Join(dir, "package.json"))
	if err != nil {
		return ""
	}
	var parsed struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		return ""
	}
	return parsed.Version
}

const k8eBundleName = "@k8e-sandbox/dsh-k8e-sandbox-bundle"

// clientCertDetail inspects the mTLS material in certDir: presence of
// ca/client cert + key, and the client cert's validity window.
func clientCertDetail(certDir string) (bool, string) {
	ca := filepath.Join(certDir, "ca.crt")
	cc := filepath.Join(certDir, "client.crt")
	ck := filepath.Join(certDir, "client.key")
	for _, f := range []string{ca, cc, ck} {
		if _, err := os.Stat(f); err != nil {
			return false, fmt.Sprintf("missing %s", filepath.Base(f))
		}
	}
	data, err := os.ReadFile(cc)
	if err != nil {
		return false, "cannot read client.crt"
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return false, "client.crt is not a valid PEM"
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false, "client.crt does not parse: " + err.Error()
	}
	now := time.Now()
	if now.Before(cert.NotBefore) {
		return false, fmt.Sprintf("client cert not valid before %s", cert.NotBefore.Format(time.RFC3339))
	}
	if now.After(cert.NotAfter) {
		return false, fmt.Sprintf("client cert expired %s", cert.NotAfter.Format(time.RFC3339))
	}
	days := int(time.Until(cert.NotAfter).Hours() / 24)
	return true, fmt.Sprintf("valid until %s (%dd left)", cert.NotAfter.Format("2006-01-02"), days)
}

// DoctorCommand checks the sandbox + dsh plugin configuration and prints
// PASS/FAIL per item with the exact fix for each failure (the dsh.profile.bundles
// registration gap after `dsh plugin add` is a common cause of a missing panel).
func DoctorCommand() cli.Command {
	return cli.Command{
		Name:  "doctor",
		Usage: "Check sandbox + dsh plugin configuration and print fixes",
		Flags: []cli.Flag{
			cli.BoolFlag{Name: "json", Usage: "Emit machine-readable JSON report"},
		},
		Action: doctorAction,
	}
}

func doctorAction(ctx *cli.Context) error {
	var checks []doctorCheck
	// 1. CLI binary on PATH
	if p, err := exec.LookPath("k8e-sandbox-cli"); err == nil {
		checks = append(checks, doctorCheck{Name: "k8e-sandbox-cli binary", OK: true, Detail: p})
	} else {
		checks = append(checks, doctorCheck{Name: "k8e-sandbox-cli binary", OK: false,
			Detail: "k8e-sandbox-cli not on PATH",
			Fix:    "run `k8e-sandbox-cli connect` (it symlinks into ~/.local/bin) or add the binary to PATH"})
	}

	// 2. Profile configuration (KIP-17)
	profilesPath, _ := DefaultProfilesPath()
	if file, path, err := LoadProfileFile(); err != nil {
		checks = append(checks, doctorCheck{Name: "profile config", OK: false, Detail: path + ": " + err.Error(),
			Fix: "run `k8e-sandbox-cli connect` to generate " + profilesPath})
	} else if file == nil {
		checks = append(checks, doctorCheck{Name: "profile config", OK: false, Detail: "no " + profilesPath,
			Fix: "run `k8e-sandbox-cli connect` (local) or `k8e-sandbox-cli connect --endpoint <host>:50051 --apikey <key>` (remote)"})
	} else {
		name := SelectProfileName(file, ctx.GlobalString("profile"))
		ep := ""
		if name != "" {
			if p, ok := file.Profiles[name]; ok {
				ep = p.Endpoint
			}
		}
		if ep == "" {
			checks = append(checks, doctorCheck{Name: "profile config", OK: false,
				Detail: fmt.Sprintf("%s: active profile %q has no endpoint", path, name),
				Fix:    "run `k8e-sandbox-cli connect --endpoint <host>:50051 --apikey <key>`"})
		} else {
			checks = append(checks, doctorCheck{Name: "profile config", OK: true,
				Detail: fmt.Sprintf("%s → profile %q → %s", path, name, ep)})
		}
	}

	// 3. mTLS material
	if resolved, err := ResolveConn(ctx.GlobalString("endpoint"), ctx.GlobalString("apikey"), ctx.GlobalString("profile"), ""); err == nil {
		certDir := resolved.CertDir
		if certDir == "" {
			certDir, _ = dataDir()
		}
		if ok, detail := clientCertDetail(certDir); ok {
			checks = append(checks, doctorCheck{Name: "mTLS client cert", OK: true, Detail: detail})
		} else {
			checks = append(checks, doctorCheck{Name: "mTLS client cert", OK: false, Detail: detail,
				Fix: "re-run `k8e-sandbox-cli connect --endpoint <host>:50051 --apikey <key>` (remove " + filepath.Join(certDir, "ca.crt") + " on trust mismatch)"})
		}
	} else {
		checks = append(checks, doctorCheck{Name: "mTLS client cert", OK: false, Detail: err.Error(),
			Fix: "re-run `k8e-sandbox-cli connect`"})
	}

	// 4. Gateway reachability (same probe as `status`)
	{
		client, exitErr := newClientFromCtx(ctx)
		if exitErr != nil {
			checks = append(checks, doctorCheck{Name: "gateway (status probe)", OK: false,
				Detail: exitErr.Error(),
				Fix:    "start the sandbox gateway (k8e-server with sandbox matrix) or fix the profile endpoint, then re-run connect"})
		} else {
			pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, err := client.SandboxServiceClient.DestroySession(pctx, &pb.DestroySessionRequest{SessionId: "healthcheck-probe-noop"})
			cancel()
			client.Close() //nolint:errcheck
			if err == nil || strings.Contains(err.Error(), errSessionNotFound) {
				checks = append(checks, doctorCheck{Name: "gateway (status probe)", OK: true, Detail: "reachable, handshake OK"})
			} else {
				checks = append(checks, doctorCheck{Name: "gateway (status probe)", OK: false, Detail: err.Error(),
					Fix: "gateway unreachable or TLS mismatch — verify the gateway is up; on CA rotation remove ~/.k8e/sandbox/ca.crt and re-run connect"})
			}
		}
	}

	// 5. dsh plugin: bundle registered in dsh.profile.bundles + installed
	profiles := dshProfilePaths()
	if len(profiles) == 0 {
		checks = append(checks, doctorCheck{Name: "dsh profile", OK: false,
			Detail: "no profiles under " + filepath.Join(dshHome(), "profiles"),
			Fix:    "install the plugin: `npx @deepseek-ai/dsh plugin --profile web add @k8e-sandbox/dsh-k8e-sandbox-bundle`"})
	}
	for _, p := range profiles {
		profileName := filepath.Base(filepath.Dir(p))
		label := "dsh profile " + profileName
		bundles := dshProfileBundles(p)
		found := false
		for _, b := range bundles {
			if b == k8eBundleName || strings.HasPrefix(b, k8eBundleName+"@") {
				found = true
				break
			}
		}
		if found {
			checks = append(checks, doctorCheck{Name: label + " bundle registered", OK: true,
				Detail: k8eBundleName + " in dsh.profile.bundles"})
		} else {
			checks = append(checks, doctorCheck{Name: label + " bundle registered", OK: false,
				Detail: k8eBundleName + " NOT in " + p + " dsh.profile.bundles — installed but never loaded",
				Fix:    "append \"" + k8eBundleName + "\" to the bundles array in " + p + " (the `dsh plugin add` pnpm non-zero exit can skip this), then restart `npx @deepseek-ai/dsh web`"})
		}
		if v := dshBundleInstalledVersion(p); v != "" {
			checks = append(checks, doctorCheck{Name: label + " bundle installed", OK: true,
				Detail: k8eBundleName + "@" + v + " in node_modules"})
		} else {
			checks = append(checks, doctorCheck{Name: label + " bundle installed", OK: false,
				Detail: k8eBundleName + " missing from " + filepath.Join(filepath.Dir(p), "node_modules"),
				Fix:    "re-run `npx @deepseek-ai/dsh plugin --profile " + profileName + " add " + k8eBundleName + "`"})
		}
	}

	// 6. k8e-sandbox skill installed for dsh (+ cross-harness roots)
	skillRoots, _ := skillDestsForAgent("dsh", homeDir())
	for _, d := range skillRoots {
		dest := filepath.Join(d.dir, skillDirName, skillFileName)
		if _, err := os.Stat(dest); err == nil {
			checks = append(checks, doctorCheck{Name: "skill (" + d.label + ")", OK: true, Detail: dest})
		} else {
			checks = append(checks, doctorCheck{Name: "skill (" + d.label + ")", OK: false,
				Detail: "missing " + dest,
				Fix:    "run `k8e-sandbox-cli connect --agent dsh --skill-only`"})
		}
	}

	// Report
	problems := 0
	for _, c := range checks {
		if !c.OK {
			problems++
		}
	}
	report := doctorReport{OK: problems == 0, Problems: problems, Checks: checks}

	if ctx.Bool("json") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(report)
	} else {
		for _, c := range checks {
			mark := "✔"
			if !c.OK {
				mark = "✘"
			}
			fmt.Fprintf(os.Stderr, "%s %-38s %s\n", mark, c.Name+":", c.Detail)
			if !c.OK && c.Fix != "" {
				fmt.Fprintf(os.Stderr, "    fix: %s\n", c.Fix)
			}
		}
		if problems > 0 {
			fmt.Fprintf(os.Stderr, "\n%d problem(s) found — apply the fixes above, then re-run `k8e-sandbox-cli doctor`.\n", problems)
		} else {
			fmt.Fprintf(os.Stderr, "\nAll checks passed.\n")
		}
	}
	if problems > 0 {
		return &ExitError{ExitCode: 1}
	}
	return nil
}
