package prepare

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
)

const header = "apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: lab}\n"

func cfg(t *testing.T, spec string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(header + spec + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const byoh = "infrastructure: {provider: byoh, access: {ssh: {user: u}}}, controlPlane: {machines: [{host: 10.0.0.1}]}, workers: [{machines: [{host: 10.0.0.2}]}]"

// A fresh Ubuntu server: nothing installed, swap on, ufw off.
func fresh() Facts {
	return Facts{
		OSID: "ubuntu", Kernel: "6.8.0-45-generic", AdminIP: "203.0.113.7", Complete: true,
		SwapActive: []string{"/swap.img"}, SwapInFstab: 1, SwapUnits: []string{"swap.img.swap"},
		SysctlNow: map[string]string{"net.ipv4.ip_forward": "0"}, SysctlSaved: map[string]string{},
		ModuleLoaded: map[string]bool{}, ModuleSaved: map[string]bool{}, Installed: map[string]bool{},
		UFW: "inactive", UFWRule: map[int]bool{}, MemoryCgroup: true, MAC: "aa:bb:cc:00:00:01",
	}
}

func summaries(changes []Change) string {
	var out []string
	for _, c := range changes {
		out = append(out, c.Summary)
	}
	return strings.Join(out, "\n")
}

// Every item kubeadm and flannel ask for, on a fresh machine.
func TestPlanKubeadmFlannel(t *testing.T) {
	c := cfg(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: flannel}}, "+byoh+"}")
	n := NeedsFor(c, config.RoleWorker)
	changes, problems, _ := planNode(n, fresh(), nil, firewalldWant{}, clusterInfo{name: "lab"})
	if len(problems) != 0 {
		t.Fatalf("problems: %v", problems)
	}
	got := summaries(changes)
	for _, want := range []string{
		"install ca-certificates, curl, gpg, iptables (the Kubernetes package repository (pkgs.k8s.io); hostPort: the CNI's portmap plugin runs iptables)",
		"load kernel module nf_conntrack (kube-proxy sets its connection tracking limits when it starts), br_netfilter (flannel requires the br_netfilter module), now and at boot",
		"set net.ipv4.ip_forward = 1 (IPv4 forwarding (Kubernetes container runtime docs))",
		"net.bridge.bridge-nf-call-iptables = 1",
		"turn swap off, now and at boot (kubeadm: the kubelet refuses to start with swap)",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, ch := range changes {
		if ch.Record.What == "" || ch.Record.Undo == "" {
			t.Errorf("every change says what it did and how to undo it: %+v", ch.Record)
		}
	}

	// The same machine once prepared: nothing left to do.
	f := fresh()
	f.SwapActive, f.SwapInFstab, f.SwapUnits = nil, 0, nil
	for _, k := range n.Sysctls {
		f.SysctlNow[k.Name], f.SysctlSaved[k.Name] = k.Value, k.Value
	}
	for _, m := range n.Modules {
		f.ModuleLoaded[m.Name], f.ModuleSaved[m.Name] = true, true
	}
	for _, p := range n.Packages {
		f.Installed[p.Name] = true
	}
	if changes, _, _ := planNode(n, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); len(changes) != 0 {
		t.Errorf("a prepared machine needs nothing: %s", summaries(changes))
	}
}

// k3s sets its own kernel modules and sysctls, and tolerates swap: only its installer's needs.
func TestPlanK3s(t *testing.T) {
	c := cfg(t, "spec: {flavor: homelab, "+byoh+"}")
	changes, _, _ := planNode(NeedsFor(c, config.RoleControlPlane), fresh(), nil, firewalldWant{}, clusterInfo{name: "lab"})
	if got := summaries(changes); got != "install ca-certificates, curl (the k3s installer (get.k3s.io))" {
		t.Errorf("k3s changes:\n%s", got)
	}
}

func TestPlanCalicoCilium(t *testing.T) {
	calico := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: calico}}, "+byoh+"}"), config.RoleWorker)
	old := fresh()
	old.Kernel = "5.4.0-150-generic"
	_, problems, _ := planNode(calico, old, nil, firewalldWant{}, clusterInfo{name: "lab"})
	if len(problems) != 1 || !strings.Contains(problems[0].What, "kernel 5.4.0-150-generic is older than 5.10, the oldest Calico supports") {
		t.Errorf("problems: %+v", problems)
	}

	f := fresh()
	f.NetworkManager, f.UFW = true, "active"
	changes, _, warnings := planNode(calico, f, nil, firewalldWant{}, clusterInfo{name: "lab"})
	if !strings.Contains(summaries(changes), "tell NetworkManager to leave Calico's interfaces alone") {
		t.Errorf("NetworkManager change missing:\n%s", summaries(changes))
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "Calico's docs ask for no other iptables manager") {
		t.Errorf("warnings: %v", warnings)
	}
	for _, ch := range changes {
		if strings.Contains(ch.Summary, "NetworkManager") && !strings.Contains(ch.Script, "unmanaged-devices=interface-name:cali*;interface-name:tunl*;interface-name:vxlan.calico;interface-name:vxlan-v6.calico;interface-name:wireguard.cali;interface-name:wg-v6.cali") {
			t.Errorf("Calico's documented setting expected:\n%s", ch.Script)
		}
	}

	cilium := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: k3s, networking: {cni: cilium}}, "+byoh+"}"), config.RoleWorker)
	if _, problems, _ := planNode(cilium, old, nil, firewalldWant{}, clusterInfo{name: "lab"}); len(problems) != 1 || !strings.Contains(problems[0].What, "Cilium") {
		t.Errorf("cilium kernel check: %+v", problems)
	}
}

