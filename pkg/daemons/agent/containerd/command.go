// +build !windows

package containerd

import (
	"os/exec"
	"syscall"
)

func addDeathSig(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Pdeathsig: syscall.SIGKILL,
	}
}
