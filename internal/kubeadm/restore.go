package kubeadm

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/ui"
)

// Sources (read 2026-11-13), followed here:
//   - restoring etcd under Kubernetes: stop all API servers, restore every etcd member,
//     start the API servers again; restarting kube-scheduler, kube-controller-manager and the
//     kubelets is recommended. etcd restores snapshots of its own major.minor release only.
//     With kubeadm, restoring into the same data directory keeps its ClusterConfiguration
//     right  https://kubernetes.io/docs/tasks/administer-cluster/configure-upgrade-etcd/#restoring-an-etcd-cluster
//   - etcd's recovery guide: every member restores the same snapshot with etcdutl, with its
//     --name, the whole --initial-cluster, its --initial-advertise-peer-urls and a new
//     --initial-cluster-token; --bump-revision with --mark-compacted keeps revisions from going
//     backwards for watchers such as Kubernetes  https://etcd.io/docs/v3.6/op-guide/recovery/
//   - kubeadm's etcd image is distroless (no shell): etcdutl is read from the running
//     container's root filesystem (crictl inspect: info.runtimeSpec.root.path, relative to
//     containerd's task bundle, absolute on CRI-O)

// Where a restore keeps its things on a control plane.
const (
	etcdutlCopy  = "/var/lib/tow/etcdutl"
	manifestsDir = "/etc/kubernetes/manifests"
	setAside     = "/etc/kubernetes/tow-restore"
	etcdData     = "/var/lib/etcd"
)

// restoreWait is how long the static pods get to stop, or a script to finish.
const restoreWait = 10 * time.Minute

// RestoreOptions say where a restore runs: the control planes, the snapshot already copied
// to each, and a stamp naming the folders moved aside.
type RestoreOptions struct {
	Runtime       string
	ControlPlanes []providers.Node
	KnownHosts    string
	Snapshot      string // its path on every control plane
	Stamp         string
	Connect       func(ctx context.Context, n providers.Node, knownHosts string) (prepare.Runner, error)
}

func (o RestoreOptions) run(ctx context.Context, n providers.Node, what, script string) (string, error) {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
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

// CheckRestore gets each control plane ready to restore, changing nothing in the cluster:
// etcdutl copied out of its running etcd (kept from a run that stopped), and the snapshot
// checked against it: etcd restores only its own minor release's snapshots.
func CheckRestore(ctx context.Context, o RestoreOptions) error {
	for _, n := range o.ControlPlanes {
		out, err := o.run(ctx, n, "reading etcdutl and the snapshot", etcdutlScript(o.Runtime, o.Snapshot))
		if err != nil {
			return err
		}
		snap, tool, err := etcdVersions(out)
		if err != nil {
			return fmt.Errorf("%s: %w", n.Label(), err)
		}
		if minorOf(snap) != minorOf(tool) {
			return fmt.Errorf("the snapshot is from etcd %s, and %s runs etcd %s: etcd restores only snapshots of its own minor release (its docs)", snap, n.Label(), tool)
		}
	}
	return nil
}

func minorOf(v string) string {
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) < 2 {
		return v
	}
	return parts[0] + "." + parts[1]
}

// etcdutlScript copies etcdutl out of the running etcd container (unless a run that stopped
// did), then prints the snapshot's etcd release and etcdutl's.
func etcdutlScript(runtime, snapshot string) string {
	return fmt.Sprintf(`set -e
u=%[1]s
if [ ! -x "$u" ]; then
  cri="crictl --runtime-endpoint %[2]s"
  id=$($cri ps --name '^etcd$' --state running --quiet | head -n 1)
  [ -n "$id" ] || { echo "etcd isn't running here, so its etcdutl can't be read"; exit 1; }
  root=$($cri inspect -o go-template --template '{{.info.runtimeSpec.root.path}}' "$id")
  case "$root" in /*) ;; *) root="/run/containerd/io.containerd.runtime.v2.task/k8s.io/$id/$root" ;; esac
  install -d -m 0700 /var/lib/tow
  install -m 0700 "$root/usr/local/bin/etcdutl" "$u"
fi
echo "snapshot=$("$u" snapshot status %[3]s -w json)"
echo "etcdutl=$("$u" version | head -n 1)"
`, etcdutlCopy, criSocket(runtime), remote.Quote(snapshot))
}

