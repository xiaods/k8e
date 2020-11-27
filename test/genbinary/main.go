package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/datadir"
	"github.com/xiaods/k8e/test/genbinary/data"
)

func main() {
	dataDir := "./"
	dir, err := extract(dataDir)
	fmt.Println(dir, err)
}

func getAssetAndDir(dataDir string) (string, string) {
	asset := data.AssetNames()[0]
	dir := filepath.Join(dataDir, "data", strings.SplitN(filepath.Base(asset), ".", 2)[0])
	return asset, dir
}

func extract(dataDir string) (string, error) {
	// first look for global asset folder so we don't create a HOME version if not needed
	_, dir := getAssetAndDir(datadir.DefaultDataDir)
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

	//tempDest := dir + "-tmp"
	//defer os.RemoveAll(tempDest)
	fmt.Println(buf.String())
	return "", nil
	// os.RemoveAll(tempDest)

	// if err := untar.Untar(buf, tempDest); err != nil {
	// 	return "", err
	// }

	// currentSymLink := filepath.Join(dataDir, "data", "current")
	// previousSymLink := filepath.Join(dataDir, "data", "previous")
	// if _, err := os.Lstat(currentSymLink); err == nil {
	// 	if err := os.Rename(currentSymLink, previousSymLink); err != nil {
	// 		return "", err
	// 	}
	// }
	// if err := os.Symlink(dir, currentSymLink); err != nil {
	// 	return "", err
	// }

	// return dir, os.Rename(tempDest, dir)
}
