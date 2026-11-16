package integration

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/runtimes"
	"github.com/towproj/tow/internal/state"
)

// Shell commands that read a machine the same way on both families: dpkg and apt on Ubuntu
// and Debian, rpm and dnf on RHEL and its rebuilds.
func pkgStatus(p string) string {
	return "dpkg-query -W -f='${db:Status-Status}' " + p + " 2>/dev/null || { rpm -q --quiet " + p + " && echo installed; }"
}

func pkgVersion(p string) string {
	return "dpkg-query -W -f='${Version}' " + p + " 2>/dev/null || rpm -q --qf '%{VERSION}-%{RELEASE}' " + p
}

// heldPackages: apt-mark's holds, or the Kubernetes repository's exclude line (dnf).
const heldPackages = "if command -v apt-mark >/dev/null 2>&1; then apt-mark showhold; else sed -n 's/^exclude=//p' /etc/yum.repos.d/kubernetes.repo; fi"

// packageMirrors refreshes the package lists: the mirrors answer.
const packageMirrors = "if command -v apt-get >/dev/null 2>&1; then sudo -n apt-get update -qq; else sudo -n dnf -q makecache; fi"

// tcpProbe connects to a port without nc, which RHEL's cloud images don't ship.
func tcpProbe(ip, port string) string {
	return "timeout 5 bash -c 'exec 3<>/dev/tcp/" + ip + "/" + port + "'"
}

// systemOf checks a machine runs a system tow prepares, and returns its ID.
func systemOf(t *testing.T, run func(what, cmd string) string) string {
	t.Helper()
	fields := strings.SplitN(run("os", `. /etc/os-release && echo "$ID|$ID_LIKE|$VERSION_ID"`), "|", 3)
	if len(fields) != 3 {
		t.Errorf("os-release: %v", fields)
		return ""
	}
	if _, err := prepare.Family(fields[0], fields[1], fields[2]); err != nil {
		t.Errorf("os: %v", err)
	}
	return fields[0]
}

// checkPrepared proves a machine was prepared for the cluster's distribution, as the official
// docs ask, and that every change is recorded on it. Run is one machine's SSH session.
func checkPrepared(t *testing.T, ctx context.Context, c *remote.Client, cluster, distribution string) {
	t.Helper()
	run := func(cmd string) string {
		t.Helper()
		out, err := c.Run(ctx, cmd)
		if err != nil {
			t.Errorf("%s: %v\n%s", cmd, err, out)
		}
		return strings.TrimSpace(out)
	}

	// Kubernetes (k3s or kubeadm) goes on the machines after preparation, and the file's
	// Helm charts after it.
	if st, err := state.Load(cluster); err != nil || (st.Stage != state.StageKubernetes && st.Stage != state.StageAddons) {
		t.Errorf("state stage = %+v, %v; want %s or %s", st, err, state.StageKubernetes, state.StageAddons)
	}
	for _, pkg := range []string{"ca-certificates", "curl"} {
		if got := run(pkgStatus(pkg)); got != "installed" {
			t.Errorf("package %s: %q", pkg, got)
		}
	}
	if distribution == config.DistributionKubeadm {
		if swap := run("swapon --noheadings --show=NAME"); swap != "" {
			t.Errorf("swap still on: %s", swap)
		}
		if v := run("sysctl -n net.ipv4.ip_forward"); v != "1" {
			t.Errorf("net.ipv4.ip_forward = %s", v)
		}
		if !strings.Contains(run("cat /etc/sysctl.d/k8s.conf"), "net.ipv4.ip_forward = 1") {
			t.Error("ip_forward isn't kept for the next boot")
		}
	}

	var l struct {
		Changes []prepare.Record `json:"changes"`
	}
	if err := json.Unmarshal([]byte(run("sudo -n cat "+prepare.RecordFile)), &l); err != nil || len(l.Changes) == 0 { // root only
		t.Fatalf("%s: %v, %+v", prepare.RecordFile, err, l)
	}
	for _, r := range l.Changes {
		if r.Cluster != cluster || r.Undo == "" {
			t.Errorf("record %+v: want cluster %s and an undo", r, cluster)
		}
	}
}