// etcdVersions reads the snapshot's etcd release ({"version": "3.6.0"}) and etcdutl's
// ("etcdutl version: 3.6.5").
func etcdVersions(out string) (snapshot, tool string, err error) {
	for _, l := range strings.Split(out, "\n") {
		switch {
		case strings.HasPrefix(l, "snapshot="):
			var s struct {
				Version string `json:"version"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(l, "snapshot=")), &s); err == nil {
				snapshot = s.Version
			}
		case strings.HasPrefix(l, "etcdutl="):
			tool = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(l, "etcdutl="), "etcdutl version:"))
		}
	}
	if snapshot == "" || tool == "" {
		return "", "", fmt.Errorf("can't read the snapshot's etcd release, or etcdutl's:\n%s", ui.LastLines(out, 5))
	}
	return snapshot, tool, nil
}

// Restore puts every etcd member back to the snapshot: the API servers and etcd stopped on
// every control plane (their static pods set aside), each member restored, then the static
// pods back. It returns the folders it moved aside (the old etcd data). CheckRestore runs
// first.
func Restore(ctx context.Context, o RestoreOptions) ([]string, error) {
	each := func(what string, script func(n providers.Node) string) error {
		for _, n := range o.ControlPlanes {
			if _, err := o.run(ctx, n, what, script(n)); err != nil {
				return err
			}
		}
		return nil
	}
	if err := ui.StepRun(fmt.Sprintf("Stopping etcd and the API servers on %s", ui.Plural(len(o.ControlPlanes), "control plane")), func() error {
		return each("stopping the control plane", func(providers.Node) string { return stopScript(o.Runtime) })
	}); err != nil {
		return nil, err
	}
	var members []string
	for _, n := range o.ControlPlanes {
		members = append(members, fmt.Sprintf("%s=https://%s:2380", n.Name, cmpIP(n)))
	}
	initial := strings.Join(members, ",")
	var aside []string
	if err := ui.StepRun("Restoring the snapshot into every etcd member", func() error {
		return each("etcdutl snapshot restore", func(n providers.Node) string {
			aside = append(aside, n.Label()+": "+etcdData+".before-restore-"+o.Stamp)
			return memberScript(o, n, initial)
		})
	}); err != nil {
		return aside, err
	}
	err := ui.StepRun("Starting etcd and the API servers again", func() error {
		return each("starting the control plane", func(providers.Node) string { return startScript })
	})
	return aside, err
}

// Cleanup removes what a restore left on the control planes: etcdutl and the snapshot.
func CleanupRestore(ctx context.Context, o RestoreOptions) {
	for _, n := range o.ControlPlanes {
		_, _ = o.run(ctx, n, "cleaning up", fmt.Sprintf("rm -f %s %s\n", etcdutlCopy, remote.Quote(o.Snapshot)))
	}
}

// stopScript sets etcd's and the control plane's static pods aside (kube-vip's stays: it
// holds the API address), then waits until etcd and the API server are gone.
func stopScript(runtime string) string {
	return fmt.Sprintf(`set -e
mkdir -p %[1]s
for c in etcd kube-apiserver kube-controller-manager kube-scheduler; do
  if [ -f %[2]s/$c.yaml ]; then mv %[2]s/$c.yaml %[1]s/; fi
done
cri="crictl --runtime-endpoint %[3]s"
for i in $(seq 1 90); do
  [ -z "$($cri ps --name '^(etcd|kube-apiserver)$' --state running --quiet)" ] && exit 0
  sleep 2
done
echo "etcd or the API server still runs after 3 minutes"
exit 1
`, setAside, manifestsDir, criSocket(runtime))
}

// memberScript restores one member into etcd's own data directory, the old one moved aside.
func memberScript(o RestoreOptions, n providers.Node, initial string) string {
	ip := cmpIP(n)
	return fmt.Sprintf(`set -e
if [ -d %[1]s ]; then mv %[1]s %[1]s.before-restore-%[2]s; fi
%[3]s snapshot restore %[4]s --data-dir %[1]s --name %[5]s \
  --initial-cluster %[6]s --initial-advertise-peer-urls https://%[7]s:2380 \
  --initial-cluster-token tow-restore-%[2]s --bump-revision 1000000000 --mark-compacted
`, etcdData, o.Stamp, etcdutlCopy, remote.Quote(o.Snapshot), remote.Quote(n.Name), remote.Quote(initial), ip)
}

// RestartControlPlaneScript restarts a control plane's static pods (kubeadm's certificates
// page: they read their certificates when they start): set aside until etcd and the API
// server are gone, then put back.
func RestartControlPlaneScript(runtime string) string {
	return fmt.Sprintf(`mkdir -p %[1]s
for c in etcd kube-apiserver kube-controller-manager kube-scheduler; do
  if [ -f %[2]s/$c.yaml ]; then mv %[2]s/$c.yaml %[1]s/; fi
done
cri="crictl --runtime-endpoint %[3]s"
for i in $(seq 1 90); do
  [ -z "$($cri ps --name '^(etcd|kube-apiserver)$' --state running --quiet)" ] && break
  sleep 2
done
`, setAside, manifestsDir, criSocket(runtime)) + startScript
}

// startScript puts the static pods back: the kubelet starts etcd and the control plane.
const startScript = `set -e
for f in ` + setAside + `/*.yaml; do
  if [ -e "$f" ]; then mv "$f" ` + manifestsDir + `/; fi
done
rmdir ` + setAside + ` 2>/dev/null || true
`
