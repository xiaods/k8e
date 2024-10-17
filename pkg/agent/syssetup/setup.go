//go:build !windows

package syssetup

import (
	"os"
	"os/exec"

	"github.com/sirupsen/logrus"
	"k8s.io/component-helpers/node/util/sysctl"
)

func loadKernelModule(moduleName string) {
	if _, err := os.Stat("/sys/module/" + moduleName); err == nil {
		logrus.Info("Module " + moduleName + " was already loaded")
		return
	}

	if err := exec.Command("modprobe", "--", moduleName).Run(); err != nil {
		logrus.Warnf("Failed to load kernel module %v with modprobe", moduleName)
	}
}

// Configure loads required kernel modules and sets sysctls required for other components to
// function properly.
func Configure(enableIPv6 bool) {
	loadKernelModule("overlay")
	loadKernelModule("nf_conntrack")
	loadKernelModule("br_netfilter")
	loadKernelModule("iptable_nat")
	loadKernelModule("iptable_filter")
	if enableIPv6 {
		loadKernelModule("ip6table_nat")
		loadKernelModule("ip6table_filter")
	}

	// Kernel is inconsistent about how devconf is configured for
	// new network namespaces between ipv4 and ipv6. Make sure to
	// enable forwarding on all and default for both ipv4 and ipv6.
	sysctls := map[string]int{
		"net/ipv4/conf/all/forwarding":       1,
		"net/ipv4/conf/default/forwarding":   1,
		"net/bridge/bridge-nf-call-iptables": 1,
	}

	if enableIPv6 {
		sysctls["net/ipv6/conf/all/forwarding"] = 1
		sysctls["net/ipv6/conf/default/forwarding"] = 1
		sysctls["net/bridge/bridge-nf-call-ip6tables"] = 1
		sysctls["net/core/devconf_inherit_init_net"] = 1
	}

	sys := sysctl.New()
	for entry, value := range sysctls {
		if val, _ := sys.GetSysctl(entry); val != value {
			logrus.Infof("Set sysctl '%v' to %v", entry, value)
			if err := sys.SetSysctl(entry, value); err != nil {
				logrus.Errorf("Failed to set sysctl: %v", err)
			}
		}
	}
}
