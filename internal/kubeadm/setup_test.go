package kubeadm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/runtimes"
)

// fakeMachine answers kubeadm's scripts as an Ubuntu machine would, or a Rocky one (dnf).
type fakeMachine struct {
	mu             sync.Mutex
	os             string // os-release's lines; "" = Ubuntu
	arch           string
	pkgs           map[string]string
	held           map[string]bool
	kubeRepo       string
	kubeletEnabled bool
	containerd     runtime
	crio           runtime
	crioRepo       string
	k3s            bool
	kubeletConf    bool
	record         []byte
	scripts        []string // the scripts that changed something, in order
	fail           string   // a script containing this fails

	cluster // kubeadm init and join (install_test.go)
}

func newMachine() *fakeMachine {
	return &fakeMachine{arch: "amd64", pkgs: map[string]string{}, held: map[string]bool{}}
}

const pause = "registry.k8s.io/pause:3.10.2"

func (f *fakeMachine) RunInput(_ context.Context, cmd, stdin string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if cmd != asRoot {
		return "", fmt.Errorf("not run as root: %s", cmd)
	}
	if strings.HasPrefix(stdin, "# tow: read-only check") {
		return f.facts(), nil
	}
	f.scripts = append(f.scripts, stdin)
	if out, err, ok := f.install(stdin); ok {
		return out, err
	}
	if f.fail != "" && strings.Contains(stdin, f.fail) {
		return "E: Unable to locate package\n", errors.New("exit status 100")
	}
	baseurl := func(file string) string {
		_, rest, _ := strings.Cut(stdin, "cat > "+file)
		return regexp.MustCompile(`baseurl=(\S+)`).FindStringSubmatch(rest)[1]
	}
	switch {
	case strings.Contains(stdin, prepare.RecordFile):
		_, rest, _ := strings.Cut(stdin, "<<'TOW_EOF'\n")
		data, _, _ := strings.Cut(rest, "\nTOW_EOF")
		f.record = []byte(data)
	case strings.Contains(stdin, "cat > "+kubeRPM):
		// dnf: the packages the command names, at their release; the exclude line holds them.
		for _, m := range regexp.MustCompile(`(kubelet|kubeadm|kubectl)-([0-9.]+)`).FindAllStringSubmatch(stdin, -1) {
			f.pkgs[m[1]] = m[2] + "-150500.1.1"
		}
		if strings.Contains(stdin, " cri-tools\n") {
			f.pkgs["cri-tools"] = "1.37.0-150500.1.1"
			f.kubeletEnabled = true
		}
		for _, p := range regexp.MustCompile(`exclude=([a-z -]+)`).FindStringSubmatch(stdin)[1:] {
			for _, q := range strings.Fields(p) {
				f.held[q] = true
			}
		}
		f.kubeRepo = baseurl(kubeRPM)
	case strings.Contains(stdin, "apt-mark hold kubelet kubeadm kubectl"):
		want := regexp.MustCompile(`exact "\$p" '([^']+)'`).FindStringSubmatch(stdin)[1]
		for _, p := range []string{"kubelet", "kubeadm", "kubectl"} {
			f.pkgs[p], f.held[p] = want+"-1.1", true
		}
		f.pkgs["cri-tools"] = "1.37.0-1.1"
		f.kubeRepo = regexp.MustCompile(`echo '(deb [^']+)' > /etc/apt/sources.list.d/kubernetes.list`).FindStringSubmatch(stdin)[1]
		f.kubeletEnabled = true
	case strings.Contains(stdin, "apt-mark hold kubeadm") || strings.Contains(stdin, "apt-mark hold kubelet kubectl"):
		// An upgrade's packages (upgrade.go): the repository moves, those packages too.
		want := regexp.MustCompile(`exact "\$p" '([^']+)'`).FindStringSubmatch(stdin)[1]
		list := regexp.MustCompile(`apt-mark hold ([a-z ]+) >/dev/null`).FindStringSubmatch(stdin)[1]
		for _, p := range strings.Fields(list) {
			f.pkgs[p], f.held[p] = want+"-1.1", true
		}
		f.kubeRepo = regexp.MustCompile(`echo '(deb [^']+)' > /etc/apt/sources.list.d/kubernetes.list`).FindStringSubmatch(stdin)[1]
	case strings.HasPrefix(stdin, "set -e\nkubeadm upgrade"):
	case strings.Contains(stdin, "containerd config default"):
		v := regexp.MustCompile(`containerd-([0-9.]+)-linux-`).FindStringSubmatch(stdin)[1]
		f.containerd = runtime{path: "/usr/local/bin/containerd", version: v, active: true, cri: true, systemd: true,
			owner: owner(regexp.MustCompile(`echo '(# written by tow[^']*)'`).FindStringSubmatch(stdin)[1]),
			pause: regexp.MustCompile(`sandbox = '([^']+)'\|`).FindStringSubmatch(stdin)[1]}
	case strings.Contains(stdin, crioDropIn):
		v := "1.37.2"
		if m := regexp.MustCompile(`exact cri-o '([^']+)'|cri-o-([0-9.]+)`).FindStringSubmatch(stdin); m != nil {
			v = m[1] + m[2]
		}
		f.crio = runtime{path: "/usr/bin/crio", version: v, pkg: "cri-o", active: true,
			owner: owner(regexp.MustCompile(`(# written by tow[^\n]*)`).FindStringSubmatch(stdin)[1]),
			pause: regexp.MustCompile(`pause_image = "([^"]+)"`).FindStringSubmatch(stdin)[1]}
		if strings.Contains(stdin, crioRPM) {
			f.crioRepo = baseurl(crioRPM)
		} else {
			f.crioRepo = regexp.MustCompile(`echo '(deb [^']+)' > /etc/apt/sources.list.d/cri-o.list`).FindStringSubmatch(stdin)[1]
		}
	default:
		return "", fmt.Errorf("unexpected script:\n%s", stdin)
	}
	return "", nil
}

