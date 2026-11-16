package addons

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"gopkg.in/yaml.v3"
)

// call is one command a CNI install ran: the program, its arguments, its stdin, and its
// values file as it was then.
type call struct {
	program, stdin string
	args           []string
	values         map[string]any
}

func useRun(t *testing.T, fail string) *[]call {
	t.Helper()
	var calls []call
	orig := run
	run = func(_ context.Context, program string, args []string, stdin string) (string, string, error) {
		c := call{program: program, args: args, stdin: stdin}
		for i, a := range args {
			if i > 0 && args[i-1] == "--values" {
				data, err := os.ReadFile(a)
				if info, _ := os.Stat(a); err != nil || info.Mode().Perm() != 0o600 {
					t.Errorf("values file %s: %v", a, err)
				}
				yaml.Unmarshal(data, &c.values)
			}
		}
		calls = append(calls, c)
		if fail != "" && args[0] == fail {
			return "", "Error: the server could not find the requested resource", errors.New("exit status 1")
		}
		if args[0] == "template" {
			return "apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\n", "", nil
		}
		return "", "", nil
	}
	t.Cleanup(func() { run = orig })
	return &calls
}

func network(dir string, n config.Networking) Network {
	n.PodCIDR = "10.42.0.0/16"
	return Network{Helm: "/bin/helm", Kubectl: "/bin/kubectl", Kubeconfig: "/state/kc", Dir: dir, Networking: n,
		KubeMinor: 37, APIHost: "10.0.1.10", APIPort: "6443", Nodes: 2}
}

func at(m map[string]any, path ...string) any {
	var cur any = m
	for _, p := range path {
		cur = cur.(map[string]any)[p]
	}
	return cur
}

// Calico as its docs say: CRDs rendered and applied server-side, then the operator with the
// cluster's pool; VXLAN without BGP by default, iptables dataplane.
func TestCalico(t *testing.T) {
	calls := useRun(t, "")
	dir := t.TempDir()
	if err := InstallCNI(context.Background(), network(dir, config.Networking{CNI: config.CNICalico})); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 3 {
		t.Fatalf("calls = %+v", *calls)
	}
	crds, apply, operator := (*calls)[0], (*calls)[1], (*calls)[2]
	if crds.program != "/bin/helm" || strings.Join(crds.args, " ") != "template calico-crds projectcalico.org.v3 --repo https://docs.tigera.io/calico/charts --version v3.33.0 --api-versions admissionregistration.k8s.io/v1/MutatingAdmissionPolicy" {
		t.Errorf("crds: %s %v", crds.program, crds.args)
	}
	if apply.program != "/bin/kubectl" || strings.Join(apply.args, " ") != "apply --server-side --force-conflicts --kubeconfig /state/kc -f -" || !strings.Contains(apply.stdin, "CustomResourceDefinition") {
		t.Errorf("apply: %s %v stdin %q", apply.program, apply.args, apply.stdin)
	}
	if !strings.HasPrefix(strings.Join(operator.args, " "), "upgrade --install calico tigera-operator --repo https://docs.tigera.io/calico/charts --version v3.33.0 --namespace tigera-operator --create-namespace --kubeconfig /state/kc --wait") {
		t.Errorf("operator: %v", operator.args)
	}
	network := at(operator.values, "installation", "calicoNetwork").(map[string]any)
	pool := network["ipPools"].([]any)[0].(map[string]any)
	if network["bgp"] != "Disabled" || pool["cidr"] != "10.42.0.0/16" || pool["encapsulation"] != "VXLAN" || network["linuxDataplane"] != nil {
		t.Errorf("calicoNetwork: %v", network)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("values files left behind: %v", entries)
	}
}

// The tests run no helm: helm 3 unless one says otherwise.
func TestMain(m *testing.M) {
	helmMajor = func(context.Context, string) int { return 3 }
	os.Exit(m.Run())
}

// useHelmMajor makes the helm the tests "run" report this major release.
func useHelmMajor(t *testing.T, major int) {
	orig := helmMajor
	helmMajor = func(context.Context, string) int { return major }
	t.Cleanup(func() { helmMajor = orig })
}

