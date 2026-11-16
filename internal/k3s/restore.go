package k3s

import (
	"context"
	"fmt"
	"path"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/parallel"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/ui"
)

// Sources (read 2026-11-13), followed here:
//   - restoring a snapshot of k3s's embedded etcd: stop k3s on every server; on the first,
//     'k3s server --cluster-reset --cluster-reset-restore-path=<file>' (it prints "restart
//     without --cluster-reset" when done), then start k3s there; on each other server, move
//     ${data-dir}/server/db aside and start k3s, which joins the restored cluster. The token
//     must be the one the snapshot was taken with (the servers' own config.yaml has it). With
//     etcd-s3 configured, a local file needs --etcd-s3=false. Agents keep running
//     https://docs.k3s.io/cli/etcd-snapshot

// RestoreOptions say where the restore runs: the control planes (the first one restores),
// the snapshot already copied to the first one, and the file's k3s settings that matter.
type RestoreOptions struct {
	ControlPlanes []providers.Node
	KnownHosts    string
	Snapshot      string // its path on the first control plane
	DataDir       string // k3s's data-dir
	S3            bool   // etcd-s3 is configured: the reset must not look there
	Stamp         string // names the folders moved aside: db.before-restore-<stamp>
	Connect       func(ctx context.Context, n providers.Node, knownHosts string) (prepare.Runner, error)
}

// DataDir is k3s's data-dir as the file sets it.
func DataDir(cfg *config.Config) string {
	if v, ok := cfg.Spec.Kubernetes.K3s.Server.ExtraArgs["data-dir"]; ok && len(v.Values) == 1 {
		return v.Values[0]
	}
	return defaultDataDir
}

// S3 reports whether the file has k3s upload its snapshots to S3.
func S3(cfg *config.Config) bool {
	v, ok := cfg.Spec.Kubernetes.K3s.Server.ExtraArgs["etcd-s3"]
	return ok && (len(v.Values) == 0 || v.Values[0] != "false")
}

// restoreWait is how long k3s gets to reset, or a control plane to answer again.
const restoreWait = 10 * time.Minute

// Restore puts the cluster's etcd back to the snapshot, as k3s's docs say. It returns the
// folders it moved aside on the other control planes (their old etcd data).
func Restore(ctx context.Context, o RestoreOptions) ([]string, error) {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
	run := func(n providers.Node, what, script string) (string, error) {
		ctx, cancel := context.WithTimeout(ctx, restoreWait)
		defer cancel()
		r, err := o.Connect(ctx, n, o.KnownHosts)
		if err != nil {
			return "", err
		}
		defer r.Close()
		out, err := r.RunInput(ctx, asRoot, script)
		if err != nil {
			return out, fmt.Errorf("%s: %s failed: %w\n%s", n.Label(), what, err, ui.LastLines(out, 20))
		}
		return out, nil
	}
	first, others := o.ControlPlanes[0], o.ControlPlanes[1:]

	type stop struct {
		n   providers.Node
		err error
	}
	stops := make([]*stop, len(o.ControlPlanes))
	for i, n := range o.ControlPlanes {
		stops[i] = &stop{n: n}
	}
	if err := ui.StepRun(fmt.Sprintf("Stopping k3s on %s", ui.Plural(len(o.ControlPlanes), "control plane")), func() error {
		parallel.Each(stops, func(s *stop) { _, s.err = run(s.n, "stopping k3s", "systemctl stop k3s\n") })
		for _, s := range stops {
			if s.err != nil {
				return s.err
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	if err := ui.StepRun(fmt.Sprintf("Restoring the snapshot on %s (k3s --cluster-reset)", first.Label()), func() error {
		_, err := run(first, "k3s --cluster-reset", resetScript(o))
		return err
	}); err != nil {
		return nil, err
	}
	if err := ui.StepRun(fmt.Sprintf("Starting k3s on %s", first.Label()), func() error {
		_, err := run(first, "starting k3s", "systemctl start k3s\n"+ReadyScript)
		return err
	}); err != nil {
		return nil, err
	}

	var aside []string
	for _, n := range others {
		moved := path.Join(o.DataDir, "server", "db.before-restore-"+o.Stamp)
		if err := ui.StepRun(fmt.Sprintf("%s joins the restored cluster", n.Label()), func() error {
			_, err := run(n, "rejoining", rejoinScript(o.DataDir, moved))
			return err
		}); err != nil {
			return aside, err
		}
		aside = append(aside, n.Label()+": "+moved)
	}
	_, _ = run(first, "removing the snapshot's copy", "rm -f "+remote.Quote(o.Snapshot)+"\n")
	return aside, nil
}

// ReadyScript waits until the machine's own API server is ready, etcd included.
const ReadyScript = `for i in $(seq 1 120); do
  [ "$(k3s kubectl get --raw=/readyz 2>/dev/null)" = ok ] && exit 0
  sleep 5
done
echo "k3s's API isn't ready after 10 minutes"
journalctl -u k3s --no-pager -n 20 -o cat
exit 1
`

// resetScript resets etcd to the snapshot; done when k3s says to restart without the flag.
// A reset that already ran (a restore run again before k3s started) left its reset-flag,
// which refuses another reset: it goes first, as the snapshot is the same.
func resetScript(o RestoreOptions) string {
	s3 := ""
	if o.S3 {
		s3 = " --etcd-s3=false"
	}
	return fmt.Sprintf(`set -e
rm -f %[3]s
out=$(k3s server --cluster-reset --cluster-reset-restore-path=%[1]s%[2]s 2>&1) || { echo "$out" | tail -n 30; exit 1; }
echo "$out" | grep -q 'restart without --cluster-reset' || { echo "$out" | tail -n 30; exit 1; }
`, remote.Quote(o.Snapshot), s3, remote.Quote(path.Join(o.DataDir, "server", "db", "reset-flag")))
}

// rejoinScript moves a control plane's old etcd data aside and starts k3s: it joins the
// restored cluster.
func rejoinScript(dataDir, moved string) string {
	return fmt.Sprintf(`set -e
if [ -d %[1]s ]; then mv %[1]s %[2]s; fi
systemctl start k3s
`, remote.Quote(path.Join(dataDir, "server", "db")), remote.Quote(moved)) + ReadyScript
}
