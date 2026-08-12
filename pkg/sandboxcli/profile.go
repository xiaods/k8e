package sandboxcli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v2"
)

// ProfileFile is the multi-cluster CLI config (KIP-17): ~/.k8e/config.yaml.
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

// profileConfigPaths returns candidate config.yaml paths in priority order.
func profileConfigPaths() []string {
	var paths []string
	if p := strings.TrimSpace(os.Getenv("K8E_SANDBOX_CONFIG")); p != "" {
		paths = append(paths, p)
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		paths = append(paths, filepath.Join(xdg, "k8e", "config.yaml"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		paths = append(paths, filepath.Join(home, ".k8e", "config.yaml"))
	}
	return paths
}

// LoadProfileFile reads the first existing profile config. Returns (nil, nil) when none exist.
func LoadProfileFile() (*ProfileFile, string, error) {
	for _, path := range profileConfigPaths() {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, path, fmt.Errorf("read profile config %s: %w", path, err)
		}
		var f ProfileFile
		if err := yaml.Unmarshal(data, &f); err != nil {
			return nil, path, fmt.Errorf("parse profile config %s: %w", path, err)
		}
		if f.Profiles == nil {
			f.Profiles = map[string]Profile{}
		}
		return &f, path, nil
	}
	return nil, "", nil
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
		return nil, fmt.Errorf("sandbox profile %q not found in config.yaml", name)
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
