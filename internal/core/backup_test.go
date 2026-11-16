package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
)

// useSnapshots fakes the control planes: each takes a snapshot unless it's in down, and
// gives it back as data (or, with corrupt, data that doesn't match its sha256).
func useSnapshots(t *testing.T, data string, corrupt bool, down ...string) *[]string {
	t.Helper()
	var took []string
	origTake, origFetch := takeSnapshot, fetchFile
	takeSnapshot = func(_ context.Context, cfg *config.Config, n providers.Node, _ string) (string, error) {
		for _, d := range down {
			if n.Name == d {
				return "", fmt.Errorf("%s: etcd isn't running on this machine", n.Name)
			}
		}
		took = append(took, cfg.Distribution()+" "+n.Name)
		return "/var/lib/rancher/k3s/server/db/snapshots/tow-" + n.Name + "-1763035200", nil
	}
	fetchFile = func(_ context.Context, n providers.Node, _, path string, w io.Writer) (string, error) {
		sum := sha256.Sum256([]byte(data))
		copied := data
		if corrupt {
			copied += "!"
		}
		_, err := io.WriteString(w, copied)
		return hex.EncodeToString(sum[:]), err
	}
	t.Cleanup(func() { takeSnapshot, fetchFile = origTake, origFetch })
	return &took
}

// withControlPlane adds a second control plane, cp2, to a running cluster's state.
func withControlPlane(t *testing.T, name string) {
	t.Helper()
	st, err := state.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	st.Outputs.Nodes = append([]providers.Node{{Name: "cp2", Host: "10.0.0.3", Role: config.RoleControlPlane}}, st.Outputs.Nodes...)
	if err := state.Save(name, *st); err != nil {
		t.Fatal(err)
	}
}

// A backup is taken on the first control plane that answers, copied here owner-only with
// what it is, and listed; it outlives the cluster's state.
func TestBackup(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	withControlPlane(t, "lab")
	took := useSnapshots(t, "etcd data", false, "cp2")

	file, err := NewOrchestrator(false).Backup(context.Background(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	if len(*took) != 1 || (*took)[0] != "k3s cp" {
		t.Errorf("taken on %v, want cp (cp2 is down)", *took)
	}
	if filepath.Dir(file) != state.BackupsDir("lab") || !strings.HasSuffix(file, ".db") {
		t.Errorf("copy at %s", file)
	}
	if data, _ := os.ReadFile(file); string(data) != "etcd data" {
		t.Errorf("copy holds %q", data)
	}
	for _, p := range []string{file, strings.TrimSuffix(file, ".db") + ".json"} {
		if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v, %v", p, err, fi)
		}
	}
	if fi, _ := os.Stat(state.BackupsDir("lab")); fi.Mode().Perm() != 0o700 {
		t.Errorf("folder mode %v", fi.Mode().Perm())
	}
	var meta map[string]any
	data, _ := os.ReadFile(strings.TrimSuffix(file, ".db") + ".json")
	if err := json.Unmarshal(data, &meta); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte("etcd data"))
	if meta["cluster"] != "lab" || meta["distribution"] != "k3s" || meta["kubernetes"] != "v1.37.2" || meta["node"] != "cp" ||
		meta["sha256"] != hex.EncodeToString(sum[:]) || meta["takenAt"] == nil || meta["snapshot"] == nil {
		t.Errorf("metadata %s", data)
	}

	// The state goes; the backups stay, and are listed.
	if err := state.Remove("lab"); err != nil {
		t.Fatal(err)
	}
	list, err := ListBackups("lab")
	if err != nil || len(list) != 1 || list[0].File != file || list[0].Size != int64(len("etcd data")) {
		t.Errorf("list %+v, %v", list, err)
	}
	if err := PrintBackups("lab"); err != nil {
		t.Error(err)
	}
}

// A backup waits for no other run on the cluster: it stops at once, taking nothing.
func TestBackupTakesTheLock(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	took := useSnapshots(t, "etcd data", false)
	unlock, err := state.Lock("lab")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if _, err := NewOrchestrator(false).Backup(context.Background(), "lab"); err == nil || !strings.Contains(err.Error(), "another tow run is changing cluster 'lab'") {
		t.Errorf("err = %v", err)
	}
	if len(*took) != 0 {
		t.Errorf("took %v", *took)
	}
}

