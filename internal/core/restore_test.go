package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
)

// restoreLog records what a restore did, with fakes for every part that touches machines.
type restoreLog struct {
	calls []string
	k3s   []k3s.RestoreOptions
	kad   []kubeadm.RestoreOptions
}

func useRestore(t *testing.T) *restoreLog {
	t.Helper()
	l := &restoreLog{}
	origPut, origK3s, origCheck, origKad, origClean, origKubelets, origHealthy := putFile, restoreK3s, checkKubeadmRestore, restoreKubeadm, cleanupKubeadm, restartKubelets, controlPlanesHealthy
	putFile = func(_ context.Context, n providers.Node, _, file, local, _ string) error {
		data, err := os.ReadFile(local)
		l.calls = append(l.calls, fmt.Sprintf("put %s %s %q", n.Name, file, data))
		return err
	}
	restoreK3s = func(_ context.Context, o k3s.RestoreOptions) ([]string, error) {
		l.calls, l.k3s = append(l.calls, "k3s restore"), append(l.k3s, o)
		return []string{"cp-1: /var/lib/rancher/k3s/server/db.before-restore-x"}, nil
	}
	checkKubeadmRestore = func(context.Context, kubeadm.RestoreOptions) error {
		l.calls = append(l.calls, "kubeadm check")
		return nil
	}
	restoreKubeadm = func(_ context.Context, o kubeadm.RestoreOptions) ([]string, error) {
		l.calls, l.kad = append(l.calls, "kubeadm restore"), append(l.kad, o)
		return nil, nil
	}
	cleanupKubeadm = func(context.Context, kubeadm.RestoreOptions) { l.calls = append(l.calls, "kubeadm cleanup") }
	restartKubelets = func(_ context.Context, nodes []providers.Node, _ string) error {
		l.calls = append(l.calls, fmt.Sprintf("kubelets %d", len(nodes)))
		return nil
	}
	controlPlanesHealthy = func(context.Context, *config.Config, []providers.Node, string) error {
		l.calls = append(l.calls, "healthy")
		return nil
	}
	t.Cleanup(func() {
		putFile, restoreK3s, checkKubeadmRestore, restoreKubeadm, cleanupKubeadm, restartKubelets, controlPlanesHealthy = origPut, origK3s, origCheck, origKad, origClean, origKubelets, origHealthy
	})
	return l
}

// The newest backup by default, or the one named (its file, with or without .db).
func TestPickBackup(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	useSnapshots(t, "etcd data", false)
	first, _ := NewOrchestrator(false).Backup(context.Background(), "lab")
	second, _ := NewOrchestrator(false).Backup(context.Background(), "lab")
	if first == second {
		t.Fatal("two backups in the same second share a file")
	}
	for want, file := range map[string]string{"": second, filepath.Base(first): first, strings.TrimSuffix(filepath.Base(first), ".db"): first, first: first} {
		if b, err := pickBackup("lab", want); err != nil || b.File != file {
			t.Errorf("pick %q: %s, %v", want, b.File, err)
		}
	}
	if _, err := pickBackup("lab", "nope"); err == nil || !strings.Contains(err.Error(), "there are: "+filepath.Base(first)) {
		t.Errorf("unknown: %v", err)
	}
	if _, err := pickBackup("other", ""); err == nil || !strings.Contains(err.Error(), "no backup of 'other'") {
		t.Errorf("none: %v", err)
	}
}

// Only a snapshot of the running release, or the one taken right before upgrading to it.
func TestRestoreRefusals(t *testing.T) {
	cfg := parse(t, "lab", fmt.Sprintf(lab, "", ""))
	st := &state.ClusterState{Name: "lab", K8sVersion: "v1.37.2"}
	for _, tc := range []struct {
		b    BackupInfo
		want string
	}{
		{BackupInfo{Cluster: "lab", Distribution: "k3s", Kubernetes: "v1.37.2"}, ""},
		{BackupInfo{Cluster: "lab", Distribution: "k3s", Kubernetes: "v1.37.2+k3s1"}, ""},
		{BackupInfo{Cluster: "lab", Distribution: "k3s", Kubernetes: "v1.36.5", BeforeUpgradeTo: "v1.37.2"}, ""},
		{BackupInfo{Cluster: "lab", Distribution: "k3s", Kubernetes: "v1.36.5"}, "only a snapshot of v1.37.2+k3s1, which runs, or the one taken right before upgrading to it"},
		{BackupInfo{Cluster: "lab", Distribution: "k3s", Kubernetes: "v1.38.0"}, "newer than v1.37.2+k3s1"},
		{BackupInfo{Cluster: "prod", Distribution: "k3s", Kubernetes: "v1.37.2"}, "a snapshot of cluster 'prod'"},
		{BackupInfo{Cluster: "lab", Distribution: "kubeadm", Kubernetes: "v1.37.2"}, "it's a kubeadm snapshot"},
	} {
		got := restoreRefusals(tc.b, cfg, st)
		if (tc.want == "") != (len(got) == 0) || (tc.want != "" && !strings.Contains(strings.Join(got, "; "), tc.want)) {
			t.Errorf("%+v: %v, want %q", tc.b, got, tc.want)
		}
	}
}

