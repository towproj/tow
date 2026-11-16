package kubeadm

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/runtimes"
)

// snapshotMachine keeps its record and the scripts it ran.
type snapshotMachine struct {
	mu      sync.Mutex
	record  string
	scripts []string
}

func (m *snapshotMachine) RunInput(_ context.Context, cmd, stdin string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if strings.HasPrefix(stdin, "cat "+prepare.RecordFile) {
		return m.record, nil
	}
	m.scripts = append(m.scripts, stdin)
	if _, rest, ok := strings.Cut(stdin, "cat > "+prepare.RecordFile+" <<'TOW_EOF'\n"); ok {
		m.record, _, _ = strings.Cut(rest, "\nTOW_EOF")
	}
	if strings.Contains(stdin, "snapshot save") && !strings.Contains(stdin, "systemctl") {
		return SnapshotDir + "/etcd-cp-0-20261113T120000Z.db\n", nil
	}
	return "", nil
}
func (m *snapshotMachine) Close() error { return nil }

func snapshotOptions(machines map[string]*snapshotMachine) Options {
	var mu sync.Mutex // the machines are reached in parallel
	return Options{Cluster: "lab", Runtime: runtimes.Containerd,
		Nodes: []providers.Node{{Name: "cp-0", Role: config.RoleControlPlane}, {Name: "cp-1", Role: config.RoleControlPlane}, {Name: "w-0", Role: config.RoleWorker}},
		Connect: func(_ context.Context, n providers.Node, _ string) (prepare.Runner, error) {
			mu.Lock()
			defer mu.Unlock()
			if machines[n.Name] == nil {
				machines[n.Name] = &snapshotMachine{}
			}
			return machines[n.Name], nil
		}}
}

// The timer goes on every control plane (never a worker), recorded once; a new schedule is
// written over it; taking it off leaves the snapshots.
func TestSetSnapshots(t *testing.T) {
	machines := map[string]*snapshotMachine{}
	o := snapshotOptions(machines)
	o.Snapshots = &Snapshots{Calendar: "*-*-* 0/6:0:00", Keep: 5}
	ctx := context.Background()
	if err := SetSnapshots(ctx, o); err != nil {
		t.Fatal(err)
	}
	if machines["w-0"] != nil {
		t.Error("a worker has no etcd")
	}
	cp := machines["cp-1"]
	s := cp.scripts[0]
	for _, want := range []string{"systemd-analyze calendar '*-*-* 0/6:0:00'", "OnCalendar=*-*-* 0/6:0:00", "After=containerd.service",
		"crictl --runtime-endpoint unix:///var/run/containerd/containerd.sock", "keep=5", "systemctl restart tow-etcd-snapshot.timer"} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if strings.Count(cp.record, `"what": "etcd snapshots"`) != 1 || !strings.Contains(cp.record, `"kubernetes": true`) {
		t.Errorf("record %s", cp.record)
	}
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v\n%s", err, out)
	}

	// A new schedule: written again, recorded still once.
	o.Snapshots = &Snapshots{Calendar: "Mon..Fri *-*-* 2:30:00", Keep: 14}
	if err := SetSnapshots(ctx, o); err != nil {
		t.Fatal(err)
	}
	if s := cp.scripts[1]; !strings.Contains(s, "OnCalendar=Mon..Fri *-*-* 2:30:00") || !strings.Contains(s, "keep=14") || strings.Contains(s, prepare.RecordFile) {
		t.Errorf("rewritten without a second record:\n%s", s)
	}

	// Off: the timer goes, the snapshots stay.
	o.Snapshots = nil
	if err := SetSnapshots(ctx, o); err != nil {
		t.Fatal(err)
	}
	if s := cp.scripts[2]; s != removeSnapshotsScript || strings.Contains(s, SnapshotDir) {
		t.Errorf("removal:\n%s", s)
	}
	if out, err := exec.Command("sh", "-n", "-c", removeSnapshotsScript).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v\n%s", err, out)
	}

	// CRI-O's socket and unit.
	o.Runtime, o.Snapshots = runtimes.CRIO, &Snapshots{Calendar: "*-*-* 3:0:00", Keep: 1}
	if err := SetSnapshots(ctx, o); err != nil {
		t.Fatal(err)
	}
	if s := cp.scripts[3]; !strings.Contains(s, "/var/run/crio/crio.sock") || !strings.Contains(s, "After=crio.service") {
		t.Error("CRI-O's socket and unit")
	}
}

// The snapshot script, run in a shell (its paths moved into a temporary folder, crictl a
// stub): each run adds a snapshot, root-only, and keeps the newest; keep 0 keeps them all.
func TestSnapshotScriptRuns(t *testing.T) {
	dir := t.TempDir()
	etcd, snaps, bin := filepath.Join(dir, "etcd"), filepath.Join(dir, "snapshots"), filepath.Join(dir, "bin")
	os.MkdirAll(etcd, 0o755)
	os.MkdirAll(bin, 0o755)
	// crictl: "ps" names the etcd container; "exec ... snapshot save PATH" writes PATH.
	os.WriteFile(filepath.Join(bin, "crictl"), []byte(`#!/bin/sh
case "$3" in
ps) echo abc123 ;;
exec) for a; do last=$a; done; echo snapshot > "$last" ;;
esac
`), 0o755)
	// date: a new second each run, so names differ.
	os.WriteFile(filepath.Join(bin, "date"), []byte("#!/bin/sh\nn=$(cat "+dir+"/n 2>/dev/null || echo 0); n=$((n+1)); echo $n > "+dir+"/n; echo 2026111${n}T120000Z\n"), 0o755)
	run := func(keep int) string {
		t.Helper()
		script := strings.NewReplacer("/var/lib/etcd/", etcd+"/", SnapshotDir, snaps).Replace(snapshotScript(runtimes.Containerd, keep))
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	for range 3 {
		run(2)
	}
	files, _ := filepath.Glob(filepath.Join(snaps, "etcd-*.db"))
	if len(files) != 2 || !strings.HasSuffix(files[1], "20261113T120000Z.db") {
		t.Errorf("kept %v, want the newest 2", files)
	}
	if fi, _ := os.Stat(files[1]); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", fi.Mode().Perm())
	}
	if left, _ := filepath.Glob(filepath.Join(etcd, "*")); len(left) != 0 {
		t.Errorf("left in etcd's folder: %v", left)
	}
	path := run(0)
	files, _ = filepath.Glob(filepath.Join(snaps, "etcd-*.db"))
	if len(files) != 3 || !slices.Contains(files, path) {
		t.Errorf("keep 0 keeps all: %v, printed %s", files, path)
	}
}

func TestSnapshotNow(t *testing.T) {
	machines := map[string]*snapshotMachine{}
	o := snapshotOptions(machines)
	path, err := Snapshot(context.Background(), o, o.Nodes[0])
	if err != nil || path != SnapshotDir+"/etcd-cp-0-20261113T120000Z.db" {
		t.Errorf("path %q, %v", path, err)
	}
	if s := machines["cp-0"].scripts[0]; !strings.Contains(s, "keep=0") {
		t.Error("with no schedule, a snapshot taken now is never pruned")
	}
}
