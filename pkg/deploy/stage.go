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
			// addresses. A removal failure is fatal: silently proceeding
			// would let the watcher re-apply the stale manifest while the
			// operator believes it is skip-listed.
			if err := removeStagedCopy(dataDir, name); err != nil {
				return errors.Wrapf(err, "failed to remove stale staged manifest %s", filepath.Join(dataDir, name))
			}
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

// removeStagedCopy deletes a previously staged manifest copy. The manifest
// watcher applies whatever is on disk, so skip-listed manifests must not
// leave a stale file behind (see Stage's skip branch). Returns nil when the
// file is already absent or was removed; returns the removal error otherwise
// so Stage can fail closed instead of silently leaving the stale copy for
// the watcher to re-apply.
func removeStagedCopy(dataDir, name string) error {
	p := filepath.Join(dataDir, name)
	if _, err := os.Stat(p); os.IsNotExist(err) {
		return nil
	}
	if err := os.Remove(p); err != nil {
		return err
	}
	logrus.Warnf("removed stale staged manifest %s (manifest is skip-listed)", p)
	return nil
}
