package k3s

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/towproj/tow/internal/kubevip"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/ui"
	"gopkg.in/yaml.v3"
	"k8s.io/client-go/tools/clientcmd"
)

// fakeMachine answers the scripts the installer sends as a real machine would.
type fakeMachine struct {
	name      string
	log       *[]string // shared: the order installs happen in
	mu        *sync.Mutex
	installed bool
	version   string
	active    map[string]bool
	config    []byte
	record    []byte
	installs  []string // install scripts run here
	fail      bool
	addrs     string   // what 'ip -o -4 addr show' prints
	kubeVIP   []string // kube-vip manifests written here, each logged before k3s
	selinux   string   // what getenforce prints
}

const k3sKubeconfig = `apiVersion: v1
kind: Config
clusters:
- cluster: {server: "https://127.0.0.1:6443", certificate-authority-data: Y2E=}
  name: default
contexts:
- context: {cluster: default, user: default}
  name: default
current-context: default
users:
- name: default
  user: {client-certificate-data: Y2VydA==, client-key-data: a2V5}
`

const (
	k10Server = "K1040e56adcbdfe4fa908debd7a0d3a706b39d074a2736f6967e1b8c86ef0b8506c::server:pw-from-the-first"
	k10Agent  = "K1040e56adcbdfe4fa908debd7a0d3a706b39d074a2736f6967e1b8c86ef0b8506c::node:agent-pw"
)

func (f *fakeMachine) RunInput(_ context.Context, cmd, stdin string) (string, error) {
	if cmd != asRoot {
		return "", fmt.Errorf("not run as root: %s", cmd)
	}
	switch {
	case stdin == checkScript:
		installed := "no"
		if f.installed {
			installed = "yes"
		}
		active := func(s string) string {
			if f.active[s] {
				return "active"
			}
			return "inactive"
		}
		return fmt.Sprintf("installed=%s\nversion=%s\nk3s=%s\nk3s-agent=%s\nconfig=%s\nrecord=%s\nselinux=%s\ncomplete=yes\n",
			installed, f.version, active("k3s"), active("k3s-agent"),
			base64.StdEncoding.EncodeToString(f.config), base64.StdEncoding.EncodeToString(f.record), f.selinux), nil
	case strings.HasPrefix(stdin, "set -e\nmkdir -p /etc/rancher/k3s"):
		f.mu.Lock()
		*f.log = append(*f.log, f.name)
		f.mu.Unlock()
		f.installs = append(f.installs, stdin)
		if f.fail {
			return "[INFO]  systemd: Starting k3s\nJob for k3s.service failed", errors.New("exit status 1")
		}
		_, rest, _ := strings.Cut(stdin, "<<'"+heredocEnd+"'\n")
		config, _, _ := strings.Cut(rest, heredocEnd+"\n)")
		f.config = []byte(config)
		f.version = regexp.MustCompile(`INSTALL_K3S_VERSION='([^']+)'`).FindStringSubmatch(stdin)[1]
		exec := regexp.MustCompile(`INSTALL_K3S_EXEC='([^']+)'`).FindStringSubmatch(stdin)[1]
		f.installed, f.active = true, map[string]bool{map[string]string{"server": "k3s", "agent": "k3s-agent"}[exec]: true}
		if strings.Contains(stdin, prepare.RecordFile) {
			f.record = []byte(`{"changes": [{"cluster": "lab", "what": "k3s (x)"}]}`)
		}
		return "[INFO]  Using v1.37.2+k3s1 as release\n[INFO]  Verifying binary download", nil
	case stdin == "ip -o -4 addr show\n":
		return f.addrs, nil
	case strings.HasPrefix(stdin, "set -e\nnew=$(mktemp)") && strings.Contains(stdin, kubevip.K3sManifest):
		f.mu.Lock()
		*f.log = append(*f.log, f.name+" kube-vip")
		f.mu.Unlock()
		f.kubeVIP = append(f.kubeVIP, stdin)
		return "", nil
	case strings.HasPrefix(stdin, "cat "+tokenFile):
		return k10Server + "\n" + k10Agent + "\n", nil
	case stdin == "cat "+kubeconfigFile:
		return k3sKubeconfig, nil
	case strings.HasPrefix(stdin, "journalctl -u k3s"):
		return "level=fatal msg=\"etcd cluster join failed\"\n", nil
	}
	return "", fmt.Errorf("unexpected script:\n%s", stdin)
}

