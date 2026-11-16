package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/state"
)

// machinesLab is a k3s cluster on the user's own machines (examples/homelab.yaml: a control
// plane and two workers, local-path storage) whose records say what it left there.
type machinesLab struct {
	fake    *fakeProvider
	left    []prepare.Left
	readErr error
	undoErr error
	undone  []bool // each undo's preparation flag
	drivers []string
	vols    []kubeapi.Volume
}

func useMachinesLab(t *testing.T) *machinesLab {
	t.Helper()
	_, fake := useFakes(t)
	if err := NewOrchestrator(false).Apply(context.Background(), load(t, "homelab.yaml")); err != nil {
		t.Fatal(err)
	}
	l := &machinesLab{fake: fake}
	k3s := prepare.Record{Cluster: "homelab", What: "k3s (control plane)", Undo: "sudo k3s-uninstall.sh", Revert: "k3s-uninstall.sh", Kubernetes: true}
	pkgs := prepare.Record{Cluster: "homelab", What: "packages", Detail: "installed conntrack", Undo: "sudo apt-get remove conntrack", Revert: "apt-get remove conntrack"}
	old := prepare.Record{Cluster: "homelab", What: "firewall", Detail: "ufw rules", Undo: "delete each rule marked 'tow:homelab'"} // from before revert scripts
	st, _ := state.Load("homelab")
	for _, n := range st.Spec.Nodes {
		l.left = append(l.left, prepare.Left{Node: n, Kubernetes: []prepare.Record{k3s}, Preparation: []prepare.Record{pkgs, old}})
	}

	origRead, origUndo, origList := readLeft, undoMachines, cloudVolumes
	readLeft = func(_ context.Context, o prepare.UndoOptions) ([]prepare.Left, error) {
		if o.Cluster != "homelab" || len(o.Nodes) != 3 || !strings.HasSuffix(o.KnownHosts, "/homelab/known_hosts") {
			t.Errorf("read options %+v", o)
		}
		fake.calls = append(fake.calls, "read")
		return l.left, l.readErr
	}
	undoMachines = func(_ context.Context, _ prepare.UndoOptions, preparation bool) error {
		fake.calls = append(fake.calls, "undo")
		l.undone = append(l.undone, preparation)
		return l.undoErr
	}
	cloudVolumes = func(_ context.Context, _ string, drivers []string) ([]kubeapi.Volume, error) {
		l.drivers = drivers
		return l.vols, nil
	}
	t.Cleanup(func() { readLeft, undoMachines, cloudVolumes = origRead, origUndo, origList })
	fake.calls = nil
	return l
}

// delete on the user's own machines removes Kubernetes, and keeps the preparation unless
// asked: the summary lists both, machine by machine, and what tow can't undo itself.
func TestBYOHDeleteRemovesKubernetes(t *testing.T) {
	l := useMachinesLab(t)
	var summary string
	if err := NewOrchestrator(false).Delete(context.Background(), "homelab", func(s string) bool { summary = s; return true }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.fake.calls, []string{"preflight", "read", "undo", "teardown"}) || !slices.Equal(l.undone, []bool{false}) {
		t.Errorf("calls %v, undone %v", l.fake.calls, l.undone)
	}
	for _, want := range []string{
		"This removes cluster 'homelab' from your 3 machines.\nKubernetes goes:\n  192.168.1.10: k3s (control plane)",
		"What tow prepared on them stays (--reset undoes it too; afterwards, only by hand: docs/reset-machines.md):\n  192.168.1.10:\n    packages: installed conntrack",
		"firewall: ufw rules (undo it yourself: delete each rule marked 'tow:homelab')",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary lacks %q:\n%s", want, summary)
		}
	}
	if state.Exists("homelab") {
		t.Error("state must go")
	}
	// Its local-path volumes keep their data on the machines: they count.
	if !slices.Contains(l.drivers, "rancher.io/local-path") {
		t.Errorf("drivers listed: %v", l.drivers)
	}
}

func TestBYOHDeleteReset(t *testing.T) {
	l := useMachinesLab(t)
	var summary string
	if err := NewOrchestrator(false).WithReset().Delete(context.Background(), "homelab", func(s string) bool { summary = s; return true }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.undone, []bool{true}) || !strings.Contains(summary, "What tow prepared on them is undone too (--reset):") {
		t.Errorf("undone %v, summary:\n%s", l.undone, summary)
	}
}

