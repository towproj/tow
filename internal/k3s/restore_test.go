package k3s

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
)

// restoreMachine records the scripts a restore ran, across machines, in order.
type restoreMachine struct {
	name string
	log  *[]string
	mu   *sync.Mutex
	fail string
}

func (m *restoreMachine) RunInput(_ context.Context, _, stdin string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	first, _, _ := strings.Cut(strings.TrimPrefix(stdin, "set -e\n"), "\n")
	*m.log = append(*m.log, m.name+": "+first)
	if m.fail != "" && strings.Contains(stdin, m.fail) {
		return "boom", errors.New("exit status 1")
	}
	return "", nil
}
func (m *restoreMachine) Close() error { return nil }

// The servers stop, the first resets to the snapshot and starts, then each other one moves
// its etcd data aside and joins, one at a time.
func TestRestore(t *testing.T) {
	var log []string
	mu := &sync.Mutex{}
	cps := []providers.Node{{Name: "cp-0", Host: "10.0.0.1"}, {Name: "cp-1", Host: "10.0.0.2"}, {Name: "cp-2", Host: "10.0.0.3"}}
	machines := map[string]*restoreMachine{}
	for _, n := range cps {
		machines[n.Host] = &restoreMachine{name: n.Name, log: &log, mu: mu}
	}
	o := RestoreOptions{ControlPlanes: cps, Snapshot: "/var/lib/tow/restore/lab.db", DataDir: "/var/lib/rancher/k3s", Stamp: "20261113T120000Z",
		Connect: func(_ context.Context, n providers.Node, _ string) (prepare.Runner, error) {
			return machines[n.Host], nil
		}}
	aside, err := Restore(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	stops, rest := log[:3], log[3:]
	for _, s := range stops {
		if !strings.HasSuffix(s, ": systemctl stop k3s") {
			t.Errorf("stops: %v", stops)
		}
	}
	want := []string{
		"cp-0: rm -f '/var/lib/rancher/k3s/server/db/reset-flag'",
		"cp-0: systemctl start k3s",
		"cp-1: if [ -d '/var/lib/rancher/k3s/server/db' ]; then mv '/var/lib/rancher/k3s/server/db' '/var/lib/rancher/k3s/server/db.before-restore-20261113T120000Z'; fi",
		"cp-2: if [ -d '/var/lib/rancher/k3s/server/db' ]; then mv '/var/lib/rancher/k3s/server/db' '/var/lib/rancher/k3s/server/db.before-restore-20261113T120000Z'; fi",
		"cp-0: rm -f '/var/lib/tow/restore/lab.db'",
	}
	if strings.Join(rest, "\n") != strings.Join(want, "\n") {
		t.Errorf("order:\n  %s\nwant:\n  %s", strings.Join(rest, "\n  "), strings.Join(want, "\n  "))
	}
	if len(aside) != 2 || !strings.Contains(aside[0], "cp-1") || !strings.HasSuffix(aside[0], "db.before-restore-20261113T120000Z") {
		t.Errorf("aside: %v", aside)
	}

	for _, s := range []string{resetScript(o), rejoinScript(o.DataDir, "/x"), ReadyScript} {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v\n%s\n%s", err, out, s)
		}
	}
	if !strings.Contains(resetScript(o), "--cluster-reset-restore-path='/var/lib/tow/restore/lab.db' 2>&1") {
		t.Errorf("reset: %s", resetScript(o))
	}
	o.S3 = true
	if !strings.Contains(resetScript(o), "lab.db' --etcd-s3=false 2>&1") {
		t.Error("with etcd-s3 configured, the reset must not look in S3")
	}

	// A control plane that can't rejoin stops the restore there.
	log = nil
	machines["10.0.0.2"].fail = "db.before-restore"
	if _, err := Restore(context.Background(), o); err == nil || !strings.Contains(err.Error(), "cp-1") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(strings.Join(log, "\n"), "cp-2: if") {
		t.Error("cp-2 went on after cp-1 failed")
	}
}