func (f *fakeMachine) Close() error { return nil }

type fakeAPI struct {
	reachable  bool
	registered [][]string
	ready      [][]string
	labels     map[string]map[string]string
	scaled     []string
}

func (a *fakeAPI) ScaleUp(_ context.Context, namespace, name string, replicas int32) error {
	if len(a.registered) == 0 {
		return errors.New("scaled before the nodes joined")
	}
	a.scaled = append(a.scaled, fmt.Sprintf("%s/%s=%d", namespace, name, replicas))
	return nil
}

func (a *fakeAPI) WaitReachable(context.Context) error { a.reachable = true; return nil }
func (a *fakeAPI) WaitNodes(_ context.Context, names []string, ready bool) error {
	if ready {
		a.ready = append(a.ready, names)
	} else {
		a.registered = append(a.registered, names)
	}
	return nil
}
func (a *fakeAPI) Label(_ context.Context, node string, labels map[string]string) error {
	if len(labels) > 0 {
		a.labels[node] = labels
	}
	return nil
}

// lab is a cluster of two control planes and two workers in two pools.
type lab struct {
	machines map[string]*fakeMachine
	order    []string
	api      *fakeAPI
	kubeconf string
}

func newLab() *lab {
	l := &lab{machines: map[string]*fakeMachine{}, api: &fakeAPI{labels: map[string]map[string]string{}}}
	mu := &sync.Mutex{}
	for _, n := range labNodes {
		l.machines[n.Host] = &fakeMachine{name: n.Name, log: &l.order, mu: mu,
			addrs: "1: lo    inet 127.0.0.1/8 scope host lo\n2: ens18    inet " + n.PrivateIP + "/24 brd 10.0.1.255 scope global ens18\n"}
	}
	return l
}

var labNodes = []providers.Node{
	{Name: "lab-w-gpu", Host: "203.0.113.13", PrivateIP: "10.0.1.13", Role: "worker", Pool: "gpu"},
	{Name: "lab-cp-0", Host: "203.0.113.10", PrivateIP: "10.0.1.10", Role: "control-plane", Pool: "control-plane"},
	{Name: "lab-cp-1", Host: "203.0.113.11", PrivateIP: "10.0.1.11", Role: "control-plane", Pool: "control-plane"},
	{Name: "lab-w-general", Host: "203.0.113.12", PrivateIP: "10.0.1.12", Role: "worker", Pool: "general"},
}

func (l *lab) options() Options {
	return Options{
		Cluster:      "lab",
		Version:      "v1.37.2",
		Nodes:        labNodes,
		Endpoint:     "lab-api.example.com:6443",
		NodeEndpoint: "internal-lab-nodes.example.com:6443",
		ServerFlags: []providers.Flag{
			{Name: "tls-san", Value: "k8s.lab.example.com"},
			{Name: "disable", Value: "traefik"},
			{Name: "kube-apiserver-arg", Value: "audit-log-maxage=30"},
			{Name: "etcd-s3-secret-key", Env: "S3_KEY"},
		},
		AgentFlags: func(pool string) ([]providers.Flag, bool) {
			switch pool {
			case "gpu":
				return []providers.Flag{{Name: "node-label", Value: "gpu=true"}, {Name: "kubelet-arg", Value: "max-pods=250"}}, true
			case "general":
				return nil, true
			}
			return nil, false
		},
		RoleLabels: func(n providers.Node) map[string]string {
			if n.Pool == "gpu" {
				return map[string]string{"node-role.kubernetes.io/gpu": ""}
			}
			return nil
		},
		Secrets:   map[string]string{"S3_KEY": "s3-secret-value"},
		WaitReady: true,
		Connect: func(_ context.Context, n providers.Node, _ string) (prepare.Runner, error) {
			return l.machines[n.Host], nil
		},
		API: func(kubeconfig []byte) (API, error) { l.kubeconf = string(kubeconfig); return l.api, nil },
	}
}

