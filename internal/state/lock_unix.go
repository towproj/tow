//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Lock keeps two tow runs from changing the same cluster at once (apply, delete, backup, restore): it
// takes the cluster's lock file and returns its release. A second run stops at once instead
// of waiting. The kernel releases the lock when a run ends, even a killed one, so it never
// goes stale. Lock files live apart from the clusters' folders, so a first apply that stops
// early leaves no folder behind.
func Lock(name string) (func(), error) {
	dir := filepath.Join(Root(), lockDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, name+".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("another tow run is changing cluster '%s' right now (an apply, a delete, a backup or a restore); let it finish, then run this again", name)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // closing the file releases it anyway
		f.Close()
	}, nil
}
