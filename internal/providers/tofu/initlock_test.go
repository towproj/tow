//go:build unix

package tofu

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// One init at a time: a second one waits until the first releases the lock, and gives up
// when its context ends.
func TestLockInit(t *testing.T) {
	path := filepath.Join(t.TempDir(), initLockFile)
	unlock, err := lockInit(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := lockInit(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second init must wait while the first holds the lock: %v", err)
	}

	got := make(chan error, 1)
	go func() {
		release, err := lockInit(context.Background(), path)
		if err == nil {
			release()
		}
		got <- err
	}()
	select {
	case err := <-got:
		t.Fatalf("got the lock while it was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lock never came free")
	}
}
