package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// BackupInfo is what a backup on this machine is: written beside it as JSON.
type BackupInfo struct {
	Cluster      string    `json:"cluster"`
	Distribution string    `json:"distribution"`
	Kubernetes   string    `json:"kubernetes"`
	Node         string    `json:"node"`     // the control plane it was taken on
	Snapshot     string    `json:"snapshot"` // where that machine keeps it
	TakenAt      time.Time `json:"takenAt"`
	SHA256       string    `json:"sha256"`
	Size         int64     `json:"size"`
	// BeforeUpgradeTo: taken by apply right before upgrading to this release, so it may be
	// restored on it too.
	BeforeUpgradeTo string `json:"beforeUpgradeTo,omitempty"`
	File            string `json:"-"` // the copy on this machine
}

// secretsWarning goes with every backup: etcd holds the cluster's Secrets.
const secretsWarning = "a snapshot holds the cluster's Secrets, in plain text unless they're encrypted at rest (k3s: secrets-encryption; kubeadm: an EncryptionConfiguration): keep it as safe as the cluster's credentials"

// Swappable so tests run without machines.
var (
	// takeSnapshot takes a snapshot now on a control plane, as the cluster's distribution
	// does, and returns where that machine keeps it.
	takeSnapshot = func(ctx context.Context, cfg *config.Config, n providers.Node, knownHosts string) (string, error) {
		if cfg.Distribution() == config.DistributionK3s {
			return k3s.Snapshot(ctx, n, knownHosts, k3s.SnapshotDir(cfg), nil)
		}
		o := kubeadmOptions(cfg)
		o.KnownHosts = knownHosts
		return kubeadm.Snapshot(ctx, *o, n)
	}
	// fetchFile copies a root-only file on a machine into w, streamed (a snapshot may be
	// gigabytes), and returns its sha256 there.
	fetchFile = func(ctx context.Context, n providers.Node, knownHosts, path string, w io.Writer) (string, error) {
		c, err := dialNode(ctx, n, knownHosts)
		if err != nil {
			return "", err
		}
		defer c.Close()
		out, err := c.RunInput(ctx, remote.AsRoot, sumScript(path))
		if err != nil {
			return "", fmt.Errorf("read %s's sha256 on %s: %w\n%s", path, n.Label(), err, ui.LastLines(out, 5))
		}
		sum, err := parseSum(out)
		if err != nil {
			return "", err
		}
		if errOut, err := c.Stream(ctx, remote.AsRootCommand("cat -- "+remote.Quote(path)), nil, w); err != nil {
			return "", fmt.Errorf("copy %s from %s: %w\n%s", path, n.Label(), err, ui.LastLines(errOut, 5))
		}
		return sum, nil
	}
)

// dialNode connects to a machine over SSH.
func dialNode(ctx context.Context, n providers.Node, knownHosts string) (*remote.Client, error) {
	return remote.Dial(ctx, remote.Target{Host: n.Host, Port: n.Port, User: n.User, KeyPath: n.KeyPath}, knownHosts)
}

// sumScript prints a file's sha256 after a marker: what sudo may print first (a host name it
// can't resolve) stays out of it.
func sumScript(path string) string {
	return "set -e\nsum=$(sha256sum -- " + remote.Quote(path) + ")\necho \"tow-sha256 ${sum%% *}\"\n"
}

func parseSum(out string) (string, error) {
	for l := range strings.SplitSeq(out, "\n") {
		if sum, ok := strings.CutPrefix(l, "tow-sha256 "); ok && strings.TrimSpace(sum) != "" {
			return strings.TrimSpace(sum), nil
		}
	}
	return "", fmt.Errorf("unexpected output: %s", ui.LastLines(out, 3))
}

// Backup takes an etcd snapshot now on the cluster's first control plane that answers, and
// keeps a copy in state.BackupsDir, with what it is. It returns the copy's path.
// It takes the cluster's lock: a snapshot taken while an apply or a restore changes the
// cluster could catch it halfway.
func (o *Orchestrator) Backup(ctx context.Context, name string) (string, error) {
	if !state.Exists(name) {
		return "", fmt.Errorf("no cluster named '%s' (run 'tow status' to list them)", name)
	}
	unlock, err := state.Lock(name)
	if err != nil {
		return "", err
	}
	defer unlock()
	return o.backup(ctx, name, "")
}