// Without --reset or --yes, a terminal is asked; only when there is preparation to undo.
func TestBYOHDeleteAsksAboutPreparation(t *testing.T) {
	for _, answer := range []bool{true, false} {
		l := useMachinesLab(t)
		var asked []string
		ask := func(q string) bool { asked = append(asked, q); return answer }
		if err := NewOrchestrator(false).AskReset(ask).Delete(context.Background(), "homelab", func(string) bool { return true }); err != nil {
			t.Fatal(err)
		}
		if len(asked) != 1 || !slices.Equal(l.undone, []bool{answer}) {
			t.Errorf("answer %v: asked %q, undone %v", answer, asked, l.undone)
		}
	}

	l := useMachinesLab(t)
	for i := range l.left {
		l.left[i].Preparation = nil
	}
	ask := func(string) bool { t.Error("nothing prepared: nothing to ask"); return true }
	if err := NewOrchestrator(false).AskReset(ask).Delete(context.Background(), "homelab", func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
}

// A machine that can't be read stops everything before any change; one that fails to undo
// keeps the state, so delete can be run again. --forget is the way out for a machine that's
// gone for good.
func TestBYOHDeleteFailures(t *testing.T) {
	yes := func(string) bool { return true }
	l := useMachinesLab(t)
	l.readErr = errors.New("could not read what tow changed on 192.168.1.12: dial tcp: no route to host")
	err := NewOrchestrator(false).Delete(context.Background(), "homelab", yes)
	if err == nil || !strings.Contains(err.Error(), "no route to host") || !strings.Contains(err.Error(), "Nothing was changed") || !strings.Contains(err.Error(), "--forget") {
		t.Errorf("err = %v", err)
	}
	if slices.Contains(l.fake.calls, "undo") || !state.Exists("homelab") {
		t.Errorf("calls %v", l.fake.calls)
	}

	l.readErr, l.undoErr = nil, errors.New("could not undo what tow changed on 192.168.1.11: undo k3s (worker): exit status 1")
	err = NewOrchestrator(false).Delete(context.Background(), "homelab", yes)
	if err == nil || !strings.Contains(err.Error(), "State for 'homelab' was kept") || !strings.Contains(err.Error(), "--forget") {
		t.Errorf("err = %v", err)
	}
	if slices.Contains(l.fake.calls, "teardown") || !state.Exists("homelab") {
		t.Errorf("calls %v", l.fake.calls)
	}

	// --forget touches no machine.
	l.fake.calls = nil
	var summary string
	if err := NewOrchestrator(false).Forget().Delete(context.Background(), "homelab", func(s string) bool { summary = s; return true }); err != nil {
		t.Fatal(err)
	}
	if len(l.fake.calls) != 0 || state.Exists("homelab") || !strings.Contains(summary, "without touching your 3 machines") {
		t.Errorf("calls %v, summary %q", l.fake.calls, summary)
	}
}

// Volumes on the machines' own disks are kept unless -v, as cloud volumes are.
func TestBYOHDeleteLocalVolumes(t *testing.T) {
	l := useMachinesLab(t)
	l.vols = []kubeapi.Volume{{Name: "pvc-1", Claim: "shop/data", Size: "1Gi", Driver: "rancher.io/local-path"}}
	err := NewOrchestrator(false).Delete(context.Background(), "homelab", func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "with data on your machines' disks") || !strings.Contains(err.Error(), "pvc-1") {
		t.Errorf("err = %v", err)
	}
	if slices.Contains(l.fake.calls, "undo") {
		t.Errorf("calls %v", l.fake.calls)
	}
}

// --reset and --forget are only for the user's own machines.
func TestResetAndForgetOnlyOnYourMachines(t *testing.T) {
	useFakes(t)
	if err := NewOrchestrator(false).Apply(context.Background(), load(t, "local.yaml")); err != nil {
		t.Fatal(err)
	}
	for _, o := range []*Orchestrator{NewOrchestrator(false).WithReset(), NewOrchestrator(false).Forget()} {
		if err := o.Delete(context.Background(), "dev", func(string) bool { return true }); err == nil || !strings.Contains(err.Error(), "for clusters on your own machines") {
			t.Errorf("err = %v", err)
		}
	}
}