func (f *fakeMachine) Close() error { return nil }

// facts answers the check script.
func (f *fakeMachine) facts() string {
	var b strings.Builder
	yes := func(on bool, word string) string {
		if on {
			return word
		}
		return ""
	}
	b.WriteString(f.os)
	fmt.Fprintf(&b, "arch=%s\n", f.arch)
	for p, v := range f.pkgs {
		fmt.Fprintf(&b, "pkg:%s=installed %s\n", p, v)
	}
	var held []string
	for p := range f.held {
		held = append(held, p)
	}
	fmt.Fprintf(&b, "held=%s\nkube_repo=%s\nkubelet_enabled=%s\n", strings.Join(held, " "), f.kubeRepo, yes(f.kubeletEnabled, "enabled"))
	if _, ok := f.pkgs["kubeadm"]; ok {
		fmt.Fprintf(&b, "pause=%s\n", pause)
	}
	markerOf := func(r runtime) string {
		if r.owner == "" {
			return ""
		}
		return marker(r.owner)
	}
	c := f.containerd
	fmt.Fprintf(&b, "containerd=%s\ncontainerd_version=%s\ncontainerd_package=%s\ncontainerd_cri=%s\ncontainerd_active=%s\ncontainerd_marker=%s\ncontainerd_systemd=%s\ncontainerd_pause=%s\n",
		c.path, c.version, c.pkg, yes(c.cri, "ok"), yes(c.active, "active"), markerOf(c), yes(c.systemd, "1"), c.pause)
	x := f.crio
	fmt.Fprintf(&b, "crio=%s\ncrio_version=%s\ncrio_package=%s\ncrio_active=%s\ncrio_marker=%s\ncrio_pause=%s\ncrio_repo=%s\n",
		x.path, x.version, x.pkg, yes(x.active, "active"), markerOf(x), x.pause, f.crioRepo)
	fmt.Fprintf(&b, "k3s=%s\nkubelet_conf=%s\nadmin_conf=%s\nkubeadm_done=%s\nrecord=%s\ncomplete=yes\n",
		yes(f.k3s, "yes"), yes(f.kubeletConf, "yes"), yes(f.adminConf, "yes"), f.kubeadmDone, base64.StdEncoding.EncodeToString(f.record))
	return b.String()
}