// backup is Backup without the lock, for the runs that already hold it (an upgrade's
// snapshot, a restore's safety backup); beforeUpgradeTo names the release an upgrade is
// about to install.
func (o *Orchestrator) backup(ctx context.Context, name, beforeUpgradeTo string) (string, error) {
	st, err := state.Load(name)
	if err != nil {
		return "", fmt.Errorf("no cluster named '%s' (run 'tow status' to list them)", name)
	}
	if st.Mode == config.ModeLocal {
		return "", fmt.Errorf("cluster '%s' is a local one (k3d): tow takes no etcd snapshots there", name)
	}
	if st.Outputs == nil || st.File == "" || st.Status != state.StatusReady {
		return "", fmt.Errorf("cluster '%s' isn't running yet ('tow apply' finishes it first)", name)
	}
	cfg, err := config.Restore(st.File)
	if err != nil {
		return "", err
	}
	known := filepath.Join(state.Dir(name), "known_hosts")
	var cps []providers.Node
	for _, n := range st.Outputs.Nodes {
		if n.Role == config.RoleControlPlane {
			cps = append(cps, n)
		}
	}
	var failed []string
	for _, n := range cps {
		done := ui.Step(fmt.Sprintf("Taking an etcd snapshot on %s", n.Label()))
		at := time.Now().UTC()
		path, err := takeSnapshot(ctx, cfg, n, known)
		if err != nil {
			fmt.Println("failed")
			failed = append(failed, err.Error())
			continue
		}
		done()
		done = ui.Step("Copying it to this machine")
		info := BackupInfo{Cluster: name, Distribution: cfg.Distribution(), Kubernetes: st.K8sVersion, Node: n.Name, Snapshot: path,
			TakenAt: at, BeforeUpgradeTo: beforeUpgradeTo}
		file, err := saveBackup(&info, func(w io.Writer) (string, error) { return fetchFile(ctx, n, known, path, w) })
		if err != nil {
			fmt.Println("failed")
			return "", err
		}
		done()
		ui.Info(fmt.Sprintf("Backup of '%s' (Kubernetes %s, %s) in %s", name, st.K8sVersion, humanSize(info.Size), file))
		ui.Warn(secretsWarning)
		return file, nil
	}
	return "", fmt.Errorf("no control plane of '%s' took a snapshot:\n  - %s", name, strings.Join(failed, "\n  - "))
}

// saveBackup writes a snapshot, which fetch streams into w and whose sha256 on its machine
// it returns, and what it is in the cluster's backups folder, owner-only. The copy is
// checked against that sha256 before it gets its name; info gets its sum and size.
func saveBackup(info *BackupInfo, fetch func(w io.Writer) (string, error)) (string, error) {
	dir := state.BackupsDir(info.Cluster)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".copy-*") // CreateTemp makes it 0600
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name()) // nothing once renamed
	h := sha256.New()
	sum, err := fetch(io.MultiWriter(tmp, h))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if local := hex.EncodeToString(h.Sum(nil)); local != sum {
		return "", fmt.Errorf("the copy of %s doesn't match it (sha256 %s on %s, %s here): try again", info.Snapshot, sum, info.Node, local)
	}
	fi, err := os.Stat(tmp.Name())
	if err != nil {
		return "", err
	}
	info.SHA256, info.Size = sum, fi.Size()
	// Named by the second it was taken; another one that second gets a number: a backup never
	// replaces one.
	base := filepath.Join(dir, fmt.Sprintf("%s-%s", info.Cluster, info.TakenAt.Format("20060102T150405Z")))
	for i := 2; ; i++ {
		if _, err := os.Stat(base + ".db"); errors.Is(err, fs.ErrNotExist) {
			break
		}
		base = filepath.Join(dir, fmt.Sprintf("%s-%s-%d", info.Cluster, info.TakenAt.Format("20060102T150405Z"), i))
	}
	if err := os.Rename(tmp.Name(), base+".db"); err != nil {
		return "", err
	}
	meta, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(base+".json", append(meta, '\n'), 0o600); err != nil {
		return "", err
	}
	return base + ".db", nil
}

