package kubeadm

import (
	"cmp"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubevip"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/runtimes"
)

// cluster is what kubeadm's install leaves on a fake machine.
type cluster struct {
	adminConf   bool
	kubeadmDone string
	ca          string   // the control plane's CA, PEM
	validated   []string // the configs 'kubeadm config validate' checked
	configs     []string // the configs kubeadm init or join ran with
	runs        []string // init, join, reset+init, reset+join, in order
	tokens      []string // created with 'kubeadm token create'
	deleted     []string // token ids deleted
	uploads     []string // certificate keys 'kubeadm init phase upload-certs' was given
	certsGone   int      // times the uploaded certificates' Secret was deleted
	lbAnswer    string   // what the load balancer test finds from here: refused (default), timeout, open
	order       *order   // shared by a lab's machines: which ran kubeadm init or join, when
	name        string
	addrs       string   // what 'ip -o -4 addr show' prints
	kubeVIP     []string // the kubeconfig each kube-vip manifest written here reads
}

var vipKubeconfig = regexp.MustCompile(`(?m)^      path: (\S+)$`)

// order is the sequence kubeadm init and join ran in, across machines.
type order struct {
	mu   sync.Mutex
	runs []string
}

func (o *order) add(s string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.runs = append(o.runs, s)
}

var (
	configDoc = regexp.MustCompile(`(?s)cat > "\$cfg" <<'TOW_EOF'\n(.*?)TOW_EOF\n`)
	recordDoc = regexp.MustCompile(`(?s)cat > /etc/tow/prepared.json <<'TOW_EOF'\n(.*?)\nTOW_EOF`)
	doneLine  = regexp.MustCompile(`echo '([^']*)' > /etc/tow/kubeadm-done`)
	tokenLine = regexp.MustCompile(`(?m)token: (\S+)$`)
)

// install answers the install's scripts as kubeadm would; ok is false for any other script.
func (f *fakeMachine) install(script string) (out string, err error, ok bool) {
	cfg := ""
	if m := configDoc.FindStringSubmatch(script); m != nil {
		cfg = m[1]
	}
	failing := f.fail != "" && strings.Contains(script, f.fail)
	if strings.Contains(script, kubevip.StaticPodFile) {
		f.kubeVIP = append(f.kubeVIP, vipKubeconfig.FindStringSubmatch(script)[1])
	}
	switch {
	case script == "ip -o -4 addr show\n":
		return f.addrs, nil, true
	case strings.HasPrefix(script, "set -e\nnew=$(mktemp)") && strings.Contains(script, kubevip.StaticPodFile):
		if f.order != nil {
			f.order.add(f.name + " kube-vip")
		}
	case strings.Contains(script, "kubeadm config validate"):
		f.validated = append(f.validated, cfg)
		if failing {
			return "strict decoding error: unknown field \"podSubnett\"\n", errors.New("exit status 1"), true
		}
	case strings.Contains(script, "kubeadm init phase upload-certs --upload-certs"):
		f.uploads = append(f.uploads, regexp.MustCompile(`--certificate-key '([0-9a-f]{64})'`).FindStringSubmatch(script)[1])
	case script == deleteCertsScript:
		f.certsGone++
	case strings.Contains(script, "/dev/tcp/") && !strings.Contains(script, "kubeadm join"): // a join waits for kube-vip's address first
		return cmp.Or(f.lbAnswer, "refused") + "\n", nil, true
	case strings.Contains(script, "kubeadm init") || strings.Contains(script, "kubeadm join"):
		if !strings.Contains(script, "rm -f /etc/tow/kubeadm-done") || !strings.Contains(script, "trap 'rm -f \"$cfg\"' EXIT") {
			return "", errors.New("the run must forget an earlier finish and remove its config"), true
		}
		f.kubeadmDone = ""
		if m := recordDoc.FindStringSubmatch(script); m != nil {
			f.record = []byte(m[1])
		}
		run := "join"
		if strings.Contains(script, "kubeadm init --skip-token-print") {
			run = "init"
		}
		if strings.Contains(script, "kubeadm reset -f --cri-socket") {
			f.kubeletConf, f.adminConf = false, false
			run = "reset+" + run
		}
		f.runs, f.configs = append(f.runs, run), append(f.configs, cfg)
		if f.order != nil {
			f.order.add(f.name + " " + run)
		}
		// kubeadm writes its files and starts the kubelet before it waits for the control plane;
		// a control plane (init, or a join with a controlPlane part) gets admin.conf.
		f.kubeletConf, f.adminConf = true, strings.HasSuffix(run, "init") || strings.Contains(cfg, "\ncontrolPlane:\n")
		if failing {
			return "[wait-control-plane] Waiting for the kubelet to boot up the control plane\nerror execution phase wait-control-plane: timed out\n", errors.New("exit status 1"), true
		}
		f.kubeadmDone = doneLine.FindStringSubmatch(script)[1]
	case strings.Contains(script, "kubeadm token create"):
		f.tokens = append(f.tokens, regexp.MustCompile(`token create '([^']+)' --ttl 1h`).FindStringSubmatch(script)[1])
	case strings.Contains(script, "kubeadm token delete"):
		f.deleted = append(f.deleted, regexp.MustCompile(`token delete '([^']+)'`).FindStringSubmatch(script)[1])
	case script == "cat "+caCert:
		return f.ca, nil, true
	case script == "cat "+adminConf:
		return adminKubeconfig, nil, true
	case strings.HasPrefix(script, "journalctl -u kubelet"):
		return "E1110 kubelet.go:2412] \"Error getting node\" err=\"node \\\"lab-cp-0\\\" not found\"\n", nil, true
	case strings.HasPrefix(script, "crictl --runtime-endpoint 'unix:///var/run/containerd/containerd.sock' ps -a"):
		return "CONTAINER  IMAGE  STATE  NAME\nabc123  def456  Exited  kube-apiserver\n", nil, true
	case strings.HasPrefix(script, "for id in $(crictl --runtime-endpoint 'unix:///var/run/containerd/containerd.sock' ps -a --state exited -q"):
		return "== abc123\nE1110 run.go:72] \"command failed\" err=\"failed to listen on 0.0.0.0:6443: address already in use\"\n", nil, true
	default:
		return "", nil, false
	}
	return "", nil, true
}

const adminKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: lab, cluster: {server: "https://10.0.0.10:6443"}}]
contexts: [{name: kubernetes-admin@lab, context: {cluster: lab, user: kubernetes-admin}}]
current-context: kubernetes-admin@lab
users: [{name: kubernetes-admin, user: {token: abc}}]
`

// newCA is a cluster CA as kubeadm makes one, and the hash 'kubeadm join' checks it by.
func newCA(t *testing.T) (pemCert, hash string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "kubernetes"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), "sha256:" + hex.EncodeToString(sum[:])
}

// A control plane and two workers of different pools, each with a public and a private address.
var clusterNodes = []providers.Node{
	{Name: "lab-cp-0", Host: "203.0.113.10", PrivateIP: "10.0.0.10", Role: "control-plane", User: "u"},
	{Name: "lab-gpu-0", Host: "203.0.113.20", PrivateIP: "10.0.0.20", Role: "worker", Pool: "gpu", User: "u"},
	{Name: "lab-workers-0", Host: "203.0.113.21", PrivateIP: "10.0.0.21", Role: "worker", Pool: "workers", User: "u"},
}

// fakeAPI is the cluster's API as seen from this machine.
type fakeAPI struct {
	reachable  bool
	registered [][]string
	labels     map[string]map[string]string
	spread     []string
}

func (a *fakeAPI) SpreadOverNodes(_ context.Context, namespace, name string) error {
	a.spread = append(a.spread, namespace+"/"+name)
	return nil
}

func (a *fakeAPI) WaitReachable(context.Context) error { a.reachable = true; return nil }
func (a *fakeAPI) WaitNodes(_ context.Context, names []string, ready bool) error {
	if ready {
		return errors.New("the install doesn't wait for Ready: the CNI comes after it")
	}
	a.registered = append(a.registered, names)
	return nil
}
func (a *fakeAPI) Label(_ context.Context, node string, labels map[string]string) error {
	if len(labels) > 0 {
		a.labels[node] = labels
	}
	return nil
}

// clusterLab runs Install against fake machines and a fake API.
type clusterLab struct {
	machines   map[string]*fakeMachine
	api        *fakeAPI
	kubeconfig []byte // what the API was reached with
	caHash     string
}

func newClusterLab(t *testing.T) *clusterLab {
	t.Helper()
	l := &clusterLab{machines: map[string]*fakeMachine{}, api: &fakeAPI{labels: map[string]map[string]string{}}}
	for _, n := range clusterNodes {
		l.machines[n.Host] = newMachine()
	}
	var ca string
	ca, l.caHash = newCA(t)
	l.cp().ca = ca
	return l
}

func (l *clusterLab) cp() *fakeMachine      { return l.machines["203.0.113.10"] }
func (l *clusterLab) gpu() *fakeMachine     { return l.machines["203.0.113.20"] }
func (l *clusterLab) workers() *fakeMachine { return l.machines["203.0.113.21"] }

// settings is what the file says of each group: the gpu pool has labels, a taint, a kubelet
// flag and a role.
func settings(n providers.Node) (NodeSettings, bool) {
	switch {
	case n.Role == config.RoleControlPlane:
		return NodeSettings{}, true
	case n.Pool == "gpu":
		return NodeSettings{
			KubeletArgs: []providers.Flag{{Name: "max-pods", Value: "50"}},
			Labels:      map[string]string{"zone": "a", "accelerator": "nvidia"},
			Taints:      []config.Taint{{Key: "gpu", Value: "true", Effect: "NoSchedule"}},
			RoleLabels:  map[string]string{"node-role.kubernetes.io/gpu": ""},
		}, true
	case n.Pool == "workers":
		return NodeSettings{RoleLabels: map[string]string{"node-role.kubernetes.io/worker": ""}}, true
	}
	return NodeSettings{}, false
}

func (l *clusterLab) options(o Options) Options {
	o.Cluster, o.Version, o.Nodes = "lab", "v1.37.2", clusterNodes
	if o.Runtime == "" {
		o.Runtime = runtimes.Containerd
	}
	o.PodCIDR, o.ServiceCIDR, o.DNSDomain = "10.244.0.0/16", "10.96.0.0/12", "cluster.local"
	if o.Node == nil {
		o.Node = settings
	}
	o.Connect = func(_ context.Context, n providers.Node, _ string) (prepare.Runner, error) {
		return l.machines[n.Host], nil
	}
	o.API = func(kc []byte) (API, error) { l.kubeconfig = kc; return l.api, nil }
	return o
}

const initConfigWant = `apiVersion: kubeadm.k8s.io/v1beta4
kind: InitConfiguration
bootstrapTokens:
  - token: TOKEN
    description: 'tow: nodes joining; deleted once they joined'
    ttl: 1h0m0s
    usages:
      - signing
      - authentication
    groups:
      - system:bootstrappers:kubeadm:default-node-token
