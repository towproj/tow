package remote

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// ErrHostKeyChanged means a machine presented a different key than the one pinned on first contact.
var ErrHostKeyChanged = errors.New("host key changed")

// Nodes are dialed in parallel; appends to the same file must not interleave.
var knownHostsMu sync.Mutex

// Forget drops the keys pinned for hosts (on SSH's port): a machine tow just created at
// an address another machine had brings a new key. A missing file has nothing to forget.
func Forget(path string, hosts []string) error {
	if len(hosts) == 0 {
		return nil
	}
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	forget := map[string]bool{}
	for _, h := range hosts {
		forget[knownhosts.Normalize(h)] = true
	}
	var kept []string
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		names, _, _ := strings.Cut(line, " ")
		if !slices.ContainsFunc(strings.Split(names, ","), func(n string) bool { return forget[n] }) {
			kept = append(kept, line)
		}
	}
	out := strings.Join(kept, "\n")
	if out != "" {
		out += "\n"
	}
	return os.WriteFile(path, []byte(out), 0600) //nolint:gosec // tow's own known_hosts file for the cluster
}

// pinningCallback trusts a host on first contact and refuses it if its key ever changes.
// Each cluster gets its own file, so a cloud IP reused by another cluster can't trip it.
func pinningCallback(path string) (ssh.HostKeyCallback, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()

	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		knownHostsMu.Lock()
		defer knownHostsMu.Unlock()

		// Reloaded on every call so keys pinned earlier in this run are honored.
		check, err := knownhosts.New(path)
		if err != nil {
			return err
		}
		err = check(hostname, remote, key)
		var keyErr *knownhosts.KeyError
		if !errors.As(err, &keyErr) {
			return err // nil when the key matches the pinned one
		}
		if len(keyErr.Want) > 0 {
			return fmt.Errorf("%w for %s: refusing to connect (possible man-in-the-middle); if the machine was rebuilt, remove its line from %s",
				ErrHostKeyChanged, hostname, path)
		}

		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = fmt.Fprintln(f, knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key))
		return err
	}, nil
}