// On helm 4 every helm upgrade takes back fields a controller took over (--force-conflicts,
// a flag helm 3 doesn't know): Calico's IP pools, which its operator fills in. helm template
// and kubectl don't get it.
func TestHelm4ForceConflicts(t *testing.T) {
	for _, major := range []int{3, 4} {
		useHelmMajor(t, major)
		for _, cni := range []string{config.CNICalico, config.CNICilium, config.CNIFlannel} {
			calls := useRun(t, "")
			if err := InstallCNI(context.Background(), network(t.TempDir(), config.Networking{CNI: cni})); err != nil {
				t.Fatal(err)
			}
			for _, c := range *calls {
				forced := slices.Contains(c.args, "--force-conflicts")
				switch upgrade := c.args[0] == "upgrade"; {
				case upgrade && forced != (major == 4):
					t.Errorf("helm %d, %s: %v", major, cni, c.args)
				case !upgrade && forced && c.program == "/bin/helm":
					t.Errorf("helm %d, %s: only upgrades: %v", major, cni, c.args)
				}
			}
		}

		// The file's own charts too.
		helmCalls := useHelm(t, nil)
		if err := Install(context.Background(), options(t.TempDir())); err != nil {
			t.Fatal(err)
		}
		for _, c := range *helmCalls {
			if slices.Contains(c.args, "--force-conflicts") != (major == 4) {
				t.Errorf("helm %d, the file's chart: %v", major, c.args)
			}
		}
	}
}