// ListBackups lists a cluster's backups on this machine, oldest first. It needs no state:
// they outlive the cluster.
func ListBackups(name string) ([]BackupInfo, error) {
	metas, err := filepath.Glob(filepath.Join(state.BackupsDir(name), "*.json"))
	if err != nil {
		return nil, err
	}
	var out []BackupInfo
	for _, m := range metas {
		data, err := os.ReadFile(m)
		if err != nil {
			return nil, err
		}
		var info BackupInfo
		if err := json.Unmarshal(data, &info); err != nil {
			return nil, fmt.Errorf("%s: %w", m, err)
		}
		info.File = strings.TrimSuffix(m, ".json") + ".db"
		out = append(out, info)
	}
	slices.SortFunc(out, func(a, b BackupInfo) int { return a.TakenAt.Compare(b.TakenAt) })
	return out, nil
}

// PrintBackups lists a cluster's backups on this machine, oldest first.
func PrintBackups(name string) error {
	backups, err := ListBackups(name)
	if err != nil {
		return err
	}
	if len(backups) == 0 {
		fmt.Printf("No backups of '%s' on this machine ('tow backup --name %s' takes one)\n", name, name)
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "TAKEN\tKUBERNETES\tTAKEN ON\tSIZE\tFILE")
	for _, b := range backups {
		fmt.Fprintf(tw, "%s\t%s (%s)\t%s\t%s\t%s\n", b.TakenAt.Local().Format("2006-01-02 15:04:05"), b.Kubernetes, b.Distribution, b.Node, humanSize(b.Size), b.File)
	}
	tw.Flush()
	ui.Warn(secretsWarning)
	return nil
}

func humanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// snapshotsOf is the file's snapshot schedule as kubeadm's timer takes it; nil when none.
func snapshotsOf(cfg *config.Config) *kubeadm.Snapshots {
	b := cfg.Spec.Kubernetes.Backups
	if b == nil {
		return nil
	}
	calendar, err := config.OnCalendar(b.Schedule)
	if err != nil {
		return nil // validated with the file
	}
	return &kubeadm.Snapshots{Calendar: calendar, Keep: b.Keep}
}

// planBackups changes kubeadm's snapshot timer on the control planes when the file's
// schedule changed: put on, written again, or taken off (the snapshots taken stay). It runs
// after new machines joined (they got the file's timer when they did), on the control planes
// the cluster has then. k3s's schedule is a server setting: planSettings rolls it.
func planBackups(plan *updatePlan, cfg, old *config.Config, prev *state.ClusterState) {
	if old == nil || cfg.Distribution() != config.DistributionKubeadm || sameBackups(old, cfg) {
		return
	}
	kad := kubeadmOptions(cfg)
	line := fmt.Sprintf("etcd snapshots taken off the control planes (their timer goes; the snapshots taken stay in %s)", kubeadm.SnapshotDir)
	if b := cfg.Spec.Kubernetes.Backups; b != nil {
		line = fmt.Sprintf("etcd snapshots on every control plane at %q (systemd: %s), the newest %d kept in %s", b.Schedule, kad.Snapshots.Calendar, b.Keep, kubeadm.SnapshotDir)
	}
	plan.cluster = append(plan.cluster, planStep{lines: []string{line}, run: func(ctx context.Context) error {
		kad.Nodes, kad.KnownHosts = prev.Outputs.Nodes, filepath.Join(state.Dir(cfg.Name()), "known_hosts")
		done := ui.Step("Changing the etcd snapshots on the control planes")
		if err := setSnapshots(ctx, *kad); err != nil {
			fmt.Println("failed")
			return err
		}
		done()
		return nil
	}})
}

func sameBackups(a, b *config.Config) bool {
	x, y := a.Spec.Kubernetes.Backups, b.Spec.Kubernetes.Backups
	return (x == nil && y == nil) || (x != nil && y != nil && *x == *y)
}
