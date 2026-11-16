package prepare

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
)

// stubs are the commands revert scripts call, as small scripts that log how they were called
// (ufw keeps its rules in a file).
var stubs = map[string]string{
	"systemctl": `echo "systemctl $*" >> "$LOG"`,
	"swapon":    `echo "swapon $*" >> "$LOG"`,
	"apt-get":   `echo "apt-get $*" >> "$LOG"`,
	// conntrack is installed, socat was removed (only its config files are left).
	"dpkg-query": `for p in "$@"; do case "$p" in -*|'${Package}'*) ;; conntrack) echo "conntrack install ok installed";; socat) echo "socat deinstall ok config-files";; esac; done`,
	"ufw": `echo "ufw $*" >> "$LOG"
case "$1" in
status) echo "Status: active"; echo; n=0; while IFS= read -r l; do n=$((n + 1)); printf '[%2d] %s\n' "$n" "$l"; done < "$UFW" ;;
--force) [ "$2" = delete ] && sed -i "${3}d" "$UFW" ;;
esac`,
}

// runRevert runs a revert script as delete would, in a shell whose /etc is root's and whose
// commands are the stubs. It returns what the stubs logged.
func runRevert(t *testing.T, root, script string) []string {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range stubs {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	log := filepath.Join(root, "log")
	os.Remove(log)
	cmd := exec.Command("sh", "-c", "set -e\n"+strings.ReplaceAll(script, "/etc/", root+"/etc/"))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "LOG="+log, "UFW="+filepath.Join(root, "ufw"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, script)
	}
	b, _ := os.ReadFile(log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(b)
}

// Each revert script undoes exactly what its change did, and nothing that was there before.
func TestRevertScripts(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("scripts target Linux nodes")
	}
	root := t.TempDir()
	revert := map[string]string{}
	f := fresh()
	f.UFW = "active"
	n := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: flannel}}, "+byoh+"}"), config.RoleControlPlane)
	info := clusterInfo{name: "lab", peers: []string{"10.0.0.2"}, podCIDR: "10.244.0.0/16", serviceCIDR: "10.96.0.0/12"}
	changes, _, _ := planNode(n, f, ufwRules(n, info), firewalldWanted(n, info), info)
	for _, c := range changes {
		if c.Record.Revert == "" {
			t.Errorf("%s has no revert script", c.Record.What)
		}
		revert[c.Record.What] = c.Record.Revert
	}

	// Swap: the lines tow commented out come back, units unmasked, swap on again.
	write(t, root+"/etc/fstab", "UUID=1 / ext4 defaults 0 1\n# tow: /swap.img none swap sw 0 0\n# my own comment\n")
	log := runRevert(t, root, revert["swap"])
	if got := read(t, root+"/etc/fstab"); got != "UUID=1 / ext4 defaults 0 1\n/swap.img none swap sw 0 0\n# my own comment\n" {
		t.Errorf("fstab:\n%s", got)
	}
	if !slices.Contains(log, "systemctl unmask swap.img.swap") || !slices.Contains(log, "swapon -a") {
		t.Errorf("swap: %v", log)
	}

	// Modules and settings: only tow's lines go, and the file once it's empty.
	write(t, root+modulesConf, "mine\nbr_netfilter\n")
	write(t, root+sysctlConf, "vm.mine = 1\nnet.ipv4.ip_forward = 1\n")
	runRevert(t, root, revert["kernel modules"])
	runRevert(t, root, revert["kernel settings"])
	if got := read(t, root+modulesConf); got != "mine\n" {
		t.Errorf("modules: %q", got)
	}
	if got := read(t, root+sysctlConf); got != "vm.mine = 1\n" {
		t.Errorf("sysctl: %q", got)
	}
	write(t, root+modulesConf, "br_netfilter\n")
	write(t, root+sysctlConf, "net.ipv4.ip_forward = 1\n")
	runRevert(t, root, revert["kernel modules"])
	runRevert(t, root, revert["kernel settings"])
	for _, p := range []string{modulesConf, sysctlConf} {
		if _, err := os.Stat(root + p); !os.IsNotExist(err) {
			t.Errorf("%s left although empty", p)
		}
	}

	// Firewall: the cluster's rules go, IPv6 ones too; not the user's, not lab2's.
	write(t, root+"/ufw", "22/tcp ALLOW IN Anywhere\n6443/tcp ALLOW IN 203.0.113.7 # tow:lab\nAnywhere ALLOW IN 10.0.0.2 # tow:lab\n"+
		"80/tcp ALLOW IN 203.0.113.7 # tow:lab2\n6443/tcp (v6) ALLOW IN Anywhere (v6) # tow:lab\n")
	runRevert(t, root, revert["firewall"])
	if got := read(t, root+"/ufw"); got != "22/tcp ALLOW IN Anywhere\n80/tcp ALLOW IN 203.0.113.7 # tow:lab2\n" {
		t.Errorf("ufw rules left:\n%s", got)
	}

	// Packages: only those still installed are removed.
	log = runRevert(t, root, RemovePackages("remove", []string{"conntrack", "socat"}))
	if !slices.ContainsFunc(log, func(l string) bool {
		return strings.HasPrefix(l, "apt-get") && strings.HasSuffix(l, "remove -y -qq conntrack")
	}) {
		t.Errorf("packages: %v", log)
	}
}