// A service load balancer's pool is reachable from this machine, any port: ufw takes CIDRs.
func TestPoolFirewall(t *testing.T) {
	c := cfg(t, "spec: {kubernetes: {distribution: kubeadm, serviceLoadBalancer: {provider: metallb, metallb: {addresses: [192.168.1.200-192.168.1.210]}}}, "+byoh+"}")
	var got []string
	for _, r := range ufwRules(NeedsFor(c, config.RoleWorker), clusterInfo{name: "lab"}) {
		if strings.Contains(r.args, " to 192.168.1.2") {
			got = append(got, r.args)
		}
	}
	want := []string{
		`allow from "$ADMIN" to 192.168.1.200/29 comment 'tow:lab'`,
		`allow from "$ADMIN" to 192.168.1.208/31 comment 'tow:lab'`,
		`allow from "$ADMIN" to 192.168.1.210/32 comment 'tow:lab'`,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules:\n%s", strings.Join(got, "\n"))
	}
	if cidrs := PoolCIDRs(c.Spec.Kubernetes.ServiceLoadBalancer); len(cidrs) != 3 {
		t.Errorf("pool CIDRs %v", cidrs)
	}
}

// Longhorn: multipathd kept off its volumes with Longhorn's own blacklist, unless multipath
// manages devices there (a SAN), which only the user can sort out.
func TestPlanLonghornMultipath(t *testing.T) {
	longhorn := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: k3s, storage: {csi: [longhorn]}}, "+byoh+"}"), config.RoleWorker)
	if longhorn.QuietMultipath != "Longhorn" || !strings.Contains(strings.Join(Describe(longhorn), "\n"), "sd* disks blacklisted in /etc/multipath/conf.d/longhorn.conf") {
		t.Errorf("needs: %+v", longhorn)
	}

	f := fresh()
	f.Multipathd = true
	changes, problems, _ := planNode(longhorn, f, nil, firewalldWant{}, clusterInfo{name: "lab"})
	var script string
	for _, ch := range changes {
		if strings.Contains(ch.Summary, "multipathd") {
			script = ch.Script + ch.Record.Undo
		}
	}
	if len(problems) != 0 || !strings.Contains(script, `devnode "^sd[a-z0-9]+"`) || !strings.Contains(script, "systemctl restart multipathd") || !strings.Contains(script, "sudo rm /etc/multipath/conf.d/longhorn.conf") {
		t.Errorf("problems %+v, script:\n%s", problems, script)
	}

	f.MultipathConf = true
	if changes, _, _ := planNode(longhorn, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); strings.Contains(summaries(changes), "multipathd") {
		t.Error("written once")
	}
	f.MultipathConf, f.MultipathMaps = false, 2
	if _, problems, _ := planNode(longhorn, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); len(problems) != 1 || !strings.Contains(problems[0].What, "multipath manages 2 device(s) here") {
		t.Errorf("problems: %+v", problems)
	}
	f.Multipathd, f.MultipathMaps = false, 0
	if changes, _, _ := planNode(longhorn, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); strings.Contains(summaries(changes), "multipathd") {
		t.Error("nothing to do without multipathd")
	}
}

func TestPlanProblems(t *testing.T) {
	n := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: k3s}, "+byoh+"}"), config.RoleWorker)

	f := fresh()
	f.MemoryCgroup, f.PiCmdline = false, "/boot/firmware/cmdline.txt"
	if _, p, _ := planNode(n, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); len(p) != 1 || !strings.Contains(p[0].Hint, "add 'cgroup_memory=1 cgroup_enable=memory' to the end of the line in /boot/firmware/cmdline.txt, then reboot") {
		t.Errorf("Raspberry Pi cgroups: %+v", p)
	}

}