localAPIEndpoint:
  advertiseAddress: 10.0.0.10
  bindPort: 6443
nodeRegistration:
  name: lab-cp-0
  criSocket: unix:///var/run/containerd/containerd.sock
  kubeletExtraArgs:
    - name: node-ip
      value: 10.0.0.10
---
apiVersion: kubeadm.k8s.io/v1beta4
kind: ClusterConfiguration
clusterName: lab
kubernetesVersion: v1.37.2
networking:
  podSubnet: 10.244.0.0/16
  serviceSubnet: 10.96.0.0/12
  dnsDomain: cluster.local
apiServer:
  certSANs:
    - api.lab.example
    - 203.0.113.10
    - 10.0.0.10
  extraArgs:
    - name: audit-log-maxage
      value: "30"
    - name: oidc-client-secret
      value: read-from-the-environment
etcd:
  local:
    extraArgs:
      - name: quota-backend-bytes
        value: "8589934592"
---
apiVersion: kubelet.config.k8s.io/v1beta1
kind: KubeletConfiguration
cgroupDriver: systemd
`

const joinConfigWant = `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    token: TOKEN
    apiServerEndpoint: 10.0.0.10:6443
    caCertHashes:
      - HASH
nodeRegistration:
  name: lab-gpu-0
  criSocket: unix:///var/run/containerd/containerd.sock
  taints:
    - key: gpu
      value: "true"
      effect: NoSchedule
  kubeletExtraArgs:
    - name: node-ip
      value: 10.0.0.20
    - name: node-labels
      value: accelerator=nvidia,zone=a
    - name: max-pods
      value: "50"
`

func TestInstall(t *testing.T) {
	l := newClusterLab(t)
	o := l.options(Options{
		Endpoint: "203.0.113.10:6443", CertSANs: []string{"api.lab.example"},
		APIServer: []providers.Flag{{Name: "audit-log-maxage", Value: "30"}, {Name: "oidc-client-secret", Env: "OIDC_SECRET"}},
		Etcd:      []providers.Flag{{Name: "quota-backend-bytes", Value: "8589934592"}},
		Secrets:   map[string]string{"OIDC_SECRET": "read-from-the-environment"},
	})
	kubeconfig, err := Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}

	// init on the control plane, then each worker joins; each config was validated first.
	cp, gpu, workers := l.cp(), l.gpu(), l.workers()
	if !slices.Equal(cp.runs, []string{"init"}) || !slices.Equal(gpu.runs, []string{"join"}) || !slices.Equal(workers.runs, []string{"join"}) {
		t.Fatalf("runs: cp %v, gpu %v, workers %v", cp.runs, gpu.runs, workers.runs)
	}
	m := tokenLine.FindStringSubmatch(cp.configs[0])
	if m == nil {
		t.Fatalf("no token in the init config:\n%s", cp.configs[0])
	}
	token := m[1]
	if !regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`).MatchString(token) {
		t.Errorf("token %q isn't in kubeadm's form", token)
	}
	if got := strings.ReplaceAll(cp.configs[0], token, "TOKEN"); got != initConfigWant {
		t.Errorf("init config:\n%s\nwant:\n%s", got, initConfigWant)
	}
	want := strings.ReplaceAll(joinConfigWant, "HASH", l.caHash)
	if got := strings.ReplaceAll(gpu.configs[0], token, "TOKEN"); got != want {
		t.Errorf("join config:\n%s\nwant:\n%s", got, want)
	}
	// The workers' configs were validated before the CA existed: with a stand-in hash.
	if len(gpu.validated) != 1 || !strings.Contains(gpu.validated[0], "sha256:"+strings.Repeat("0", 64)) || cp.validated[0] != cp.configs[0] {
		t.Errorf("validated: cp %d, gpu %v", len(cp.validated), gpu.validated)
	}
	if !strings.Contains(workers.configs[0], "taints: []") || !strings.Contains(workers.configs[0], "name: lab-workers-0") {
		t.Errorf("a worker without taints says so:\n%s", workers.configs[0])
	}

	// The token is deleted once the workers joined; recorded runs; the API reached and labeled.
	id, _, _ := strings.Cut(token, ".")
	if !slices.Equal(cp.deleted, []string{id}) || len(cp.tokens) != 0 {
		t.Errorf("tokens created %v, deleted %v", cp.tokens, cp.deleted)
	}
	if got := cp.recorded(); !slices.Equal(got, []string{"lab: kubeadm (control plane)"}) {
		t.Errorf("control plane recorded %v", got)
	}
	if got := gpu.recorded(); !slices.Equal(got, []string{"lab: kubeadm (worker)"}) {
		t.Errorf("worker recorded %v", got)
	}
	if !strings.Contains(string(kubeconfig), "server: https://203.0.113.10:6443") || string(l.kubeconfig) != string(kubeconfig) {
		t.Errorf("kubeconfig:\n%s", kubeconfig)
	}
	if !l.api.reachable || len(l.api.registered) != 1 || !slices.Equal(l.api.registered[0], []string{"lab-cp-0", "lab-gpu-0", "lab-workers-0"}) {
		t.Errorf("api: %+v", l.api)
	}
	_, gpuRole := l.api.labels["lab-gpu-0"]["node-role.kubernetes.io/gpu"]
	_, workerRole := l.api.labels["lab-workers-0"]["node-role.kubernetes.io/worker"]
	if len(l.api.labels) != 2 || !gpuRole || !workerRole {
		t.Errorf("labels: %v", l.api.labels)
	}
	if !slices.Equal(l.api.spread, []string{"kube-system/coredns"}) {
		t.Errorf("CoreDNS not spread over the nodes: %v", l.api.spread)
	}

	// Every script the install ran is valid sh.
	for host, m := range l.machines {
		for _, s := range m.scripts {
			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(s)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s: %v\n%s\n%s", host, err, out, s)
			}
		}
	}

	// Again: every machine finished for this cluster; nothing runs, no token is made.
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(cp.runs)+len(gpu.runs)+len(workers.runs) != 3 || len(cp.tokens) != 0 || len(cp.deleted) != 1 {
		t.Errorf("a second run changed things: runs %v %v %v, tokens %v, deleted %v", cp.runs, gpu.runs, workers.runs, cp.tokens, cp.deleted)
	}
}

