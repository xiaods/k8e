package sandboxcli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v2"
)

// Canonical multi-profile file for k8e-sandbox-cli (KIP-17).
// Intentionally NOT named config.yaml — that name is reserved for the
// k8e server/agent daemon flag file at /etc/k8e/config.yaml (configfilearg).
const profilesFileName = "profiles.yaml"

// legacyHomeProfilesPath is the short-lived KIP-17 path that collided in name
// with the server config. Still read for one release with a stderr warning.
const legacyHomeProfilesRel = ".k8e/config.yaml"

// ProfileFile is the multi-cluster CLI config (KIP-17).
// Default path: ~/.k8e/sandbox/profiles.yaml
type ProfileFile struct {
	Version        int                `yaml:"version"`
	CurrentProfile string             `yaml:"current_profile"`
	Profiles       map[string]Profile `yaml:"profiles"`
}

// Profile is one named gateway connection.
type Profile struct {
	Endpoint   string `yaml:"endpoint"`
	CertDir    string `yaml:"cert_dir"`
	DeviceName string `yaml:"device_name"`
}

// ResolvedConn is the effective dial configuration after flag/env/profile merge.
type ResolvedConn struct {
	Endpoint   string
	APIKey     string
	CertDir    string
	DeviceName string
	Profile    string // empty if no profile file/selection
}

// DefaultProfilesPath returns the canonical user profiles file path.
func DefaultProfilesPath() (string, error) {
	dir, err := dataDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, profilesFileName), nil
}

// profileConfigPaths returns candidate profile file paths in priority order.
//
//  1. K8E_SANDBOX_CONFIG (explicit override)
//  2. ~/.k8e/sandbox/profiles.yaml (or $K8E_SANDBOX_CERT_DIR/profiles.yaml)
//  3. ~/.k8e/config.yaml (legacy; name collides with server /etc/k8e/config.yaml)
func profileConfigPaths() []string {
	var paths []string
	if p := strings.TrimSpace(os.Getenv("K8E_SANDBOX_CONFIG")); p != "" {
		paths = append(paths, p)
	}
	if p, err := DefaultProfilesPath(); err == nil {
		paths = append(paths, p)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, legacyHomeProfilesRel))
	}
	return paths
}

// LoadProfileFile reads the first existing profile config. Returns (nil, nil) when none exist.
// If the legacy ~/.k8e/config.yaml path is used, a deprecation warning is printed to stderr.
func LoadProfileFile() (*ProfileFile, string, error) {
	for _, path := range profileConfigPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, path, fmt.Errorf("read sandbox profiles %s: %w", path, err)
		}
		var f ProfileFile
		if err := yaml.Unmarshal(data, &f); err != nil {
			return nil, path, fmt.Errorf("parse sandbox profiles %s: %w", path, err)
		}
		if f.Profiles == nil {
			f.Profiles = map[string]Profile{}
		}
		if isLegacyHomeProfilesPath(path) {
			canonical, _ := DefaultProfilesPath()
			if canonical == "" {
				canonical = "~/.k8e/sandbox/profiles.yaml"
			}
			fmt.Fprintf(os.Stderr, "⚠ sandbox profiles: %s is deprecated (name collides with server /etc/k8e/config.yaml).\n  Move this file to %s\n", path, canonical)
		}
		return &f, path, nil
	}
	return nil, "", nil
}

func isLegacyHomeProfilesPath(path string) bool {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return false
	}
	// Compare cleaned absolute-ish paths.
	legacy := filepath.Clean(filepath.Join(home, legacyHomeProfilesRel))
	return filepath.Clean(path) == legacy
}

// SelectProfileName picks the active profile name.
// priority: explicit → K8E_SANDBOX_PROFILE → file.current_profile → "default" if present.
func SelectProfileName(file *ProfileFile, explicit string) string {
	if name := strings.TrimSpace(explicit); name != "" {
		return name
	}
	if name := strings.TrimSpace(os.Getenv("K8E_SANDBOX_PROFILE")); name != "" {
		return name
	}
	if file == nil {
		return ""
	}
	if name := strings.TrimSpace(file.CurrentProfile); name != "" {
		return name
	}
	if _, ok := file.Profiles["default"]; ok {
		return "default"
	}
	return ""
}

// ResolveConn merges CLI flag values with env and optional profile (KIP-17).
// empty endpoint/apikey/certDir/device from flags fall through to env then profile.
func ResolveConn(flagEndpoint, flagAPIKey, flagProfile, flagDevice string) (*ResolvedConn, error) {
	out := &ResolvedConn{
		Endpoint:   strings.TrimSpace(flagEndpoint),
		APIKey:     strings.TrimSpace(flagAPIKey),
		DeviceName: strings.TrimSpace(flagDevice),
	}
	if out.Endpoint == "" {
		out.Endpoint = strings.TrimSpace(os.Getenv("K8E_SANDBOX_ENDPOINT"))
	}
	if out.APIKey == "" {
		out.APIKey = strings.TrimSpace(os.Getenv("K8E_SANDBOX_APIKEY"))
	}
	if out.DeviceName == "" {
		out.DeviceName = strings.TrimSpace(os.Getenv("K8E_SANDBOX_DEVICE_NAME"))
	}
	// Cert dir: flag is not a CLI global yet; env then profile.
	out.CertDir = strings.TrimSpace(os.Getenv("K8E_SANDBOX_CERT_DIR"))

	file, _, err := LoadProfileFile()
	if err != nil {
		return nil, err
	}
	name := SelectProfileName(file, flagProfile)
	if name == "" || file == nil {
		return out, nil
	}
	prof, ok := file.Profiles[name]
	if !ok {
		return nil, fmt.Errorf("sandbox profile %q not found in profiles.yaml", name)
	}
	out.Profile = name
	if out.Endpoint == "" {
		out.Endpoint = strings.TrimSpace(prof.Endpoint)
	}
	if out.CertDir == "" && strings.TrimSpace(prof.CertDir) != "" {
		out.CertDir = expandHome(strings.TrimSpace(prof.CertDir))
	}
	if out.DeviceName == "" {
		out.DeviceName = strings.TrimSpace(prof.DeviceName)
	}
	return out, nil
}

// ApplyResolvedConn exports cert_dir / device_name into process env so
// pkg/sandbox/client and dataDir() pick them up without API churn.
func ApplyResolvedConn(r *ResolvedConn) {
	if r == nil {
		return
	}
	if r.CertDir != "" {
		_ = os.Setenv("K8E_SANDBOX_CERT_DIR", r.CertDir)
	}
	if r.DeviceName != "" {
		_ = os.Setenv("K8E_SANDBOX_DEVICE_NAME", r.DeviceName)
	}
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
