package client

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type credentialFile struct {
	name string
	data []byte
	mode os.FileMode
}

// publishCredentials requires credentials.lock. Prepare the entire update and
// backups before replacing live files; roll back if any replacement fails.
// rename is injectable to exercise filesystem failures after partial publication.
// This is not a crash-atomic transaction across files.
func publishCredentials(dir string, files []credentialFile, rename func(string, string) error) error {
	staging, err := os.MkdirTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(staging)
	existed := make([]bool, len(files))
	for i, file := range files {
		old, err := os.ReadFile(filepath.Join(dir, file.name))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("backup %s: %w", file.name, err)
		}
		existed[i] = err == nil
		if existed[i] {
			if err := atomicWriteFile(filepath.Join(staging, file.name+".old"), old, file.mode); err != nil {
				return err
			}
		}
		if err := atomicWriteFile(filepath.Join(staging, file.name), file.data, file.mode); err != nil {
			return err
		}
	}
	for i, file := range files {
		if err := rename(filepath.Join(staging, file.name), filepath.Join(dir, file.name)); err != nil {
			failures := []error{fmt.Errorf("publish %s: %w", file.name, err)}
			for j := i - 1; j >= 0; j-- {
				target := filepath.Join(dir, files[j].name)
				var rollbackErr error
				if existed[j] {
					rollbackErr = os.Rename(filepath.Join(staging, files[j].name+".old"), target)
				} else {
					rollbackErr = os.Remove(target)
				}
				if rollbackErr != nil {
					failures = append(failures, fmt.Errorf("restore %s: %w", files[j].name, rollbackErr))
				}
			}
			return errors.Join(failures...)
		}
	}
	return nil
}