// The control plane's taint: kubeadm's own unless workloads may run there; the file's taints
// come on top.
func TestControlPlaneTaints(t *testing.T) {
	cp := clusterNodes[0]
	cases := []struct {
		name        string
		schedulable bool
		taints      []config.Taint
		want        string // the taints part of the config; "" = none written
	}{
		{"kubeadm's default", false, nil, ""},
		{"schedulable", true, nil, "  taints: []\n"},
		{"schedulable, the file's", true, []config.Taint{{Key: "a", Effect: "NoExecute"}}, "  taints:\n    - key: a\n      effect: NoExecute\n"},
		{"the file's on top", false, []config.Taint{{Key: "a", Value: "b", Effect: "PreferNoSchedule"}},
			"  taints:\n    - key: node-role.kubernetes.io/control-plane\n      effect: NoSchedule\n    - key: a\n      value: b\n      effect: PreferNoSchedule\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := Options{Cluster: "lab", Version: "v1.37.2", Runtime: runtimes.CRIO, Schedulable: c.schedulable}
			b, err := o.initConfig(cp, NodeSettings{Taints: c.taints}, "abcdef.0123456789abcdef", "")
			if err != nil {
				t.Fatal(err)
			}
			got := string(b)
			if !strings.Contains(got, "criSocket: unix:///var/run/crio/crio.sock\n") {
				t.Errorf("CRI-O's socket:\n%s", got)
			}
			if c.want == "" && strings.Contains(got, "taints:") || c.want != "" && !strings.Contains(got, "criSocket: unix:///var/run/crio/crio.sock\n"+c.want) {
				t.Errorf("taints:\n%s\nwant:\n%s", got, c.want)
			}
		})
	}
}

// Without kube-proxy when the CNI replaces it; the user's load balancer goes into the config.
func TestInitConfigOptions(t *testing.T) {
	o := Options{Cluster: "lab", Version: "v1.37.2", SkipKubeProxy: true, ControlPlaneEndpoint: "api.lab.example:6443", Endpoint: "api.lab.example:6443", Nodes: clusterNodes}
	b, err := o.initConfig(clusterNodes[0], NodeSettings{}, "abcdef.0123456789abcdef", "")
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"skipPhases:\n  - addon/kube-proxy\n", "controlPlaneEndpoint: api.lab.example:6443\n", "certSANs:\n    - api.lab.example\n    - 203.0.113.10\n    - 10.0.0.10\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("config lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "serverTLSBootstrap") {
		t.Errorf("kubelets sign their own serving certificates unless metrics-server is asked for:\n%s", got)
	}

	// metrics-server: the cluster signs the kubelets' serving certificates.
	o.ServerTLSBootstrap = true
	if b, _ := o.initConfig(clusterNodes[0], NodeSettings{}, "abcdef.0123456789abcdef", ""); !strings.HasSuffix(string(b), "cgroupDriver: systemd\nserverTLSBootstrap: true\n") {
		t.Errorf("kubelet configuration:\n%s", b)
	}
}

func TestNewToken(t *testing.T) {
	form := regexp.MustCompile(`^[a-z0-9]{6}\.[a-z0-9]{16}$`)
	seen := map[string]bool{}
	for range 200 {
		tok, err := newToken()
		if err != nil || !form.MatchString(tok) || seen[tok] {
			t.Fatalf("token %q, %v (seen before: %v)", tok, err, seen[tok])
		}
		seen[tok] = true
	}
}

// A worker that failed to join is reset and tried again; the others are left alone, and a
// new token is made for it.
func TestInstallJoinFailureResumes(t *testing.T) {
	l := newClusterLab(t)
	o := l.options(Options{})
	l.gpu().fail = "kubeadm join"
	_, err := Install(context.Background(), o)
	for _, want := range []string{"203.0.113.20 (lab-gpu-0): kubeadm join failed", "error execution phase wait-control-plane", "Last lines of the kubelet's log",
		"crictl ps -a", "kube-apiserver", "Last lines of the containers that stopped (at most 3):\n  == abc123\n  E1110", "address already in use",
		"The next 'tow apply' resets this machine (kubeadm reset) and tries again"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, err)
		}
	}
	for _, s := range l.gpu().scripts { // the failure's own scripts too
		cmd := exec.Command("sh", "-n")
		cmd.Stdin = strings.NewReader(s)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%v\n%s\n%s", err, out, s)
		}
	}
	if len(l.cp().deleted) != 1 {
		t.Errorf("the token is deleted even when a join fails: %v", l.cp().deleted)
	}

	l.gpu().fail = ""
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	cp, gpu, workers := l.cp(), l.gpu(), l.workers()
	if !slices.Equal(cp.runs, []string{"init"}) || !slices.Equal(gpu.runs, []string{"join", "reset+join"}) || !slices.Equal(workers.runs, []string{"join"}) {
		t.Errorf("runs: cp %v, gpu %v, workers %v", cp.runs, gpu.runs, workers.runs)
	}
	if len(cp.tokens) != 1 || len(cp.deleted) != 2 || !strings.Contains(gpu.configs[1], "token: "+cp.tokens[0]) {
		t.Errorf("a new token for the retry: created %v, deleted %v", cp.tokens, cp.deleted)
	}
	if got := gpu.recorded(); !slices.Equal(got, []string{"lab: kubeadm (worker)"}) {
		t.Errorf("recorded once: %v", got)
	}
}

