package kubeadm

import (
	"context"
	"fmt"
	"net"
	"strings"

	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/runtimes"
	"github.com/towproj/tow/internal/ui"
)

// Sources (read 2026-11-13), followed here:
//   - upgrading kubeadm clusters: the package repository moved to the new minor release, the
//     kubeadm package first, then 'kubeadm upgrade apply vX' on the first control plane
//     ('kubeadm upgrade plan' checks it first) and 'kubeadm upgrade node' on the others and
//     on workers, then kubelet and kubectl and the kubelet restarted, the node drained before
//     the kubelet changes. The kubelet is never newer than the API server. 'kubeadm upgrade'
//     is idempotent: run again after a failure. It renews the certificates it manages, and
//     upgrades CoreDNS and kube-proxy once the last control plane is upgraded (kubeadm 1.28+),
//     so control planes go one at a time
//     https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-upgrade/
//   - changing the package repository: the minor release in kubernetes.list
//     https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/change-package-repository/
//   - kube-proxy left out at init (skipPhases addon/kube-proxy) is recorded in the
//     ClusterConfiguration (proxy.disabled), and 'kubeadm upgrade' leaves it out too
//     (kubeadm's cmd/kubeadm/app/cmd/init.go manageSkippedAddons, phases/upgrade/addons.go)

// upgradePackagesScript moves the Kubernetes repository to the release's minor and upgrades
// pkgs to the release, held there. On RHEL-family machines the repository file is written
// for the new minor (its exclude line still holds them), and dnf takes them to the release
// with the exclude lifted for that command.
func upgradePackagesScript(version string, f facts, pkgs ...string) string {
	want, list := strings.TrimPrefix(version, "v"), strings.Join(pkgs, " ")
	if f.rhel() {
		var specs []string
		for _, p := range pkgs {
			specs = append(specs, p+"-"+want)
		}
		return "set -e\n" + rpmRepo(kubeRPM, "kubernetes", "Kubernetes", kubeRepo(runtimes.KubeMinor(version), true), kubePackages) +
			dnfKube(f.major) + " " + strings.Join(specs, " ") + "\n"
	}
	return "set -e\n" + aptHelpers + addRepo("kubernetes", kubeRepo(runtimes.KubeMinor(version), false)) + fmt.Sprintf(`aptget update -qq
sel=""
for p in %[1]s; do
  v=$(exact "$p" '%[2]s')
  [ -n "$v" ] || { echo "pkgs.k8s.io has no $p %[2]s"; exit 1; }
  sel="$sel $p=$v"
done
apt-mark unhold %[1]s >/dev/null
aptget install -y -qq $sel
apt-mark hold %[1]s >/dev/null
`, list, want)
}

// UpgradeNode upgrades one node of a running cluster to o.Version, its pods already moved
// away: the kubeadm package, then 'kubeadm upgrade apply' (first: the first control plane)
// or 'kubeadm upgrade node', then the container runtime when it doesn't go with the new
// release, then kubelet and kubectl, the kubelet restarted. Run again, it picks up where it
// stopped.
func UpgradeNode(ctx context.Context, o Options, n providers.Node, first bool) error {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
	ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	r, err := o.Connect(ctx, n, o.KnownHosts)
	if err != nil {
		return err
	}
	defer r.Close()
	m := &machine{n: n, label: n.Label(), r: r}
	run := func(what, script string) error {
		if out, err := r.RunInput(ctx, asRoot, script); err != nil {
			return fmt.Errorf("%s: %s failed: %w\n%s", m.label, what, err, ui.LastLines(out, 20))
		}
		return nil
	}
	if m.facts, err = o.read(ctx, m); err != nil {
		return err
	}
	if v := m.facts.pkgs["kubeadm"]; !strings.HasPrefix(v, o.want()+"-") {
		if err := run("the kubeadm package", upgradePackagesScript(o.Version, m.facts, "kubeadm")); err != nil {
			return err
		}
	}
	upgrade := "kubeadm upgrade node"
	if first {
		upgrade = fmt.Sprintf("kubeadm upgrade plan %[1]s\nkubeadm upgrade apply %[1]s --yes", o.Version)
	}
	if err := run(strings.ReplaceAll(upgrade, "\n", ", then "), "set -e\n"+upgrade+"\n"); err != nil {
		return err
	}
	// The runtime, set up for the new release: its kubeadm names the pause image.
	if m.facts, err = o.read(ctx, m); err != nil {
		return err
	}
	if missing := o.missingRuntime(m.facts); len(missing) > 0 {
		ui.Debug("%s: %s", m.label, strings.Join(missing, "; "))
		script, rec, err := o.runtimeScript(m)
		if err != nil {
			return err
		}
		if err := run(o.runtimeName(), script); err != nil {
			return err
		}
		if err := o.record(ctx, m, []prepare.Record{rec}); err != nil {
			return err
		}
	}
	return run("kubelet and kubectl", upgradePackagesScript(o.Version, m.facts, "kubelet", "kubectl")+"systemctl daemon-reload\nsystemctl restart kubelet\n")
}

