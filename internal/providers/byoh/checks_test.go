package byoh

import (
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
)

// goodFacts is a machine that passes every check for any role and engine.
func goodFacts() facts {
	return facts{
		"kernel": "Linux", "arch": "x86_64", "os_id": "ubuntu", "os_name": "Ubuntu 24.04 LTS",
		"cpus": "2", "mem_kb": strconv.Itoa(1950 * 1024), // a "2 GB" machine as the kernel reports it
		"disk_kb": strconv.Itoa(15 * 1024 * 1024), "systemd": "yes", "sudo": "yes",
		"hostname": "node-1", "machine_id": "abc", "ip": "10.0.0.1",
		"listening": "22 53", "probe_tools": "yes", "internet": "ok",
	}
}

func checks(fs []finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.check)
	}
	return out
}

func mustReq(t *testing.T, engine, role, cni string) requirements {
	t.Helper()
	r, err := requirementsFor(engine, role, cni)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The published minimums: k3s server 2 CPU / 2 GB, agent 1 CPU / 512 MB; kubeadm 2 GB
// everywhere, 2 CPUs on control planes.
func TestRequirementsMatchPublishedMinimums(t *testing.T) {
	tests := []struct {
		engine, role string
		cpus, memMB  int
	}{
		{config.DistributionK3s, config.RoleControlPlane, 2, 2048},
		{config.DistributionK3s, config.RoleWorker, 1, 512},
		{config.DistributionKubeadm, config.RoleControlPlane, 2, 2048},
		{config.DistributionKubeadm, config.RoleWorker, 1, 2048},
	}
	for _, tt := range tests {
		r := mustReq(t, tt.engine, tt.role, "")
		if r.cpus != tt.cpus || r.memMB != tt.memMB {
			t.Errorf("%s %s: %d CPU / %d MB, want %d / %d", tt.engine, tt.role, r.cpus, r.memMB, tt.cpus, tt.memMB)
		}
	}
	if minMemMB(2048) != 1699 {
		t.Errorf("a 2 GB machine must pass with ~1700 MB reported, like kubeadm's preflight; threshold = %d", minMemMB(2048))
	}
	if _, err := requirementsFor("nomad", config.RoleWorker, ""); err == nil {
		t.Error("unknown engine should be an error")
	}
}

// Without a CNI, only the distribution's own ports are checked: nothing installs Calico's or
// Cilium's, so nothing needs them open.
func TestNoCNINoCNIPorts(t *testing.T) {
	none := mustReq(t, config.DistributionKubeadm, config.RoleWorker, config.CNINone)
	if !slices.Equal(none.ports, []int{10250, 10256}) {
		t.Errorf("cni none: ports %v, want kubeadm's own only", none.ports)
	}
	if calico := mustReq(t, config.DistributionKubeadm, config.RoleWorker, config.CNICalico); !slices.Contains(calico.ports, 179) {
		t.Errorf("calico needs BGP (179): %v", calico.ports)
	}
	if cniUDPNote[config.CNINone] != "" {
		t.Error("no CNI, no overlay traffic to mention")
	}
}

func TestEvaluate(t *testing.T) {
	cp := mustReq(t, config.DistributionKubeadm, config.RoleControlPlane, "calico")

	if got := evaluate(goodFacts(), cp); len(got) != 0 {
		t.Fatalf("good machine: %+v", got)
	}

	tests := []struct {
		name   string
		change facts
		want   string
		warn   bool
	}{
		{"one cpu", facts{"cpus": "1"}, "cpu", false},
		{"1 GB of RAM", facts{"mem_kb": strconv.Itoa(1000 * 1024)}, "memory", false},
		{"small disk", facts{"disk_kb": strconv.Itoa(3 * 1024 * 1024)}, "disk", false},
		{"no sudo", facts{"sudo": "no"}, "sudo", false},
		{"root is fine", facts{"sudo": "root"}, "", false},
		{"no internet", facts{"internet": "fail https://pkgs.k8s.io"}, "internet", false},
		{"no curl or wget", facts{"internet": "notool"}, "internet", false},
		{"api port taken", facts{"listening": "22 6443"}, "ports", false},
		{"calico bgp port taken", facts{"listening": "179"}, "ports", false},
		{"armv7 on kubeadm", facts{"arch": "armv7l"}, "arch", false},
		{"no systemd", facts{"systemd": "no"}, "systemd", false},
		{"bad hostname", facts{"hostname": "my_node"}, "hostname", false},
		{"no default route", facts{"ip": ""}, "network", false},
		{"RHEL family 9 is fine", facts{"os_id": "rocky", "os_like": "rhel centos fedora", "os_version": "9.6", "os_name": "Rocky Linux 9.6"}, "", false},
		{"RHEL family 10 is fine", facts{"os_id": "almalinux", "os_like": "rhel centos fedora", "os_version": "10.0"}, "", false},
		{"RHEL family 8 is too old", facts{"os_id": "rocky", "os_like": "rhel centos fedora", "os_version": "8.10"}, "os", false},
		{"neither family", facts{"os_id": "fedora", "os_version": "41"}, "os", false},
		{"a Debian derivative is fine", facts{"os_id": "linuxmint", "os_like": "ubuntu debian"}, "", false},
		{"no os-release", facts{"os_id": ""}, "os", false},
		{"no ss", facts{"listening": "unknown"}, "ports", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := goodFacts()
			for k, v := range tt.change {
				f[k] = v
			}
			got := evaluate(f, cp)
			if tt.want == "" {
				if len(got) != 0 {
					t.Errorf("want no findings, got %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].check != tt.want || got[0].warn != tt.warn {
				t.Errorf("got %+v, want one %q (warn=%v)", got, tt.want, tt.warn)
			}
		})
	}

	t.Run("not linux stops early", func(t *testing.T) {
		f := goodFacts()
		f["kernel"] = "FreeBSD"
		if got := checks(evaluate(f, cp)); len(got) != 1 || got[0] != "os" {
			t.Errorf("got %v", got)
		}
	})
	t.Run("k3s runs on a Pi 2", func(t *testing.T) {
		f := goodFacts()
		f["arch"], f["mem_kb"], f["cpus"] = "armv7l", strconv.Itoa(900*1024), "1"
		if got := evaluate(f, mustReq(t, config.DistributionK3s, config.RoleWorker, "flannel")); len(got) != 0 {
			t.Errorf("k3s agent on 1 GB armv7: %+v", got)
		}
	})
}

func view(label, role, ip string) nodeView { return nodeView{label: label, role: role, ip: ip} }

func ports(ps []peer, ip string) []int {
	var out []int
	for _, p := range ps {
		if p.ip == ip {
			out = append(out, p.port)
		}
	}
	return out
}

func TestPeersFor(t *testing.T) {
	nodes := []nodeView{
		view("cp1", config.RoleControlPlane, "10.0.0.1"),
		view("w1", config.RoleWorker, "10.0.0.2"),
		view("w2", config.RoleWorker, "10.0.0.3"),
	}

	// kubeadm: workers reach the API server; only control planes call the kubelet.
	w1 := peersFor(1, nodes, config.DistributionKubeadm, "")
	if got := ports(w1, "10.0.0.1"); !equal(got, []int{6443}) {
		t.Errorf("kubeadm worker -> cp: %v, want [6443]", got)
	}
	if got := ports(w1, "10.0.0.3"); len(got) != 0 {
		t.Errorf("kubeadm worker -> worker: %v, want none", got)
	}
	if got := ports(peersFor(0, nodes, config.DistributionKubeadm, ""), "10.0.0.2"); !equal(got, []int{10250}) {
		t.Errorf("kubeadm cp -> worker: %v, want [10250]", got)
	}

	// k3s: kubelet is all to all.
	if got := ports(peersFor(1, nodes, config.DistributionK3s, ""), "10.0.0.3"); !equal(got, []int{10250}) {
		t.Errorf("k3s worker -> worker: %v, want [10250]", got)
	}

	// CNI ports go between every pair.
	if got := ports(peersFor(1, nodes, config.DistributionKubeadm, "calico"), "10.0.0.3"); !equal(got, []int{179}) {
		t.Errorf("calico worker -> worker: %v, want [179]", got)
	}

	// HA: etcd between control planes only.
	ha := []nodeView{
		view("cp1", config.RoleControlPlane, "10.0.0.1"),
		view("cp2", config.RoleControlPlane, "10.0.0.2"),
		view("w1", config.RoleWorker, "10.0.0.3"),
	}
	if got := ports(peersFor(0, ha, config.DistributionKubeadm, ""), "10.0.0.2"); !equal(got, []int{6443, 10250, 2379, 2380}) {
		t.Errorf("HA cp -> cp: %v", got)
	}
	if got := ports(peersFor(2, ha, config.DistributionKubeadm, ""), "10.0.0.1"); !equal(got, []int{6443}) {
		t.Errorf("HA worker -> cp: %v, want [6443]", got)
	}
}

func TestEvaluatePeers(t *testing.T) {
	ps := []peer{{"a", "10.0.0.1", 6443}, {"b", "10.0.0.2", 10250}, {"c", "10.0.0.3", 10250}}
	out := "10.0.0.1:6443=refused\n10.0.0.2:10250=open\n10.0.0.3:10250=timeout\n"
	got := evaluatePeers(out, ps, nil)
	if len(got) != 1 || got[0].warn || !strings.Contains(got[0].problem, "c (10.0.0.3) port 10250") || strings.Contains(got[0].problem, "10.0.0.1") {
		t.Errorf("only the timed-out port should fail: %+v", got)
	}
	if got := evaluatePeers(strings.Replace(out, "timeout", "refused", 1), ps, nil); len(got) != 0 {
		t.Errorf("refused means reachable: %+v", got)
	}

	// ufw (or firewalld) is on at c: preparation opens the port there, so it's a warning
	// saying what else could block it.
	got = evaluatePeers(out, ps, map[string]string{"10.0.0.3": "ufw"})
	if len(got) != 1 || !got[0].warn || !strings.Contains(got[0].problem, "ufw blocks c (10.0.0.3) port 10250 for now") ||
		!strings.Contains(got[0].hint, "cloud security group") {
		t.Errorf("ufw on: %+v", got)
	}
	got = evaluatePeers(out, ps, map[string]string{"10.0.0.3": "firewalld"})
	if len(got) != 1 || !got[0].warn || !strings.Contains(got[0].problem, "firewalld blocks c (10.0.0.3) port 10250 for now; preparing the machines opens them in firewalld") {
		t.Errorf("firewalld on: %+v", got)
	}
	if fw := (facts{"ufw": "inactive", "firewalld": "active"}).hostFirewall(); fw != "firewalld" {
		t.Errorf("host firewall %q", fw)
	}
}

func TestEvaluateClusterCatchesClones(t *testing.T) {
	nodes := []nodeView{
		{label: "a", hostname: "pi", machineID: "m1", ip: "10.0.0.1"},
		{label: "b", hostname: "PI", machineID: "m1", ip: "10.0.0.2"},
		{label: "c", hostname: "pi3", machineID: "m3", ip: "10.0.0.3"},
	}
	got := evaluateCluster(nodes)
	if len(got[0]) != 0 || len(got[2]) != 0 {
		t.Errorf("first and unique nodes should pass: %+v", got)
	}
	if c := checks(got[1]); !equal(c, []string{"hostname", "machine ID"}) {
		t.Errorf("clone: %v, want hostname and machine ID", c)
	}
}

// The scripts must never change the machine: the only sudo are the password probe and
// reading ufw's state.
func TestScriptsAreReadOnly(t *testing.T) {
	sudo := regexp.MustCompile(`sudo\s+(\S+\s+\S+(\s+status)?)`)
	for _, s := range []string{factsScript([]string{"https://example.com"}), peerScript([]peer{{"a", "10.0.0.1", 6443}})} {
		for _, m := range sudo.FindAllStringSubmatch(s, -1) {
			if m[1] != "-n true" && m[1] != "-n ufw status" {
				t.Errorf("unexpected sudo use: %q", m[0])
			}
		}
		for _, bad := range []string{"apt", "yum", "dnf", "rm ", "swapoff", "sysctl -w", "systemctl", "tee ", "mkdir"} {
			if strings.Contains(s, bad) {
				t.Errorf("script contains %q", bad)
			}
		}
	}
}

// Runs the real scripts with this machine's shell, so the sh/awk/bash parts are tested too.
func TestScriptsRunOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("scripts target Linux nodes")
	}

	// Port 1 on localhost refuses at once, so the internet check fails fast and offline.
	out, err := exec.Command("sh", "-c", factsScript([]string{"http://127.0.0.1:1"})).CombinedOutput()
	if err != nil {
		t.Fatalf("facts script: %v\n%s", err, out)
	}
	f := parseFacts(string(out))
	if f["kernel"] != "Linux" {
		t.Errorf("kernel = %q", f["kernel"])
	}
	for _, k := range []string{"cpus", "mem_kb", "disk_kb"} {
		if n, ok := f.int(k); !ok || n <= 0 {
			t.Errorf("%s = %q, want a positive number", k, f[k])
		}
	}
	if f["hostname"] == "" || f["arch"] == "" {
		t.Errorf("missing hostname or arch: %v", f)
	}
	if !strings.HasPrefix(f["internet"], "fail ") && f["internet"] != "notool" {
		t.Errorf("internet = %q, want a failure for an unreachable URL", f["internet"])
	}

	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not installed")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	open := l.Addr().(*net.TCPAddr).Port
	closed := freePort(t)
	ps := []peer{{"open", "127.0.0.1", open}, {"closed", "127.0.0.1", closed}}
	out, err = exec.Command("sh", "-c", peerScript(ps)).CombinedOutput()
	if err != nil {
		t.Fatalf("peer script: %v\n%s", err, out)
	}
	r := parseFacts(string(out))
	if got := r["127.0.0.1:"+strconv.Itoa(open)]; got != "open" {
		t.Errorf("listening port = %q, want open\n%s", got, out)
	}
	if got := r["127.0.0.1:"+strconv.Itoa(closed)]; got != "refused" {
		t.Errorf("closed port = %q, want refused\n%s", got, out)
	}
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return p
}

func equal[T comparable](a, b []T) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
