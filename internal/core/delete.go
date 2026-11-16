package core

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// resetDoc tells how to undo by hand what tow changed on the user's own machines.
const resetDoc = "docs/reset-machines.md"

// undoOptions reach the cluster's machines as apply did: the nodes it found (with their
// names), else the file's, with the host keys pinned then.
func undoOptions(st *state.ClusterState) prepare.UndoOptions {
	o := prepare.UndoOptions{Cluster: st.Name, KnownHosts: filepath.Join(state.Dir(st.Name), "known_hosts")}
	switch {
	case st.Outputs != nil && len(st.Outputs.Nodes) > 0:
		o.Nodes = st.Outputs.Nodes
	case st.Spec != nil:
		o.Nodes = st.Spec.Nodes
	}
	return o
}

// forgetCluster forgets a cluster on the user's own machines without touching them: for a
// machine that's gone for good (delete --forget).
func (o *Orchestrator) forgetCluster(st *state.ClusterState, confirm func(summary string) bool) error {
	n := 0
	if st.Spec != nil {
		n = len(st.Spec.Nodes)
	}
	if !confirm(fmt.Sprintf("tow will forget cluster '%s' without touching your %s: Kubernetes keeps running on them, and what tow changed there stays, listed in %s on each (%s says how to undo it by hand)",
		st.Name, ui.Plural(n, "machine"), prepare.RecordFile, resetDoc)) {
		return errors.New("delete cancelled")
	}
	removeContext(st)
	if err := state.Remove(st.Name); err != nil {
		return fmt.Errorf("the cluster's state could not be removed: %w", err)
	}
	ui.Info(fmt.Sprintf("Cluster '%s' forgotten; your machines weren't touched", st.Name))
	backupsLeft(st.Name, nil)
	return nil
}

// backupsLeft says where a deleted cluster's backups stay, Secrets in them: the copies on
// this machine, and the snapshots kubeadm's timer took on the user's own machines.
func backupsLeft(name string, left []prepare.Left) {
	if b, _ := ListBackups(name); len(b) > 0 {
		what := "Its backup on this machine stays in %s ('tow backup --name %s --list'); delete it once you don't need it: it holds the cluster's Secrets"
		if len(b) > 1 {
			what = fmt.Sprintf("Its %d backups on this machine stay in %%s ('tow backup --name %%s --list'); delete them once you don't need them: they hold the cluster's Secrets", len(b))
		}
		ui.Info(fmt.Sprintf(what, state.BackupsDir(name), name))
	}
	var on []string
	for _, l := range left {
		if slices.ContainsFunc(l.Kubernetes, func(r prepare.Record) bool { return r.What == kubeadm.SnapshotsRecord }) {
			on = append(on, l.Node.Label())
		}
	}
	if len(on) > 0 {
		ui.Info(fmt.Sprintf("The etcd snapshots taken on %s stay in %s there; delete them once you don't need them: they hold the cluster's Secrets",
			strings.Join(on, ", "), kubeadm.SnapshotDir))
	}
}

// describeMachines is what delete will do on the user's own machines, machine by machine,
// from what their records say the cluster left there.
func describeMachines(st *state.ClusterState, left []prepare.Left, reset bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This removes cluster '%s' from your %s", st.Name, ui.Plural(len(left), "machine"))
	if !toUndo(left, true) {
		b.WriteString(": tow changed nothing on them, so it only forgets the cluster")
		return b.String()
	}
	if toUndo(left, false) {
		b.WriteString(".\nKubernetes goes:")
		for _, l := range left {
			if len(l.Kubernetes) > 0 {
				var what []string
				for _, r := range l.Kubernetes {
					what = append(what, r.What+byHand(r))
				}
				fmt.Fprintf(&b, "\n  %s: %s", l.Node.Label(), strings.Join(what, ", "))
			}
		}
	}
	if preparationLeft(left) {
		if reset {
			b.WriteString("\nWhat tow prepared on them is undone too (--reset):")
		} else {
			fmt.Fprintf(&b, "\nWhat tow prepared on them stays (--reset undoes it too; afterwards, only by hand: %s):", resetDoc)
		}
		for _, l := range left {
			if len(l.Preparation) == 0 {
				continue
			}
			fmt.Fprintf(&b, "\n  %s:", l.Node.Label())
			for _, r := range l.Preparation {
				fmt.Fprintf(&b, "\n    %s: %s%s", r.What, r.Detail, byHand(r))
			}
		}
	}
	return b.String()
}

// byHand marks a record tow can't undo itself (no revert script): the user does.
func byHand(r prepare.Record) string {
	if r.Revert != "" {
		return ""
	}
	return " (undo it yourself: " + r.Undo + ")"
}

func preparationLeft(left []prepare.Left) bool {
	for _, l := range left {
		if len(l.Preparation) > 0 {
			return true
		}
	}
	return false
}

// toUndo reports whether there's anything to undo: Kubernetes, and the preparation too when
// preparation is set.
func toUndo(left []prepare.Left, preparation bool) bool {
	for _, l := range left {
		if len(l.Kubernetes) > 0 || preparation && len(l.Preparation) > 0 {
			return true
		}
	}
	return false
}

func undoLabel(machines int, reset bool) string {
	if reset {
		return fmt.Sprintf("Removing Kubernetes from %s and undoing their preparation", ui.Plural(machines, "machine"))
	}
	return "Removing Kubernetes from " + ui.Plural(machines, "machine")
}

// afterUndo says what's left on the machines once delete is done: what the user must undo
// by hand, and what lasts until a reboot.
func afterUndo(st *state.ClusterState, left []prepare.Left, reset bool) {
	var manual []string
	modules, kubeadm := false, false
	for _, l := range left {
		undone := l.Kubernetes
		if reset {
			undone = append(undone, l.Preparation...)
		}
		for _, r := range prepare.Manual(undone) {
			manual = append(manual, fmt.Sprintf("  %s: %s: %s", l.Node.Label(), r.What, r.Undo))
		}
		for _, r := range undone {
			if r.Revert != "" && (r.What == "kernel modules" || r.What == "kernel settings") {
				modules = true
			}
		}
		kubeadm = kubeadm || len(l.Kubernetes) > 0 && st.Spec != nil && st.Spec.Engine == config.DistributionKubeadm
	}
	if len(manual) > 0 {
		ui.Warn("tow recorded these changes without a way to undo them itself; undo them on each machine:\n" + strings.Join(manual, "\n"))
	}
	if modules {
		ui.Info("The kernel modules and settings tow set stay in effect until the machines restart")
	}
	if kubeadm {
		ui.Info("kube-proxy's network rules stay until the machines restart; kubeadm's docs say they're harmless")
	}
	if !reset && preparationLeft(left) {
		ui.Info(fmt.Sprintf("What tow prepared stays on the machines, listed in %s on each; %s says how to undo it by hand", prepare.RecordFile, resetDoc))
	}
}