func (f *fakeMachine) recorded() []string {
	var l struct {
		Changes []prepare.Record `json:"changes"`
	}
	json.Unmarshal(f.record, &l)
	var out []string
	for _, r := range l.Changes {
		out = append(out, r.Cluster+": "+r.What)
	}
	return out
}

var nodes = []providers.Node{
	{Name: "lab-cp-0", Host: "192.168.1.10", Role: "control-plane", User: "u"},
	{Name: "lab-workers-0", Host: "192.168.1.20", Role: "worker", User: "u"},
}

// lab runs Setup against fake machines.
type lab struct{ machines map[string]*fakeMachine }

func newLab() *lab {
	l := &lab{machines: map[string]*fakeMachine{}}
	for _, n := range nodes {
		l.machines[n.Host] = newMachine()
	}
	return l
}

func (l *lab) options(o Options) Options {
	o.Cluster, o.Nodes = "lab", nodes
	if o.Version == "" {
		o.Version = "v1.37.2"
	}
	if o.Runtime == "" {
		o.Runtime = runtimes.Containerd
	}
	o.Connect = func(_ context.Context, n providers.Node, _ string) (prepare.Runner, error) {
		return l.machines[n.Host], nil
	}
	return o
}

func TestSetupContainerd(t *testing.T) {
	l := newLab()
	if err := Setup(context.Background(), l.options(Options{})); err != nil {
		t.Fatal(err)
	}
	for host, m := range l.machines {
		if len(m.scripts) != 3 {
			t.Fatalf("%s ran %d scripts, want packages, containerd, record", host, len(m.scripts))
		}
		pkgs, ctrd := m.scripts[0], m.scripts[1]
		for _, want := range []string{
			"https://pkgs.k8s.io/core:/stable:/v1.37/deb/Release.key", "gpg --batch --yes --dearmor -o /etc/apt/keyrings/kubernetes-apt-keyring.gpg",
			"deb [signed-by=/etc/apt/keyrings/kubernetes-apt-keyring.gpg] https://pkgs.k8s.io/core:/stable:/v1.37/deb/ /",
			`exact "$p" '1.37.2'`, "--allow-downgrades $pkgs cri-tools", "apt-mark hold kubelet kubeadm kubectl", "systemctl enable kubelet",
		} {
			if !strings.Contains(pkgs, want) {
				t.Errorf("packages script lacks %q:\n%s", want, pkgs)
			}
		}
		for _, want := range []string{
			"https://github.com/containerd/containerd/releases/download/v2.3.7/containerd-2.3.7-linux-amd64.tar.gz",
			"echo '" + runtimes.ContainerdSum("2.3.7", "amd64") + "  containerd-2.3.7-linux-amd64.tar.gz'",
			"https://github.com/opencontainers/runc/releases/download/v1.5.2/runc.amd64", "echo '" + runtimes.RuncSum("amd64") + "  runc'",
			"sha256sum -c --quiet sums", "tar -C /usr/local -xzf", "install -m 0755 \"$tmp/runc\" /usr/local/sbin/runc",
			"ExecStart=/usr/local/bin/containerd", "SystemdCgroup = true", "sandbox = '" + pause + "'",
			"echo '# written by tow for cluster lab; tow rewrites this file'", "systemctl restart containerd",
		} {
			if !strings.Contains(ctrd, want) {
				t.Errorf("containerd script lacks %q", want)
			}
		}
		if got := m.recorded(); strings.Join(got, ",") != "lab: Kubernetes packages,lab: containerd" {
			t.Errorf("%s recorded %v", host, got)
		}
	}

	// Again: everything is there, nothing runs.
	for _, m := range l.machines {
		m.scripts = nil
	}
	if err := Setup(context.Background(), l.options(Options{})); err != nil {
		t.Fatal(err)
	}
	for host, m := range l.machines {
		if len(m.scripts) != 0 {
			t.Errorf("%s: a second run changed things:\n%s", host, m.scripts)
		}
	}
}

