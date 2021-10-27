//go:build !windows
// +build !windows

package rootless

import (
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runc/libcontainer/cgroups"
	"github.com/pkg/errors"
	"github.com/rootless-containers/rootlesskit/pkg/child"
	"github.com/rootless-containers/rootlesskit/pkg/copyup/tmpfssymlink"
	"github.com/rootless-containers/rootlesskit/pkg/network/slirp4netns"
	"github.com/rootless-containers/rootlesskit/pkg/parent"
	portbuiltin "github.com/rootless-containers/rootlesskit/pkg/port/builtin"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

var (
	pipeFD   = "_K8E_ROOTLESS_FD"
	childEnv = "_K8E_ROOTLESS_SOCK"
	Sock     = ""
)

func Rootless(stateDir string) error {
	defer func() {
		os.Unsetenv(pipeFD)
		os.Unsetenv(childEnv)
	}()

	hasFD := os.Getenv(pipeFD) != ""
	hasChildEnv := os.Getenv(childEnv) != ""

	if hasFD {
		logrus.Debug("Running rootless child")
		childOpt, err := createChildOpt()
		if err != nil {
			logrus.Fatal(err)
		}
		if err := child.Child(*childOpt); err != nil {
			logrus.Fatalf("child died: %v", err)
		}
	}

	if hasChildEnv {
		Sock = os.Getenv(childEnv)
		logrus.Debug("Running rootless process")
		return setupMounts(stateDir)
	}

	logrus.Debug("Running rootless parent")
	parentOpt, err := createParentOpt(filepath.Join(stateDir, "rootless"))
	if err != nil {
		logrus.Fatal(err)
	}

	os.Setenv(childEnv, filepath.Join(parentOpt.StateDir, parent.StateFileAPISock))
	if err := parent.Parent(*parentOpt); err != nil {
		logrus.Fatal(err)
	}
	os.Exit(0)

	return nil
}

func parseCIDR(s string) (*net.IPNet, error) {
	if s == "" {
		return nil, nil
	}
	ip, ipnet, err := net.ParseCIDR(s)
	if err != nil {
		return nil, err
	}
	if !ip.Equal(ipnet.IP) {
		return nil, errors.Errorf("cidr must be like 10.0.2.0/24, not like 10.0.2.100/24")
	}
	return ipnet, nil
}

func createParentOpt(stateDir string) (*parent.Opt, error) {
	if err := os.MkdirAll(stateDir, 0755); err != nil {
		return nil, errors.Wrapf(err, "failed to mkdir %s", stateDir)
	}

	stateDir, err := ioutil.TempDir("", "rootless")
	if err != nil {
		return nil, err
	}

	opt := &parent.Opt{
		StateDir:       stateDir,
		CreatePIDNS:    true,
		CreateCgroupNS: true,
		CreateUTSNS:    true,
		CreateIPCNS:    true,
	}

	selfCgroupMap, err := cgroups.ParseCgroupFile("/proc/self/cgroup")
	if err != nil {
		return nil, err
	}
	if selfCgroup2 := selfCgroupMap[""]; selfCgroup2 == "" {
		logrus.Warnf("enabling cgroup2 is highly recommended, see https://rootlesscontaine.rs/getting-started/common/cgroup2/")
	} else {
		selfCgroup2Dir := filepath.Join("/sys/fs/cgroup", selfCgroup2)
		if unix.Access(selfCgroup2Dir, unix.W_OK) == nil {
			opt.EvacuateCgroup2 = "k3s_evac"
		} else {
			logrus.Warn("cannot set cgroup2 evacuation, make sure to run k3s as a systemd unit")
		}
	}

	mtu := 0
	ipnet, err := parseCIDR("10.41.0.0/16")
	if err != nil {
		return nil, err
	}
	disableHostLoopback := true
	binary := "slirp4netns"
	if _, err := exec.LookPath(binary); err != nil {
		return nil, err
	}
	debugWriter := &logrusDebugWriter{}
	opt.NetworkDriver, err = slirp4netns.NewParentDriver(debugWriter, binary, mtu, ipnet, "tap0", disableHostLoopback, "", false, false, false)
	if err != nil {
		return nil, err
	}

	opt.PortDriver, err = portbuiltin.NewParentDriver(debugWriter, stateDir)
	if err != nil {
		return nil, err
	}

	opt.PipeFDEnvKey = pipeFD

	return opt, nil
}

type logrusDebugWriter struct {
}

func (w *logrusDebugWriter) Write(p []byte) (int, error) {
	s := strings.TrimSuffix(string(p), "\n")
	logrus.Debug(s)
	return len(p), nil
}

func createChildOpt() (*child.Opt, error) {
	opt := &child.Opt{}
	opt.TargetCmd = os.Args
	opt.PipeFDEnvKey = pipeFD
	opt.NetworkDriver = slirp4netns.NewChildDriver()
	opt.PortDriver = portbuiltin.NewChildDriver(&logrusDebugWriter{})
	opt.CopyUpDirs = []string{"/etc", "/run", "/var/lib"}
	opt.CopyUpDriver = tmpfssymlink.NewChildDriver()
	opt.MountProcfs = true
	opt.Reaper = true
	return opt, nil
}
