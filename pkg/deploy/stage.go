//go:build !no_stage

package deploy

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

func Stage(dataDir string, templateVars map[string]string, skips map[string]bool) error {
staging:
	for _, name := range AssetNames() {
		nameNoExtension := strings.TrimSuffix(name, filepath.Ext(name))
		if skips[name] || skips[nameNoExtension] {
			// Fail closed: a skipped manifest must not linger on disk either.
			// The deploy watcher applies whatever it finds on disk, so a stale
			// copy left by an earlier successful run would otherwise be
			// re-applied with its obsolete (possibly loopback) endpoint
			// addresses. Best effort — a removal failure must not block
			// staging the remaining manifests.
			removeStagedCopy(dataDir, name)
			continue staging
		}
		namePath := strings.Split(name, string(os.PathSeparator))
		for i := 1; i < len(namePath); i++ {
			subPath := filepath.Join(namePath[0:i]...)
			if skips[subPath] {
				continue staging
			}
		}

		content, err := Asset(name)
		if err != nil {
			return err
		}
		for k, v := range templateVars {
			content = bytes.Replace(content, []byte(k), []byte(v), -1)
		}
		p := filepath.Join(dataDir, name)
		os.MkdirAll(filepath.Dir(p), 0700)
		logrus.Info("Writing manifest: ", p)
		if err := os.WriteFile(p, content, 0600); err != nil {
			return errors.Wrapf(err, "failed to write to %s", name)
		}
	}

	return nil
}

// removeStagedCopy best-effort deletes a previously staged manifest copy.
// The manifest watcher applies whatever is on disk, so skip-listed manifests
// must not leave a stale file behind (see Stage's skip branch).
func removeStagedCopy(dataDir, name string) {
	p := filepath.Join(dataDir, name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return
	}
	if err := os.Remove(p); err != nil {
		logrus.Warnf("failed to remove stale staged manifest %s: %v", p, err)
		return
	}
	logrus.Warnf("removed stale staged manifest %s (manifest is skip-listed)", p)
}