// kubeadm leaves its files and a running kubelet when init fails while waiting for the
// control plane: the next run still resets and retries it.
func TestInstallInitFailureResumes(t *testing.T) {
	l := newClusterLab(t)
	o := l.options(Options{})
	l.cp().fail = "kubeadm init"
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "203.0.113.10 (lab-cp-0): kubeadm init failed") {
		t.Fatalf("err = %v", err)
	}
	if len(l.gpu().runs)+len(l.workers().runs) != 0 {
		t.Error("no worker joins a control plane that failed")
	}
	if cp := l.cp(); !cp.kubeletConf || !cp.adminConf || cp.kubeadmDone != "" {
		t.Fatalf("a failed init leaves kubeadm's files but no finish: %+v", cp.cluster)
	}

	l.cp().fail = ""
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(l.cp().runs, []string{"init", "reset+init"}) || !slices.Equal(l.gpu().runs, []string{"join"}) {
		t.Errorf("runs: cp %v, gpu %v", l.cp().runs, l.gpu().runs)
	}
}

// A control plane kubeadm initializes again is a new cluster: every worker joins it again,
// not only the ones that hadn't finished.
func TestInstallReinitRejoinsWorkers(t *testing.T) {
	l := newClusterLab(t)
	o := l.options(Options{})
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	l.cp().kubeadmDone = "" // its finish is lost: kubeadm runs there again
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	for host, m := range l.machines {
		want := []string{"join", "reset+join"}
		if host == "203.0.113.10" {
			want = []string{"init", "reset+init"}
		}
		if !slices.Equal(m.runs, want) {
			t.Errorf("%s runs %v, want %v", host, m.runs, want)
		}
	}
	// The workers trust the new CA (same fake CA here, so: the hash they were given).
	if !strings.Contains(l.gpu().configs[1], l.caHash) {
		t.Errorf("rejoin config:\n%s", l.gpu().configs[1])
	}
}

// The token tow made is deleted on every way out, even before any worker tried to join.
func TestInstallTokenDeletedOnEarlyFailure(t *testing.T) {
	l := newClusterLab(t)
	o := l.options(Options{})
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	l.gpu().kubeadmDone = ""        // a worker to join again: a token is made for it
	l.cp().ca = "not a certificate" // then reading the CA fails
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "isn't a PEM certificate") {
		t.Fatalf("err = %v", err)
	}
	cp := l.cp()
	if len(cp.tokens) != 1 || len(cp.deleted) != 2 || cp.deleted[1] != strings.Split(cp.tokens[0], ".")[0] {
		t.Errorf("created %v, deleted %v", cp.tokens, cp.deleted)
	}
}

// Three control planes behind a load balancer, and a worker.
var haNodes = []providers.Node{
	{Name: "ha-cp-0", Host: "203.0.113.10", PrivateIP: "10.0.0.10", Role: "control-plane", User: "u"},
	{Name: "ha-cp-1", Host: "203.0.113.11", PrivateIP: "10.0.0.11", Role: "control-plane", User: "u"},
	{Name: "ha-cp-2", Host: "203.0.113.12", PrivateIP: "10.0.0.12", Role: "control-plane", User: "u"},
	{Name: "ha-workers-0", Host: "203.0.113.20", PrivateIP: "10.0.0.20", Role: "worker", Pool: "workers", User: "u"},
}

func newHALab(t *testing.T) (*clusterLab, *order) {
	t.Helper()
	l := &clusterLab{machines: map[string]*fakeMachine{}, api: &fakeAPI{labels: map[string]map[string]string{}}}
	ord := &order{}
	for _, n := range haNodes {
		m := newMachine()
		m.order, m.name = ord, n.Name
		m.addrs = "1: lo    inet 127.0.0.1/8 scope host lo\n2: ens18    inet " + n.PrivateIP + "/24 brd 10.0.0.255 scope global ens18\n"
		l.machines[n.Host] = m
	}
	var ca string
	ca, l.caHash = newCA(t)
	l.cp().ca = ca
	return l, ord
}

func (l *clusterLab) haOptions(o Options) Options {
	o = l.options(o)
	o.Nodes, o.ControlPlaneEndpoint, o.Endpoint = haNodes, "api-lb.example:6443", "api-lb.example:6443"
	return o
}

const haJoinWant = `apiVersion: kubeadm.k8s.io/v1beta4
kind: JoinConfiguration
discovery:
  bootstrapToken:
    token: TOKEN
    apiServerEndpoint: 10.0.0.10:6443
    caCertHashes:
      - HASH
nodeRegistration:
  name: ha-cp-1
  criSocket: unix:///var/run/containerd/containerd.sock
  kubeletExtraArgs:
    - name: node-ip
      value: 10.0.0.11
controlPlane:
  localAPIEndpoint:
    advertiseAddress: 10.0.0.11
    bindPort: 6443
  certificateKey: KEY
`