// RHEL 9 and 10 and their rebuilds are prepared with dnf; older releases and other systems
// are refused with the reason. kubeadm on them comes later.
func TestPlanFamilies(t *testing.T) {
	n := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: k3s}, "+byoh+"}"), config.RoleWorker)
	for _, tt := range []struct {
		id, like, version, problem string
	}{
		{"rocky", "rhel centos fedora", "10.0", ""},
		{"almalinux", "rhel centos fedora", "9.6", ""},
		{"rhel", "fedora", "9.4", ""},
		{"ol", "rhel fedora", "9.5", ""}, // a rebuild that says it's like RHEL
		{"rocky", "rhel centos fedora", "8.10", "rocky 8.10 is older than tow supports"},
		{"fedora", "", "41", `"fedora" isn't a system tow prepares`},
		{"amzn", "centos rhel fedora", "2023", ""}, // Amazon Linux 2023 calls itself like RHEL, and is 2023
		{"", "", "", "no /etc/os-release"},
		{"linuxmint", "ubuntu debian", "22", ""},
	} {
		f := fresh()
		f.OSID, f.OSLike, f.OSVersion = tt.id, tt.like, tt.version
		_, p, _ := planNode(n, f, nil, firewalldWant{}, clusterInfo{name: "lab"})
		switch {
		case tt.problem == "" && len(p) != 0:
			t.Errorf("%s %s: %+v", tt.id, tt.version, p)
		case tt.problem != "" && (len(p) != 1 || !strings.Contains(p[0].What, tt.problem)):
			t.Errorf("%s %s: %+v, want %q", tt.id, tt.version, p, tt.problem)
		}
	}

}

// kubeadm sets SELinux to permissive, now and at boot, as its install page does on Red
// Hat-based systems; the undo enforces again as it did. k3s leaves it enforcing.
func TestPlanSELinux(t *testing.T) {
	kubeadm := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: kubeadm}, "+byoh+"}"), config.RoleWorker)
	f := rocky10()
	f.SELinux, f.SELinuxConfig = "Enforcing", "enforcing"
	changes, p, _ := planNode(kubeadm, f, nil, firewalldWant{}, clusterInfo{name: "lab"})
	var se *Change
	for i := range changes {
		if changes[i].Record.What == "SELinux" {
			se = &changes[i]
		}
	}
	if len(p) != 0 || se == nil || se.Script != "setenforce 0\nsed -i 's/^SELINUX=enforcing$/SELINUX=permissive/' /etc/selinux/config\n" ||
		se.Record.Revert != "setenforce 1 || true\nsed -i 's/^SELINUX=permissive$/SELINUX=enforcing/' /etc/selinux/config\n" {
		t.Fatalf("problems %+v, change %+v", p, se)
	}
	// Only what enforced is undone: here SELinux enforced at boot but not now.
	f.SELinux = "Permissive"
	changes, _, _ = planNode(kubeadm, f, nil, firewalldWant{}, clusterInfo{name: "lab"})
	for _, c := range changes {
		if c.Record.What == "SELinux" && (strings.Contains(c.Script, "setenforce") || strings.Contains(c.Record.Revert, "setenforce")) {
			t.Errorf("permissive now: %+v", c)
		}
	}
	f.SELinuxConfig = "permissive"
	if changes, _, _ := planNode(kubeadm, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); strings.Contains(summaries(changes), "SELinux") {
		t.Errorf("already permissive: %s", summaries(changes))
	}
	// A kubeadm control plane under kubeadm's 1700 MB is refused before anything changes, with
	// RHEL's kdump reservation named; a worker isn't checked.
	cp := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: kubeadm}, "+byoh+"}"), config.RoleControlPlane)
	f.MemoryMB = 1649
	if _, p, _ := planNode(cp, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); len(p) != 1 || !strings.Contains(p[0].What, "1649 MB of memory: a kubeadm control plane needs at least 1700 MB") || !strings.Contains(p[0].Hint, "kdump") {
		t.Errorf("small control plane: %+v", p)
	}
	if _, p, _ := planNode(kubeadm, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); len(p) != 0 {
		t.Errorf("worker: %+v", p)
	}
	f.MemoryMB = 1900
	if _, p, _ := planNode(cp, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); len(p) != 0 {
		t.Errorf("1900 MB: %+v", p)
	}
	k3s := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: k3s}, "+byoh+"}"), config.RoleWorker)
	f.SELinux, f.SELinuxConfig = "Enforcing", "enforcing"
	if changes, _, _ := planNode(k3s, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); strings.Contains(summaries(changes), "SELinux") {
		t.Errorf("k3s keeps SELinux: %s", summaries(changes))
	}
}

// A fresh Rocky Linux 10 cloud image: SELinux on, firewalld off, curl there.
func rocky10() Facts {
	f := fresh()
	f.OSID, f.OSLike, f.OSVersion, f.Kernel = "rocky", "rhel centos fedora", "10.0", "6.12.0-55.9.1.el10_0.x86_64"
	f.SwapActive, f.SwapInFstab, f.SwapUnits = nil, 0, nil
	f.Installed = map[string]bool{"ca-certificates": true, "curl": true}
	f.UFW = "none"
	return f
}

