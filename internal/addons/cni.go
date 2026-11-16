package addons

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/tools"
	"github.com/towproj/tow/internal/ui"
	"gopkg.in/yaml.v3"
)

// Sources (read 2026-11-09), followed here:
//   - Calico 3.33 with Helm: the CRDs rendered with 'helm template' (their API version picked
//     by the cluster's Kubernetes release) and applied with 'kubectl apply --server-side',
//     then the tigera-operator chart; the IP pool through installation.calicoNetwork
//     https://docs.tigera.io/calico/latest/getting-started/kubernetes/helm
//   - Calico on k3s: flannel and k3s's network policy off, the pool matching the pod range
//     https://docs.tigera.io/calico/latest/getting-started/kubernetes/k3s/multi-node-install
//   - Calico eBPF: linuxDataplane BPF, kube-proxy off, the API's real address in the
//     kubernetes-services-endpoint ConfigMap; VXLAN recommended, IPIP not supported
//     https://docs.tigera.io/calico/latest/operations/ebpf/enabling-ebpf
//   - Cilium 1.20 on k3s: the pod range through ipam.operator.clusterPoolIPv4PodCIDRList
//     https://docs.cilium.io/en/stable/installation/k3s/
//   - Cilium without kube-proxy: kubeProxyReplacement, k8sServiceHost and k8sServicePort
//     https://docs.cilium.io/en/stable/network/kubernetes/kubeproxy-free/
//   - Cilium WireGuard: encryption.enabled and encryption.type, UDP 51871 between nodes
//     https://docs.cilium.io/en/stable/security/network/encryption-wireguard/
//   - flannel with Helm (kubeadm; k3s bundles its own): namespace kube-flannel labeled
//     pod-security privileged, then the chart with podCidr; the backend is flannel.backend
//     https://github.com/flannel-io/flannel#deploying-flannel-with-helm

// Tested releases, installed when the file pins none.
const (
	CalicoVersion  = "3.33.0"
	CiliumVersion  = "1.20.4"
	FlannelVersion = "0.28.10"

	calicoCharts  = "https://docs.tigera.io/calico/charts"
	ciliumCharts  = "https://helm.cilium.io"
	flannelCharts = "https://flannel-io.github.io/flannel/"
)

// Network is what installing the file's CNI needs. k3s brings flannel itself; Calico and
// Cilium are installed here, and flannel too on kubeadm.
type Network struct {
	Helm, Kubectl string
	Kubeconfig    string // the cluster's, in the state folder
	Dir           string // the state folder: values files live there, briefly
	Networking    config.Networking
	ServiceLB     config.ServiceLoadBalancer // Cilium's own load balancer is turned on with it
	Ingress       config.Ingress             // and its Ingress and Gateway API
	KubeMinor     int                        // the cluster's Kubernetes 1.<minor>: Calico's CRDs depend on it
	// APIHost and APIPort are the API server's real address, for a CNI that replaces
	// kube-proxy: nothing translates the in-cluster address any more.
	APIHost, APIPort string
	Nodes            int
	// Running is the CNI's version that runs: a running cluster keeps it unless the file
	// pins another.
	Running string
	// UpgradeFrom is the version that runs when this moves the CNI to another one (an
	// upgrade): Cilium's pre-flight check goes first then.
	UpgradeFrom string
	// Compat is Cilium's upgradeCompatibility: the minor release it was first installed at
	// ("1.19"); "" when not known yet: the one it upgrades from, then.
	Compat string
}

// CompatMinor is Cilium's upgradeCompatibility for this install: "" for a fresh install.
func (n Network) CompatMinor() string {
	if n.Compat != "" || n.UpgradeFrom == "" {
		return n.Compat
	}
	parts := strings.SplitN(strings.TrimPrefix(n.UpgradeFrom, "v"), ".", 3)
	if len(parts) < 2 {
		return ""
	}
	return parts[0] + "." + parts[1]
}

