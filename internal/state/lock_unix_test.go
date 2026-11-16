//go:build unix

package state

import (
	"os"
	"strings"
	"testing"
)

// A second run on the same cluster stops at once; another cluster isn't held up, and the
// lock is free again once released.
func TestLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	unlock, err := Lock("lab")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock("lab"); err == nil || !strings.Contains(err.Error(), "another tow run is changing cluster 'lab'") {
		t.Errorf("second lock: %v", err)
	}
	other, err := Lock("prod")
	if err != nil {
		t.Errorf("another cluster: %v", err)
	} else {
		other()
	}
	unlock()
	again, err := Lock("lab")
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
	if _, err := os.Stat(Dir("lab")); !os.IsNotExist(err) {
		t.Errorf("a lock alone makes no cluster folder: %v", err)
	}
}