const rockyRelease = "os_id=rocky\nos_like=rhel centos fedora\nos_version=10.0\n"

// On RHEL-family machines, the packages come from pkgs.k8s.io's rpm repository, written as
// kubeadm's install page writes it (its exclude line holds them), installed with dnf at the
// release; containerd as on Ubuntu; CRI-O from its rpm repository. The records undo it with
// dnf. A second run changes nothing.
func TestSetupRHEL(t *testing.T) {
	for _, rt := range []string{runtimes.Containerd, runtimes.CRIO} {
		t.Run(rt, func(t *testing.T) {
			l := newLab()
			for _, m := range l.machines {
				m.os = rockyRelease
			}
			if err := Setup(context.Background(), l.options(Options{Runtime: rt})); err != nil {
				t.Fatal(err)
			}
			m := l.machines["192.168.1.10"]
			pkgs, rtScript := m.scripts[0], m.scripts[1]
			for _, want := range []string{
				"cat > /etc/yum.repos.d/kubernetes.repo <<'TOW_EOF'\n[kubernetes]\nname=Kubernetes\nbaseurl=https://pkgs.k8s.io/core:/stable:/v1.37/rpm/\nenabled=1\ngpgcheck=1\ngpgkey=https://pkgs.k8s.io/core:/stable:/v1.37/rpm/repodata/repomd.xml.key\nexclude=kubelet kubeadm kubectl cri-tools kubernetes-cni\nTOW_EOF\n",
				"dnf install -y -q --setopt=disable_excludes=kubernetes --setopt=install_weak_deps=False kubelet-1.37.2 kubeadm-1.37.2 kubectl-1.37.2 cri-tools\n",
				"systemctl enable kubelet",
			} {
				if !strings.Contains(pkgs, want) {
					t.Errorf("packages script lacks %q:\n%s", want, pkgs)
				}
			}
			if strings.Contains(pkgs, "apt") {
				t.Errorf("apt on Rocky:\n%s", pkgs)
			}
			switch rt {
			case runtimes.CRIO:
				for _, want := range []string{"baseurl=https://download.opensuse.org/repositories/isv:/cri-o:/stable:/v1.37/rpm/\n", "dnf install -y -q cri-o\n", "systemctl restart crio"} {
					if !strings.Contains(rtScript, want) {
						t.Errorf("CRI-O script lacks %q:\n%s", want, rtScript)
					}
				}
			default:
				if !strings.Contains(rtScript, "containerd-2.3.7-linux-amd64.tar.gz") {
					t.Errorf("containerd script:\n%s", rtScript)
				}
			}
			var l2 struct {
				Changes []prepare.Record `json:"changes"`
			}
			json.Unmarshal(m.record, &l2)
			if len(l2.Changes) == 0 || !strings.Contains(l2.Changes[0].Revert, "dnf remove -y -q $pkgs") || !strings.Contains(l2.Changes[0].Revert, "rm -f /etc/yum.repos.d/kubernetes.repo") {
				t.Errorf("records: %+v", l2.Changes)
			}

			for _, m := range l.machines {
				m.scripts = nil
			}
			if err := Setup(context.Background(), l.options(Options{Runtime: rt})); err != nil {
				t.Fatal(err)
			}
			for host, m := range l.machines {
				if len(m.scripts) != 0 {
					t.Errorf("%s: a second run changed things:\n%s", host, m.scripts)
				}
			}
		})
	}

	// RHEL 9: weak dependencies stay as dnf has them.
	if s := packagesScript("v1.37.2", facts{family: prepare.FamilyRHEL, major: 9}); strings.Contains(s, "install_weak_deps") || !strings.Contains(s, "--setopt=disable_excludes=kubernetes kubelet-1.37.2") {
		t.Errorf("RHEL 9:\n%s", s)
	}
	// Packages of another release are refused with dnf's command to remove them.
	l := newLab()
	m := l.machines["192.168.1.10"]
	m.os, m.pkgs["kubelet"] = rockyRelease, "1.36.4-150500.1.1"
	if err := Setup(context.Background(), l.options(Options{})); err == nil || !strings.Contains(err.Error(), "sudo dnf remove kubelet kubeadm kubectl") {
		t.Errorf("err = %v", err)
	}
}

