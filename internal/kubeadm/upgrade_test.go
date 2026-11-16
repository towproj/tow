package kubeadm

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/runtimes"
)

// A node goes as kubeadm's upgrade page says: the kubeadm package (its repository moved to
// the new minor), kubeadm upgrade apply on the first control plane (plan first) or upgrade
// node elsewhere, then containerd when it doesn't go with the new release, then kubelet and
// kubectl and the kubelet restarted. Run again, the parts already done are skipped.
func TestUpgradeNode(t *testing.T) {
	l := newLab()
	old := l.options(Options{Version: "v1.36.6", Containerd: "2.2.1"})
	if err := Setup(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	o := l.options(Options{Version: "v1.37.2"})
	changes, err := RuntimeChanges(context.Background(), o)
	if err != nil || len(changes) != 2 || changes["lab-cp-0"] != "containerd 2.2.1 → 2.3.7" {
		t.Errorf("runtime changes %v, %v", changes, err)
	}

	cp := l.machines["192.168.1.10"]
	cp.scripts = nil
	if err := UpgradeNode(context.Background(), o, nodes[0], true); err != nil {
		t.Fatal(err)
	}
	// containerd is recorded already (its undo removes any release): no new record.
	if len(cp.scripts) != 4 {
		t.Fatalf("%d scripts, want kubeadm, upgrade, containerd, kubelet+kubectl:\n%s", len(cp.scripts), strings.Join(cp.scripts, "\n---\n"))
	}
	for i, want := range [][]string{
		{"deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.37/deb/ /", `exact "$p" '1.37.2'`, "apt-mark unhold kubeadm >/dev/null", "apt-mark hold kubeadm >/dev/null"},
		{"kubeadm upgrade plan v1.37.2\nkubeadm upgrade apply v1.37.2 --yes"},
		{"containerd-2.3.7-linux-amd64.tar.gz", "systemctl restart containerd"},
		{"apt-mark unhold kubelet kubectl", "apt-mark hold kubelet kubectl", "systemctl daemon-reload\nsystemctl restart kubelet"},
	} {
		for _, w := range want {
			if !strings.Contains(cp.scripts[i], w) {
				t.Errorf("script %d lacks %q:\n%s", i, w, cp.scripts[i])
			}
		}
	}
	if strings.Contains(cp.scripts[0], "kubelet") || strings.Contains(cp.scripts[1], "apt") {
		t.Error("the kubelet goes after kubeadm upgrade, never before")
	}
	for _, s := range []string{cp.scripts[0], cp.scripts[3]} {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("sh -n: %v\n%s", err, out)
		}
	}
	if cp.pkgs["kubelet"] != "1.37.2-1.1" || cp.pkgs["kubeadm"] != "1.37.2-1.1" || cp.containerd.version != "2.3.7" {
		t.Errorf("packages %v, containerd %s", cp.pkgs, cp.containerd.version)
	}

	// The worker: kubeadm upgrade node.
	w := l.machines["192.168.1.20"]
	w.scripts = nil
	if err := UpgradeNode(context.Background(), o, nodes[1], false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(w.scripts[1], "kubeadm upgrade node") || strings.Contains(w.scripts[1], "apply") {
		t.Errorf("the worker: %s", w.scripts[1])
	}

	// Again on the control plane (a run that stopped after it): kubeadm upgrade again
	// (idempotent), the rest already there.
	cp.scripts = nil
	if err := UpgradeNode(context.Background(), o, nodes[0], true); err != nil {
		t.Fatal(err)
	}
	if len(cp.scripts) != 2 || !strings.Contains(cp.scripts[0], "kubeadm upgrade apply") || !strings.Contains(cp.scripts[1], "apt-mark hold kubelet kubectl") {
		t.Errorf("again: %d scripts\n%s", len(cp.scripts), strings.Join(cp.scripts, "\n---\n"))
	}
	if changes, err := RuntimeChanges(context.Background(), o); err != nil || changes["lab-cp-0"] != "" {
		t.Errorf("after: %v %v", changes, err)
	}
}

// On RHEL-family machines the repository file moves to the new minor, its exclude line still
// holding the packages, and dnf takes kubeadm, then kubelet and kubectl, to the release.
func TestUpgradeNodeRHEL(t *testing.T) {
	l := newLab()
	for _, m := range l.machines {
		m.os = rockyRelease
	}
	if err := Setup(context.Background(), l.options(Options{Version: "v1.36.6", Containerd: "2.2.1"})); err != nil {
		t.Fatal(err)
	}
	cp := l.machines["192.168.1.10"]
	cp.scripts = nil
	if err := UpgradeNode(context.Background(), l.options(Options{Version: "v1.37.2"}), nodes[0], true); err != nil {
		t.Fatal(err)
	}
	if len(cp.scripts) != 4 {
		t.Fatalf("%d scripts:\n%s", len(cp.scripts), strings.Join(cp.scripts, "\n---\n"))
	}
	for i, want := range [][]string{
		{"baseurl=https://pkgs.k8s.io/core:/stable:/v1.37/rpm/", "exclude=kubelet kubeadm kubectl cri-tools kubernetes-cni",
			"dnf install -y -q --setopt=disable_excludes=kubernetes --setopt=install_weak_deps=False kubeadm-1.37.2\n"},
		{"kubeadm upgrade apply v1.37.2 --yes"},
		{"containerd-2.3.7-linux-amd64.tar.gz"},
		{"kubelet-1.37.2 kubectl-1.37.2\n", "systemctl restart kubelet"},
	} {
		for _, w := range want {
			if !strings.Contains(cp.scripts[i], w) {
				t.Errorf("script %d lacks %q:\n%s", i, w, cp.scripts[i])
			}
		}
	}
	if strings.Contains(cp.scripts[0], "kubelet-") || strings.Contains(strings.Join(cp.scripts, ""), "apt") {
		t.Error("kubeadm alone first, and no apt on Rocky")
	}
	if cp.pkgs["kubelet"] != "1.37.2-150500.1.1" || cp.pkgs["kubeadm"] != "1.37.2-150500.1.1" || !strings.Contains(cp.kubeRepo, "/v1.37/rpm/") {
		t.Errorf("packages %v, repo %s", cp.pkgs, cp.kubeRepo)
	}
}

// A runtime tow didn't install that doesn't go with the new release stops the upgrade
// before anything changes.
func TestRuntimeChangesRefusesTheirs(t *testing.T) {
	l := newLab()
	for _, m := range l.machines {
		m.containerd = runtime{path: "/usr/bin/containerd", version: "1.7.24", pkg: "containerd.io", active: true, cri: true, systemd: true}
	}
	_, err := RuntimeChanges(context.Background(), l.options(Options{Version: "v1.37.2"}))
	if err == nil || !strings.Contains(err.Error(), "Kubernetes v1.37.2 can't go on 2 machines") {
		t.Errorf("err = %v", err)
	}
}

// The control-plane check asks the machine's own API server, its containers and etcd.
func TestHealthScript(t *testing.T) {
	s := HealthScript(runtimes.Containerd, providers.Node{Name: "cp", Host: "203.0.113.10", PrivateIP: "10.0.0.10", Role: config.RoleControlPlane})
	for _, want := range []string{"--server https://10.0.0.10:6443", "get --raw=/readyz", "kube-controller-manager kube-scheduler etcd", "endpoint health --cluster", "unix:///var/run/containerd/containerd.sock"} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %q", want)
		}
	}
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v\n%s", err, out)
	}
}
