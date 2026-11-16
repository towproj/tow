// Package remote is tow's SSH plumbing, shared by every adapter that reaches machines:
// tofu waits for the servers it created, byoh checks the ones the user listed.
package remote

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

const (
	dialTimeout = 10 * time.Second
	retryEvery  = 5 * time.Second
)

// AsRoot runs the script sent on stdin as root: directly, or through sudo without a password.
// Scripts go on stdin, never the command line, where 'ps' would show them.
const AsRoot = `if [ "$(id -u)" -eq 0 ]; then exec sh -s; else exec sudo -n sh -s; fi`

// AsRootCommand runs a short script as root from the command line, which leaves stdin to
// the data the script reads (a file copied over). 'ps' shows the command line: only for
// scripts with nothing secret in them, such as paths.
func AsRootCommand(script string) string {
	q := Quote(script)
	return `if [ "$(id -u)" -eq 0 ]; then exec sh -c ` + q + `; else exec sudo -n sh -c ` + q + `; fi`
}

// Quote makes s a single shell word, whatever it holds (a quote in it is closed, escaped
// and reopened).
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Target is one machine reachable over SSH.
type Target struct {
	Host    string
	Port    int
	User    string
	KeyPath string // empty = ssh-agent, then the default ~/.ssh keys (like ssh itself)
}

func (t Target) addr() string { return net.JoinHostPort(t.Host, strconv.Itoa(t.Port)) }

type Client struct {
	ssh   *ssh.Client
	agent net.Conn      // kept open: agent signers need it for the whole session
	done  chan struct{} // closed by Close: the keepalives stop
	once  sync.Once
}

// keepAliveEvery spaces the requests that keep a quiet session's connection open (as
// OpenSSH's ServerAliveInterval does): a command that prints nothing for minutes, like
// kubeadm init waiting for its control plane, must not lose its connection to a NAT or
// firewall along the way that forgets idle ones.
const keepAliveEvery = 30 * time.Second

func (c *Client) keepAlive() {
	t := time.NewTicker(keepAliveEvery)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			// The answer doesn't matter (servers reply "unknown request"), only the traffic.
			if _, _, err := c.ssh.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}
}

// Dial connects once. Host keys are pinned in knownHosts: an unknown host is recorded on
// first contact (like ssh's accept-new) and a changed key is refused with ErrHostKeyChanged.
func Dial(ctx context.Context, t Target, knownHosts string) (*Client, error) {
	auth, agentConn, err := authMethods(t.KeyPath)
	if err != nil {
		return nil, err
	}
	closeAgent := func() {
		if agentConn != nil {
			agentConn.Close()
		}
	}

	hostKey, err := pinningCallback(knownHosts)
	if err != nil {
		closeAgent()
		return nil, err
	}

	d := net.Dialer{Timeout: dialTimeout}
	conn, err := d.DialContext(ctx, "tcp", t.addr())
	if err != nil {
		closeAgent()
		return nil, err
	}

	// The handshake ignores ctx, so a deadline stops a silent server from hanging it.
	conn.SetDeadline(time.Now().Add(dialTimeout))
	c, chans, reqs, err := ssh.NewClientConn(conn, t.addr(), &ssh.ClientConfig{
		User:            t.User,
		Auth:            auth,
		HostKeyCallback: hostKey,
		Timeout:         dialTimeout,
	})
	if err != nil {
		conn.Close()
		closeAgent()
		return nil, fmt.Errorf("ssh %s@%s: %w", t.User, t.addr(), err)
	}
	conn.SetDeadline(time.Time{})
	client := &Client{ssh: ssh.NewClient(c, chans, reqs), agent: agentConn, done: make(chan struct{})}
	go client.keepAlive()
	return client, nil
}

// WaitDial retries Dial until it works or ctx ends: new machines refuse connections, then
// reject the key, until cloud-init has set them up. A changed host key is never retried.
func WaitDial(ctx context.Context, t Target, knownHosts string) (*Client, error) {
	for {
		c, err := Dial(ctx, t, knownHosts)
		if err == nil || errors.Is(err, ErrHostKeyChanged) {
			return c, err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("%s not reachable over SSH: %w", t.addr(), err)
		case <-time.After(retryEvery):
		}
	}
}

// Run executes cmd and returns its combined output. A non-zero exit is reported by ExitCode.
func (c *Client) Run(ctx context.Context, cmd string) (string, error) {
	return c.RunInput(ctx, cmd, "")
}