func TestSetupCRIO(t *testing.T) {
	l := newLab()
	if err := Setup(context.Background(), l.options(Options{Runtime: runtimes.CRIO, CRIO: "1.37.1"})); err != nil {
		t.Fatal(err)
	}
	m := l.machines["192.168.1.10"]
	crio := m.scripts[1]
	for _, want := range []string{
		"cat > /etc/crio/crio.conf.d/20-tow.conf", "# written by tow for cluster lab", `pause_image = "` + pause + `"`,
		"https://download.opensuse.org/repositories/isv:/cri-o:/stable:/v1.37/deb/Release.key",
		"deb [signed-by=/etc/apt/keyrings/cri-o-apt-keyring.gpg] https://download.opensuse.org/repositories/isv:/cri-o:/stable:/v1.37/deb/ /",
		"exact cri-o '1.37.1'", `install -y -qq --allow-downgrades "cri-o=$v"`, "systemctl restart crio",
	} {
		if !strings.Contains(crio, want) {
			t.Errorf("CRI-O script lacks %q:\n%s", want, crio)
		}
	}
	// The drop-in goes first: it marks tow's CRI-O even when the install stops halfway.
	if strings.Index(crio, crioDropIn) > strings.Index(crio, "aptget install") {
		t.Error("the drop-in must be written before the package is installed")
	}
	if strings.Contains(crio, "containerd") {
		t.Error("CRI-O machines get no containerd")
	}
	if got := m.recorded(); strings.Join(got, ",") != "lab: Kubernetes packages,lab: CRI-O" {
		t.Errorf("recorded %v", got)
	}

	// No release named: the newest of the minor.
	l = newLab()
	if err := Setup(context.Background(), l.options(Options{Runtime: runtimes.CRIO})); err != nil {
		t.Fatal(err)
	}
	if s := l.machines["192.168.1.10"].scripts[1]; strings.Contains(s, "exact cri-o") || !strings.Contains(s, "--allow-downgrades cri-o\n") {
		t.Errorf("want the newest cri-o of the minor:\n%s", s)
	}
}

// A containerd release tow doesn't pin is checked against the sum published beside it.
func TestSetupOtherContainerd(t *testing.T) {
	l := newLab()
	l.machines["192.168.1.20"].arch = "arm64"
	if err := Setup(context.Background(), l.options(Options{Containerd: "2.4.2"})); err != nil {
		t.Fatal(err)
	}
	s := l.machines["192.168.1.20"].scripts[1]
	if !strings.Contains(s, "get containerd-2.4.2-linux-arm64.tar.gz.sha256sum 'https://github.com/containerd/containerd/releases/download/v2.4.2/containerd-2.4.2-linux-arm64.tar.gz.sha256sum'") ||
		!strings.Contains(s, "runc.arm64") || !strings.Contains(s, runtimes.RuncSum("arm64")) {
		t.Errorf("want the published sum for 2.4.2 on arm64:\n%s", s)
	}
}