func configOf(t *testing.T, f *fakeMachine) map[string]any {
	t.Helper()
	var m map[string]any
	if err := yaml.Unmarshal(f.config, &m); err != nil {
		t.Fatalf("%s: config.yaml doesn't parse: %v\n%s", f.name, err, f.config)
	}
	return m
}

// With an API name (GCP), each machine resolves it after k3s is in: a control plane to itself,
// a worker to the load balancer. It goes in the certificate, and delete takes the line away.
func TestInstallWithAPIName(t *testing.T) {
	l := newLab()
	o := l.options()
	o.NodeEndpoint, o.APIName = "10.0.1.5:6443", "api.lab.internal"
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for host, ip := range map[string]string{"203.0.113.10": "10.0.1.10", "203.0.113.11": "10.0.1.11", "203.0.113.12": "10.0.1.5", "203.0.113.13": "10.0.1.5"} {
		s := l.machines[host].installs[0]
		if !strings.Contains(s, prepare.HostsScript("api.lab.internal", ip)) || !strings.Contains(s, "# tow API name") {
			t.Errorf("%s: the API name isn't %s:\n%s", host, ip, s)
		}
		if strings.Index(s, "/etc/hosts") < strings.Index(s, "install.sh") {
			t.Errorf("%s: the line comes before k3s:\n%s", host, s)
		}
	}
	if sans := fmt.Sprint(configOf(t, l.machines["203.0.113.10"])["tls-san"]); !strings.Contains(sans, "api.lab.internal") {
		t.Errorf("tls-san = %s", sans)
	}
}