// RunInput executes cmd with stdin as its standard input: scripts and file contents travel
// this way, never on a command line where 'ps' or a shell history would show them.
func (c *Client) RunInput(ctx context.Context, cmd, stdin string) (string, error) {
	var out lockedBuffer
	err := c.session(ctx, cmd, func(s *ssh.Session) {
		if stdin != "" {
			s.Stdin = strings.NewReader(stdin)
		}
		s.Stdout, s.Stderr = &out, &out
	})
	return out.String(), err
}

// lockedBuffer takes both of a session's outputs: SSH copies each one in its own goroutine.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Stream executes cmd with its standard input read from in (nil: none) and its standard
// output written to out, for data too large to hold in memory (an etcd snapshot). Its error
// output comes back apart, to explain a failure.
func (c *Client) Stream(ctx context.Context, cmd string, in io.Reader, out io.Writer) (string, error) {
	var errOut bytes.Buffer
	err := c.session(ctx, cmd, func(s *ssh.Session) {
		s.Stdin, s.Stdout, s.Stderr = in, out, &errOut
	})
	return errOut.String(), err
}

// session runs cmd in a new session that setup wires up; ctx ending closes it.
func (c *Client) session(ctx context.Context, cmd string, setup func(*ssh.Session)) error {
	s, err := c.ssh.NewSession()
	if err != nil {
		return err
	}
	defer s.Close()
	setup(s)

	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.Close()
		case <-done:
		}
	}()

	err = s.Run(cmd)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func (c *Client) Close() error {
	if c.done != nil {
		c.once.Do(func() { close(c.done) })
	}
	if c.agent != nil {
		c.agent.Close()
	}
	return c.ssh.Close()
}

// ExitCode returns the remote command's exit status, or -1 when err isn't one.
func ExitCode(err error) int {
	var exit *ssh.ExitError
	if errors.As(err, &exit) {
		return exit.ExitStatus()
	}
	return -1
}

// CheckKeys fails fast when no SSH key can be offered, before any machine is contacted.
func CheckKeys(keyPath string) error {
	_, agentConn, err := authMethods(keyPath)
	if agentConn != nil {
		agentConn.Close()
	}
	return err
}

var defaultKeys = []string{"id_ed25519", "id_ecdsa", "id_rsa"}

// authMethods offers the configured key (or the default ones), then the agent's keys, as a
// single publickey method: Go's client tries each method name only once.
func authMethods(keyPath string) ([]ssh.AuthMethod, net.Conn, error) {
	var signers []ssh.Signer
	paths := defaultKeyPaths()
	if keyPath != "" {
		paths = []string{expandHome(keyPath)}
	}
	for _, p := range paths {
		s, err := loadSigner(p)
		var missingPass *ssh.PassphraseMissingError
		switch {
		case err == nil:
			signers = append(signers, s)
		case keyPath != "" && !errors.As(err, &missingPass):
			// An explicit key must work; a passphrase-protected one is left to the agent.
			return nil, nil, fmt.Errorf("ssh key %s: %w", keyPath, err)
		}
	}

	var agentConn net.Conn
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			agentConn = conn
			if more, err := agent.NewClient(conn).Signers(); err == nil {
				signers = append(signers, more...)
			}
		}
	}

	if len(signers) == 0 {
		if agentConn != nil {
			agentConn.Close()
		}
		return nil, nil, errors.New("no usable SSH key: set ssh.keyPath, load a key into ssh-agent, or create one with 'ssh-keygen -t ed25519'")
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, agentConn, nil
}

func loadSigner(path string) (ssh.Signer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKey(data)
}

// PublicKey returns the authorized_keys line to install on new machines: keyPath's ".pub"
// sibling, or the first default key pair in ~/.ssh when keyPath is empty.
func PublicKey(keyPath string) (string, error) {
	paths := defaultKeyPaths()
	if keyPath != "" {
		paths = []string{expandHome(keyPath)}
	}
	for _, p := range paths {
		data, err := os.ReadFile(p + ".pub")
		if err != nil {
			if keyPath != "" {
				return "", fmt.Errorf("public key for ssh.keyPath: %w", err)
			}
			continue
		}
		if _, _, _, _, err := ssh.ParseAuthorizedKey(data); err != nil {
			return "", fmt.Errorf("%s.pub is not an SSH public key: %w", p, err)
		}
		return strings.TrimSpace(string(data)), nil
	}
	return "", errors.New("no SSH public key in ~/.ssh (id_ed25519.pub, id_ecdsa.pub, id_rsa.pub): create one with 'ssh-keygen -t ed25519' or set ssh.keyPath")
}

func defaultKeyPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	paths := make([]string, len(defaultKeys))
	for i, k := range defaultKeys {
		paths[i] = filepath.Join(home, ".ssh", k)
	}
	return paths
}

func expandHome(p string) string {
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, "~") {
		return filepath.Join(home, p[1:])
	}
	return p
}
