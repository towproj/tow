package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// restoreDir is where a snapshot is copied on the control planes for a restore.
const restoreDir = "/var/lib/tow/restore"

// Swappable so tests run without machines.
var (
	// putFile copies the local file to a root-only file on a machine, streamed (a snapshot
	// may be gigabytes), and checks its sha256 there.
	putFile = func(ctx context.Context, n providers.Node, known, file, local, sum string) error {
		f, err := os.Open(local)
		if err != nil {
			return err
		}
		defer f.Close()
		c, err := dialNode(ctx, n, known)
		if err != nil {
			return err
		}
		defer c.Close()
		write := "umask 077; install -d -m 0700 " + remote.Quote(path.Dir(file)) + " && cat > " + remote.Quote(file)
		if errOut, err := c.Stream(ctx, remote.AsRootCommand(write), f, io.Discard); err != nil {
			return fmt.Errorf("copy %s to %s: %w\n%s", path.Base(file), n.Label(), err, ui.LastLines(errOut, 5))
		}
		check := "printf '%s  %s\\n' " + remote.Quote(sum) + " " + remote.Quote(file) + " | sha256sum -c --quiet\n"
		if out, err := c.RunInput(ctx, remote.AsRoot, check); err != nil {
			return fmt.Errorf("the copy of %s on %s doesn't match it: %w\n%s", path.Base(file), n.Label(), err, ui.LastLines(out, 5))
		}
		return nil
	}
	restoreK3s          = k3s.Restore
	checkKubeadmRestore = kubeadm.CheckRestore
	restoreKubeadm      = kubeadm.Restore
	cleanupKubeadm      = kubeadm.CleanupRestore
	// restartKubelets restarts the kubelet on every node, as the docs advise after a restore.
	restartKubelets = func(ctx context.Context, nodes []providers.Node, known string) error {
		for _, n := range nodes {
			if _, err := runOn(ctx, n, known, "systemctl restart kubelet\n"); err != nil {
				return fmt.Errorf("restart the kubelet on %s: %w", n.Label(), err)
			}
		}
		return nil
	}
)

// pickBackup finds the backup to restore: the one named (its file, with or without .db),
// else the newest.
func pickBackup(name, want string) (BackupInfo, error) {
	list, err := ListBackups(name)
	if err != nil {
		return BackupInfo{}, err
	}
	if len(list) == 0 {
		return BackupInfo{}, fmt.Errorf("no backup of '%s' on this machine ('tow backup --name %s' takes one)", name, name)
	}
	if want == "" {
		return list[len(list)-1], nil
	}
	var names []string
	for _, b := range list {
		base := filepath.Base(b.File)
		if want == b.File || want == base || want == strings.TrimSuffix(base, ".db") {
			return b, nil
		}
		names = append(names, base)
	}
	return BackupInfo{}, fmt.Errorf("no backup %q of '%s' on this machine; there are: %s", want, name, strings.Join(names, ", "))
}

// restoreRefusals says why a backup can't be restored on the cluster as it runs: only a
// snapshot of the release it runs, or the one taken right before upgrading to it.
func restoreRefusals(b BackupInfo, cfg *config.Config, st *state.ClusterState) []string {
	d := cfg.Distribution()
	var out []string
	if b.Cluster != st.Name {
		out = append(out, fmt.Sprintf("it's a snapshot of cluster '%s'", b.Cluster))
	}
	if b.Distribution != d {
		out = append(out, fmt.Sprintf("it's a %s snapshot; '%s' runs %s", b.Distribution, st.Name, d))
	}
	running, taken := nodeRelease(d, st.K8sVersion), nodeRelease(d, b.Kubernetes)
	switch r, ok1 := parseRelease(running); {
	case taken == running:
	case b.BeforeUpgradeTo != "" && nodeRelease(d, b.BeforeUpgradeTo) == running:
	default:
		t, ok2 := parseRelease(taken)
		if ok1 && ok2 && t.compare(r) > 0 {
			out = append(out, fmt.Sprintf("it was taken on %s, newer than %s, which runs: the cluster's API can't read what a newer one wrote", taken, running))
		} else {
			out = append(out, fmt.Sprintf("it was taken on %s; only a snapshot of %s, which runs, or the one taken right before upgrading to it, can be restored", taken, running))
		}
	}
	return out
}

