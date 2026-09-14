//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly

/*
Copyright 2016 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package flock

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// checkLock reports whether the lock on path is currently held by another open
// file description.
//
// This probes the lock directly instead of shelling out to lsof: lsof's -F
// output is not portable, as macOS reports a blank lock field instead of lW/lR
// for flock(2) locks, which made the previous implementation fail on non-Linux
// hosts. flock() locks are bound to the open file description, so a second
// open() in the same process still conflicts with the lock under test.
func checkLock(path string) bool {
	fd, err := unix.Open(path, unix.O_RDWR, 0600)
	if err != nil {
		return false
	}
	defer unix.Close(fd)

	// LOCK_NB makes the call return immediately instead of blocking when the
	// lock is already held.
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return true
	}

	// We were able to take the lock, so it is not currently held. Release it
	// again so that the caller's lock state is left unchanged.
	_ = unix.Flock(fd, unix.LOCK_UN)
	return false
}

func Test_UnitFlock(t *testing.T) {
	tests := []struct {
		name      string
		path      string
		wantCheck bool
		wantErr   bool
	}{
		{
			name: "Basic Flock Test",
			path: filepath.Join(t.TempDir(), "testlock.test"),

			wantCheck: true,
			wantErr:   false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lock, err := Acquire(tt.path)
			if (err != nil) != tt.wantErr {
				t.Errorf("Acquire() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if got := checkLock(tt.path); got != tt.wantCheck {
				t.Errorf("checkLock() = %+v\nWant = %+v", got, tt.wantCheck)
			}

			if err := Release(lock); (err != nil) != tt.wantErr {
				t.Errorf("Release() error = %v, wantErr %v", err, tt.wantErr)
			}

			if got := checkLock(tt.path); got == tt.wantCheck {
				t.Errorf("checkLock() = %+v\nWant = %+v", got, !tt.wantCheck)
			}

			if err := unix.Close(lock); err != nil {
				t.Errorf("Close() error = %v", err)
			}
		})
	}
}