// InstallsCNI reports whether tow installs this CNI itself on a distribution: k3s brings
// flannel, kubeadm brings none. NeedsKubectl says whether that takes kubectl too (Calico's
// CRDs, flannel's labeled namespace).
func InstallsCNI(cni, distribution string) bool {
	switch cni {
	case config.CNICalico, config.CNICilium:
		return true
	case config.CNIFlannel:
		return distribution == config.DistributionKubeadm
	}
	return false
}
func NeedsKubectl(cni string) bool { return cni == config.CNICalico || cni == config.CNIFlannel }

// NeedsKubectl reports whether this install takes kubectl: Calico's CRDs, flannel's namespace,
// or the Gateway API CRDs that go in before Cilium.
func (n Network) NeedsKubectl() bool {
	return NeedsKubectl(n.Networking.CNI) || (n.Networking.CNI == config.CNICilium && (CiliumIngress(n.Ingress) || n.UpgradeFrom != ""))
}

// step is one command of an add-on's install.
type step struct {
	program string   // "helm" or "kubectl", resolved to the found paths
	args    []string // "{values}" stands for the values file, "{kubeconfig}" for namespaced's
	values  map[string]any
	stdin   string // kubectl apply -f -
	// pipe feeds this step's output to the next one's stdin (helm template | kubectl apply).
	pipe bool
	// namespaced is the namespace kubectl puts objects that name none into: "{kubeconfig}" is
	// then a copy of the cluster's kubeconfig whose context says so (kubectl's --namespace
	// would refuse objects that name another).
	namespaced string
}

// helmMajor is the major release of the helm at path; 0 when it can't tell. Swappable for tests.
var helmMajor = func(ctx context.Context, path string) int { return tools.Major(ctx, tools.Helm, path) }

// forceOnHelm4 adds --force-conflicts to a helm upgrade on helm 4, which applies server-side
// and stops on a field a controller took over (the tigera operator fills in its IP pools'
// defaults): the file's values win, as they always did with helm 3, which overwrites. Helm 3
// doesn't know the flag.
func forceOnHelm4(ctx context.Context, helm string, args []string) []string {
	if len(args) == 0 || args[0] != "upgrade" || helmMajor(ctx, helm) < 4 {
		return args
	}
	return append(slices.Clone(args), "--force-conflicts")
}

// runner runs steps with the tools found on this machine, against one cluster.
type runner struct {
	helm, kubectl string
	kubeconfig    string // the cluster's, in the state folder
	dir           string // the state folder: values files and kubeconfig copies live there, briefly
}

// steps runs steps in order; what failed is named with the tool's own output.
func (r runner) steps(ctx context.Context, what string, steps []step) error {
	var piped string
	for _, s := range steps {
		out, errOut, err := r.step(ctx, what, s, piped)
		if err != nil {
			return fmt.Errorf("%s: %s %s: %w\n%s", what, s.program, s.args[0], err, strings.TrimSpace(errOut+"\n"+out))
		}
		piped = ""
		if s.pipe {
			piped = out
		}
	}
	return nil
}

func (r runner) step(ctx context.Context, what string, s step, piped string) (string, string, error) {
	program := map[string]string{"helm": r.helm, "kubectl": r.kubectl}[s.program]
	args := s.args
	if s.program == "helm" {
		args = forceOnHelm4(ctx, r.helm, args)
	}
	if s.values != nil {
		data, err := yaml.Marshal(s.values)
		if err != nil {
			return "", "", err
		}
		path, cleanup, err := r.temp("values-"+what, data)
		defer cleanup()
		if err != nil {
			return "", "", err
		}
		args = replace(args, "{values}", path)
	}
	if s.namespaced != "" {
		data, err := kubecfg.WithNamespace(r.kubeconfig, s.namespaced)
		if err != nil {
			return "", "", err
		}
		path, cleanup, err := r.temp("kubeconfig-"+what, data)
		defer cleanup()
		if err != nil {
			return "", "", err
		}
		args = replace(args, "{kubeconfig}", path)
	}
	return run(ctx, program, args, cmp.Or(s.stdin, piped))
}