// Several control planes, as kubeadm's HA page sets them up: init uploads the certificates,
// encrypted with a key tow makes; the others join one at a time with that key, then the
// workers; the certificates and the token are deleted once used.
func TestInstallSeveralControlPlanes(t *testing.T) {
	l, ord := newHALab(t)
	o := l.haOptions(Options{})
	kubeconfig, err := Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"ha-cp-0 init", "ha-cp-1 join", "ha-cp-2 join", "ha-workers-0 join"}; !slices.Equal(ord.runs, want) {
		t.Fatalf("order %v, want %v", ord.runs, want)
	}
	cp0, cp1 := l.cp(), l.machines["203.0.113.11"]
	uploads := func(s string) bool {
		return strings.Contains(s, "kubeadm init --skip-token-print --upload-certs --config")
	}
	if !slices.ContainsFunc(cp0.scripts, uploads) {
		t.Error("init doesn't upload the certificates")
	}
	m := regexp.MustCompile(`(?m)^certificateKey: ([0-9a-f]{64})$`).FindStringSubmatch(cp0.configs[0])
	if m == nil {
		t.Fatalf("no certificate key in the init config:\n%s", cp0.configs[0])
	}
	key, token := m[1], tokenLine.FindStringSubmatch(cp0.configs[0])[1]
	for _, want := range []string{"controlPlaneEndpoint: api-lb.example:6443\n",
		"certSANs:\n    - api-lb.example\n    - 203.0.113.10\n    - 10.0.0.10\n    - 203.0.113.11\n    - 10.0.0.11\n    - 203.0.113.12\n    - 10.0.0.12\n"} {
		if !strings.Contains(cp0.configs[0], want) {
			t.Errorf("init config lacks %q:\n%s", want, cp0.configs[0])
		}
	}
	want := strings.NewReplacer("TOKEN", token, "HASH", l.caHash, "KEY", key).Replace(haJoinWant)
	if cp1.configs[0] != want {
		t.Errorf("control plane join config:\n%s\nwant:\n%s", cp1.configs[0], want)
	}
	if w := l.machines["203.0.113.20"].configs[0]; strings.Contains(w, "controlPlane:") || strings.Contains(w, key) {
		t.Errorf("a worker gets no control plane part, nor the key:\n%s", w)
	}
	if cp0.certsGone != 1 || len(cp0.deleted) != 1 {
		t.Errorf("certificates deleted %d times, tokens deleted %v", cp0.certsGone, cp0.deleted)
	}
	if got := cp1.recorded(); !slices.Equal(got, []string{"lab: kubeadm (control plane)"}) {
		t.Errorf("recorded %v", got)
	}
	if !strings.Contains(string(kubeconfig), "server: https://api-lb.example:6443") {
		t.Errorf("kubeconfig:\n%s", kubeconfig)
	}
	if len(l.api.registered) != 1 || len(l.api.registered[0]) != 4 {
		t.Errorf("registered %v", l.api.registered)
	}

	// Again: nothing runs, nothing is uploaded or deleted.
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if len(ord.runs) != 4 || len(cp0.uploads) != 0 || cp0.certsGone != 1 || len(cp0.tokens) != 0 {
		t.Errorf("a second run changed things: %v, uploads %v, certs deleted %d", ord.runs, cp0.uploads, cp0.certsGone)
	}
}

// A control plane that failed to join is reset and joins on the next run, with the
// certificates uploaded again (kubeadm deletes them after two hours) and a new token; the
// workers wait for every control plane.
func TestInstallControlPlaneJoinResumes(t *testing.T) {
	l, ord := newHALab(t)
	o := l.haOptions(Options{})
	cp0, cp2 := l.cp(), l.machines["203.0.113.12"]
	cp2.fail = "kubeadm join"
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "203.0.113.12 (ha-cp-2): kubeadm join failed") {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"ha-cp-0 init", "ha-cp-1 join", "ha-cp-2 join"}; !slices.Equal(ord.runs, want) {
		t.Errorf("order %v, want %v: no worker joins before every control plane did", ord.runs, want)
	}
	if cp0.certsGone != 1 || len(cp0.deleted) != 1 {
		t.Errorf("after a failure the certificates (%d) and the token (%v) are deleted too", cp0.certsGone, cp0.deleted)
	}

	cp2.fail = ""
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ha-cp-0 init", "ha-cp-1 join", "ha-cp-2 join", "ha-cp-2 reset+join", "ha-workers-0 join"}; !slices.Equal(ord.runs, want) {
		t.Errorf("order %v, want %v", ord.runs, want)
	}
	if len(cp0.uploads) != 1 || !strings.Contains(cp2.configs[1], "certificateKey: "+cp0.uploads[0]) || cp0.certsGone != 2 || len(cp0.tokens) != 1 {
		t.Errorf("uploads %v, certificates deleted %d, tokens %v", cp0.uploads, cp0.certsGone, cp0.tokens)
	}
	scriptsParse(t, l)
}