func TestInstall(t *testing.T) {
	l := newLab()
	kubeconfig, err := Install(context.Background(), l.options())
	if err != nil {
		t.Fatal(err)
	}

	// The first control plane starts the cluster, the second joins it, then the workers.
	if len(l.order) != 4 || l.order[0] != "lab-cp-0" || l.order[1] != "lab-cp-1" {
		t.Fatalf("install order = %v", l.order)
	}

	first := configOf(t, l.machines["203.0.113.10"])
	token, _ := first["token"].(string)
	agentToken, _ := first["agent-token"].(string)
	if first["cluster-init"] != true || len(token) != 64 || len(agentToken) != 64 || token == agentToken || first["server"] != nil {
		t.Errorf("first control plane: %v", first)
	}
	if first["node-name"] != "lab-cp-0" || first["node-ip"] != "10.0.1.10" || first["node-external-ip"] != nil {
		t.Errorf("first control plane's node settings: %v", first)
	}
	// The user's address and the nodes' (an internal load balancer on AWS) both.
	wantSANs := []any{"k8s.lab.example.com", "lab-api.example.com", "internal-lab-nodes.example.com", "203.0.113.10", "10.0.1.10", "203.0.113.11", "10.0.1.11"}
	if fmt.Sprint(first["tls-san"]) != fmt.Sprint(wantSANs) {
		t.Errorf("tls-san = %v, want %v", first["tls-san"], wantSANs)
	}
	if fmt.Sprint(first["disable"]) != "[traefik]" || fmt.Sprint(first["kube-apiserver-arg"]) != "[audit-log-maxage=30]" || first["etcd-s3-secret-key"] != "s3-secret-value" {
		t.Errorf("the file's flags: %v", first)
	}
	if c, role, ok := owner(l.machines["203.0.113.10"].config); !ok || c != "lab" || role != roleServer {
		t.Errorf("marker: %q %q %v", c, role, ok)
	}

	second := configOf(t, l.machines["203.0.113.11"])
	if second["server"] != "https://10.0.1.10:6443" || second["token"] != k10Server || second["agent-token"] != agentToken || second["cluster-init"] != nil {
		t.Errorf("second control plane joins with the secure server token and the same worker token: %v", second)
	}
	if fmt.Sprint(second["tls-san"]) != fmt.Sprint(wantSANs) || fmt.Sprint(second["disable"]) != "[traefik]" {
		t.Errorf("every control plane gets the same settings: %v", second)
	}

	gpu := configOf(t, l.machines["203.0.113.13"])
	if gpu["server"] != "https://10.0.1.10:6443" || gpu["token"] != k10Agent || gpu["agent-token"] != nil || gpu["tls-san"] != nil || gpu["disable"] != nil {
		t.Errorf("worker joins with the secure worker token only: %v", gpu)
	}
	if fmt.Sprint(gpu["node-label"]) != "[gpu=true]" || fmt.Sprint(gpu["kubelet-arg"]) != "[max-pods=250]" {
		t.Errorf("worker gets its pool's flags: %v", gpu)
	}
	if general := configOf(t, l.machines["203.0.113.12"]); general["node-label"] != nil || general["node-name"] != "lab-w-general" {
		t.Errorf("the general pool has no flags of its own: %v", general)
	}

	// The tokens are hidden from all output.
	for _, s := range []string{token, agentToken, k10Server, k10Agent} {
		if got := ui.Redact("x " + s + " y"); strings.Contains(got, s) {
			t.Errorf("secret not redacted: %s", got)
		}
	}

	// Each install runs the release's own installer, checks nothing on the command line,
	// and is recorded.
	script := l.machines["203.0.113.12"].installs[0]
	for _, want := range []string{
		"https://raw.githubusercontent.com/k3s-io/k3s/v1.37.2%2Bk3s1/install.sh",
		// The installer skips the restart when only config.yaml changed: forced.
		"INSTALL_K3S_VERSION='v1.37.2+k3s1' INSTALL_K3S_EXEC='agent' INSTALL_K3S_FORCE_RESTART=true",
		"(umask 077; cat > " + ConfigFile,
		prepare.RecordFile, "k3s-agent-uninstall.sh",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install script lacks %q:\n%s", want, script)
		}
	}

	cfg, err := clientcmd.Load(kubeconfig)
	if err != nil || cfg.Clusters["default"].Server != "https://lab-api.example.com:6443" {
		t.Errorf("kubeconfig points to %v, %v", cfg.Clusters["default"], err)
	}
	if l.kubeconf != string(kubeconfig) || !l.api.reachable {
		t.Error("the API must be reached with the kubeconfig the user gets")
	}
	all := []string{"lab-cp-0", "lab-cp-1", "lab-w-gpu", "lab-w-general"}
	if len(l.api.registered) != 1 || !slices.Equal(l.api.registered[0], all) || len(l.api.ready) != 1 {
		t.Errorf("waits: registered %v, ready %v", l.api.registered, l.api.ready)
	}
	if len(l.api.labels) != 1 || l.api.labels["lab-w-gpu"] == nil {
		t.Errorf("role labels: %v", l.api.labels)
	}
	if len(l.api.scaled) != 0 {
		t.Errorf("CoreDNS stays as k3s ships it unless asked: %v", l.api.scaled)
	}

	// Several control planes: CoreDNS gets two replicas once the nodes joined.
	l = newLab()
	o := l.options()
	o.CoreDNSReplicas = 2
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.api.scaled, []string{"kube-system/coredns=2"}) {
		t.Errorf("scaled %v", l.api.scaled)
	}
	if lines := strings.Join(Describe(o), "\n"); !strings.Contains(lines, "CoreDNS scaled to 2 replicas on different machines") {
		t.Errorf("dry run:\n%s", lines)
	}
}

