package agent

import (
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/version"
)

func getServerConfig(info *clientaccess.Info) (*config.Control, error) {
	data, err := clientaccess.Get("/v1-"+version.Program+"/config", info)
	if err != nil {
		return nil, err
	}

	controlControl := &config.Control{}
	return controlControl, json.Unmarshal(data, controlControl)
}

func getHostFile(filename, keyFile string, info *clientaccess.Info) error {
	basename := filepath.Base(filename)
	fileBytes, err := clientaccess.Get("/v1-"+version.Program+"/"+basename, info)
	if err != nil {
		return err
	}
	if keyFile == "" {
		if err := ioutil.WriteFile(filename, fileBytes, 0600); err != nil {
			return errors.Wrapf(err, "failed to write cert %s", filename)
		}
	} else {
		fileBytes, keyBytes := splitCertKeyPEM(fileBytes)
		if err := ioutil.WriteFile(filename, fileBytes, 0600); err != nil {
			return errors.Wrapf(err, "failed to write cert %s", filename)
		}
		if err := ioutil.WriteFile(keyFile, keyBytes, 0600); err != nil {
			return errors.Wrapf(err, "failed to write key %s", filename)
		}
	}
	return nil
}

func SplitCertKeyPEM(bytes []byte) (certPem []byte, keyPem []byte) {
	return splitCertKeyPEM(bytes)
}

func splitCertKeyPEM(bytes []byte) (certPem []byte, keyPem []byte) {
	for {
		b, rest := pem.Decode(bytes)
		if b == nil {
			break
		}
		bytes = rest

		if strings.Contains(b.Type, "PRIVATE KEY") {
			keyPem = append(keyPem, pem.EncodeToMemory(b)...)
		} else {
			certPem = append(certPem, pem.EncodeToMemory(b)...)
		}
	}

	return
}

func getNodeNamedHostFile(filename, keyFile, nodeName, nodeIP, nodePasswordFile string, info *clientaccess.Info) error {
	basename := filepath.Base(filename)
	fileBytes, err := Request("/v1-"+version.Program+"/"+basename, info, getNodeNamedCrt(nodeName, nodeIP, nodePasswordFile))
	if err != nil {
		return err
	}
	fileBytes, keyBytes := splitCertKeyPEM(fileBytes)

	if err := ioutil.WriteFile(filename, fileBytes, 0600); err != nil {
		return errors.Wrapf(err, "failed to write cert %s", filename)
	}
	if keyFile != "" {
		if err := ioutil.WriteFile(keyFile, keyBytes, 0600); err != nil {
			return errors.Wrapf(err, "failed to write key %s", filename)
		}
	}
	return nil
}

type HTTPRequester func(u string, client *http.Client, username, password string) ([]byte, error)

func Request(path string, info *clientaccess.Info, requester HTTPRequester) ([]byte, error) {
	u, err := url.Parse(info.URL)
	if err != nil {
		return nil, err
	}
	u.Path = path
	username, password, _ := clientaccess.ParseUsernamePassword(info.Token)
	return requester(u.String(), clientaccess.GetHTTPClient(info.CACerts), username, password)
}

func GetNodeNamedCrt(nodeName, nodeIP, nodePasswordFile string) HTTPRequester {
	return getNodeNamedCrt(nodeName, nodeIP, nodePasswordFile)
}

func getNodeNamedCrt(nodeName, nodeIP, nodePasswordFile string) HTTPRequester {
	return func(u string, client *http.Client, username, password string) ([]byte, error) {
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}

		if username != "" {
			req.SetBasicAuth(username, password)
		}

		req.Header.Set(version.Program+"-Node-Name", nodeName)
		// nodePassword, err := ensureNodePassword(nodePasswordFile)
		// if err != nil {
		// 	return nil, err
		// }
		// req.Header.Set(version.Program+"-Node-Password", nodePassword)
		req.Header.Set(version.Program+"-Node-IP", nodeIP)

		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusForbidden {
			return nil, fmt.Errorf("Node password rejected, duplicate hostname or contents of '%s' may not match server node-passwd entry, try enabling a unique node name with the --with-node-id flag", nodePasswordFile)
		}

		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s", u, resp.Status)
		}

		return ioutil.ReadAll(resp.Body)
	}
}