// Restore puts a running cluster's etcd back to one of its backups on this machine (the
// newest, unless snapshot names one), on the same machines: everything the API knew comes
// back as it was. A safety backup of the state it leaves is taken first, unless
// WithoutBackup. The plan is shown and confirmed (--yes; --dry-run only shows it).
func (o *Orchestrator) Restore(ctx context.Context, name, snapshot string) error {
	st, err := state.Load(name)
	if err != nil {
		return fmt.Errorf("no cluster named '%s' (run 'tow status' to list them)", name)
	}
	if st.Mode == config.ModeLocal {
		return fmt.Errorf("cluster '%s' is a local one (k3d): it has no etcd snapshots to restore", name)
	}
	if st.Outputs == nil || st.File == "" || st.Status != state.StatusReady {
		return fmt.Errorf("cluster '%s' isn't running yet ('tow apply' finishes it first)", name)
	}
	unlock, err := state.Lock(name)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := config.Restore(st.File)
	if err != nil {
		return err
	}
	b, err := pickBackup(name, snapshot)
	if err != nil {
		return err
	}
	if refused := restoreRefusals(b, cfg, st); len(refused) > 0 {
		return fmt.Errorf("%s can't be restored on '%s'; nothing was changed:\n  - %s", filepath.Base(b.File), name, strings.Join(refused, "\n  - "))
	}
	if sum, err := fileSum(b.File); err != nil {
		return err
	} else if sum != b.SHA256 {
		return fmt.Errorf("%s doesn't match the sha256 its .json recorded: it changed since it was taken; nothing was changed", b.File)
	}

	d := cfg.Distribution()
	known := filepath.Join(state.Dir(name), "known_hosts")
	var cps []providers.Node
	for _, n := range st.Outputs.Nodes {
		if n.Role == config.RoleControlPlane {
			cps = append(cps, n)
		}
	}
	if len(cps) == 0 {
		return fmt.Errorf("cluster '%s' has no control plane in tow's state", name)
	}
	onNode := path.Join(restoreDir, filepath.Base(b.File))
	stamp := time.Now().UTC().Format("20060102T150405Z")

	// The plan.
	out := func(l string) { fmt.Println(l) }
	if o.dryRun {
		out = ui.DryRun
	}
	upgradeNote := ""
	if b.BeforeUpgradeTo != "" {
		upgradeNote = fmt.Sprintf(", right before the upgrade to %s; the release stays %s", b.BeforeUpgradeTo, st.K8sVersion)
	}
	out(fmt.Sprintf("Restore cluster '%s' to its snapshot of %s (Kubernetes %s%s, %s, taken on %s):", name,
		b.TakenAt.Local().Format("2006-01-02 15:04:05"), b.Kubernetes, upgradeNote, humanSize(b.Size), b.Node))
	out("  - everything the cluster's API knew comes back as it was then: every change since is lost; volumes' data isn't in etcd and stays as it is")
	if o.noBackup {
		out("  - no safety backup first (--no-backup)")
	} else {
		out("  - a safety backup first, of the cluster as it is now; if no control plane can take one, the restore goes on without it")
	}
	switch d {
	case config.DistributionK3s:
		out(fmt.Sprintf("  - the snapshot copied to %s, its sha256 checked there", cps[0].Label()))
		out(fmt.Sprintf("  - k3s stopped on every control plane, then 'k3s server --cluster-reset' with the snapshot on %s, which starts again first", cps[0].Label()))
		if len(cps) > 1 {
			out("  - then each other control plane, one at a time: its etcd data moved aside, and it joins the restored cluster")
		}
		out("  - the workers keep running")
	default:
		out("  - the snapshot copied to every control plane, its sha256 checked there; etcdutl read from each one's etcd, and the snapshot's etcd release checked against it")
		out("  - etcd and the API servers stopped on every control plane (their static pods set aside), every etcd member restored from the snapshot (its old data moved aside), then started again")
		out("  - the kubelet restarted on every node (Kubernetes' docs advise it); the workers' pods keep running")
	}
	out("  - Nodes the snapshot has that the cluster no longer has come back NotReady: named at the end, for you to delete")
	if o.dryRun {
		return nil
	}
	if !o.yes {
		if !ui.Interactive() {
			ui.Hint("Nobody to ask here: run it with --yes to restore")
			return errors.New("nothing was changed")
		}
		if !ui.Confirm("Restore it?") {
			return errors.New("nothing was changed")
		}
	}

	if !o.noBackup {
		if _, err := o.backup(ctx, name, ""); err != nil {
			ui.Warn(fmt.Sprintf("no safety backup (%v): restoring without one", firstLine(err.Error())))
		}
	}
	again := fmt.Sprintf("\nRun 'tow restore --name %s --snapshot %s' again to finish it.", name, filepath.Base(b.File))
	var aside []string
	switch d {
	case config.DistributionK3s:
		if err := ui.StepRun(fmt.Sprintf("Copying the snapshot to %s", cps[0].Label()), func() error {
			return putFile(ctx, cps[0], known, onNode, b.File, b.SHA256)
		}); err != nil {
			return err
		}
		if aside, err = restoreK3s(ctx, k3s.RestoreOptions{ControlPlanes: cps, KnownHosts: known, Snapshot: onNode,
			DataDir: k3s.DataDir(cfg), S3: k3s.S3(cfg), Stamp: stamp}); err != nil {
			return fmt.Errorf("%w%s", err, again)
		}
	default:
		ro := kubeadm.RestoreOptions{Runtime: cfg.Spec.Kubernetes.ContainerRuntime, ControlPlanes: cps, KnownHosts: known, Snapshot: onNode, Stamp: stamp}
		if err := ui.StepRun(fmt.Sprintf("Copying the snapshot to %s", ui.Plural(len(cps), "control plane")), func() error {
			for _, n := range cps {
				if err := putFile(ctx, n, known, onNode, b.File, b.SHA256); err != nil {
					return err
				}
			}
			return checkKubeadmRestore(ctx, ro)
		}); err != nil {
			return err
		}
		if aside, err = restoreKubeadm(ctx, ro); err != nil {
			return fmt.Errorf("%w%s", err, again)
		}
		if err := ui.StepRun("Checking the control planes (the API ready, etcd healthy)", func() error {
			return controlPlanesHealthy(ctx, cfg, cps, known)
		}); err != nil {
			return fmt.Errorf("%w%s", err, again)
		}
		if err := ui.StepRun("Restarting the kubelet on every node", func() error {
			return restartKubelets(ctx, st.Outputs.Nodes, known)
		}); err != nil {
			return err
		}
		cleanupKubeadm(ctx, ro)
	}

	// The nodes: the cluster's own Ready again; those only the snapshot had, named.
	var names []string
	for _, n := range st.Outputs.Nodes {
		names = append(names, n.Name)
	}
	if err := ui.StepRun("Waiting from this machine for every node to be Ready", func() error {
		return waitReady(ctx, st.Outputs.Kubeconfig, names)
	}); err != nil {
		return err
	}
	if live, err := liveNodes(ctx, st.Outputs.Kubeconfig); err == nil {
		var extra []string
		for _, n := range live {
			if !slices.Contains(names, n.Name) {
				extra = append(extra, n.Name)
			}
		}
		if len(extra) > 0 {
			ui.Warn(fmt.Sprintf("the snapshot has Nodes this cluster no longer has (or that were joined outside tow): %s; delete those that are gone: kubectl delete node <name>", strings.Join(extra, ", ")))
		}
	}
	ui.Info(fmt.Sprintf("Cluster '%s' is back to its snapshot of %s", name, b.TakenAt.Local().Format("2006-01-02 15:04:05")))
	if len(aside) > 0 {
		ui.Info("The old etcd data stays, moved aside; delete it once the cluster runs well:\n  " + strings.Join(aside, "\n  "))
	}
	ui.Hint("Run 'tow apply' with the cluster's file: the snapshot holds the add-ons and charts as they were, and apply puts back what the file says")
	return nil
}

// fileSum is a local file's sha256, read in pieces.
func fileSum(name string) (string, error) {
	f, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