// A second run finds k3s running as the file asks everywhere and changes nothing; the
// passwords come back from the first control plane.
func TestInstallAgainChangesNothing(t *testing.T) {
	l := newLab()
	if _, err := Install(context.Background(), l.options()); err != nil {
		t.Fatal(err)
	}
	l.order = nil
	if _, err := Install(context.Background(), l.options()); err != nil {
		t.Fatal(err)
	}
	if len(l.order) != 0 {
		t.Errorf("reinstalled on %v", l.order)
	}

	// A stopped service, or a changed setting, is installed again on that machine only.
	l.machines["203.0.113.12"].active = map[string]bool{}
	o := l.options()
	o.ServerFlags = append(o.ServerFlags, providers.Flag{Name: "disable", Value: "servicelb"})
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.order, []string{"lab-cp-0", "lab-cp-1", "lab-w-general"}) {
		t.Errorf("reinstalled on %v", l.order)
	}
}

// An upgrade rolls: one machine at a time inside Rolling, control planes first; a machine
// already on the release is left alone (a resumed upgrade), and a failure stops the roll.
func TestInstallRolls(t *testing.T) {
	l := newLab()
	if _, err := Install(context.Background(), l.options()); err != nil {
		t.Fatal(err)
	}
	l.order = nil
	l.machines["203.0.113.12"].version = "v1.38.0+k3s1" // the worker went first, last time
	var rolled []string
	o := l.options()
	o.Version = "v1.38.0"
	o.Rolling = func(_ context.Context, n providers.Node, change func() error) error {
		rolled = append(rolled, "drain "+n.Name)
		if err := change(); err != nil {
			return err
		}
		rolled = append(rolled, "back "+n.Name)
		return nil
	}
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	want := []string{"drain lab-cp-0", "back lab-cp-0", "drain lab-cp-1", "back lab-cp-1", "drain lab-w-gpu", "back lab-w-gpu"}
	if !slices.Equal(rolled, want) || !slices.Equal(l.order, []string{"lab-cp-0", "lab-cp-1", "lab-w-gpu"}) {
		t.Errorf("rolled %v, installed on %v", rolled, l.order)
	}
	for _, m := range l.machines {
		if m.version != "v1.38.0+k3s1" {
			t.Errorf("%s runs %s", m.name, m.version)
		}
	}

	// A machine that fails stops the roll there.
	o.Version, rolled = "v1.38.1", nil
	l.machines["203.0.113.11"].fail = true
	if _, err := Install(context.Background(), o); err == nil || !slices.Equal(rolled, []string{"drain lab-cp-0", "back lab-cp-0", "drain lab-cp-1"}) {
		t.Errorf("%v, rolled %v", err, rolled)
	}
}

// Labels and taints in config.yaml only count when a node registers: a machine whose file
// differs only there keeps running (apply changes them through the API).
func TestSameSettingsIgnoresRegistration(t *testing.T) {
	const marker = "# written by tow for cluster lab (server)\n"
	have := []byte(marker + "node-name: lab-cp-0\nnode-label:\n  - tier=web\nnode-taint:\n  - a:NoSchedule\n")
	for want, same := range map[string]bool{
		marker + "node-name: lab-cp-0\nnode-label:\n  - tier=batch\n":                      true,
		marker + "node-name: lab-cp-0\n":                                                   true,
		marker + "node-name: lab-cp-0\nnode-label:\n  - tier=web\ndisable:\n  - traefik\n": false,
		"# written by tow for cluster lab (agent)\nnode-name: lab-cp-0\n":                  false,
	} {
		if got := sameSettings(have, []byte(want)); got != same {
			t.Errorf("%q: same = %v, want %v", want, got, same)
		}
	}
}