// With an API name (GCP), each machine resolves it in /etc/hosts before kubeadm runs there: the
// first control plane to itself, a joining one to the first, then to itself once it joined; a
// worker to the load balancer. delete takes the line away with Kubernetes.
func TestInstallWithAPIName(t *testing.T) {
	l, ord := newHALab(t)
	o := l.haOptions(Options{})
	o.ControlPlaneEndpoint, o.APIName, o.APIAddress = "api.ha.internal:6443", "api.ha.internal", "10.0.0.5"
	if _, err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ha-cp-0 init", "ha-cp-1 join", "ha-cp-2 join", "ha-workers-0 join"}; !slices.Equal(ord.runs, want) {
		t.Fatalf("order %v, want %v", ord.runs, want)
	}
	kubeadmRun := func(f *fakeMachine) string {
		for _, s := range f.scripts {
			if strings.Contains(s, "kubeadm init --") || strings.Contains(s, "kubeadm join --") {
				return s
			}
		}
		t.Fatalf("%s ran no kubeadm", f.name)
		return ""
	}
	hosts := func(ip string) string { return prepare.HostsScript("api.ha.internal", ip) }
	for host, want := range map[string][]string{
		"203.0.113.10": {hosts("10.0.0.10"), "kubeadm init"},
		"203.0.113.11": {hosts("10.0.0.10"), "kubeadm join", hosts("10.0.0.11")},
		"203.0.113.12": {hosts("10.0.0.10"), "kubeadm join", hosts("10.0.0.12")},
		"203.0.113.20": {hosts("10.0.0.5"), "kubeadm join"},
	} {
		s := kubeadmRun(l.machines[host])
		at := 0
		for _, w := range want {
			i := strings.Index(s[at:], w)
			if i < 0 {
				t.Errorf("%s: %q missing, or out of order, in:\n%s", host, w, s)
				break
			}
			at += i + len(w)
		}
		if host == "203.0.113.20" && strings.Count(s, "# tow API name' >> /etc/hosts") != 1 {
			t.Errorf("a worker's line changes once:\n%s", s)
		}
	}
	if rec := string(l.machines["203.0.113.11"].record); !strings.Contains(rec, "# tow API name") {
		t.Errorf("the record doesn't take the line away:\n%s", rec)
	}
	if !strings.Contains(l.cp().configs[0], "controlPlaneEndpoint: api.ha.internal:6443\n") {
		t.Errorf("init config:\n%s", l.cp().configs[0])
	}
	scriptsParse(t, l)
}

// Your own load balancer must reach the first control plane before init, as kubeadm's HA page
// tests it: a timeout stops everything; "connection refused" (no API server yet) is fine.
func TestInstallChecksYourLoadBalancer(t *testing.T) {
	l, ord := newHALab(t)
	l.cp().lbAnswer = "timeout"
	_, err := Install(context.Background(), l.haOptions(Options{CheckEndpoint: true}))
	if err == nil || !strings.Contains(err.Error(), "can't reach your load balancer api-lb.example:6443 (no answer within 5 s); nothing was changed") {
		t.Fatalf("err = %v", err)
	}
	if len(ord.runs) != 0 {
		t.Errorf("ran %v", ord.runs)
	}

	l.cp().lbAnswer = "refused"
	if _, err := Install(context.Background(), l.haOptions(Options{CheckEndpoint: true})); err != nil {
		t.Fatal(err)
	}
	scriptsParse(t, l)

	// Not tested when tow made the load balancer.
	l, _ = newHALab(t)
	if _, err := Install(context.Background(), l.haOptions(Options{})); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(l.cp().scripts, func(s string) bool { return strings.Contains(s, "/dev/tcp/") }) {
		t.Error("tested a load balancer tow made")
	}
}

// kube-vip, as its static pod page says: on the first control plane before kubeadm init,
// reading super-admin.conf, then admin.conf once init is done; on the others once they
// joined; never on workers. The nodes and the kubeconfig use the virtual IP.
func TestInstallWithKubeVIP(t *testing.T) {
	l, ord := newHALab(t)
	o := l.haOptions(Options{})
	o.ControlPlaneEndpoint, o.Endpoint, o.KubeVIP = "10.0.0.100:6443", "10.0.0.100:6443", "10.0.0.100"
	cp0 := l.cp()
	cp0.fail = "kubeadm init" // the first try fails: the retry resets, then puts kube-vip back
	if _, err := Install(context.Background(), o); err == nil {
		t.Fatal("want the init failure")
	}
	cp0.fail = ""
	kubeconfig, err := Install(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ha-cp-0 init", "ha-cp-0 reset+init", "ha-cp-0 kube-vip", "ha-cp-1 join", "ha-cp-2 join",
		"ha-cp-0 kube-vip", "ha-cp-1 kube-vip", "ha-cp-2 kube-vip", "ha-workers-0 join"}
	if !slices.Equal(ord.runs, want) {
		t.Errorf("order %v\nwant  %v", ord.runs, want)
	}
	if want := []string{kubevip.SuperAdminConf, kubevip.SuperAdminConf, kubevip.AdminConf, kubevip.AdminConf}; !slices.Equal(cp0.kubeVIP, want) {
		t.Errorf("first control plane's kube-vip read %v, want %v", cp0.kubeVIP, want)
	}
	if got := l.machines["203.0.113.11"].kubeVIP; !slices.Equal(got, []string{kubevip.AdminConf}) {
		t.Errorf("second control plane's kube-vip read %v", got)
	}
	if got := l.machines["203.0.113.20"].kubeVIP; len(got) != 0 {
		t.Errorf("a worker got kube-vip: %v", got)
	}
	var retry string
	for _, s := range cp0.scripts {
		if strings.Contains(s, "kubeadm reset") {
			retry = s
		}
	}
	reset, manifest, init := strings.Index(retry, "kubeadm reset"), strings.Index(retry, kubevip.StaticPodFile), strings.Index(retry, "kubeadm init --skip-token-print")
	if reset < 0 || !(reset < manifest && manifest < init) || !strings.Contains(retry, "value: ens18") || !strings.Contains(retry, "value: 10.0.0.100") {
		t.Errorf("kube-vip goes in after the reset, before init:\n%s", retry)
	}
	if !strings.Contains(cp0.configs[1], "controlPlaneEndpoint: 10.0.0.100:6443\n") || !strings.Contains(cp0.configs[1], "certSANs:\n    - 10.0.0.100\n") {
		t.Errorf("init config:\n%s", cp0.configs[1])
	}
	if !strings.Contains(string(kubeconfig), "server: https://10.0.0.100:6443") {
		t.Errorf("kubeconfig:\n%s", kubeconfig)
	}
	// Every join waits for the virtual IP first: kube-vip restarts on the first control
	// plane right after init, to read admin.conf.
	for _, host := range []string{"203.0.113.11", "203.0.113.20"} {
		var join string
		for _, s := range l.machines[host].scripts {
			if strings.Contains(s, "kubeadm join") {
				join = s
			}
		}
		if wait := strings.Index(join, "/dev/tcp/10.0.0.100/6443"); wait < 0 || wait > strings.Index(join, "kubeadm join") {
			t.Errorf("%s joins without waiting for the virtual IP:\n%s", host, join)
		}
	}
	scriptsParse(t, l)
}

