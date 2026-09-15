package agent

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/sirupsen/logrus"
	daemonconfig "github.com/xiaods/k8e/pkg/daemons/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"
	"sigs.k8s.io/yaml"

	// Registers the Kubernetes feature gates, so that
	// utilfeature.DefaultFeatureGate knows which gates this build accepts.
	_ "k8s.io/kubernetes/pkg/features"
)

const (
	// kubeletDropInFileName is the KubeletConfiguration drop-in k8e writes into
	// Agent.KubeletConfigDir. kubelet merges every *.conf in that directory in
	// lexical order, so the low prefix leaves room for a user's 99-*.conf to
	// override anything k8e sets.
	kubeletDropInFileName = "10-k8e.conf"

	// kubeletDropInDirFlag is the kubelet flag pointing at the drop-in directory.
	kubeletDropInDirFlag = "config-dir"
)

// kubeletSettings is everything k8e hands to kubelet.
//
// kubelet is configured through a mix of CLI flags and a configuration file, and
// the two are not equivalent. A flag that upstream deletes makes kubelet exit
// with `unknown flag` before it ever reads a configuration file; a field
// upstream removes from KubeletConfiguration is silently ignored when it appears
// in a drop-in file, because kubelet decodes drop-ins without strict field
// checking. Everything that has a KubeletConfiguration field therefore belongs in
// config, and only flags without one stay in flags.
type kubeletSettings struct {
	flags  map[string]string
	config *kubeletconfigv1beta1.KubeletConfiguration
	gates  map[string]bool
}

func newKubeletSettings() *kubeletSettings {
	return &kubeletSettings{
		flags: map[string]string{},
		config: &kubeletconfigv1beta1.KubeletConfiguration{
			TypeMeta: metav1.TypeMeta{
				APIVersion: kubeletconfigv1beta1.SchemeGroupVersion.String(),
				Kind:       "KubeletConfiguration",
			},
		},
		gates: map[string]bool{},
	}
}

// setFlag records a kubelet CLI flag. Use it only for flags that have no
// KubeletConfiguration equivalent; anything else belongs on config.
func (s *kubeletSettings) setFlag(key, value string) {
	s.flags[key] = value
}

// addFeatureGate requests a kubelet feature gate. Gates unknown to this build
// are dropped when the drop-in file is rendered instead of being passed to
// kubelet, which would refuse to start.
func (s *kubeletSettings) addFeatureGate(name string, value bool) {
	s.gates[name] = value
}

// featureGates returns the requested gates that this Kubernetes build actually
// registers. A gate removed upstream degrades to a warning instead of taking the
// node down.
func (s *kubeletSettings) featureGates() map[string]bool {
	known := map[string]bool{}
	for _, name := range utilfeature.DefaultFeatureGate.KnownFeatures() {
		known[name] = true
	}

	names := make([]string, 0, len(s.gates))
	for name := range s.gates {
		names = append(names, name)
	}
	sort.Strings(names)

	gates := make(map[string]bool, len(s.gates))
	for _, name := range names {
		if !known[name] {
			logrus.Warnf("Not setting kubelet feature gate %q: this Kubernetes build does not register it", name)
			continue
		}
		gates[name] = s.gates[name]
	}
	return gates
}

// renderDropIn renders only the settings k8e actually set.
//
// kubelet merges a drop-in file onto its base configuration with a JSON merge
// patch, so marshalling the whole KubeletConfiguration would also write every
// field k8e never touched as its Go zero value — and those zeros would then
// overwrite kubelet's own defaults, because a 0s reconcile period is not the
// same thing as an unset one. Diffing against an empty configuration turns the
// file into a patch carrying nothing but k8e's own settings.
func (s *kubeletSettings) renderDropIn() ([]byte, error) {
	ours, err := toJSON(s.config)
	if err != nil {
		return nil, err
	}
	empty, err := toJSON(&kubeletconfigv1beta1.KubeletConfiguration{})
	if err != nil {
		return nil, err
	}
	patch, err := jsonpatch.CreateMergePatch(empty, ours)
	if err != nil {
		return nil, fmt.Errorf("failed to build kubelet configuration drop-in: %w", err)
	}
	out, err := yaml.JSONToYAML(patch)
	if err != nil {
		return nil, fmt.Errorf("failed to convert kubelet configuration drop-in to YAML: %w", err)
	}
	return out, nil
}

func toJSON(obj interface{}) ([]byte, error) {
	encoded, err := yaml.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal KubeletConfiguration: %w", err)
	}
	jsonBytes, err := yaml.YAMLToJSON(encoded)
	if err != nil {
		return nil, fmt.Errorf("failed to convert KubeletConfiguration to JSON: %w", err)
	}
	return jsonBytes, nil
}

// writeDropIn renders config into <dir>/10-k8e.conf and points kubelet at the
// directory with --config-dir.
func (s *kubeletSettings) writeDropIn(dir string) error {
	s.config.FeatureGates = s.featureGates()

	data, err := s.renderDropIn()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create kubelet config directory %s: %w", dir, err)
	}
	path := filepath.Join(dir, kubeletDropInFileName)
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write kubelet config drop-in %s: %w", path, err)
	}
	logrus.Infof("Wrote kubelet configuration drop-in %s", path)

	s.setFlag(kubeletDropInDirFlag, dir)
	return nil
}

// args renders the CLI flags, with --kubelet-arg applied on top. Flags keep
// precedence over the drop-in file, so --kubelet-arg stays the escape hatch.
func (s *kubeletSettings) args(extraArgs []string) []string {
	return daemonconfig.GetArgs(s.flags, extraArgs)
}

// kubeletConfigDir returns the directory holding KubeletConfiguration drop-in
// files, or an error when the agent config does not carry one.
func kubeletConfigDir(cfg *daemonconfig.Agent) (string, error) {
	if cfg.KubeletConfigDir == "" {
		return "", fmt.Errorf("kubelet configuration directory is not set; agent config is incomplete")
	}
	return cfg.KubeletConfigDir, nil
}

func boolPtr(b bool) *bool {
	return &b
}

func stringPtr(s string) *string {
	return &s
}

func ipStrings(ips []net.IP) []string {
	strs := make([]string, 0, len(ips))
	for _, ip := range ips {
		strs = append(strs, ip.String())
	}
	return strs
}