// eBPF: the API's real address first, then the BPF dataplane. Older clusters get the
// v1beta1 CRDs; IPIP needs BGP.
func TestCalicoVariants(t *testing.T) {
	calls := useRun(t, "")
	n := network(t.TempDir(), config.Networking{CNI: config.CNICalico, Calico: &config.Calico{Version: "3.33.1", Dataplane: config.CalicoEBPF}})
	if err := InstallCNI(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 4 {
		t.Fatalf("calls = %+v", *calls)
	}
	endpoint := (*calls)[2]
	if !strings.Contains(endpoint.stdin, "name: kubernetes-services-endpoint, namespace: tigera-operator") || !strings.Contains(endpoint.stdin, `KUBERNETES_SERVICE_HOST: "10.0.1.10", KUBERNETES_SERVICE_PORT: "6443"`) {
		t.Errorf("endpoint ConfigMap: %s", endpoint.stdin)
	}
	if got := at((*calls)[3].values, "installation", "calicoNetwork", "linuxDataplane"); got != "BPF" || !strings.Contains(strings.Join((*calls)[3].args, " "), "--version v3.33.1") {
		t.Errorf("operator: %v, %v", got, (*calls)[3].args)
	}

	*calls = nil
	n = network(t.TempDir(), config.Networking{CNI: config.CNICalico, Calico: &config.Calico{Encapsulation: "ipip-cross-subnet"}})
	n.KubeMinor = 35
	if err := InstallCNI(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join((*calls)[0].args, " "), "admissionregistration.k8s.io/v1beta1/MutatingAdmissionPolicy") {
		t.Errorf("crds for 1.35: %v", (*calls)[0].args)
	}
	network := at((*calls)[2].values, "installation", "calicoNetwork").(map[string]any)
	if network["bgp"] != "Enabled" || network["ipPools"].([]any)[0].(map[string]any)["encapsulation"] != "IPIPCrossSubnet" {
		t.Errorf("calicoNetwork: %v", network)
	}
}

func TestCilium(t *testing.T) {
	calls := useRun(t, "")
	yes := true
	n := network(t.TempDir(), config.Networking{CNI: config.CNICilium, Cilium: &config.Cilium{KubeProxyReplacement: &yes, Encryption: "wireguard"}})
	if err := InstallCNI(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %+v", *calls)
	}
	c := (*calls)[0]
	if !strings.HasPrefix(strings.Join(c.args, " "), "upgrade --install cilium cilium --repo https://helm.cilium.io --version 1.20.4 --namespace kube-system --kubeconfig /state/kc --wait") {
		t.Errorf("args: %v", c.args)
	}
	v := c.values
	if at(v, "ipam", "operator", "clusterPoolIPv4PodCIDRList").([]any)[0] != "10.42.0.0/16" || v["kubeProxyReplacement"] != true ||
		v["k8sServiceHost"] != "10.0.1.10" || v["k8sServicePort"] != "6443" || at(v, "encryption", "type") != "wireguard" || v["operator"] != nil {
		t.Errorf("values: %v", v)
	}

	// Nothing asked: Cilium's defaults, kube-proxy stays; one node: one operator.
	*calls = nil
	n = network(t.TempDir(), config.Networking{CNI: config.CNICilium})
	n.Nodes = 1
	InstallCNI(context.Background(), n)
	v = (*calls)[0].values
	if v["kubeProxyReplacement"] != nil || v["k8sServiceHost"] != nil || v["encryption"] != nil || at(v, "operator", "replicas") != 1 {
		t.Errorf("values: %v", v)
	}
}

// Cilium moved to another release (an upgrade): its pre-flight check pulls the new images and
// is waited for, then deleted, before the chart; the defaults of the release it was first
// installed at stay (upgradeCompatibility).
func TestCiliumUpgrade(t *testing.T) {
	calls := useRun(t, "")
	yes := true
	n := network(t.TempDir(), config.Networking{CNI: config.CNICilium, Cilium: &config.Cilium{KubeProxyReplacement: &yes}})
	n.UpgradeFrom = "1.19.2"
	if err := UpdateCNI(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range *calls {
		got = append(got, c.args[0]+" "+c.args[1])
	}
	want := []string{"template cilium", "apply --kubeconfig", "rollout status", "rollout status", "template cilium", "delete --ignore-not-found", "upgrade --install"}
	if !slices.Equal(got, want) {
		t.Fatalf("calls %v, want %v", got, want)
	}
	pre := (*calls)[0].values
	if at(pre, "preflight", "enabled") != true || pre["agent"] != false || at(pre, "operator", "enabled") != false || pre["k8sServiceHost"] != "10.0.1.10" {
		t.Errorf("pre-flight values: %v", pre)
	}
	if args := strings.Join((*calls)[2].args, " "); !strings.Contains(args, "daemonset/cilium-pre-flight-check") || !strings.Contains(args, "--namespace kube-system") {
		t.Errorf("wait: %s", args)
	}
	if v := (*calls)[6].values; v["upgradeCompatibility"] != "1.19" {
		t.Errorf("upgradeCompatibility = %v, want the release it upgrades from", v["upgradeCompatibility"])
	}
	// Known since its first upgrade: that one stays.
	n.Compat = "1.18"
	if n.CompatMinor() != "1.18" {
		t.Error("the recorded release wins")
	}
	// A fresh install: none.
	if (Network{}).CompatMinor() != "" {
		t.Error("a fresh install has no upgradeCompatibility")
	}
}

func TestCNIFailureAndDryRun(t *testing.T) {
	useRun(t, "apply")
	err := InstallCNI(context.Background(), network(t.TempDir(), config.Networking{CNI: config.CNICalico}))
	if err == nil || !strings.Contains(err.Error(), "calico: kubectl apply: exit status 1") || !strings.Contains(err.Error(), "could not find the requested resource") {
		t.Errorf("err = %v", err)
	}

	got := strings.Join(CNICommands(network("/state", config.Networking{CNI: config.CNICalico, Calico: &config.Calico{Dataplane: config.CalicoEBPF}})), "\n")
	for _, want := range []string{"helm template calico-crds", "--api-versions admissionregistration.k8s.io/v1/MutatingAdmissionPolicy |", "/kubectl apply --server-side", "(stdin: apiVersion: v1 …)", "--values <the calico values>"} {
		if !strings.Contains(strings.ReplaceAll(got, "/bin", ""), strings.TrimPrefix(want, "/")) {
			t.Errorf("dry run lacks %q:\n%s", want, got)
		}
	}
	// k3s brings flannel; kubeadm brings no CNI.
	if !InstallsCNI(config.CNICilium, config.DistributionK3s) || InstallsCNI(config.CNIFlannel, config.DistributionK3s) ||
		!InstallsCNI(config.CNIFlannel, config.DistributionKubeadm) || InstallsCNI(config.CNINone, config.DistributionKubeadm) ||
		NeedsKubectl(config.CNICilium) || !NeedsKubectl(config.CNICalico) || !NeedsKubectl(config.CNIFlannel) {
		t.Error("which CNIs tow installs, and which need kubectl")
	}
}

// flannel on kubeadm, as its Helm instructions say: its namespace labeled privileged, then
// the chart with the cluster's pod range and the file's backend.
func TestFlannel(t *testing.T) {
	calls := useRun(t, "")
	n := network(t.TempDir(), config.Networking{CNI: config.CNIFlannel, Flannel: &config.Flannel{Backend: "wireguard"}})
	if err := InstallCNI(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls: %+v", *calls)
	}
	ns, chart := (*calls)[0], (*calls)[1]
	if ns.program != "/bin/kubectl" || !strings.Contains(ns.stdin, "name: kube-flannel") || !strings.Contains(ns.stdin, "pod-security.kubernetes.io/enforce: privileged") {
		t.Errorf("namespace: %+v", ns)
	}
	if got := strings.Join(chart.args, " "); !strings.HasPrefix(got, "upgrade --install flannel flannel --repo https://flannel-io.github.io/flannel/ --version v0.28.10 --namespace kube-flannel --kubeconfig /state/kc --wait") {
		t.Errorf("chart: %s", got)
	}
	if chart.values["podCidr"] != "10.42.0.0/16" || at(chart.values, "flannel", "backend") != "wireguard" {
		t.Errorf("values: %v", chart.values)
	}

	// No backend named: flannel's own default (vxlan); a pinned release, with or without v.
	calls = useRun(t, "")
	n = network(t.TempDir(), config.Networking{CNI: config.CNIFlannel, Flannel: &config.Flannel{Version: "v0.28.9"}})
	if err := InstallCNI(context.Background(), n); err != nil {
		t.Fatal(err)
	}
	if c := (*calls)[1]; c.values["flannel"] != nil || !strings.Contains(strings.Join(c.args, " "), "--version v0.28.9 ") {
		t.Errorf("chart: %+v", c)
	}
}