// A runtime already there that goes with this Kubernetes is used as it is.
func TestSetupUsesCompatibleRuntime(t *testing.T) {
	l := newLab()
	for _, m := range l.machines {
		m.containerd = runtime{path: "/usr/bin/containerd", version: "2.3.5", pkg: "containerd.io", active: true, cri: true}
	}
	if err := Setup(context.Background(), l.options(Options{})); err != nil {
		t.Fatal(err)
	}
	for host, m := range l.machines {
		for _, s := range m.scripts {
			if strings.Contains(s, "containerd config default") {
				t.Errorf("%s: the containerd already there must be left alone", host)
			}
		}
		if got := m.recorded(); strings.Join(got, ",") != "lab: Kubernetes packages" {
			t.Errorf("%s recorded %v", host, got)
		}
	}
}

// Anything the user must solve stops everything before a single change, on every machine.
func TestSetupRefusals(t *testing.T) {
	tests := []struct {
		name  string
		setup func(m *fakeMachine)
		opts  Options
		want  []string
	}{
		{"docker's containerd", func(m *fakeMachine) {
			m.containerd = runtime{path: "/usr/bin/containerd", version: "1.7.28", pkg: "containerd.io", active: true}
		}, Options{}, []string{"containerd 1.7.28 (package containerd.io) is already here and can't run Kubernetes 1.37.2 as it is",
			"is a 1.x release", "its Kubernetes plugin (CRI) is off: Docker's containerd.io turns it off", "tow doesn't change a container runtime it didn't install"}},
		{"too old for this Kubernetes", func(m *fakeMachine) {
			m.containerd = runtime{path: "/usr/bin/containerd", version: "2.2.10", pkg: "containerd", active: true, cri: true}
		}, Options{}, []string{"containerd 2.2.10 isn't one containerd recommends for Kubernetes 1.37 (2.4.0+ or 2.3.0+"}},
		{"not the release the file names", func(m *fakeMachine) {
			m.containerd = runtime{path: "/usr/local/bin/containerd", version: "2.3.5", active: true, cri: true}
		}, Options{Containerd: "2.4.2"}, []string{"containerd 2.3.5 (/usr/local/bin/containerd)", "the file asks for containerd 2.4.2"}},
		{"stopped", func(m *fakeMachine) {
			m.containerd = runtime{path: "/usr/bin/containerd", version: "2.3.5", pkg: "containerd"}
		}, Options{}, []string{"it isn't running"}},
		{"another cluster's containerd", func(m *fakeMachine) {
			m.containerd = runtime{path: "/usr/local/bin/containerd", version: "2.3.7", owner: "other", active: true, cri: true}
		}, Options{}, []string{"containerd here was set up by tow for cluster 'other'"}},
		{"CRI-O of another minor", func(m *fakeMachine) {
			m.crio = runtime{path: "/usr/bin/crio", version: "1.36.4", pkg: "cri-o", active: true}
		}, Options{Runtime: runtimes.CRIO}, []string{"CRI-O 1.36.4 (package cri-o) is already here", "must be a 1.37 release", "set spec.kubernetes.containerRuntime: containerd"}},
		{"k3s", func(m *fakeMachine) { m.k3s = true }, Options{}, []string{"k3s is installed here: kubeadm and k3s can't share a machine"}},
		{"a node already", func(m *fakeMachine) { m.kubeletConf = true }, Options{}, []string{"already a Kubernetes node", "sudo kubeadm reset"}},
		{"packages of another release", func(m *fakeMachine) { m.pkgs["kubeadm"] = "1.36.4-1.1" }, Options{},
			[]string{"kubeadm 1.36.4-1.1 already installed, and the file asks for Kubernetes 1.37.2"}},
		{"armhf", func(m *fakeMachine) { m.arch = "armhf" }, Options{}, []string{"this machine is armhf: containerd and runc publish amd64 and arm64 builds"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newLab()
			tt.setup(l.machines["192.168.1.20"])
			err := Setup(context.Background(), l.options(tt.opts))
			if err == nil {
				t.Fatal("want a refusal")
			}
			for _, want := range append(tt.want, "nothing was changed", "192.168.1.20 (lab-workers-0), worker:") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error lacks %q:\n%v", want, err)
				}
			}
			for host, m := range l.machines {
				if len(m.scripts) > 0 {
					t.Errorf("%s changed although a machine was refused", host)
				}
			}
		})
	}
}