// kubeadm with Calico or flannel needs the machine's iptables (portmap) and nf_conntrack loaded
// (kube-proxy); k3s brings both, and a CNI that replaces kube-proxy needs no nf_conntrack.
func TestPlanKubeadmNetfilter(t *testing.T) {
	plan := func(spec string) string {
		n := NeedsFor(cfg(t, "spec: {kubernetes: "+spec+", "+byoh+"}"), config.RoleWorker)
		changes, _, _ := planNode(n, rocky10(), nil, firewalldWant{}, clusterInfo{name: "lab"})
		var all []string
		for _, c := range changes {
			all = append(all, c.Script)
		}
		return strings.Join(all, "\n")
	}
	got := plan("{distribution: kubeadm, networking: {cni: calico}}")
	if !strings.Contains(got, " iptables-nft") || !strings.Contains(got, "modprobe nf_conntrack\n") {
		t.Errorf("kubeadm + Calico on RHEL:\n%s", got)
	}
	got = plan("{distribution: kubeadm, networking: {cni: cilium, cilium: {kubeProxyReplacement: true}}}")
	if strings.Contains(got, "iptables") || strings.Contains(got, "nf_conntrack") {
		t.Errorf("kubeadm + Cilium without kube-proxy:\n%s", got)
	}
	got = plan("{distribution: k3s, networking: {cni: calico}}")
	if strings.Contains(got, "iptables") || strings.Contains(got, "nf_conntrack") {
		t.Errorf("k3s + Calico:\n%s", got)
	}
}

// k3s and kubeadm on RHEL 10 need kernel-modules-extra, built for the running kernel; Longhorn's packages
// take their RHEL names. Each is installed with dnf and removed again by its record.
func TestPlanRHELPackages(t *testing.T) {
	n := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: k3s, storage: {csi: [longhorn]}}, "+byoh+"}"), config.RoleWorker)
	changes, problems, _ := planNode(n, rocky10(), nil, firewalldWant{}, clusterInfo{name: "lab"})
	if len(problems) != 0 {
		t.Fatalf("problems: %+v", problems)
	}
	var install *Change
	for i := range changes {
		if changes[i].Record.What == "packages" {
			install = &changes[i]
		}
	}
	const spec = "kernel-modules-extra-6.12.0-55.9.1.el10_0.x86_64"
	if install == nil || install.Script != "dnf install -y -q "+spec+" iscsi-initiator-utils nfs-utils" ||
		!strings.Contains(install.Summary, "the network plugins' kernel modules on RHEL 10 (k3s docs)") ||
		!strings.Contains(install.Record.Revert, "p="+spec+"; rpm -q --quiet") || !strings.Contains(install.Record.Revert, "dnf remove -y -q $pkgs") {
		t.Fatalf("install: %+v", install)
	}

	// RHEL 9: no kernel-modules-extra; once installed, nothing to do.
	f := rocky10()
	f.OSVersion = "9.6"
	f.Installed["iscsi-initiator-utils"], f.Installed["nfs-utils"] = true, true
	if changes, _, _ := planNode(n, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); strings.Contains(summaries(changes), "install") {
		t.Errorf("RHEL 9, packages there:\n%s", summaries(changes))
	}
	f.OSVersion = "10.1"
	f.Installed["kernel-modules-extra"] = true
	if changes, _, _ := planNode(n, f, nil, firewalldWant{}, clusterInfo{name: "lab"}); strings.Contains(summaries(changes), "install") {
		t.Errorf("RHEL 10, packages there:\n%s", summaries(changes))
	}

	// The revert script, against stand-ins for rpm (one package still there) and dnf.
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "rpm"), []byte("#!/bin/sh\n[ \"$3\" = iscsi-initiator-utils ]\n"), 0o755)
	os.WriteFile(filepath.Join(bin, "dnf"), []byte("#!/bin/sh\necho \"dnf $*\" >> "+root+"/log\n"), 0o755)
	cmd := exec.Command("sh", "-c", "set -e\n"+install.Record.Revert)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if log, _ := os.ReadFile(root + "/log"); strings.TrimSpace(string(log)) != "dnf remove -y -q iscsi-initiator-utils" {
		t.Errorf("dnf ran: %q", log)
	}
}