func TestInstallRefusesSomeoneElsesK3s(t *testing.T) {
	cases := map[string]func(*fakeMachine){
		"not by tow": func(f *fakeMachine) {
			f.installed, f.config = true, []byte("token: theirs\n")
		},
		"for cluster 'other'": func(f *fakeMachine) {
			f.installed, f.config = true, []byte(marker("other", roleAgent)+"\ntoken: x\n")
		},
		"runs as a control plane, but the file makes this machine a worker": func(f *fakeMachine) {
			f.installed, f.config = true, []byte(marker("lab", roleServer)+"\ntoken: x\n")
		},
	}
	for want, setup := range cases {
		t.Run(want, func(t *testing.T) {
			l := newLab()
			setup(l.machines["203.0.113.12"])
			_, err := Install(context.Background(), l.options())
			if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "nothing was changed") {
				t.Fatalf("err = %v", err)
			}
			if len(l.order) != 0 {
				t.Errorf("installed on %v despite the refusal", l.order)
			}
		})
	}
}

func TestInstallFailureShowsTheLog(t *testing.T) {
	l := newLab()
	l.machines["203.0.113.11"].fail = true
	_, err := Install(context.Background(), l.options())
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"203.0.113.11 (lab-cp-1): installing k3s failed", "Job for k3s.service failed", "Last lines of the k3s log", "etcd cluster join failed"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error lacks %q:\n%s", want, msg)
		}
	}
	if slices.Contains(l.order, "lab-w-gpu") {
		t.Error("workers join only once every control plane is up")
	}
}

// The user's own join passwords ({env}) replace the generated ones.
func TestInstallUsesTheUsersTokens(t *testing.T) {
	l := newLab()
	o := l.options()
	o.ServerFlags = append(o.ServerFlags, providers.Flag{Name: "token", Env: "K3S_TOKEN"}, providers.Flag{Name: "agent-token", Env: "K3S_AGENT_TOKEN"})
	o.Secrets["K3S_TOKEN"], o.Secrets["K3S_AGENT_TOKEN"] = "users-server-password", "users-agent-password"
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	first := configOf(t, l.machines["203.0.113.10"])
	if first["token"] != "users-server-password" || first["agent-token"] != "users-agent-password" {
		t.Errorf("first control plane: %v", first)
	}
	if strings.Count(string(l.machines["203.0.113.10"].config), "token:") != 2 {
		t.Errorf("each token once:\n%s", l.machines["203.0.113.10"].config)
	}
}

// With kube-vip, each control plane gets its DaemonSet before k3s starts there, on the
// interface holding its address; workers get none. The kubeconfig and the certificate use
// the virtual IP.
func TestInstallWithKubeVIP(t *testing.T) {
	l := newLab()
	o := l.options()
	o.Endpoint, o.NodeEndpoint, o.KubeVIP = "10.0.1.100:6443", "", "10.0.1.100"
	kubeconfig, err := Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.order[:4], []string{"lab-cp-0 kube-vip", "lab-cp-0", "lab-cp-1 kube-vip", "lab-cp-1"}) {
		t.Errorf("order = %v", l.order)
	}
	for _, host := range []string{"203.0.113.12", "203.0.113.13"} {
		if len(l.machines[host].kubeVIP) != 0 {
			t.Errorf("worker %s got kube-vip", host)
		}
	}
	script := l.machines["203.0.113.10"].kubeVIP[0]
	for _, want := range []string{"kind: DaemonSet", "value: ens18", "value: 10.0.1.100", kubevip.Image, "path: " + kubevip.K3sKubeconfig, "node-role.kubernetes.io/control-plane"} {
		if !strings.Contains(script, want) {
			t.Errorf("kube-vip manifest lacks %q:\n%s", want, script)
		}
	}
	if script != l.machines["203.0.113.11"].kubeVIP[0] {
		t.Error("every control plane gets the same DaemonSet")
	}
	cfg, _ := clientcmd.Load(kubeconfig)
	if got := cfg.Clusters["default"].Server; got != "https://10.0.1.100:6443" {
		t.Errorf("server = %s", got)
	}
	if !strings.Contains(string(l.machines["203.0.113.10"].config), "- 10.0.1.100") {
		t.Error("the virtual IP belongs in the certificate")
	}
	if lines := strings.Join(Describe(o), "\n"); !strings.Contains(lines, "10.0.1.100:6443, a virtual IP kube-vip "+kubevip.Version) {
		t.Errorf("dry run:\n%s", lines)
	}
}

