package kubeadm

import (
	"context"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/runtimes"
)

// restoreCP answers a restore's scripts and keeps them, in order across machines.
type restoreCP struct {
	name     string
	log      *[]string
	mu       *sync.Mutex
	snapshot string // the snapshot's etcd release
}

func (m *restoreCP) RunInput(_ context.Context, _, stdin string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	*m.log = append(*m.log, m.name+"\n"+stdin)
	if strings.Contains(stdin, "snapshot status") {
		return `snapshot={"hash":1,"revision":2,"totalKey":3,"totalSize":4,"version":"` + m.snapshot + `"}` + "\netcdutl=etcdutl version: 3.6.5\n", nil
	}
	return "", nil
}
func (m *restoreCP) Close() error { return nil }

func restoreLab(snapshot string, n int) (RestoreOptions, *[]string) {
	var log []string
	mu := &sync.Mutex{}
	machines := map[string]*restoreCP{}
	var cps []providers.Node
	for i := range n {
		node := providers.Node{Name: "lab-cp-" + string(rune('0'+i)), Host: "203.0.113.1" + string(rune('0'+i)), PrivateIP: "10.0.0.1" + string(rune('0'+i))}
		cps = append(cps, node)
		machines[node.Host] = &restoreCP{name: node.Name, log: &log, mu: mu, snapshot: snapshot}
	}
	return RestoreOptions{Runtime: runtimes.Containerd, ControlPlanes: cps, Snapshot: "/var/lib/tow/restore/lab.db", Stamp: "20261113T120000Z",
		Connect: func(_ context.Context, n providers.Node, _ string) (prepare.Runner, error) {
			return machines[n.Host], nil
		}}, &log
}

// etcdutl comes from each control plane's etcd, and the snapshot must be from its minor
// release; then the API servers and etcd stop everywhere before any member is restored,
// each with its own name and address and the whole cluster, and start again.
func TestRestoreKubeadm(t *testing.T) {
	o, log := restoreLab("3.6.0", 3)
	if err := CheckRestore(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(*log) != 3 || !strings.Contains((*log)[0], "install -m 0700 \"$root/usr/local/bin/etcdutl\" \"$u\"") {
		t.Errorf("check: %v", *log)
	}
	bad, _ := restoreLab("3.5.21", 1)
	if err := CheckRestore(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "the snapshot is from etcd 3.5.21, and 203.0.113.10 (lab-cp-0) runs etcd 3.6.5") {
		t.Errorf("another etcd minor: %v", err)
	}

	*log = nil
	aside, err := Restore(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if len(*log) != 9 {
		t.Fatalf("%d scripts, want stop×3, restore×3, start×3", len(*log))
	}
	for i, s := range *log {
		switch {
		case i < 3 && !strings.Contains(s, "mv /etc/kubernetes/manifests/$c.yaml /etc/kubernetes/tow-restore/"):
			t.Errorf("script %d isn't a stop:\n%s", i, s)
		case i >= 3 && i < 6 && !strings.Contains(s, "snapshot restore"):
			t.Errorf("script %d isn't a restore:\n%s", i, s)
		case i >= 6 && !strings.Contains(s, "mv \"$f\" /etc/kubernetes/manifests/"):
			t.Errorf("script %d isn't a start:\n%s", i, s)
		}
	}
	member := (*log)[4]
	for _, want := range []string{
		"lab-cp-1\n", "mv /var/lib/etcd /var/lib/etcd.before-restore-20261113T120000Z",
		"/var/lib/tow/etcdutl snapshot restore '/var/lib/tow/restore/lab.db' --data-dir /var/lib/etcd --name 'lab-cp-1'",
		"--initial-cluster 'lab-cp-0=https://10.0.0.10:2380,lab-cp-1=https://10.0.0.11:2380,lab-cp-2=https://10.0.0.12:2380'",
		"--initial-advertise-peer-urls https://10.0.0.11:2380", "--initial-cluster-token tow-restore-20261113T120000Z",
		"--bump-revision 1000000000 --mark-compacted",
	} {
		if !strings.Contains(member, want) {
			t.Errorf("member script lacks %q:\n%s", want, member)
		}
	}
	if len(aside) != 3 || !strings.HasSuffix(aside[2], "/var/lib/etcd.before-restore-20261113T120000Z") {
		t.Errorf("aside %v", aside)
	}
	for _, s := range []string{etcdutlScript(runtimes.Containerd, o.Snapshot), stopScript(runtimes.Containerd), memberScript(o, o.ControlPlanes[0], "a=b"), startScript} {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v\n%s\n%s", err, out, s)
		}
	}
	if !strings.Contains(stopScript(runtimes.Containerd), "for c in etcd kube-apiserver kube-controller-manager kube-scheduler") {
		t.Error("kube-vip's static pod stays: only etcd and the control plane's are set aside")
	}
}
