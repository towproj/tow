//go:build unix

package tofu

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// lockInit takes an exclusive lock on path, shared by every tow on this machine, and
// returns its release. One 'tofu init' at a time fills the shared provider cache:
// OpenTofu says the cache isn't safe for concurrent use, and two inits writing the same
// provider at once fail ("text file busy") or leave a broken one. It waits as long as another
// init holds the lock, unless ctx ends first.
func lockInit(ctx context.Context, path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) // closing the file releases it anyway
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