// temp writes data to an owner-only file in the state folder; cleanup removes it. The name
// starts with prefix, made safe for a file name (a manifest's URL is part of it).
func (r runner) temp(prefix string, data []byte) (string, func(), error) {
	prefix = strings.Trim(unsafeName.ReplaceAllString(prefix, "-"), "-")
	f, err := os.CreateTemp(r.dir, prefix+"-*.yaml") // 0600
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { os.Remove(f.Name()) }
	if _, err := f.Write(data); err != nil {
		f.Close()
		return "", cleanup, err
	}
	return f.Name(), cleanup, f.Close()
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// describe is the commands steps would run, for a dry run.
func describe(what string, steps []step) []string {
	var out []string
	for i, s := range steps {
		args := replace(s.args, "{values}", "<the "+what+" values>")
		args = replace(args, "{kubeconfig}", "<the cluster's kubeconfig, namespace "+s.namespaced+">")
		line := s.program + " " + strings.Join(args, " ")
		if s.stdin != "" {
			line += "   (stdin: " + strings.ReplaceAll(strings.TrimSpace(firstLine(s.stdin)), "\n", " ") + " …)"
		}
		if s.pipe && i+1 < len(steps) {
			line += " |"
		}
		out = append(out, line)
	}
	return out
}

// InstallCNI installs Calico, Cilium or flannel as their docs say and waits for helm to report
// it ready; the caller then waits for every node to be Ready.
func InstallCNI(ctx context.Context, n Network) error {
	done := ui.Step(fmt.Sprintf("Installing %s %s", n.Networking.CNI, n.version()))
	r := runner{helm: n.Helm, kubectl: n.Kubectl, kubeconfig: n.Kubeconfig, dir: n.Dir}
	if err := r.steps(ctx, n.Networking.CNI, n.steps()); err != nil {
		fmt.Println("failed")
		return err
	}
	done()
	return nil
}

// CNICommands are the commands InstallCNI would run, for a dry run.
func CNICommands(n Network) []string { return describe(n.Networking.CNI, n.steps()) }

// version is the CNI's version: the file's, else the one that runs, else the one tow
// was tested with.
func (n Network) version() string { return cmp.Or(n.Pin(), n.Running, n.Tested()) }

// Version is the CNI's version this install applies.
func (n Network) Version() string { return n.version() }

// Pin is the CNI's version in the file, "" when it names none.
func (n Network) Pin() string {
	var v string
	switch c := n.Networking; c.CNI {
	case config.CNICalico:
		if c.Calico != nil {
			v = c.Calico.Version
		}
	case config.CNICilium:
		if c.Cilium != nil {
			v = c.Cilium.Version
		}
	case config.CNIFlannel:
		if c.Flannel != nil {
			v = c.Flannel.Version
		}
	}
	return strings.TrimPrefix(v, "v")
}

// Tested is the CNI's version tow was tested with.
func (n Network) Tested() string {
	return map[string]string{config.CNICalico: CalicoVersion, config.CNICilium: CiliumVersion, config.CNIFlannel: FlannelVersion}[n.Networking.CNI]
}

// Release is the CNI's Helm release, as namespace/name.
func (n Network) Release() string {
	return map[string]string{config.CNICalico: "tigera-operator/calico", config.CNICilium: "kube-system/cilium", config.CNIFlannel: "kube-flannel/flannel"}[n.Networking.CNI]
}

// Same reports whether n installs exactly what m does: the same commands, values and objects.
func (n Network) Same(m Network) bool { return sameSteps(n.steps(), m.steps()) }

// UpdateCNI applies the CNI's settings again on a running cluster, at its version. Its pods
// restart node by node, as their DaemonSet rolls out.
func UpdateCNI(ctx context.Context, n Network) error {
	done := ui.Step(fmt.Sprintf("Applying %s %s's new settings", n.Networking.CNI, n.version()))
	r := runner{helm: n.Helm, kubectl: n.Kubectl, kubeconfig: n.Kubeconfig, dir: n.Dir}
	if err := r.steps(ctx, n.Networking.CNI, n.steps()); err != nil {
		fmt.Println("failed")
		return err
	}
	done()
	return nil
}

func (n Network) steps() []step {
	switch n.Networking.CNI {
	case config.CNICalico:
		return n.calico()
	case config.CNICilium:
		return n.cilium()
	case config.CNIFlannel:
		return n.flannel()
	}
	return nil
}

// flannel: its namespace, privileged as its pods need, then the chart with the cluster's pod
// range and the file's backend.
func (n Network) flannel() []step {
	values := map[string]any{"podCidr": n.Networking.PodCIDR}
	if f := n.Networking.Flannel; f != nil && f.Backend != "" {
		values["flannel"] = map[string]any{"backend": f.Backend} // vxlan, host-gw, wireguard: flannel's own names
	}
	return []step{
		{program: "kubectl", args: []string{"apply", "--kubeconfig", n.Kubeconfig, "-f", "-"}, stdin: `apiVersion: v1
kind: Namespace
metadata:
  name: kube-flannel
  labels: {pod-security.kubernetes.io/enforce: privileged}
`},
		{program: "helm", args: []string{"upgrade", "--install", "flannel", "flannel", "--repo", flannelCharts, "--version", "v" + n.version(),
			"--namespace", "kube-flannel", "--kubeconfig", n.Kubeconfig, "--wait", "--timeout", chartTimeout, "--values", "{values}"},
			values: values},
	}
}

// calico: the CRDs, the API address when eBPF replaces kube-proxy, then the operator with
// the cluster's IP pool.
func (n Network) calico() []step {
	c := config.Calico{}
	if n.Networking.Calico != nil {
		c = *n.Networking.Calico
	}
	version := "v" + n.version() // Calico's charts are tagged v3.33.0
	// The CRDs' admission policy API: v1 from Kubernetes 1.36, v1beta1 before (Calico docs).
	policyAPI := "admissionregistration.k8s.io/v1/MutatingAdmissionPolicy"
	if n.KubeMinor < 36 {
		policyAPI = "admissionregistration.k8s.io/v1beta1/MutatingAdmissionPolicy"
	}
	steps := []step{
		{program: "helm", args: []string{"template", "calico-crds", "projectcalico.org.v3", "--repo", calicoCharts, "--version", version, "--api-versions", policyAPI}, pipe: true},
		// --force-conflicts: an upgrade takes over fields an older release set (Calico's docs).
		{program: "kubectl", args: []string{"apply", "--server-side", "--force-conflicts", "--kubeconfig", n.Kubeconfig, "-f", "-"}},
	}

	ebpf := c.Dataplane == config.CalicoEBPF
	if ebpf {
		steps = append(steps, step{program: "kubectl", args: []string{"apply", "--kubeconfig", n.Kubeconfig, "-f", "-"},
			stdin: fmt.Sprintf(`apiVersion: v1
kind: Namespace
metadata: {name: tigera-operator}
---
apiVersion: v1
kind: ConfigMap
metadata: {name: kubernetes-services-endpoint, namespace: tigera-operator}
data: {KUBERNETES_SERVICE_HOST: %q, KUBERNETES_SERVICE_PORT: %q}
`, n.APIHost, n.APIPort)})
	}

	encapsulation := map[string]string{
		"": "VXLAN", "vxlan": "VXLAN", "vxlan-cross-subnet": "VXLANCrossSubnet",
		"ipip": "IPIP", "ipip-cross-subnet": "IPIPCrossSubnet", "none": "None",
	}[c.Encapsulation]
	// VXLAN carries pods between nodes without BGP; the others need BGP routes.
	bgp := "Enabled"
	if encapsulation == "VXLAN" {
		bgp = "Disabled"
	}
	network := map[string]any{
		"bgp": bgp,
		"ipPools": []any{map[string]any{
			"cidr": n.Networking.PodCIDR, "encapsulation": encapsulation,
			"natOutgoing": "Enabled", "nodeSelector": "all()",
		}},
	}
	if ebpf {
		network["linuxDataplane"] = "BPF"
	}
	steps = append(steps, step{program: "helm",
		args: []string{"upgrade", "--install", "calico", "tigera-operator", "--repo", calicoCharts, "--version", version,
			"--namespace", "tigera-operator", "--create-namespace", "--kubeconfig", n.Kubeconfig, "--wait", "--timeout", chartTimeout, "--values", "{values}"},
		values: map[string]any{"installation": map[string]any{"cni": map[string]any{"type": "Calico"}, "calicoNetwork": network}}})
	return steps
}

// cilium: one chart, with the cluster's pod range and the file's options.
func (n Network) cilium() []step {
	c := config.Cilium{}
	if n.Networking.Cilium != nil {
		c = *n.Networking.Cilium
	}
	values := map[string]any{
		"ipam": map[string]any{"operator": map[string]any{"clusterPoolIPv4PodCIDRList": []any{n.Networking.PodCIDR}}},
	}
	if c.KubeProxyReplacement != nil {
		values["kubeProxyReplacement"] = *c.KubeProxyReplacement
		if *c.KubeProxyReplacement {
			values["k8sServiceHost"], values["k8sServicePort"] = n.APIHost, n.APIPort
		}
	}
	if c.Encryption == "wireguard" {
		values["encryption"] = map[string]any{"enabled": true, "type": "wireguard"}
	}
	if n.Nodes == 1 {
		values["operator"] = map[string]any{"replicas": 1} // its two replicas want two nodes
	}
	for k, v := range CiliumServiceLBValues(n.ServiceLB) {
		values[k] = v
	}
	// The defaults of the release it was first installed at stay (Cilium's upgrade docs).
	if compat := n.CompatMinor(); compat != "" {
		values["upgradeCompatibility"] = compat
	}
	var steps []step
	if v := ciliumIngressValues(n.Ingress); v != nil {
		for k, x := range v {
			values[k] = x
		}
		steps = append(steps, gatewayAPICRDs(n.Kubeconfig)) // Cilium looks for them when it starts
	}
	if n.UpgradeFrom != "" {
		steps = append(steps, n.ciliumPreflight(values)...)
	}
	return append(steps, step{program: "helm",
		args: []string{"upgrade", "--install", "cilium", "cilium", "--repo", ciliumCharts, "--version", n.version(),
			"--namespace", "kube-system", "--kubeconfig", n.Kubeconfig, "--wait", "--timeout", chartTimeout, "--values", "{values}"},
		values: values})
}

// ciliumPreflight is Cilium's pre-flight check before an upgrade (its upgrade docs): a
// DaemonSet that pulls the new images on every node, and a Deployment that checks the
// cluster's network policies against the new release, both waited for, then deleted.
func (n Network) ciliumPreflight(values map[string]any) []step {
	pre := map[string]any{"preflight": map[string]any{"enabled": true}, "agent": false, "operator": map[string]any{"enabled": false}}
	for _, k := range []string{"k8sServiceHost", "k8sServicePort"} {
		if v, ok := values[k]; ok {
			pre[k] = v
		}
	}
	template := step{program: "helm", args: []string{"template", "cilium", "cilium", "--repo", ciliumCharts, "--version", n.version(),
		"--namespace", "kube-system", "--values", "{values}"}, values: pre, pipe: true}
	wait := func(kind string) step {
		return step{program: "kubectl", args: []string{"rollout", "status", kind + "/cilium-pre-flight-check", "--namespace", "kube-system",
			"--kubeconfig", n.Kubeconfig, "--timeout", chartTimeout}}
	}
	return []step{
		template,
		{program: "kubectl", args: []string{"apply", "--kubeconfig", n.Kubeconfig, "-f", "-"}},
		wait("daemonset"),
		wait("deployment"),
		template,
		{program: "kubectl", args: []string{"delete", "--ignore-not-found", "--kubeconfig", n.Kubeconfig, "-f", "-"}},
	}
}

func replace(args []string, from, to string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = strings.ReplaceAll(a, from, to)
	}
	return out
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
