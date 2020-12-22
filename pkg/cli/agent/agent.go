package agent

import (
	"bytes"
	"context"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	net2 "net"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/daemons"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/data"
	"github.com/xiaods/k8e/pkg/datadir"
	"github.com/xiaods/k8e/pkg/signals"
	"github.com/xiaods/k8e/pkg/untar"
	"github.com/xiaods/k8e/pkg/version"
)

const (
	dockershimSock = "unix:///var/run/dockershim.sock"
	containerdSock = "unix:///run/k8e/containerd/containerd.sock"
)

func Run(cmd *cobra.Command, args []string) {
	logrus.Info("start agent")
	ctx := signals.SetupSignalHandler(context.Background())
	InternlRun(ctx, &cmds.Agent)
}

func InternlRun(ctx context.Context, cfg *cmds.AgentConfig) error {
	var err error
	nodeConfig := &config.Node{}
	nodeConfig.Docker = cfg.Docker
	nodeConfig.ContainerRuntimeEndpoint = cfg.ContainerRuntimeEndpoint
	dataDir, _ := datadir.LocalHome(cfg.DataDir, true)
	nodeConfig.AgentConfig.DataDir = dataDir
	nodeConfig.AgentConfig.APIServerURL = cfg.ServerURL
	u, err := url.Parse(cfg.ServerURL)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return err
	}

	nodeConfig.AgentConfig.DaemonURL = fmt.Sprintf("http://%s:%d", u.Hostname(), port+1)
	nodeConfig.AgentConfig.DisableCCM = cfg.DisableCCM
	nodeConfig.AgentConfig.Internal = cfg.Internal
	_, nodeConfig.AgentConfig.ClusterCIDR, err = net2.ParseCIDR(cfg.ClusterCIDR)
	err = stageAndRun(datadir.DefaultDataDir, "host-local", nil)
	if err != nil {
		logrus.Error(err)
		return err
	}
	if err = setupCriCtlConfig(cfg); err != nil {
		return err
	}
	err = daemons.D.StartAgent(ctx, nodeConfig)
	if err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func setupCriCtlConfig(cfg *cmds.AgentConfig) error {
	cre := cfg.ContainerRuntimeEndpoint
	if cre == "" {
		switch {
		case cfg.Docker:
			cre = dockershimSock
		default:
			cre = containerdSock
		}
	}

	agentConfDir := datadir.DefaultDataDir + "/agent/etc"
	if _, err := os.Stat(agentConfDir); os.IsNotExist(err) {
		if err := os.MkdirAll(agentConfDir, 0755); err != nil {
			return err
		}
	}

	crp := "runtime-endpoint: " + cre + "\n"
	return ioutil.WriteFile(agentConfDir+"/crictl.yaml", []byte(crp), 0600)
}

func stageAndRun(dataDir string, cmd string, args []string) error {
	dir, err := extract(dataDir)
	if err != nil {
		return errors.Wrap(err, "extracting data")
	}

	if err := os.Setenv("PATH", filepath.Join(dir, "bin")+":"+os.Getenv("PATH")+":"+filepath.Join(dir, "bin/aux")); err != nil {
		return err
	}
	if err := os.Setenv(version.ProgramUpper+"_DATA_DIR", dir); err != nil {
		return err
	}

	cmd, err = exec.LookPath(cmd)
	if err != nil {
		return err
	}
	return nil

	//logrus.Debugf("Running %s %v", cmd, args)
	//return syscall.Exec(cmd, args, os.Environ())
}

func getAssetAndDir(dataDir string) (string, string) {
	asset := data.AssetNames()[0]
	dir := filepath.Join(dataDir, "data", strings.SplitN(filepath.Base(asset), ".", 2)[0])
	return asset, dir
}

func extract(dataDir string) (string, error) {
	// first look for global asset folder so we don't create a HOME version if not needed
	_, dir := getAssetAndDir(datadir.DefaultDataDir) //查看插件是否存在
	if _, err := os.Stat(dir); err == nil {
		logrus.Debugf("Asset dir %s", dir)
		return dir, nil
	}

	asset, dir := getAssetAndDir(dataDir)
	if _, err := os.Stat(dir); err == nil {
		logrus.Debugf("Asset dir %s", dir)
		return dir, nil
	}

	logrus.Infof("Preparing data dir %s", dir)

	content, err := data.Asset(asset)
	if err != nil {
		return "", err
	}
	buf := bytes.NewBuffer(content)

	tempDest := dir + "-tmp"
	defer os.RemoveAll(tempDest)
	os.RemoveAll(tempDest)

	if err := untar.Untar(buf, tempDest); err != nil {
		return "", err
	}

	currentSymLink := filepath.Join(dataDir, "data", "current")
	previousSymLink := filepath.Join(dataDir, "data", "previous")
	if _, err := os.Lstat(currentSymLink); err == nil {
		if err := os.Rename(currentSymLink, previousSymLink); err != nil {
			return "", err
		}
	}
	if err := os.Symlink(dir, currentSymLink); err != nil {
		return "", err
	}

	return dir, os.Rename(tempDest, dir)
}