// One DaemonSet serves every control plane: different interface names are refused, and so is
// a virtual IP off their network, before anything changes.
func TestKubeVIPRefusals(t *testing.T) {
	cases := map[string]struct {
		vip   string
		setup func(*lab)
	}{
		"must have the same name": {"10.0.1.100", func(l *lab) {
			l.machines["203.0.113.11"].addrs = "2: eth0    inet 10.0.1.11/24 brd 10.0.1.255 scope global eth0\n"
		}},
		"isn't on its network 10.0.1.0/24": {"10.0.2.100", func(*lab) {}},
	}
	for want, c := range cases {
		t.Run(want, func(t *testing.T) {
			l := newLab()
			c.setup(l)
			o := l.options()
			o.Endpoint, o.KubeVIP = c.vip+":6443", c.vip
			if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v", err)
			}
			if len(l.order) != 0 {
				t.Errorf("changed %v despite the refusal", l.order)
			}
		})
	}
}

func TestMissingPoolOrSecret(t *testing.T) {
	l := newLab()
	o := l.options()
	o.Nodes = append(o.Nodes, providers.Node{Name: "lab-w-x", Host: "203.0.113.14", Role: "worker", Pool: "gone"})
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), `pool "gone"`) {
		t.Errorf("err = %v", err)
	}
	o = l.options()
	delete(o.Secrets, "S3_KEY")
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "S3_KEY wasn't read") {
		t.Errorf("err = %v", err)
	}
}

func TestRenderConfig(t *testing.T) {
	data, err := renderConfig(setup{cluster: "lab", role: roleAgent, name: "n1", token: "t",
		flags: []providers.Flag{
			{Name: "kubelet-arg", Value: "max-pods=10"},
			{Name: "node-label", Value: "a=1"}, {Name: "node-label", Value: "b=2"},
			{Name: "secrets-encryption", Value: "true"},
			{Name: "snapshotter", Value: "yes"},
			{Name: "etcd-snapshot-retention", Value: "10"},
		}}, func(f providers.Flag) string { return f.Value })
	if err != nil {
		t.Fatal(err)
	}
	want := marker("lab", roleAgent) + `
token: t
node-name: n1
kubelet-arg:
  - max-pods=10
node-label:
  - a=1
  - b=2
secrets-encryption: true
snapshotter: "yes"
etcd-snapshot-retention: "10"
`
	if string(data) != want {
		t.Errorf("config.yaml =\n%s\nwant\n%s", data, want)
	}
	if c, role, ok := owner(data); !ok || c != "lab" || role != roleAgent {
		t.Errorf("owner = %q %q %v", c, role, ok)
	}
}

// A machine where SELinux enforces gets k3s with SELinux on (k3s docs: the installer adds the
// policy, the setting turns it on); the others don't. A custom data-dir isn't supported under
// SELinux: refused before anything changes.
func TestSELinux(t *testing.T) {
	l := newLab()
	l.machines["203.0.113.11"].selinux = "Enforcing"  // a control plane
	l.machines["203.0.113.13"].selinux = "Enforcing"  // a worker
	l.machines["203.0.113.12"].selinux = "Permissive" // a worker
	if _, err := Install(context.Background(), l.options()); err != nil {
		t.Fatal(err)
	}
	for host, want := range map[string]bool{"203.0.113.10": false, "203.0.113.11": true, "203.0.113.12": false, "203.0.113.13": true} {
		m := l.machines[host]
		if len(m.installs) == 0 {
			t.Fatalf("%s: nothing installed", host)
		}
		if got := strings.Contains(m.installs[0], "\nselinux: true\n"); got != want {
			t.Errorf("%s: selinux in config.yaml = %v, want %v", host, got, want)
		}
	}

	// The file's own setting isn't written twice.
	data, err := renderConfig(setup{cluster: "lab", role: roleAgent, name: "n1", selinux: true, flags: []providers.Flag{{Name: "selinux", Value: "true"}}},
		func(f providers.Flag) string { return f.Value })
	if err != nil || strings.Count(string(data), "selinux") != 1 {
		t.Errorf("config.yaml:\n%s %v", data, err)
	}

	l = newLab()
	l.machines["203.0.113.10"].selinux = "Enforcing"
	o := l.options()
	o.ServerFlags = append(o.ServerFlags, providers.Flag{Name: "data-dir", Value: "/data/k3s"})
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "k3s doesn't support a custom data-dir under SELinux") || len(l.order) != 0 {
		t.Errorf("data-dir under SELinux: %v, installed on %v", err, l.order)
	}
}

