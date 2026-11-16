package kubeadm

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"encoding/hex"
	"maps"
	"net"
	"slices"
	"strconv"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/runtimes"
	"gopkg.in/yaml.v3"
)

// kubeadm's configuration API (v1beta4, the current one in 1.37):
// https://kubernetes.io/docs/reference/config-api/kubeadm-config.v1beta4/
const (
	kubeadmAPI = "kubeadm.k8s.io/v1beta4"
	kubeletAPI = "kubelet.config.k8s.io/v1beta1"
	apiPort    = 6443

	controlPlaneTaint = "node-role.kubernetes.io/control-plane"
)

// NodeSettings are what the file says about one node's group.
type NodeSettings struct {
	KubeletArgs []providers.Flag  // the cluster's kubelet flags, then the pool's
	Labels      map[string]string // the ones a kubelet may register its node with
	Taints      []config.Taint
	RoleLabels  map[string]string // node-role.kubernetes.io/..., set through the API
}

// arg is one flag in kubeadm's structured form: duplicates repeat it (v1beta4).
type arg struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}

type taint struct {
	Key    string `yaml:"key"`
	Value  string `yaml:"value,omitempty"`
	Effect string `yaml:"effect"`
}

type nodeRegistration struct {
	Name      string `yaml:"name"`
	CRISocket string `yaml:"criSocket"`
	// nil = kubeadm's default (the control-plane taint on control planes); [] = none.
	Taints           *[]taint `yaml:"taints,omitempty"`
	KubeletExtraArgs []arg    `yaml:"kubeletExtraArgs,omitempty"`
}

type bootstrapToken struct {
	Token       string   `yaml:"token"`
	Description string   `yaml:"description"`
	TTL         string   `yaml:"ttl"`
	Usages      []string `yaml:"usages"`
	Groups      []string `yaml:"groups"`
}

// apiEndpoint is where a control plane's API server listens: its private address.
type apiEndpoint struct {
	AdvertiseAddress string `yaml:"advertiseAddress"`
	BindPort         int    `yaml:"bindPort"`
}

type initConfiguration struct {
	APIVersion       string           `yaml:"apiVersion"`
	Kind             string           `yaml:"kind"`
	BootstrapTokens  []bootstrapToken `yaml:"bootstrapTokens"`
	LocalAPIEndpoint apiEndpoint      `yaml:"localAPIEndpoint"`
	NodeRegistration nodeRegistration `yaml:"nodeRegistration"`
	SkipPhases       []string         `yaml:"skipPhases,omitempty"`
	// CertificateKey encrypts the certificates init uploads for the other control planes
	// (kubeadm init --upload-certs); "" with one control plane.
	CertificateKey string `yaml:"certificateKey,omitempty"`
}

// joinControlPlane makes a join a control plane's: its API server's address, and the key that
// decrypts the certificates init uploaded.
type joinControlPlane struct {
	LocalAPIEndpoint apiEndpoint `yaml:"localAPIEndpoint"`
	CertificateKey   string      `yaml:"certificateKey"`
}

type component struct {
	ExtraArgs []arg `yaml:"extraArgs,omitempty"`
}

type clusterConfiguration struct {
	APIVersion           string `yaml:"apiVersion"`
	Kind                 string `yaml:"kind"`
	ClusterName          string `yaml:"clusterName"`
	KubernetesVersion    string `yaml:"kubernetesVersion"`
	ControlPlaneEndpoint string `yaml:"controlPlaneEndpoint,omitempty"`
	Networking           struct {
		PodSubnet     string `yaml:"podSubnet"`
		ServiceSubnet string `yaml:"serviceSubnet"`
		DNSDomain     string `yaml:"dnsDomain"`
	} `yaml:"networking"`
	APIServer struct {
		CertSANs  []string `yaml:"certSANs,omitempty"`
		ExtraArgs []arg    `yaml:"extraArgs,omitempty"`
	} `yaml:"apiServer"`
	ControllerManager component `yaml:"controllerManager,omitempty"`
	Scheduler         component `yaml:"scheduler,omitempty"`
	Etcd              struct {
		Local component `yaml:"local"`
	} `yaml:"etcd"`
}

