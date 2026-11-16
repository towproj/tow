package remote

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// testServer is an SSH server on localhost that runs each command with this machine's sh,
// so the client's sessions are tested for real. It accepts the client key it is given.
func testServer(t *testing.T, client ssh.PublicKey) Target {
	t.Helper()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	hostKey, err := ssh.NewSignerFromKey(hostPriv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(k.Marshal(), client.Marshal()) {
			return nil, nil
		}
		return nil, os.ErrPermission
	}}
	cfg.AddHostKey(hostKey)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serve(conn, cfg)
		}
	}()
	return Target{Host: "127.0.0.1", Port: l.Addr().(*net.TCPAddr).Port, User: "test"}
}

func serve(conn net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, reqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for req := range reqs {
				if req.Type != "exec" {
					req.Reply(false, nil)
					continue
				}
				req.Reply(true, nil)
				n := binary.BigEndian.Uint32(req.Payload)
				cmd := exec.Command("sh", "-c", string(req.Payload[4:4+n]))
				cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
				status := 0
				if err := cmd.Run(); err != nil {
					status = 1
					if e, ok := err.(*exec.ExitError); ok {
						status = e.ExitCode()
					}
				}
				ch.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, uint32(status)))
				return
			}
		}()
	}
}

// dialTest starts a server and connects to it with a fresh key.
func dialTest(t *testing.T) *Client {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	target := testServer(t, sshPub)

	dir := t.TempDir()
	keyPath := filepath.Join(dir, "id")
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	target.KeyPath = keyPath
	t.Setenv("SSH_AUTH_SOCK", "")
	c, err := Dial(context.Background(), target, filepath.Join(dir, "known_hosts"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A file far larger than a script travels both ways intact, its error output apart from it,
// and a failing command reports its exit code.
func TestStream(t *testing.T) {
	c := dialTest(t)
	ctx := context.Background()
	data := make([]byte, 5<<20)
	rand.Read(data)
	file := filepath.Join(t.TempDir(), "it's a snapshot.db") // a quote in the path

	if errOut, err := c.Stream(ctx, "cat > "+Quote(file)+"; echo noise >&2", bytes.NewReader(data), io.Discard); err != nil || strings.TrimSpace(errOut) != "noise" {
		t.Fatalf("upload: %v, stderr %q", err, errOut)
	}
	var back bytes.Buffer
	if _, err := c.Stream(ctx, "echo noise >&2; cat -- "+Quote(file), nil, &back); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back.Bytes(), data) {
		t.Fatalf("download: %d bytes, want %d, intact %v", back.Len(), len(data), bytes.Equal(back.Bytes(), data))
	}
	errOut, err := c.Stream(ctx, "echo gone >&2; exit 3", nil, io.Discard)
	if ExitCode(err) != 3 || strings.TrimSpace(errOut) != "gone" {
		t.Errorf("failure: exit %d, stderr %q", ExitCode(err), errOut)
	}
	if out, err := c.RunInput(ctx, "sh -s", "echo out; echo err >&2"); err != nil || !strings.Contains(out, "out") || !strings.Contains(out, "err") {
		t.Errorf("RunInput keeps both outputs: %q %v", out, err)
	}
}

// AsRootCommand's script reaches sh as one word, whatever it holds; sudo here is a stand-in
// that runs what it's given.
func TestAsRootCommand(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("runs as root: sudo isn't used")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\n[ \"$1\" = -n ] && shift\nexec \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	c := dialTest(t)
	file := filepath.Join(t.TempDir(), "a 'quoted' dir", "f")
	script := "umask 077; install -d -m 0700 " + Quote(filepath.Dir(file)) + " && cat > " + Quote(file)
	if errOut, err := c.Stream(context.Background(), AsRootCommand(script), strings.NewReader("etcd"), io.Discard); err != nil {
		t.Fatalf("%v: %s", err, errOut)
	}
	if got, _ := os.ReadFile(file); string(got) != "etcd" {
		t.Errorf("file holds %q", got)
	}
	if fi, _ := os.Stat(file); fi == nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode: %v", fi)
	}
}