// The packages a node may need are asked of dpkg on Debian-family machines and of rpm on the
// others, the kernel's own build for the running kernel.
func TestPackageQuery(t *testing.T) {
	n := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: k3s, storage: {csi: [longhorn]}}, "+byoh+"}"), config.RoleWorker)
	q := packageQuery(n.Packages)
	for _, want := range []string{
		"dpkg-query -W -f='package:${Package}=${db:Status-Status}\\n' ca-certificates curl open-iscsi nfs-common 2>/dev/null",
		`echo "package:kernel-modules-extra=$(rpm -q --quiet kernel-modules-extra-$(uname -r) && echo installed)"`,
		`echo "package:iscsi-initiator-utils=$(rpm -q --quiet iscsi-initiator-utils && echo installed)"`,
	} {
		if !strings.Contains(q, want) {
			t.Errorf("missing %q in:\n%s", want, q)
		}
	}
	if out, err := exec.Command("sh", "-n", "-c", q).CombinedOutput(); err != nil {
		t.Errorf("%v\n%s", err, out)
	}
}

// With ufw on, only the rules it doesn't have yet are added, all tagged with the cluster.
func TestPlanUFW(t *testing.T) {
	c := cfg(t, "spec: {flavor: homelab, "+byoh+"}")
	n := NeedsFor(c, config.RoleControlPlane)
	info := clusterInfo{name: "lab", peers: []string{"10.0.0.2"}, podCIDR: "10.42.0.0/16", serviceCIDR: "10.43.0.0/16"}
	rules := ufwRules(n, info)
	var args []string
	for _, r := range rules {
		args = append(args, r.args)
	}
	want := []string{
		"allow from 10.0.0.2 comment 'tow:lab'",
		"allow from 10.42.0.0/16 comment 'tow:lab'",
		"allow from 10.43.0.0/16 comment 'tow:lab'",
		`allow proto tcp from "$ADMIN" to any port 6443 comment 'tow:lab'`,
		`allow proto tcp from "$ADMIN" to any port 30000:32767 comment 'tow:lab'`,
		`allow proto udp from "$ADMIN" to any port 30000:32767 comment 'tow:lab'`,
		`allow proto tcp from "$ADMIN" to any port 80 comment 'tow:lab'`,
		`allow proto tcp from "$ADMIN" to any port 443 comment 'tow:lab'`,
	}
	if strings.Join(args, "\n") != strings.Join(want, "\n") {
		t.Errorf("rules =\n%s\nwant\n%s", strings.Join(args, "\n"), strings.Join(want, "\n"))
	}

	// The file's adminCIDRs replace this machine's address; access.ports open too.
	c = cfg(t, "spec: {flavor: homelab, kubernetes: {distribution: k3s}, infrastructure: {provider: byoh, access: {ssh: {user: pi}, adminCIDRs: [192.168.1.0/24, 10.8.0.0/16], ports: [8080]}}, controlPlane: {machines: [{host: 192.168.1.10}]}}")
	if got := OpenPorts(c); !slices.Equal(got, []int{80, 443, 8080}) {
		t.Errorf("open ports %v", got)
	}
	var wide []string
	for _, r := range ufwRules(NeedsFor(c, config.RoleWorker), clusterInfo{name: "lab", admin: c.Spec.Infrastructure.Access.AdminCIDRs}) {
		if strings.Contains(r.args, "8080") || strings.Contains(r.args, "$ADMIN") {
			wide = append(wide, r.args)
		}
	}
	if strings.Join(wide, "\n") != "allow proto tcp from 192.168.1.0/24 to any port 8080 comment 'tow:lab'\nallow proto tcp from 10.8.0.0/16 to any port 8080 comment 'tow:lab'" {
		t.Errorf("rules:\n%s", strings.Join(wide, "\n"))
	}

	f := fresh()
	f.UFW = "active"
	for i := range rules {
		f.UFWRule[i] = i != 0 && i != 3 // two missing
	}
	changes, _, _ := planNode(n, f, rules, firewalldWant{}, info)
	var fw *Change
	for i := range changes {
		if changes[i].Record.What == "firewall" {
			fw = &changes[i]
		}
	}
	if fw == nil {
		t.Fatalf("no firewall change in:\n%s", summaries(changes))
	}
	if !strings.Contains(fw.Summary, "allow from 10.0.0.2; allow proto tcp from 203.0.113.7 to any port 6443") || strings.Count(fw.Script, "ufw allow") != 2 {
		t.Errorf("firewall change:\n%s\n%s", fw.Summary, fw.Script)
	}
	f.UFW = "inactive"
	for _, ch := range changes2(n, f, rules, info) {
		if ch.Record.What == "firewall" {
			t.Error("ufw off: nothing to open")
		}
	}
}

func changes2(n Needs, f Facts, rules []ufwRule, info clusterInfo) []Change {
	c, _, _ := planNode(n, f, rules, firewalldWanted(n, info), info)
	return c
}

func TestPlanLonghorn(t *testing.T) {
	n := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: cilium}, storage: {csi: [longhorn]}}, "+byoh+"}"), config.RoleWorker)
	got := summaries(changes2(n, fresh(), nil, clusterInfo{name: "lab"}))
	if !strings.Contains(got, "open-iscsi, nfs-common") || !strings.Contains(got, "iscsi_tcp (Longhorn (loaded before iscsid starts)), now and at boot") {
		t.Errorf("Longhorn changes:\n%s", got)
	}
}