type kubeletConfiguration struct {
	APIVersion   string `yaml:"apiVersion"`
	Kind         string `yaml:"kind"`
	CgroupDriver string `yaml:"cgroupDriver"`
	// ServerTLSBootstrap has each kubelet ask the cluster to sign its serving certificate,
	// instead of signing its own: metrics-server can then check it (kubeadm's certificates page).
	ServerTLSBootstrap bool `yaml:"serverTLSBootstrap,omitempty"`
}

type joinConfiguration struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Discovery  struct {
		BootstrapToken struct {
			Token             string   `yaml:"token"`
			APIServerEndpoint string   `yaml:"apiServerEndpoint"`
			CACertHashes      []string `yaml:"caCertHashes"`
		} `yaml:"bootstrapToken"`
	} `yaml:"discovery"`
	NodeRegistration nodeRegistration  `yaml:"nodeRegistration"`
	ControlPlane     *joinControlPlane `yaml:"controlPlane,omitempty"`
}

// criSocket is where the kubelet reaches the runtime (kubeadm install page's table).
func criSocket(runtime string) string {
	if runtime == runtimes.CRIO {
		return "unix:///var/run/crio/crio.sock"
	}
	return "unix:///var/run/containerd/containerd.sock"
}

// args turns flags into kubeadm's form, a secret's value read beforehand.
func (o Options) args(flags []providers.Flag) []arg {
	var out []arg
	for _, f := range flags {
		v := f.Value
		if f.Env != "" {
			v += o.Secrets[f.Env]
		}
		out = append(out, arg{f.Name, v})
	}
	return out
}

// registration is how a node registers: its name, the runtime, its kubelet flags (its
// private address, the file's labels, then the file's flags) and its taints. A control plane
// keeps kubeadm's own taint unless workloads may run there.
func (o Options) registration(n providers.Node, s NodeSettings, controlPlane bool) nodeRegistration {
	r := nodeRegistration{Name: n.Name, CRISocket: criSocket(o.Runtime)}
	if n.PrivateIP != "" {
		r.KubeletExtraArgs = append(r.KubeletExtraArgs, arg{"node-ip", n.PrivateIP})
	}
	if len(s.Labels) > 0 {
		var l []string
		for _, k := range slices.Sorted(maps.Keys(s.Labels)) {
			l = append(l, k+"="+s.Labels[k])
		}
		r.KubeletExtraArgs = append(r.KubeletExtraArgs, arg{"node-labels", strings.Join(l, ",")})
	}
	r.KubeletExtraArgs = append(r.KubeletExtraArgs, o.args(s.KubeletArgs)...)

	taints := []taint{}
	if controlPlane && !o.Schedulable {
		if len(s.Taints) == 0 {
			return r // nil: kubeadm taints it as its docs say
		}
		taints = append(taints, taint{Key: controlPlaneTaint, Effect: "NoSchedule"})
	}
	for _, t := range s.Taints {
		taints = append(taints, taint{t.Key, t.Value, t.Effect})
	}
	r.Taints = &taints
	return r
}

// initConfig is the first control plane's kubeadm config: how it registers, the cluster, and
// the kubelet's cgroup driver (systemd, as the container runtimes page asks). token joins the
// other nodes and expires within the hour; certKey, with several control planes, encrypts the
// certificates init uploads for them.
func (o Options) initConfig(n providers.Node, s NodeSettings, token, certKey string) ([]byte, error) {
	in := initConfiguration{APIVersion: kubeadmAPI, Kind: "InitConfiguration", NodeRegistration: o.registration(n, s, true), CertificateKey: certKey}
	in.BootstrapTokens = []bootstrapToken{{Token: token, Description: tokenDescription, TTL: "1h0m0s",
		Usages: []string{"signing", "authentication"}, Groups: []string{"system:bootstrappers:kubeadm:default-node-token"}}}
	in.LocalAPIEndpoint = apiEndpoint{cmp.Or(n.PrivateIP, n.Host), apiPort}
	if o.SkipKubeProxy {
		in.SkipPhases = []string{"addon/kube-proxy"}
	}

	c := clusterConfiguration{APIVersion: kubeadmAPI, Kind: "ClusterConfiguration", ClusterName: o.Cluster,
		KubernetesVersion: o.Version, ControlPlaneEndpoint: o.ControlPlaneEndpoint}
	c.Networking.PodSubnet, c.Networking.ServiceSubnet, c.Networking.DNSDomain = o.PodCIDR, o.ServiceCIDR, o.DNSDomain
	c.APIServer.CertSANs = o.sans()
	c.APIServer.ExtraArgs = o.args(o.APIServer)
	c.ControllerManager.ExtraArgs = o.args(o.ControllerManager)
	c.Scheduler.ExtraArgs = o.args(o.Scheduler)
	c.Etcd.Local.ExtraArgs = o.args(o.Etcd)

	k := kubeletConfiguration{APIVersion: kubeletAPI, Kind: "KubeletConfiguration", CgroupDriver: "systemd", ServerTLSBootstrap: o.ServerTLSBootstrap}
	return documents(in, c, k)
}