// What tow installed for this cluster is its own to change: a failed apply with another
// release picks up.
func TestSetupOwnPackagesMayChange(t *testing.T) {
	l := newLab()
	m := l.machines["192.168.1.10"]
	m.pkgs["kubeadm"] = "1.36.4-1.1"
	m.record = []byte(`{"changes": [{"cluster": "lab", "what": "Kubernetes packages"}]}`)
	if err := Setup(context.Background(), l.options(Options{})); err != nil {
		t.Fatal(err)
	}
	if m.pkgs["kubeadm"] != "1.37.2-1.1" {
		t.Errorf("kubeadm = %s", m.pkgs["kubeadm"])
	}
	if got := m.recorded(); strings.Join(got, ",") != "lab: Kubernetes packages,lab: containerd" {
		t.Errorf("recorded twice or not at all: %v", got)
	}
}

func TestSetupFailureSaysWhere(t *testing.T) {
	l := newLab()
	l.machines["192.168.1.20"].fail = "apt-mark hold"
	err := Setup(context.Background(), l.options(Options{}))
	if err == nil || !strings.Contains(err.Error(), "192.168.1.20 (lab-workers-0): installing kubelet, kubeadm and kubectl failed") ||
		!strings.Contains(err.Error(), "Unable to locate package") {
		t.Errorf("err = %v", err)
	}

	// The packages went on, then the runtime failed: the packages are recorded all the same,
	// so their undo is known and the next run takes them as tow's.
	l = newLab()
	m := l.machines["192.168.1.20"]
	m.fail = "containerd config default"
	if err := Setup(context.Background(), l.options(Options{})); err == nil || !strings.Contains(err.Error(), "installing containerd 2.3.7 failed") {
		t.Fatalf("err = %v", err)
	}
	if got := m.recorded(); strings.Join(got, ",") != "lab: Kubernetes packages" {
		t.Errorf("recorded %v", got)
	}
}

// Preparation's hook finds the same problems, before preparation changes anything.
func TestPreflight(t *testing.T) {
	l := newLab()
	m := l.machines["192.168.1.10"]
	m.k3s = true
	o := l.options(Options{})
	ps, err := o.Preflight(context.Background(), m, nodes[0])
	if err != nil || len(ps) != 1 || !strings.Contains(ps[0].What, "k3s is installed here") {
		t.Errorf("problems = %+v, %v", ps, err)
	}
	if ps, _ := o.Preflight(context.Background(), l.machines["192.168.1.20"], nodes[1]); len(ps) != 0 {
		t.Errorf("a clean machine has no problems: %+v", ps)
	}
}

func TestCheckVersions(t *testing.T) {
	// "stable" resolved to a Kubernetes containerd 2.3 isn't recommended for.
	if _, err := CheckVersions(Options{Version: "v1.34.2", Runtime: runtimes.Containerd}); err == nil || !strings.Contains(err.Error(), "Kubernetes v1.34.2: containerd 2.3.7 isn't one") {
		t.Errorf("err = %v", err)
	}
	if w, err := CheckVersions(Options{Version: "v1.38.0", Runtime: runtimes.Containerd}); err != nil || w == "" {
		t.Errorf("1.38: %q, %v", w, err)
	}
	if _, err := CheckVersions(Options{Version: "v1.38.0", Runtime: runtimes.CRIO, CRIO: "1.37.2"}); err == nil {
		t.Error("CRI-O 1.37 with Kubernetes 1.38")
	}
	if _, err := CheckVersions(Options{Version: "v1.37.2", Runtime: runtimes.CRIO}); err != nil {
		t.Error(err)
	}
}