func TestKernelAtLeast(t *testing.T) {
	for _, tt := range []struct {
		release, min string
		want         bool
	}{
		{"6.8.0-45-generic", "5.10", true}, {"5.10.0-28-amd64", "5.10", true}, {"5.4.0-150-generic", "5.10", false},
		{"5.15.0-1023-aws", "5.10", true}, {"4.19.0-26-amd64", "5.10", false}, {"6.1.21-v8+", "5.10", true},
	} {
		if got := kernelAtLeast(tt.release, tt.min); got != tt.want {
			t.Errorf("kernelAtLeast(%s, %s) = %v", tt.release, tt.min, got)
		}
	}
}

// The generated shell must at least parse, and the read-only check must run to the end in an
// ordinary shell (as a normal user here, so some values stay empty).
func TestScriptsParse(t *testing.T) {
	c := cfg(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: calico}, storage: {csi: [longhorn]}, ingress: {controllers: [traefik]}}, "+byoh+"}")
	n := NeedsFor(c, config.RoleControlPlane)
	info := clusterInfo{name: "lab", peers: []string{"10.0.0.2"}, podCIDR: "10.244.0.0/16", serviceCIDR: "10.96.0.0/12"}
	rules := ufwRules(n, info)
	fw := firewalldWanted(n, info)
	f := fresh()
	f.UFW, f.NetworkManager = "active", true
	changes, _, _ := planNode(n, f, rules, fw, info)
	record, err := RecordScript(nil, []Record{{Cluster: "lab", What: "swap", Detail: "d", Undo: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{"facts": factsScript(n, rules, fw, "lab"), "record": record, "ufw delete": ufwDeleteScript([]string{"allow from 203.0.113.7 to any port 22 proto tcp"}),
		"firewalld revert": firewalldRevert, "rpm remove": RemoveRPMs([]string{"curl", "kernel-modules-extra-6.12.0-55.el10.x86_64"})}
	// firewalld's changes, on a Rocky machine where it runs.
	r := rocky10()
	r.Firewalld, r.FWVersion = true, "1.3.4"
	k3s := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: k3s, networking: {cni: calico}, storage: {csi: [longhorn]}, ingress: {controllers: [traefik]}}, "+byoh+"}"), config.RoleControlPlane)
	fwChanges, _, _ := planNode(k3s, r, nil, firewalldWanted(k3s, info), info)
	if len(fwChanges) == 0 {
		t.Error("no changes on Rocky")
	}
	for i, ch := range fwChanges {
		scripts[fmt.Sprintf("rocky %d %s", i, ch.Record.What)] = "set -e\n" + ch.Script
	}
	for _, ch := range changes {
		scripts[ch.Record.What] = "set -e\n" + ch.Script
	}
	for name, s := range scripts {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s script doesn't parse: %v\n%s\n%s", name, err, out, s)
		}
	}

	out, err := exec.Command("sh", "-c", factsScript(n, rules, fw, "lab"), "sh", "203.0.113.7").CombinedOutput()
	f = parseFacts(string(out))
	if !f.Complete || f.Kernel == "" || f.AdminIP != "203.0.113.7" {
		t.Errorf("facts script on this machine: %v, facts %+v\n%s", err, f, out)
	}
}

// A record file that can't be read is kept under a name of its own: the next writes (two in
// a row here, as preparation then kubeadm do) never overwrite it.
func TestUnreadableRecordIsKept(t *testing.T) {
	dir := t.TempDir()
	run := func(existing []byte, r Record) {
		t.Helper()
		s, err := RecordScript(existing, []Record{r})
		if err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("sh", "-c", "set -e\n"+strings.ReplaceAll(s, "/etc/tow", dir)).CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	corrupt := []byte(`{"changes": [{"cluster": "lab", "what": "swap", "undo": "swapon -a"`)
	if err := os.WriteFile(dir+"/prepared.json", corrupt, 0o644); err != nil {
		t.Fatal(err)
	}
	run(corrupt, Record{Cluster: "lab", What: "kernel modules", Undo: "u"})
	now, _ := os.ReadFile(dir + "/prepared.json")
	run(now, Record{Cluster: "lab", What: "containerd", Undo: "u"})

	kept, _ := filepath.Glob(dir + "/prepared.json.unreadable-*")
	if len(kept) != 1 {
		t.Fatalf("kept: %v", kept)
	}
	if b, _ := os.ReadFile(kept[0]); string(b) != string(corrupt) {
		t.Errorf("kept %q", b)
	}
	if b, _ := os.ReadFile(dir + "/prepared.json"); !strings.Contains(string(b), `"what": "containerd"`) || !strings.Contains(string(b), `"what": "kernel modules"`) {
		t.Errorf("record now: %s", b)
	}
	// Root-only, the copies too, although the file found was readable by everyone.
	for _, f := range append(kept, dir+"/prepared.json", dir+"/prepared.json.prev") {
		if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("%s: %v %v, want 0600", f, st.Mode().Perm(), err)
		}
	}
}