// recordedNode is a machine with a record file, as Undo sees it over SSH.
type recordedNode struct {
	mu     sync.Mutex
	file   string
	ran    []string
	failOn string
}

func (m *recordedNode) RunInput(_ context.Context, cmd, stdin string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case cmd != asRoot:
		return "", errors.New("everything runs as root through stdin")
	case strings.HasPrefix(stdin, "cat "+RecordFile):
		return m.file, nil
	case strings.Contains(stdin, "<<'TOW_EOF'\n"):
		_, rest, _ := strings.Cut(stdin, "<<'TOW_EOF'\n")
		m.file, _, _ = strings.Cut(rest, "\nTOW_EOF")
		return "", nil
	case strings.Contains(stdin, "rm -f "+RecordFile):
		m.file = ""
		return "", nil
	}
	m.ran = append(m.ran, strings.TrimPrefix(stdin, "set -e\n"))
	if m.failOn != "" && strings.Contains(stdin, m.failOn) {
		return "boom", errors.New("exit status 1")
	}
	return "", nil
}
func (m *recordedNode) Close() error { return nil }

func (m *recordedNode) whats(t *testing.T) []string {
	t.Helper()
	if m.file == "" {
		return nil
	}
	var l ledger
	if err := json.Unmarshal([]byte(m.file), &l); err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range l.Changes {
		out = append(out, r.Cluster+"/"+r.What)
	}
	return out
}

func ledgerOf(t *testing.T, rs ...Record) string {
	t.Helper()
	b, err := json.Marshal(ledger{Changes: rs})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Undo removes Kubernetes first, then (when asked) the preparation, each newest first; what's
// undone leaves the record, what has no revert script and other clusters' changes stay.
func TestUndo(t *testing.T) {
	m := &recordedNode{file: ledgerOf(t,
		Record{Cluster: "old", What: "packages", Revert: "remove old"},
		Record{Cluster: "lab", What: "packages", Revert: "remove pkgs"},
		Record{Cluster: "lab", What: "swap", Revert: "swap on"},
		Record{Cluster: "lab", What: "firewall", Undo: "by hand"}, // from before revert scripts
		Record{Cluster: "lab", What: "containerd", Revert: "remove containerd", Kubernetes: true},
		Record{Cluster: "lab", What: "kubeadm", Revert: "kubeadm reset", Kubernetes: true},
	)}
	o := UndoOptions{Cluster: "lab", Nodes: []providers.Node{{Host: "10.0.0.1"}},
		Connect: func(context.Context, providers.Node, string) (Runner, error) { return m, nil }}
	ctx := context.Background()

	left, err := ReadLeft(ctx, o)
	if err != nil || len(left) != 1 {
		t.Fatal(left, err)
	}
	var k8s, prep []string
	for _, r := range left[0].Kubernetes {
		k8s = append(k8s, r.What)
	}
	for _, r := range left[0].Preparation {
		prep = append(prep, r.What)
	}
	if !slices.Equal(k8s, []string{"kubeadm", "containerd"}) || !slices.Equal(prep, []string{"firewall", "swap", "packages"}) {
		t.Errorf("kubernetes %v, preparation %v", k8s, prep)
	}
	if manual := Manual(left[0].Preparation); len(manual) != 1 || manual[0].What != "firewall" {
		t.Errorf("manual: %+v", manual)
	}

	// Kubernetes only.
	if err := Undo(ctx, o, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.ran, []string{"kubeadm reset", "remove containerd"}) || !slices.Equal(m.whats(t), []string{"old/packages", "lab/packages", "lab/swap", "lab/firewall"}) {
		t.Errorf("ran %v, record %v", m.ran, m.whats(t))
	}

	// Then the preparation; it fails halfway: what was undone leaves the record all the same.
	m.ran, m.failOn = nil, "remove pkgs"
	err = Undo(ctx, o, true)
	if err == nil || !strings.Contains(err.Error(), "10.0.0.1") || !strings.Contains(err.Error(), "undo packages") || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v", err)
	}
	if !slices.Equal(m.ran, []string{"swap on", "remove pkgs"}) || !slices.Equal(m.whats(t), []string{"old/packages", "lab/packages", "lab/firewall"}) {
		t.Errorf("ran %v, record %v", m.ran, m.whats(t))
	}

	// Run again: it picks up where it stopped.
	m.ran, m.failOn = nil, ""
	if err := Undo(ctx, o, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.ran, []string{"remove pkgs"}) || !slices.Equal(m.whats(t), []string{"old/packages", "lab/firewall"}) {
		t.Errorf("ran %v, record %v", m.ran, m.whats(t))
	}

	// The last of a record's changes undone: the file goes.
	m.file = ledgerOf(t, Record{Cluster: "lab", What: "k3s", Revert: "uninstall", Kubernetes: true})
	if err := Undo(ctx, o, false); err != nil || m.file != "" {
		t.Errorf("file %q, %v", m.file, err)
	}
}