// checkKubeadmMachine proves a machine has what kubeadm needs, as the file asks: kubelet,
// kubeadm and kubectl at the cluster's release, held there; the container runtime at its
// release, set up for Kubernetes and answering on its CRI socket; kubeadm's run finished for
// this cluster; all recorded.
func checkKubeadmMachine(t *testing.T, ctx context.Context, c *remote.Client, cfg *config.Config) {
	t.Helper()
	run := func(cmd string) string {
		t.Helper()
		out, err := c.Run(ctx, cmd)
		if err != nil {
			t.Errorf("%s: %v\n%s", cmd, err, out)
		}
		return strings.TrimSpace(out)
	}
	st, err := state.Load(cfg.Name())
	if err != nil {
		t.Fatal(err)
	}
	want := strings.TrimPrefix(st.K8sVersion, "v") // resolved, also for "stable"
	for _, p := range []string{"kubelet", "kubeadm", "kubectl"} {
		if v := run(pkgVersion(p)); !strings.HasPrefix(v, want+"-") {
			t.Errorf("%s %s, want %s", p, v, want)
		}
	}
	if held := strings.Fields(run(heldPackages)); !slices.Contains(held, "kubelet") || !slices.Contains(held, "kubeadm") || !slices.Contains(held, "kubectl") {
		t.Errorf("held: %v", held)
	}
	if v := run("systemctl is-enabled kubelet"); v != "enabled" {
		t.Errorf("kubelet is %s", v)
	}
	pause := run("kubeadm config images list --kubernetes-version v" + want + " 2>/dev/null | grep /pause:")
	if pause == "" {
		t.Error("kubeadm names no pause image")
	}

	k := cfg.Spec.Kubernetes
	recorded := []string{"Kubernetes packages"}
	if k.ContainerRuntime == runtimes.CRIO {
		recorded = append(recorded, "CRI-O")
		v := run(pkgVersion("cri-o"))
		if k.CRIO != nil && k.CRIO.Version != "" && !strings.HasPrefix(v, strings.TrimPrefix(k.CRIO.Version, "v")+"-") || runtimes.CheckCRIO(v, runtimes.KubeMinor(st.K8sVersion)) != nil {
			t.Errorf("cri-o %s, want the file's release or Kubernetes' minor", v)
		}
		if v := run("systemctl is-active crio"); v != "active" {
			t.Errorf("crio is %s", v)
		}
		if conf := run("cat /etc/crio/crio.conf.d/20-tow.conf"); !strings.Contains(conf, `pause_image = "`+pause+`"`) {
			t.Errorf("CRI-O's drop-in lacks the pause image %s:\n%s", pause, conf)
		}
		run("sudo -n crictl --runtime-endpoint unix:///var/run/crio/crio.sock info >/dev/null")
	} else {
		recorded = append(recorded, "containerd")
		release := runtimes.DefaultContainerd
		if k.Containerd != nil && k.Containerd.Version != "" {
			release = strings.TrimPrefix(k.Containerd.Version, "v")
		}
		if v := run("/usr/local/bin/containerd --version"); !strings.Contains(v, " v"+release+" ") {
			t.Errorf("containerd: %s, want %s", v, release)
		}
		if v := run("systemctl is-active containerd"); v != "active" {
			t.Errorf("containerd is %s", v)
		}
		// Its config as containerd itself reads it: systemd cgroups, kubeadm's pause image.
		dump := run("sudo -n /usr/local/bin/containerd config dump")
		if !strings.Contains(dump, "SystemdCgroup = true") || !strings.Contains(dump, "sandbox = '"+pause+"'") {
			t.Errorf("containerd's config lacks SystemdCgroup = true or sandbox = '%s'", pause)
		}
		run("sudo -n crictl --runtime-endpoint unix:///run/containerd/containerd.sock info >/dev/null")
	}

	// kubeadm finished here for this cluster, and the kubelet runs. On the control plane, the
	// join token tow made is gone.
	if done := run("cat /etc/tow/kubeadm-done"); done != cfg.Name() {
		t.Errorf("/etc/tow/kubeadm-done = %q, want %q", done, cfg.Name())
	}
	if v := run("systemctl is-active kubelet"); v != "active" {
		t.Errorf("kubelet is %s", v)
	}
	kubeadmRun := "kubeadm (worker)"
	if out, err := c.Run(ctx, "test -e /etc/kubernetes/admin.conf"); err == nil {
		kubeadmRun = "kubeadm (control plane)"
		if tokens := run("sudo -n kubeadm token list --kubeconfig /etc/kubernetes/admin.conf"); strings.Contains(tokens, "tow: nodes joining") {
			t.Errorf("the join token is still there:\n%s", tokens)
		}
	} else if remote.ExitCode(err) != 1 {
		t.Errorf("test -e /etc/kubernetes/admin.conf: %v\n%s", err, out)
	}
	recorded = append(recorded, kubeadmRun)

	var l struct {
		Changes []prepare.Record `json:"changes"`
	}
	json.Unmarshal([]byte(run("sudo -n cat "+prepare.RecordFile)), &l)
	for _, what := range recorded {
		if !slices.ContainsFunc(l.Changes, func(r prepare.Record) bool { return r.Cluster == cfg.Name() && r.What == what && r.Undo != "" }) {
			t.Errorf("%s isn't recorded in %s", what, prepare.RecordFile)
		}
	}
}