// RuntimeChanges says, per machine, how its container runtime changes for Kubernetes
// o.Version ("containerd 2.2.1 → 2.3.7"), and what stops the upgrade: a runtime tow
// didn't install that doesn't go with it, a machine that can't be read. It changes nothing.
func RuntimeChanges(ctx context.Context, o Options) (map[string]string, error) {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
	changes := map[string]string{}
	var problems []string
	for _, n := range o.Nodes {
		r, err := o.Connect(ctx, n, o.KnownHosts)
		if err != nil {
			return nil, err
		}
		m := &machine{n: n, label: n.Label(), r: r}
		m.facts, err = o.read(ctx, m)
		r.Close()
		if err != nil {
			return nil, err
		}
		if _, theirs := o.theirs(m.facts); theirs {
			for _, p := range o.problems(m.facts) {
				problems = append(problems, fmt.Sprintf("%s: %s (%s)", m.label, p.What, p.Hint))
			}
			continue
		}
		switch c := m.facts.containerd; {
		case o.Runtime == runtimes.CRIO && !strings.Contains(m.facts.crioRepo, crioRepo(o.minor(), m.facts.rhel())):
			changes[n.Name] = fmt.Sprintf("CRI-O %s → its v1.%d repository's", m.facts.crio.version, o.minor())
		case o.Runtime != runtimes.CRIO && !sameRelease(c.version, o.containerd()):
			changes[n.Name] = fmt.Sprintf("containerd %s → %s", c.version, o.containerd())
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("Kubernetes %s can't go on %s:\n  ✗ %s", o.Version, ui.Plural(len(problems), "machine"), strings.Join(problems, "\n  ✗ "))
	}
	return changes, nil
}

// HealthScript checks a control plane: its own API server answers /readyz (etcd included),
// its control-plane containers run, and every etcd member is healthy.
func HealthScript(runtime string, n providers.Node) string {
	api := "https://" + net.JoinHostPort(cmpIP(n), "6443")
	return fmt.Sprintf(`set -e
k="kubectl --kubeconfig /etc/kubernetes/admin.conf --server %[2]s"
out=$($k get --raw=/readyz 2>&1) || true
if [ "$out" != ok ]; then $k get --raw='/readyz?verbose' 2>&1 | tail -n 20; exit 1; fi
cri="crictl --runtime-endpoint %[1]s"
for c in kube-apiserver kube-controller-manager kube-scheduler etcd; do
  [ -n "$($cri ps --name "^$c\$" --state running --quiet)" ] || { echo "$c isn't running"; exit 1; }
done
id=$($cri ps --name '^etcd$' --state running --quiet | head -n 1)
$cri exec "$id" etcdctl --endpoints=https://127.0.0.1:2379 \
  --cacert=/etc/kubernetes/pki/etcd/ca.crt --cert=/etc/kubernetes/pki/etcd/server.crt --key=/etc/kubernetes/pki/etcd/server.key \
  endpoint health --cluster
`, criSocket(runtime), api)
}

// cmpIP is the address a control plane's API server listens on: its private one.
func cmpIP(n providers.Node) string {
	if n.PrivateIP != "" {
		return n.PrivateIP
	}
	return n.Host
}