// A machine leaving a cluster that keeps running: Kubernetes and the cluster's firewall rules
// go, the rest of its preparation stays.
func TestUndoFirewallOnly(t *testing.T) {
	m := &recordedNode{file: ledgerOf(t,
		Record{Cluster: "lab", What: "packages", Revert: "remove pkgs"},
		Record{Cluster: "lab", What: "firewall", Revert: "ufw rules gone"},
		Record{Cluster: "lab", What: "sysctl", Revert: "sysctl back"},
		Record{Cluster: "lab", What: "k3s", Revert: "uninstall", Kubernetes: true},
	)}
	o := UndoOptions{Cluster: "lab", Nodes: []providers.Node{{Host: "10.0.0.1"}}, Firewall: true,
		Connect: func(context.Context, providers.Node, string) (Runner, error) { return m, nil }}
	if err := Undo(context.Background(), o, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.ran, []string{"uninstall", "ufw rules gone"}) || !slices.Equal(m.whats(t), []string{"lab/packages", "lab/sysctl"}) {
		t.Errorf("ran %v, record %v", m.ran, m.whats(t))
	}
}

// Nothing recorded is nothing to undo; a record that can't be read, or a machine that can't
// be reached, is an error naming the machine.
func TestReadLeftProblems(t *testing.T) {
	ctx := context.Background()
	m := &recordedNode{}
	o := UndoOptions{Cluster: "lab", Nodes: []providers.Node{{Host: "10.0.0.1", Name: "cp"}},
		Connect: func(context.Context, providers.Node, string) (Runner, error) { return m, nil }}
	if left, err := ReadLeft(ctx, o); err != nil || len(left) != 1 || left[0].Node.Host != "10.0.0.1" || len(left[0].Kubernetes)+len(left[0].Preparation) != 0 {
		t.Errorf("empty: %+v, %v", left, err)
	}
	m.file = `{"changes": [`
	if _, err := ReadLeft(ctx, o); err == nil || !strings.Contains(err.Error(), "10.0.0.1 (cp)") || !strings.Contains(err.Error(), "can't be read") {
		t.Errorf("corrupt: %v", err)
	}
	o.Connect = func(context.Context, providers.Node, string) (Runner, error) {
		return nil, errors.New("no route to host")
	}
	if _, err := ReadLeft(ctx, o); err == nil || !strings.Contains(err.Error(), "no route to host") {
		t.Errorf("unreachable: %v", err)
	}
}

// A record left with no changes is removed, its copy too.
func TestEmptyRecordIsRemoved(t *testing.T) {
	dir := t.TempDir()
	write(t, dir+"/tow/prepared.json", "{}")
	write(t, dir+"/tow/prepared.json.prev", "{}")
	s, err := ledger{}.script("")
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("sh", "-c", "set -e\n"+strings.ReplaceAll(s, "/etc/tow", dir+"/tow")).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if _, err := os.Stat(dir + "/tow"); !os.IsNotExist(err) {
		t.Error("/etc/tow left behind although empty")
	}
}
