package kubeadm

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/runtimes"
)

func reconfigureOptions() Options {
	return Options{Cluster: "lab", Version: "v1.37.2", Runtime: runtimes.Containerd, PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12", DNSDomain: "cluster.local",
		APIServer: []providers.Flag{{Name: "audit-log-maxage", Value: "60"}}, CertSANs: []string{"lab.example"}, SkipKubeProxy: true, ServerTLSBootstrap: true,
		Nodes: []providers.Node{{Name: "cp-0", Host: "203.0.113.10", PrivateIP: "10.0.0.10", Role: config.RoleControlPlane}},
		Node: func(n providers.Node) (NodeSettings, bool) {
			return NodeSettings{KubeletArgs: []providers.Flag{{Name: "max-pods", Value: "200"}, {Name: "secret-thing", Env: "KUBELET_SECRET"}}}, true
		},
		Secrets: map[string]string{"KUBELET_SECRET": "s3cr3t"}}
}

// A control plane's config for kubeadm's phases is init's, without any token: the cluster,
// its address, kube-proxy left out as at init, the kubelet's configuration.
func TestControlPlaneConfig(t *testing.T) {
	o := reconfigureOptions()
	cfg, err := o.controlPlaneConfig(o.Nodes[0])
	if err != nil {
		t.Fatal(err)
	}
	s := string(cfg)
	for _, want := range []string{"kind: InitConfiguration", "advertiseAddress: 10.0.0.10", "addon/kube-proxy", "kind: ClusterConfiguration",
		"name: audit-log-maxage", "value: \"60\"", "lab.example", "kind: KubeletConfiguration", "serverTLSBootstrap: true"} {
		if !strings.Contains(s, want) {
			t.Errorf("config lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "token:") || strings.Contains(s, "certificateKey") {
		t.Errorf("no token in a reconfigure config:\n%s", s)
	}
}

// What changed decides the phases: the configuration uploaded by the first control plane,
// the API server's certificate made again (the old one set aside) and its static pod, the
// other components' static pods.
func TestReconfigureScript(t *testing.T) {
	o := reconfigureOptions()
	s, err := o.reconfigureScript(o.Nodes[0], Reconfigure{Components: []string{"scheduler", "etcd"}, SANs: true, Upload: true, Kubelet: true})
	if err != nil {
		t.Fatal(err)
	}
	order := []string{
		"kubeadm init phase upload-config kubeadm --config \"$cfg\"",
		"kubeadm init phase upload-config kubelet --config \"$cfg\"",
		"mv /etc/kubernetes/pki/$f /etc/kubernetes/pki/$f.before-tow",
		"kubeadm init phase certs apiserver --config \"$cfg\"",
		"kubeadm init phase control-plane scheduler --config \"$cfg\"",
		"kubeadm init phase etcd local --config \"$cfg\"",
		"kubeadm init phase control-plane apiserver --config \"$cfg\"",
	}
	at := -1
	for _, w := range order {
		i := strings.Index(s, w)
		if i <= at {
			t.Errorf("%q missing or out of order:\n%s", w, s)
		}
		at = i
	}
	if out, err := exec.Command("sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Errorf("sh -n: %v\n%s", err, out)
	}
	// Another control plane uploads nothing; with nothing else, no phase.
	s, _ = o.reconfigureScript(o.Nodes[0], Reconfigure{Components: []string{"apiserver"}})
	if strings.Contains(s, "upload-config") || strings.Contains(s, "certs apiserver") {
		t.Errorf("only the API server's static pod:\n%s", s)
	}
}

// kubeadm-flags.env: the flags tow set before go, the new ones come, kubeadm's own stay.
func TestEditKubeletFlags(t *testing.T) {
	in := "KUBELET_KUBEADM_ARGS=\"--container-runtime-endpoint=unix:///var/run/containerd/containerd.sock --node-ip=10.0.0.20 --max-pods=110 --pod-infra-container-image=registry.k8s.io/pause:3.10.2\"\n"
	got, err := editKubeletFlags(in, []string{"--max-pods=110"}, []string{"--max-pods=200", "--eviction-hard=memory.available<200Mi"})
	if err != nil {
		t.Fatal(err)
	}
	want := "KUBELET_KUBEADM_ARGS=\"--container-runtime-endpoint=unix:///var/run/containerd/containerd.sock --node-ip=10.0.0.20 --pod-infra-container-image=registry.k8s.io/pause:3.10.2 --max-pods=200 --eviction-hard=memory.available<200Mi\"\n"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if _, err := editKubeletFlags("OTHER=1\n", nil, []string{"--x=1"}); err == nil {
		t.Error("a file without kubeadm's line")
	}
	if args := reconfigureOptions().KubeletArgs(providers.Node{Name: "w"}); strings.Join(args, " ") != "--max-pods=200 --secret-thing=s3cr3t" {
		t.Errorf("kubelet args %v", args)
	}
}
