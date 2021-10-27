package util

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/flock"
)

// Compile-time variable
var existingServer = "False"

func findK8eExecutable() string {
	// if running on an existing cluster, it maybe installed via k8e.service
	// or run manually from dist/artifacts/k8e
	if IsExistingServer() {
		k8eBin, err := exec.LookPath("k8e")
		if err == nil {
			return k8eBin
		}
	}
	k8eBin := "dist/artifacts/k8e"
	for {
		_, err := os.Stat(k8eBin)
		if err != nil {
			k8eBin = "../" + k8eBin
			continue
		}
		break
	}
	return k8eBin
}

// IsRoot return true if the user is root (UID 0)
func IsRoot() bool {
	currentUser, err := user.Current()
	if err != nil {
		return false
	}
	return currentUser.Uid == "0"
}

func IsExistingServer() bool {
	return existingServer == "True"
}

// K8eCmd launches the provided K8e command via exec. Command blocks until finished.
// Command output from both Stderr and Stdout is provided via string.
//   cmdEx1, err := K8eCmd("etcd-snapshot", "ls")
//   cmdEx2, err := K8eCmd("kubectl", "get", "pods", "-A")
func K8eCmd(cmdName string, cmdArgs ...string) (string, error) {
	k8eBin := findK8eExecutable()
	// Only run sudo if not root
	var cmd *exec.Cmd
	if IsRoot() {
		k8eCmd := append([]string{cmdName}, cmdArgs...)
		cmd = exec.Command(k8eBin, k8eCmd...)
	} else {
		k8eCmd := append([]string{k8eBin, cmdName}, cmdArgs...)
		cmd = exec.Command("sudo", k8eCmd...)
	}
	byteOut, err := cmd.CombinedOutput()
	return string(byteOut), err
}

func contains(source []string, target string) bool {
	for _, s := range source {
		if s == target {
			return true
		}
	}
	return false
}

// ServerArgsPresent checks if the given arguments are found in the running k8e server
func ServerArgsPresent(neededArgs []string) bool {
	currentArgs := K8eServerArgs()
	for _, arg := range neededArgs {
		if !contains(currentArgs, arg) {
			return false
		}
	}
	return true
}

// K8eServerArgs returns the list of arguments that the K8e server launched with
func K8eServerArgs() []string {
	results, err := K8eCmd("kubectl", "get", "nodes", "-o", `jsonpath='{.items[0].metadata.annotations.k8e\.io/node-args}'`)
	if err != nil {
		return nil
	}
	res := strings.ReplaceAll(results, "'", "")
	var args []string
	if err := json.Unmarshal([]byte(res), &args); err != nil {
		logrus.Error(err)
		return nil
	}
	return args
}

func FindStringInCmdAsync(scanner *bufio.Scanner, target string) bool {
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), target) {
			return true
		}
	}
	return false
}

type K8eServer struct {
	cmd     *exec.Cmd
	scanner *bufio.Scanner
	lock    int
}

// K8eStartServer acquires an exclusive lock on a temporary file, then launches a k8e cluster
// with the provided arguments. Subsequent/parallel calls to this function will block until
// the original lock is cleared using K8eKillServer
func K8eStartServer(cmdArgs ...string) (*K8eServer, error) {
	logrus.Info("waiting to get server lock")
	k8eLock, err := flock.Acquire("/var/lock/k8e-test.lock")
	if err != nil {
		return nil, err
	}

	k8eBin := findK8eExecutable()
	var cmd *exec.Cmd
	if IsRoot() {
		k8eCmd := append([]string{"server"}, cmdArgs...)
		cmd = exec.Command(k8eBin, k8eCmd...)
	} else {
		k8eCmd := append([]string{k8eBin, "server"}, cmdArgs...)
		cmd = exec.Command("sudo", k8eCmd...)
	}
	cmdOut, _ := cmd.StderrPipe()
	cmd.Stderr = os.Stderr
	err = cmd.Start()
	return &K8eServer{cmd, bufio.NewScanner(cmdOut), k8eLock}, err
}

// K8eKillServer terminates the running K8e server and unlocks the file for
// other tests
func K8eKillServer(server K8eServer) error {
	if IsRoot() {
		if err := server.cmd.Process.Kill(); err != nil {
			return err
		}
	} else {
		// Since k8e was launched as sudo, we can't just kill the process
		killCmd := exec.Command("sudo", "pkill", "k8e")
		if err := killCmd.Run(); err != nil {
			return err
		}
	}
	return flock.Release(server.lock)
}