func TestDescribe(t *testing.T) {
	got := strings.Join(Describe(Options{Version: "v1.37.2", Runtime: runtimes.Containerd}), "\n")
	for _, want := range []string{"kubelet, kubeadm and kubectl 1.37.2 from https://pkgs.k8s.io/core:/stable:/v1.37/{deb,rpm}/, held", "containerd 2.3.7 and runc 1.5.2", "(2.4.0+ or 2.3.0+, its CRI plugin on)"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe lacks %q:\n%s", want, got)
		}
	}
	if got := strings.Join(Describe(Options{Version: "v1.37.2", Runtime: runtimes.CRIO}), "\n"); !strings.Contains(got, "CRI-O (the newest 1.37 release) from https://download.opensuse.org/repositories/isv:/cri-o:/stable:/v1.37/{deb,rpm}/") {
		t.Errorf("describe:\n%s", got)
	}
}

// containerd's config is claimed before the download, so a failed install is still known as
// tow's; but one tow already wrote stays whole until the new one is ready, so a
// failed download never leaves containerd without systemd cgroups and the pause image.
func TestContainerdScriptKeepsItsConfig(t *testing.T) {
	s, err := containerdScript("lab", "2.3.7", "amd64", pause)
	if err != nil {
		t.Fatal(err)
	}
	claim, _, ok := strings.Cut(s, "tmp=$(mktemp -d)")
	if !ok {
		t.Fatal("no download part in the script")
	}
	dir := t.TempDir()
	claim = strings.ReplaceAll(claim, "/etc/containerd", dir)
	config, backup := dir+"/config.toml", dir+"/config.toml.tow.bak"
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	ours := marker("lab") + "\nversion = 3\n[plugins]\n  SystemdCgroup = true\n"
	for _, c := range []struct {
		name, before, after, bak string
	}{
		{"no config", "", marker("lab") + "\n", ""},
		{"someone else's, kept aside", "version = 2\n", marker("lab") + "\n", "version = 2\n"},
		{"tow's own, left whole", ours, ours, ""},
	} {
		os.Remove(config)
		os.Remove(backup)
		if c.before != "" {
			os.WriteFile(config, []byte(c.before), 0o644)
		}
		if out, err := exec.Command("sh", "-c", claim).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", c.name, err, out)
		}
		if got := read(config); got != c.after {
			t.Errorf("%s: config.toml = %q, want %q", c.name, got, c.after)
		}
		if got := read(backup); got != c.bak {
			t.Errorf("%s: backup = %q, want %q", c.name, got, c.bak)
		}
	}
}

// Every script is valid sh.
func TestScriptsParse(t *testing.T) {
	ctrd, err := containerdScript("lab", "2.3.7", "amd64", pause)
	if err != nil {
		t.Fatal(err)
	}
	other, _ := containerdScript("lab", "2.4.2", "arm64", pause)
	crio, _ := crioScript("lab", 37, "1.37.2", pause, facts{})
	newest, _ := crioScript("lab", 37, "", pause, facts{})
	rocky := facts{family: prepare.FamilyRHEL, major: 10}
	crioRocky, _ := crioScript("lab", 37, "1.37.2", pause, rocky)
	for name, s := range map[string]string{"facts": factsScript("v1.37.2"), "packages": packagesScript("v1.37.2", facts{}),
		"containerd": ctrd, "containerd 2.4.2": other, "cri-o": crio, "cri-o newest": newest,
		"packages rocky": packagesScript("v1.37.2", rocky), "cri-o rocky": crioRocky, "upgrade rocky": upgradePackagesScript("v1.38.0", rocky, "kubeadm"),
		"upgrade": upgradePackagesScript("v1.38.0", facts{}, "kubelet", "kubectl")} {
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s script: %v\n%s", name, err, out)
		}
	}
}

func TestScriptsRefuseOddPauseImages(t *testing.T) {
	if _, err := containerdScript("lab", "2.3.7", "amd64", "x'; rm -rf /"); err == nil {
		t.Error("a pause image with quotes must be refused")
	}
	if _, err := crioScript("lab", 37, "", "$(id)", facts{}); err == nil {
		t.Error("a pause image with $ must be refused")
	}
}