// The virtual IP must be on the control planes' network; checked before anything changes.
func TestKubeVIPOffTheNetwork(t *testing.T) {
	l, ord := newHALab(t)
	o := l.haOptions(Options{})
	o.ControlPlaneEndpoint, o.Endpoint, o.KubeVIP = "10.9.0.100:6443", "10.9.0.100:6443", "10.9.0.100"
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), "isn't on its network 10.0.0.0/24 (ens18)") {
		t.Fatalf("err = %v", err)
	}
	if len(ord.runs) != 0 {
		t.Errorf("ran %v", ord.runs)
	}
}

// Every script a lab's machines ran is valid sh.
func scriptsParse(t *testing.T, l *clusterLab) {
	t.Helper()
	for host, m := range l.machines {
		for _, s := range m.scripts {
			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(s)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("%s: %v\n%s\n%s", host, err, out, s)
			}
		}
	}
}

// Every config is checked by kubeadm before anything starts.
func TestInstallValidatesFirst(t *testing.T) {
	l := newClusterLab(t)
	l.workers().fail = "kubeadm config validate"
	_, err := Install(context.Background(), l.options(Options{}))
	if err == nil || !strings.Contains(err.Error(), "kubeadm refused the config for") || !strings.Contains(err.Error(), `unknown field "podSubnett"`) {
		t.Fatalf("err = %v", err)
	}
	for host, m := range l.machines {
		if len(m.validated) != 1 || len(m.runs) != 0 {
			t.Errorf("%s: validated %d, runs %v", host, len(m.validated), m.runs)
		}
	}
}

func TestInstallRefusals(t *testing.T) {
	// A node of someone else's cluster: nothing starts anywhere.
	l := newClusterLab(t)
	l.gpu().kubeletConf = true
	_, err := Install(context.Background(), l.options(Options{}))
	if err == nil || !strings.Contains(err.Error(), "already a Kubernetes node") || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("err = %v", err)
	}
	for host, m := range l.machines {
		if len(m.runs)+len(m.validated) != 0 {
			t.Errorf("%s changed: %v", host, m.runs)
		}
	}

	// A pool the file doesn't have.
	l = newClusterLab(t)
	o := l.options(Options{Node: func(n providers.Node) (NodeSettings, bool) { return NodeSettings{}, n.Role == config.RoleControlPlane }})
	if _, err := Install(context.Background(), o); err == nil || !strings.Contains(err.Error(), `belongs to pool "gpu", which the file doesn't have`) {
		t.Errorf("err = %v", err)
	}
}

func TestDescribeInstall(t *testing.T) {
	got := strings.Join(DescribeInstall(Options{Cluster: "lab", Version: "v1.37.2", PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12", DNSDomain: "cluster.local",
		SkipKubeProxy: true, Schedulable: true, APIServer: []providers.Flag{{Name: "oidc-client-secret", Env: "OIDC_SECRET"}}}), "\n")
	for _, want := range []string{"Kubernetes v1.37.2 with kubeadm", "cluster lab, pods 10.244.0.0/16, services 10.96.0.0/12, domain cluster.local",
		"deleted once they joined", "without kube-proxy", "no control-plane taint", "oidc-client-secret ($OIDC_SECRET)"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe lacks %q:\n%s", want, got)
		}
	}

	// Several control planes: behind the load balancer tow creates, or your own.
	got = strings.Join(DescribeInstall(Options{Version: "v1.37.2", ControlPlanes: 3}), "\n")
	for _, want := range []string{"then the 2 other control planes join, one at a time", "never saved and deleted once they joined",
		"the nodes reach the API through the internal load balancer created in front of the control planes"} {
		if !strings.Contains(got, want) {
			t.Errorf("describe lacks %q:\n%s", want, got)
		}
	}
	got = strings.Join(DescribeInstall(Options{Version: "v1.37.2", ControlPlanes: 3, ControlPlaneEndpoint: "lb.lab.example:6443", CheckEndpoint: true}), "\n")
	if !strings.Contains(got, "through your load balancer lb.lab.example:6443, tested from the first control plane before init") {
		t.Errorf("describe:\n%s", got)
	}
	got = strings.Join(DescribeInstall(Options{Version: "v1.37.2", ControlPlanes: 3, ControlPlaneEndpoint: "192.168.1.100:6443", KubeVIP: "192.168.1.100"}), "\n")
	if !strings.Contains(got, "through 192.168.1.100:6443, a virtual IP kube-vip "+kubevip.Version+" holds on one control plane at a time") {
		t.Errorf("describe:\n%s", got)
	}
}
