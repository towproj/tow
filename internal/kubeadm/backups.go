package kubeadm

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/parallel"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
)

// Sources (read 2026-11-13), followed here:
//   - an etcd snapshot of a kubeadm cluster: etcdctl snapshot save, against 127.0.0.1:2379
//     with the certificates in /etc/kubernetes/pki/etcd
//     https://kubernetes.io/docs/tasks/administer-cluster/configure-upgrade-etcd/#snapshot-using-etcdctl-options
//   - kubeadm's etcd image ships etcdctl (it's distroless: no shell); its static pod mounts
//     /var/lib/etcd and /etc/kubernetes/pki/etcd from the machine; crictl (cri-tools, which
//     kubeadm's packages depend on) runs it in the running container
//   - systemd.timer(5): OnCalendar; systemd-analyze calendar checks an expression

// Where the snapshots live on each control plane, what takes them, and its record's name.
const (
	SnapshotDir     = "/var/lib/tow/snapshots"
	snapshotBin     = "/usr/local/sbin/tow-etcd-snapshot"
	snapshotUnit    = "tow-etcd-snapshot"
	SnapshotsRecord = "etcd snapshots"
)

// Snapshots is when the control planes take etcd snapshots: Calendar is the file's schedule
// as systemd reads it, Keep how many each one keeps.
type Snapshots struct {
	Calendar string
	Keep     int
}

// snapshotScript takes one snapshot with the etcdctl of the machine's etcd container, moves it
// into SnapshotDir (root only), keeps the newest keep (0: all), and prints its path. The
// snapshot is first written where the container sees the machine's disk: /var/lib/etcd.
func snapshotScript(runtime string, keep int) string {
	return fmt.Sprintf(`#!/bin/sh
# tow: one etcd snapshot, with the etcdctl of this control plane's etcd container.
set -eu
dir=%[1]s
cri="crictl --runtime-endpoint %[2]s"
id=$($cri ps --name '^etcd$' --state running --quiet | head -n 1)
[ -n "$id" ] || { echo "etcd isn't running on this machine" >&2; exit 1; }
name="etcd-$(hostname)-$(date -u +%%Y%%m%%dT%%H%%M%%SZ).db"
$cri exec "$id" etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt --cert=/etc/kubernetes/pki/etcd/server.crt --key=/etc/kubernetes/pki/etcd/server.key \
  snapshot save "/var/lib/etcd/tow-$name" >/dev/null
mkdir -p "$dir"
chmod 0700 "$dir"
mv "/var/lib/etcd/tow-$name" "$dir/$name"
chmod 0600 "$dir/$name"
keep=%[3]d
if [ "$keep" -gt 0 ]; then
  ls -1t "$dir"/etcd-*.db | tail -n +$((keep + 1)) | xargs -r rm -f --
fi
echo "$dir/$name"
`, SnapshotDir, criSocket(runtime), keep)
}

// removeSnapshotsScript takes the timer off; the snapshots stay.
const removeSnapshotsScript = "if [ -f /etc/systemd/system/" + snapshotUnit + ".timer ]; then\n" +
	"  systemctl disable --now " + snapshotUnit + ".timer >/dev/null 2>&1 || true\n" +
	"  rm -f /etc/systemd/system/" + snapshotUnit + ".timer /etc/systemd/system/" + snapshotUnit + ".service " + snapshotBin + "\n" +
	"  systemctl daemon-reload\n" +
	"fi\n"