func TestReleaseAndInstaller(t *testing.T) {
	if got := Release("v1.37.2"); got != "v1.37.2+k3s1" {
		t.Errorf("Release = %s", got)
	}
	if got := Release("v1.37.2+k3s2"); got != "v1.37.2+k3s2" {
		t.Errorf("Release = %s", got)
	}
	if got := installerURL("v1.37.2+k3s2"); got != "https://raw.githubusercontent.com/k3s-io/k3s/v1.37.2%2Bk3s2/install.sh" {
		t.Errorf("installer = %s", got)
	}
	if _, err := installScript([]byte("x: "+heredocEnd+"\n"), "v1", "server", ""); err == nil {
		t.Error("a config that would end the heredoc must be refused")
	}
}

// The scripts sent to machines are valid shell, and the read-only check runs on this machine.
func TestScriptsParse(t *testing.T) {
	record, err := prepare.RecordScript(nil, []prepare.Record{{Cluster: "lab", What: "k3s (worker)", Undo: "u"}})
	if err != nil {
		t.Fatal(err)
	}
	config, err := renderConfig(setup{cluster: "lab", role: roleServer, name: "n", token: "t", first: true}, func(f providers.Flag) string { return f.Value })
	if err != nil {
		t.Fatal(err)
	}
	install, err := installScript(config, "v1.37.2+k3s1", "server", record)
	if err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]string{"check": checkScript, "install": install, "root": asRoot} {
		if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
			t.Errorf("%s script doesn't parse: %v\n%s\n%s", name, err, out, s)
		}
	}
	out, err := exec.Command("sh", "-c", checkScript).CombinedOutput()
	if _, complete := parseFacts(string(out)); !complete {
		t.Errorf("check script on this machine: %v\n%s", err, out)
	}
}

// A dry run shows every machine's settings, and where secrets come from, never their value.
func TestDescribe(t *testing.T) {
	l := newLab()
	o := l.options()
	o.ServerFlags = append(o.ServerFlags, providers.Flag{Name: "token", Env: "K3S_TOKEN"})
	o.Pools = []string{"gpu", "general"}
	got := strings.Join(Describe(o), "\n")
	for _, want := range []string{"k3s v1.37.2+k3s1", "token from $K3S_TOKEN (control planes), agent-token generated by tow (workers)",
		"never shown: etcd-s3-secret-key ($S3_KEY)", "workers of pool gpu:", "- gpu=true", "cluster-init: true", "server: https://<the first control plane>:6443",
		"role labels, set through the API once the nodes joined: pool gpu node-role.kubernetes.io/gpu"} {
		if !strings.Contains(got, want) {
			t.Errorf("dry run lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "s3-secret-value") || strings.Contains(got, "written by tow") || strings.Contains(got, "token:") {
		t.Errorf("dry run shows a secret, the marker or a token line:\n%s", got)
	}
	// Printed through the redaction, every line stays readable.
	if redacted := ui.Redact(got); redacted != got {
		t.Errorf("redaction changes the dry run:\n%s", redacted)
	}
}