// A copy that doesn't match its sha256 isn't kept; no control plane answering is an error
// naming why each failed.
func TestBackupFails(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	useSnapshots(t, "etcd data", true)
	if _, err := NewOrchestrator(false).Backup(context.Background(), "lab"); err == nil || !strings.Contains(err.Error(), "doesn't match it") {
		t.Errorf("err = %v", err)
	}
	if left, _ := os.ReadDir(state.BackupsDir("lab")); len(left) != 0 {
		t.Errorf("kept a bad copy: %v", left)
	}

	withControlPlane(t, "lab")
	useSnapshots(t, "etcd data", false, "cp", "cp2")
	_, err := NewOrchestrator(false).Backup(context.Background(), "lab")
	if err == nil || !strings.Contains(err.Error(), "no control plane of 'lab' took a snapshot") || !strings.Contains(err.Error(), "cp2: etcd isn't running") {
		t.Errorf("err = %v", err)
	}

	if _, err := NewOrchestrator(false).Backup(context.Background(), "nope"); err == nil || !strings.Contains(err.Error(), "no cluster named 'nope'") {
		t.Errorf("err = %v", err)
	}
	if err := NewOrchestrator(false).Apply(context.Background(), load(t, "local.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewOrchestrator(false).Backup(context.Background(), "dev"); err == nil || !strings.Contains(err.Error(), "local one (k3d)") {
		t.Errorf("err = %v", err)
	}
}

// What sudo prints before the script runs stays out of the sum.
func TestParseSum(t *testing.T) {
	sum, err := parseSum("sudo: unable to resolve host cp-0: Name or service not known\ntow-sha256 abc123\n")
	if err != nil || sum != "abc123" {
		t.Errorf("%q %v", sum, err)
	}
	if _, err := parseSum("sha256sum: x: No such file or directory\n"); err == nil {
		t.Error("output without a sum")
	}
}

// k3s's schedule is one of its settings: the control planes restart with it, one at a time.
// kubeadm's timer is put on, rewritten and taken off the control planes the cluster has.
func TestUpdateBackups(t *testing.T) {
	useFakes(t)
	useNodes(t)
	k3sLab := fmt.Sprintf(lab, "", "")
	running(t, parse(t, "lab", k3sLab))
	installs := useInstall(t, nil)
	withBackups := strings.Replace(k3sLab, "distribution: k3s", `distribution: k3s, backups: {schedule: "0 */6 * * *"}`, 1)
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), parse(t, "lab", withBackups)); err != nil {
		t.Fatalf("k3s: %v", err)
	}
	if len(*installs) != 1 || (*installs)[0].Rolling == nil || !strings.Contains(strings.Join(rendered((*installs)[0].ServerFlags), " "), "etcd-snapshot-schedule-cron=0 */6 * * *") {
		t.Errorf("k3s rolls its new schedule: %+v", *installs)
	}
	os.RemoveAll(state.Dir("lab"))

	var set []*kubeadm.Snapshots
	var on [][]string
	orig := setSnapshots
	setSnapshots = func(_ context.Context, o kubeadm.Options) error {
		set = append(set, o.Snapshots)
		var names []string
		for _, n := range o.Nodes {
			names = append(names, n.Name)
		}
		on = append(on, names)
		return nil
	}
	t.Cleanup(func() { setSnapshots = orig })
	kubeadmLab := strings.Replace(k3sLab, "distribution: k3s", "distribution: kubeadm", 1)
	os.RemoveAll(state.Dir("lab"))
	running(t, parse(t, "lab", kubeadmLab))
	steps := []struct {
		file string
		want *kubeadm.Snapshots
	}{
		{strings.Replace(kubeadmLab, "distribution: kubeadm", `distribution: kubeadm, backups: {schedule: "0 */6 * * *"}`, 1), &kubeadm.Snapshots{Calendar: "*-*-* 0/6:0:00", Keep: config.DefaultBackupsKeep}},
		{strings.Replace(kubeadmLab, "distribution: kubeadm", `distribution: kubeadm, backups: {schedule: "30 2 * * 1-5", keep: 14}`, 1), &kubeadm.Snapshots{Calendar: "Mon..Fri *-*-* 2:30:00", Keep: 14}},
		{kubeadmLab, nil},
	}
	for i, s := range steps {
		if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), parse(t, "lab", s.file)); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		got := set[len(set)-1]
		if len(set) != i+1 || (got == nil) != (s.want == nil) || (got != nil && *got != *s.want) {
			t.Errorf("step %d: timer %+v, want %+v", i, got, s.want)
		}
		if names := on[len(on)-1]; strings.Join(names, ",") != "cp,w1" {
			t.Errorf("step %d: on %v (SetSnapshots picks the control planes)", i, names)
		}
	}
	// Applied again unchanged: nothing to do.
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), parse(t, "lab", kubeadmLab)); err != nil || len(set) != 3 {
		t.Errorf("unchanged: %v, %d calls", err, len(set))
	}
}

// A dry run of a kubeadm backups change says what the timer does, and touches nothing.
func TestPlanBackups(t *testing.T) {
	cfg := func(b string) *config.Config {
		return parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: kubeadm"+b, 1))
	}
	prev := &state.ClusterState{Outputs: &providers.Result{}}
	var plan updatePlan
	planBackups(&plan, cfg(`, backups: {schedule: "0 3 * * *", keep: 7}`), cfg(""), prev)
	planBackups(&plan, cfg(""), cfg(`, backups: {schedule: "0 3 * * *"}`), prev)
	planBackups(&plan, cfg(`, backups: {schedule: "0 3 * * *"}`), cfg(`, backups: {schedule: "0 3 * * *"}`), prev)
	if len(plan.cluster) != 2 {
		t.Fatalf("%d steps, want 2 (the same schedule needs none)", len(plan.cluster))
	}
	if l := plan.cluster[0].lines[0]; l != `etcd snapshots on every control plane at "0 3 * * *" (systemd: *-*-* 3:0:00), the newest 7 kept in /var/lib/tow/snapshots` {
		t.Errorf("on: %s", l)
	}
	if l := plan.cluster[1].lines[0]; !strings.Contains(l, "taken off the control planes") || !strings.Contains(l, "the snapshots taken stay") {
		t.Errorf("off: %s", l)
	}
}