// snapshotsScript puts the timer on a control plane, rewritten in full (a changed schedule
// or keep is taken at once), and records it once in the machine's record.
func (o Options) snapshotsScript(record []byte) (string, error) {
	s := o.Snapshots
	var b strings.Builder
	fmt.Fprintf(&b, "set -e\nsystemd-analyze calendar '%s' >/dev/null\n", s.Calendar)
	fmt.Fprintf(&b, "(umask 077; cat > %s <<'TOW_EOF'\n%sTOW_EOF\n)\nchmod 0700 %s\n", snapshotBin, snapshotScript(o.Runtime, s.Keep), snapshotBin)
	fmt.Fprintf(&b, "cat > /etc/systemd/system/%s.service <<'TOW_EOF'\n[Unit]\nDescription=tow: an etcd snapshot (cluster %s)\nAfter=%s.service\n\n[Service]\nType=oneshot\nExecStart=%s\nTOW_EOF\n",
		snapshotUnit, o.Cluster, runtimeUnit(o.Runtime), snapshotBin)
	fmt.Fprintf(&b, "cat > /etc/systemd/system/%s.timer <<'TOW_EOF'\n[Unit]\nDescription=tow: etcd snapshots, %s (cluster %s)\n\n[Timer]\nOnCalendar=%s\n\n[Install]\nWantedBy=timers.target\nTOW_EOF\n",
		snapshotUnit, s.Calendar, o.Cluster, s.Calendar)
	fmt.Fprintf(&b, "systemctl daemon-reload\nsystemctl enable %[1]s.timer >/dev/null 2>&1\nsystemctl restart %[1]s.timer\n", snapshotUnit)
	if !prepare.Recorded(record, func(r prepare.Record) bool { return r.Cluster == o.Cluster && r.What == SnapshotsRecord }) {
		rec, err := prepare.RecordScript(record, []prepare.Record{{
			Cluster: o.Cluster, What: SnapshotsRecord, At: time.Now().UTC(),
			Detail: fmt.Sprintf("systemd timer %s.timer, taking etcd snapshots into %s", snapshotUnit, SnapshotDir),
			Undo: fmt.Sprintf("sudo systemctl disable --now %[1]s.timer; sudo rm /etc/systemd/system/%[1]s.* %[2]s (the snapshots in %[3]s stay)",
				snapshotUnit, snapshotBin, SnapshotDir),
			Revert:     removeSnapshotsScript,
			Kubernetes: true,
		}})
		if err != nil {
			return "", err
		}
		b.WriteString(rec)
	}
	return b.String(), nil
}

func runtimeUnit(runtime string) string {
	if strings.Contains(criSocket(runtime), "crio") {
		return "crio"
	}
	return "containerd"
}

// SetSnapshots puts the snapshot timer on every control plane, as o.Snapshots says, or
// takes it off where it is (nil). Run again, it rewrites the same files.
func SetSnapshots(ctx context.Context, o Options) error {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
	type cp struct {
		n   providers.Node
		err error
	}
	var cps []*cp
	for _, n := range o.Nodes {
		if n.Role == config.RoleControlPlane {
			cps = append(cps, &cp{n: n})
		}
	}
	parallel.Each(cps, func(c *cp) {
		r, err := o.Connect(ctx, c.n, o.KnownHosts)
		if err != nil {
			c.err = err
			return
		}
		defer r.Close()
		script := removeSnapshotsScript
		if o.Snapshots != nil {
			record, _ := r.RunInput(ctx, asRoot, "cat "+prepare.RecordFile+" 2>/dev/null || true")
			if script, err = o.snapshotsScript([]byte(record)); err != nil {
				c.err = err
				return
			}
		}
		if out, err := r.RunInput(ctx, asRoot, script); err != nil {
			c.err = fmt.Errorf("%s: etcd snapshots: %w\n%s", c.n.Label(), err, strings.TrimSpace(out))
		}
	})
	var errs []string
	for _, c := range cps {
		if c.err != nil {
			errs = append(errs, c.err.Error())
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "\n"))
	}
	return nil
}

// Snapshot takes an etcd snapshot now on a control plane, next to the scheduled ones and
// pruned with them (the newest Keep stay; all of them with no schedule), and returns its
// path there.
func Snapshot(ctx context.Context, o Options, n providers.Node) (string, error) {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
	r, err := o.Connect(ctx, n, o.KnownHosts)
	if err != nil {
		return "", err
	}
	defer r.Close()
	keep := 0
	if o.Snapshots != nil {
		keep = o.Snapshots.Keep
	}
	out, err := r.RunInput(ctx, asRoot, snapshotScript(o.Runtime, keep))
	if err != nil {
		return "", fmt.Errorf("%s: etcd snapshot: %w\n%s", n.Label(), err, strings.TrimSpace(out))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	return lines[len(lines)-1], nil
}