// k3s: the plan, a safety backup first (unless --no-backup), the snapshot to the first
// control plane, k3s's restore with the file's settings, the nodes waited for. A dry run, or
// nobody to say yes, changes nothing; a copy that changed since it was taken is refused.
func TestRestoreK3s(t *testing.T) {
	useFakes(t)
	useNodes(t)
	l := useRestore(t)
	running(t, parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", `distribution: k3s, k3s: {server: {extraArgs: {data-dir: /data/k3s, etcd-s3: true}}}`, 1)))
	useSnapshots(t, "etcd data", false)
	taken, err := NewOrchestrator(false).Backup(context.Background(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := NewOrchestrator(true).Restore(ctx, "lab", ""); err != nil || len(l.calls) != 0 {
		t.Fatalf("dry run: %v, %v", err, l.calls)
	}
	if err := NewOrchestrator(false).Restore(ctx, "lab", ""); err == nil || !strings.Contains(err.Error(), "nothing was changed") || len(l.calls) != 0 {
		t.Fatalf("nobody to ask: %v, %v", err, l.calls)
	}

	time.Sleep(time.Millisecond)
	if err := NewOrchestrator(false).AssumeYes().Restore(ctx, "lab", ""); err != nil {
		t.Fatal(err)
	}
	remote := "/var/lib/tow/restore/" + filepath.Base(taken)
	if want := []string{fmt.Sprintf("put cp %s %q", remote, "etcd data"), "k3s restore"}; !slices.Equal(l.calls, want) {
		t.Errorf("calls %v, want %v", l.calls, want)
	}
	o := l.k3s[0]
	if o.Snapshot != remote || o.DataDir != "/data/k3s" || !o.S3 || len(o.ControlPlanes) != 1 || o.ControlPlanes[0].Name != "cp" || o.Stamp == "" {
		t.Errorf("options %+v", o)
	}
	if list, _ := ListBackups("lab"); len(list) != 2 {
		t.Errorf("a safety backup first: %d backups", len(list))
	}

	// --no-backup; the snapshot named.
	l.calls = nil
	if err := NewOrchestrator(false).AssumeYes().WithoutBackup().Restore(ctx, "lab", filepath.Base(taken)); err != nil {
		t.Fatal(err)
	}
	if list, _ := ListBackups("lab"); len(list) != 2 {
		t.Errorf("--no-backup took one: %d backups", len(list))
	}

	// A copy that changed since it was taken.
	os.WriteFile(taken, []byte("tampered"), 0o600)
	l.calls = nil
	if err := NewOrchestrator(false).AssumeYes().Restore(ctx, "lab", filepath.Base(taken)); err == nil || !strings.Contains(err.Error(), "doesn't match the sha256") || len(l.calls) != 0 {
		t.Errorf("tampered: %v, %v", err, l.calls)
	}
}

// kubeadm: the snapshot to every control plane, checked there, every member restored, the
// control planes checked, every kubelet restarted, the copies cleaned up.
func TestRestoreKubeadmFlow(t *testing.T) {
	useFakes(t)
	useNodes(t)
	l := useRestore(t)
	running(t, parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: kubeadm", 1)))
	withControlPlane(t, "lab")
	useSnapshots(t, "etcd data", false)
	if _, err := NewOrchestrator(false).Backup(context.Background(), "lab"); err != nil {
		t.Fatal(err)
	}
	if err := NewOrchestrator(false).AssumeYes().WithoutBackup().Restore(context.Background(), "lab", ""); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range l.calls {
		f := strings.Fields(c)
		got = append(got, strings.Join(f[:min(2, len(f))], " "))
	}
	want := []string{"put cp2", "put cp", "kubeadm check", "kubeadm restore", "healthy", "kubelets 3", "kubeadm cleanup"}
	if !slices.Equal(got, want) {
		t.Errorf("calls %v, want %v", got, want)
	}
	if o := l.kad[0]; len(o.ControlPlanes) != 2 || o.Runtime != "containerd" {
		t.Errorf("options %+v", o)
	}
}