// A module listed for boot is found even when some of the places looked in don't exist, as
// /run/modules-load.d on AWS's Ubuntu (grep then exits 2 unless -q, found line or not).
func TestModuleSavedWithMissingFiles(t *testing.T) {
	n := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: kubeadm, networking: {cni: flannel}}, "+byoh+"}"), config.RoleControlPlane)
	var line string
	for _, l := range strings.Split(factsScript(n, nil, firewalldWant{}, "lab"), "\n") {
		if strings.Contains(l, "module_saved:br_netfilter=") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("flannel's facts don't ask whether br_netfilter is listed for boot")
	}
	root := t.TempDir()
	if err := os.MkdirAll(root+"/etc/modules-load.d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(root+"/etc/modules-load.d/k8s.conf", []byte("overlay\nbr_netfilter\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/etc/modules ", "/etc/modules-load.d/", "/run/modules-load.d/", "/usr/lib/modules-load.d/"} {
		line = strings.ReplaceAll(line, p, root+p)
	}
	out, err := exec.Command("sh", "-c", line).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "module_saved:br_netfilter=yes" {
		t.Errorf("%q, %v\n%s", out, err, line)
	}
}

// fakeNode answers the read-only check with its facts and records every other script.
type fakeNode struct {
	mu      sync.Mutex
	facts   string // what the check prints
	after   string // what it prints once changes ran ("" = same as before)
	scripts []string
	failOn  string
}

func (m *fakeNode) RunInput(_ context.Context, cmd, stdin string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cmd != asRoot {
		return "", errors.New("everything runs as root through stdin")
	}
	if strings.HasPrefix(stdin, "# tow: read-only") {
		if len(m.scripts) > 0 && m.after != "" {
			return m.after, nil
		}
		return m.facts, nil
	}
	m.scripts = append(m.scripts, stdin)
	if m.failOn != "" && strings.Contains(stdin, m.failOn) {
		return "E: Unable to locate package", errors.New("exit status 100")
	}
	return "", nil
}
func (m *fakeNode) Close() error { return nil }

const freshOut = `os_id=ubuntu
kernel=6.8.0-45-generic
admin_ip=203.0.113.7
swap_active=/swap.img
swap_fstab=1
swap_units=swap.img.swap
sysctl_now:net.ipv4.ip_forward=0
sysctl_saved:net.ipv4.ip_forward=
ufw=inactive
memory_cgroup=yes
product_uuid=UUID-1
mac=aa:bb:cc:00:00:01
done=yes`

const preparedOut = `os_id=ubuntu
kernel=6.8.0-45-generic
swap_active=
swap_fstab=0
sysctl_now:net.ipv4.ip_forward=1
sysctl_saved:net.ipv4.ip_forward=1
module_now:nf_conntrack=yes
module_saved:nf_conntrack=yes
package:ca-certificates=installed
package:curl=installed
package:gpg=installed
ufw=inactive
memory_cgroup=yes
done=yes`

func runOn(t *testing.T, machines map[string]*fakeNode, confirm func(string) bool) error {
	t.Helper()
	c := cfg(t, "spec: {kubernetes: {distribution: kubeadm}, "+byoh+"}")
	var nodes []providers.Node
	for _, host := range []string{"10.0.0.1", "10.0.0.2"} {
		role := config.RoleWorker
		if host == "10.0.0.1" {
			role = config.RoleControlPlane
		}
		nodes = append(nodes, providers.Node{Host: host, PrivateIP: host, Role: role, User: "u"})
	}
	return Run(context.Background(), Options{
		Cluster: "lab", Nodes: nodes, Distro: config.DistributionKubeadm, PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12",
		Needs:   func(role string) Needs { return NeedsFor(c, role) },
		Confirm: confirm,
		Connect: func(_ context.Context, n providers.Node, _ string) (Runner, error) { return machines[n.Host], nil },
	})
}