// joinConfig is a joining node's: the first control plane's address, the token, and the hash
// of the cluster's CA, which the node checks before it trusts what it's told. Another control
// plane also gets its API server's address and the key to the certificates init uploaded.
func (o Options) joinConfig(n providers.Node, s NodeSettings, server, token, caHash, certKey string) ([]byte, error) {
	controlPlane := n.Role == config.RoleControlPlane
	j := joinConfiguration{APIVersion: kubeadmAPI, Kind: "JoinConfiguration", NodeRegistration: o.registration(n, s, controlPlane)}
	j.Discovery.BootstrapToken.Token = token
	j.Discovery.BootstrapToken.APIServerEndpoint = server
	j.Discovery.BootstrapToken.CACertHashes = []string{caHash}
	if controlPlane {
		j.ControlPlane = &joinControlPlane{apiEndpoint{cmp.Or(n.PrivateIP, n.Host), apiPort}, certKey}
	}
	return documents(j)
}

// sans are the names the API's certificate gets, the same on every control plane: the file's
// certSANs, the address in front of the control planes, and every control plane's own
// addresses (the kubeconfig may use the first one's public address).
func (o Options) sans() []string {
	var out []string
	add := func(h string) {
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	for _, s := range o.CertSANs {
		add(s)
	}
	for _, e := range []string{o.ControlPlaneEndpoint, o.Endpoint} {
		if e != "" {
			add(hostOf(e))
		}
	}
	for _, n := range o.Nodes {
		if n.Role == config.RoleControlPlane {
			add(n.Host)
			add(n.PrivateIP)
		}
	}
	return out
}

// newCertificateKey is the key kubeadm encrypts the uploaded certificates with: "a hex encoded
// string that is an AES key of size 32 bytes" (kubeadm's HA page).
func newCertificateKey() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func documents(docs ...any) ([]byte, error) {
	var b bytes.Buffer
	for i, d := range docs {
		if i > 0 {
			b.WriteString("---\n")
		}
		enc := yaml.NewEncoder(&b)
		enc.SetIndent(2)
		if err := enc.Encode(d); err != nil {
			return nil, err
		}
		if err := enc.Close(); err != nil {
			return nil, err
		}
	}
	return b.Bytes(), nil
}

const tokenDescription = "tow: nodes joining; deleted once they joined"

// newToken is a bootstrap token in kubeadm's form: [a-z0-9]{6}.[a-z0-9]{16}, each character
// drawn evenly (bytes past the last whole run of 36 are drawn again).
func newToken() (string, error) {
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	out := make([]byte, 0, 23)
	b := make([]byte, 1)
	for len(out) < 23 {
		if len(out) == 6 {
			out = append(out, '.')
			continue
		}
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		if int(b[0]) < 252 { // 252 = 7 × 36
			out = append(out, chars[int(b[0])%len(chars)])
		}
	}
	return string(out), nil
}

// hostOf is the host part of host:port, or the address itself.
func hostOf(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// endpoint is a host's API address.
func endpoint(host string) string { return net.JoinHostPort(host, strconv.Itoa(apiPort)) }
