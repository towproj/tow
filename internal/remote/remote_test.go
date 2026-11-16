package remote

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func newKey(t *testing.T) ssh.PublicKey {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestHostKeyPinning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster", "known_hosts")
	check, err := pinningCallback(path)
	if err != nil {
		t.Fatal(err)
	}
	addr := &net.TCPAddr{IP: net.ParseIP("203.0.113.10"), Port: 22}
	first, other := newKey(t), newKey(t)

	if err := check("203.0.113.10:22", addr, first); err != nil {
		t.Fatalf("first contact should pin the key: %v", err)
	}
	if err := check("203.0.113.10:22", addr, first); err != nil {
		t.Errorf("pinned key should be accepted: %v", err)
	}
	if err := check("203.0.113.10:22", addr, other); !errors.Is(err, ErrHostKeyChanged) {
		t.Errorf("changed key: err = %v, want ErrHostKeyChanged", err)
	}
	if err := check("203.0.113.11:22", addr, other); err != nil {
		t.Errorf("another host should pin its own key: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("known_hosts mode = %o, want 600", perm)
	}
}

func TestPublicKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if _, err := PublicKey(""); err == nil || !strings.Contains(err.Error(), "ssh-keygen") {
		t.Errorf("no keys: err = %v, want a hint to create one", err)
	}

	line := string(ssh.MarshalAuthorizedKey(newKey(t)))
	os.MkdirAll(filepath.Join(home, ".ssh"), 0700)
	os.WriteFile(filepath.Join(home, ".ssh", "id_rsa.pub"), []byte(line), 0644)
	got, err := PublicKey("")
	if err != nil || got != strings.TrimSpace(line) {
		t.Errorf("default key: got %q, %v", got, err)
	}

	if _, err := PublicKey("~/.ssh/missing"); err == nil {
		t.Error("an explicit key path without a .pub file must fail, not fall back")
	}
}

// Forgetting a host drops its pinned keys, on SSH's port or another, and keeps the others.
func TestForget(t *testing.T) {
	path := filepath.Join(t.TempDir(), "known_hosts")
	if err := Forget(path, []string{"10.0.0.1"}); err != nil {
		t.Errorf("a missing file has nothing to forget: %v", err)
	}
	os.WriteFile(path, []byte("10.0.0.1 ssh-ed25519 AAAA1\n10.0.0.2 ssh-ed25519 AAAA2\n[10.0.0.3]:2222 ssh-ed25519 AAAA3\n"), 0o600)
	if err := Forget(path, []string{"10.0.0.1", "[10.0.0.3]:2222"}); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "10.0.0.2 ssh-ed25519 AAAA2\n" {
		t.Errorf("left %q", data)
	}
}

func TestExitCode(t *testing.T) {
	if ExitCode(errors.New("boom")) != -1 {
		t.Error("non-exit errors should report -1")
	}
}