func TestRun(t *testing.T) {
	m := map[string]*fakeNode{
		"10.0.0.1": {facts: freshOut, after: preparedOut},
		"10.0.0.2": {facts: strings.ReplaceAll(strings.ReplaceAll(freshOut, "UUID-1", "UUID-2"), ":01", ":02"), after: preparedOut},
	}
	var asked string
	if err := runOn(t, m, func(s string) bool { asked = s; return true }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked, "10.0.0.1:\n  - install ca-certificates, curl, gpg") || !strings.Contains(asked, "turn swap off") {
		t.Errorf("the question lists every change per machine:\n%s", asked)
	}
	for host, node := range m {
		all := strings.Join(node.scripts, "\n---\n")
		if !strings.Contains(all, "apt-get") || !strings.Contains(all, "swapoff -a") || !strings.Contains(all, "net.ipv4.ip_forward = 1") {
			t.Errorf("%s: changes not run:\n%s", host, all)
		}
		last := node.scripts[len(node.scripts)-1]
		const open = "<<'TOW_EOF'\n"
		body := last[strings.Index(last, open)+len(open) : strings.LastIndex(last, "\nTOW_EOF")]
		var l ledger
		if err := json.Unmarshal([]byte(body), &l); err != nil || len(l.Changes) != 4 || l.Changes[0].Cluster != "lab" {
			t.Errorf("%s: record = %v, %+v", host, err, l)
		}
	}
}

func TestRunStopsBeforeChanging(t *testing.T) {
	// Declined: nothing runs.
	m := map[string]*fakeNode{"10.0.0.1": {facts: freshOut}, "10.0.0.2": {facts: strings.ReplaceAll(freshOut, ":01", ":02")}}
	if err := runOn(t, m, func(string) bool { return false }); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("declined: %v", err)
	}
	if len(m["10.0.0.1"].scripts)+len(m["10.0.0.2"].scripts) != 0 {
		t.Error("declining must change nothing")
	}

	// Cloned VMs: same MAC and product_uuid, refused before anything changes.
	m = map[string]*fakeNode{"10.0.0.1": {facts: freshOut}, "10.0.0.2": {facts: freshOut}}
	err := runOn(t, m, func(string) bool { t.Error("must not ask"); return true })
	if err == nil || !strings.Contains(err.Error(), "same MAC address as 10.0.0.1") || !strings.Contains(err.Error(), "same product_uuid as 10.0.0.1") {
		t.Errorf("clones: %v", err)
	}

	// A change that fails is reported with the tool's own output, and the rest stops.
	m = map[string]*fakeNode{"10.0.0.1": {facts: freshOut, failOn: "apt-get"}, "10.0.0.2": {facts: strings.ReplaceAll(strings.ReplaceAll(freshOut, "UUID-1", "UUID-2"), ":01", ":02"), after: preparedOut}}
	err = runOn(t, m, func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "could not prepare 10.0.0.1: install ca-certificates") || !strings.Contains(err.Error(), "Unable to locate package") {
		t.Errorf("failure: %v", err)
	}

	// sudo with a password: said plainly.
	m = map[string]*fakeNode{"10.0.0.1": {facts: "sudo: a password is required"}, "10.0.0.2": {facts: preparedOut}}
	if err := runOn(t, m, nil); err == nil || !strings.Contains(err.Error(), "sudo needs a password") {
		t.Errorf("sudo: %v", err)
	}
}

// What comes after preparation checks the machines too (kubeadm: a container runtime already
// there), and its problems stop everything before preparation changes anything.
func TestRunPreflight(t *testing.T) {
	m := map[string]*fakeNode{"10.0.0.1": {facts: freshOut}, "10.0.0.2": {facts: strings.ReplaceAll(strings.ReplaceAll(freshOut, "UUID-1", "UUID-2"), ":01", ":02")}}
	c := cfg(t, "spec: {kubernetes: {distribution: kubeadm}, "+byoh+"}")
	var checked []string
	var mu sync.Mutex
	err := Run(context.Background(), Options{
		Cluster: "lab", Distro: config.DistributionKubeadm, PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12",
		Nodes:   []providers.Node{{Host: "10.0.0.1", PrivateIP: "10.0.0.1", Role: config.RoleControlPlane}, {Host: "10.0.0.2", PrivateIP: "10.0.0.2", Role: config.RoleWorker}},
		Needs:   func(role string) Needs { return NeedsFor(c, role) },
		Confirm: func(string) bool { t.Error("must not ask"); return true },
		Connect: func(_ context.Context, n providers.Node, _ string) (Runner, error) { return m[n.Host], nil },
		Preflight: func(_ context.Context, r Runner, n providers.Node) ([]Problem, error) {
			mu.Lock()
			checked = append(checked, n.Host)
			mu.Unlock()
			if n.Host == "10.0.0.2" {
				return []Problem{{What: "containerd 1.7.28 (package containerd.io) is already here", Hint: "upgrade it yourself"}}, nil
			}
			return nil, nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "10.0.0.2, worker:\n  ✗ containerd 1.7.28 (package containerd.io) is already here\n    → upgrade it yourself") ||
		!strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("err = %v", err)
	}
	if len(checked) != 2 || len(m["10.0.0.1"].scripts)+len(m["10.0.0.2"].scripts) != 0 {
		t.Errorf("checked %v; nothing may change", checked)
	}
}
