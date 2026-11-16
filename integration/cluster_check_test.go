package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/core"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

// The Kubernetes project's own test image: netexec serves HTTP, answers /hostname with the
// pod's name, and /dial makes the pod call another address and report who answered.
const agnhost = "registry.k8s.io/e2e-test-images/agnhost:2.61"

const controlPlaneTaint = "node-role.kubernetes.io/control-plane"

// clusterCheck is a cluster tow built from a file, and how to reach it.
type clusterCheck struct {
	cfg     *config.Config
	st      *state.ClusterState // its saved state: nodes, kubeconfig, release
	present []string            // kube-system pods that must run (name contains)
	absent  []string            // kube-system pods that must not exist: not asked for
	// nodePortHost is a node address this machine reaches; the hello world is then called
	// through a NodePort, which proves the firewall rules. "" = through the API's service
	// proxy (k3d's nodes live in Docker).
	nodePortHost string
}

// wantNode is one node the file asks for.
type wantNode struct {
	name         string
	controlPlane bool
	group        *config.Group
}

func (c clusterCheck) nodes() []wantNode {
	cfg, st := c.cfg, c.st
	pool := func(name string) *config.Group {
		for i := range cfg.Spec.Workers {
			if cfg.Spec.Workers[i].Name == name {
				return &cfg.Spec.Workers[i].Group
			}
		}
		return &cfg.Spec.Workers[0].Group
	}
	var out []wantNode
	if cfg.Mode == config.ModeLocal { // k3d names its own nodes
		for i := range cfg.ControlPlaneCount() {
			out = append(out, wantNode{fmt.Sprintf("k3d-%s-server-%d", cfg.Name(), i), true, &cfg.Spec.ControlPlane.Group})
		}
		for i := range cfg.WorkerCount() {
			out = append(out, wantNode{fmt.Sprintf("k3d-%s-agent-%d", cfg.Name(), i), false, pool(cfg.Spec.Workers[0].Name)})
		}
		return out
	}
	for _, n := range st.Outputs.Nodes {
		if n.Role == config.RoleControlPlane {
			out = append(out, wantNode{n.Name, true, &cfg.Spec.ControlPlane.Group})
		} else {
			out = append(out, wantNode{n.Name, false, pool(n.Pool)})
		}
	}
	return out
}

// checkCluster proves a cluster is what its file asks for, nothing more, and that it works:
// through the kubeconfig tow handed out, from this machine, as the user would.
func checkCluster(t *testing.T, ctx context.Context, c clusterCheck) {
	t.Helper()
	cs := clientset(t, c.st)
	// Without a CNI, nodes stay NotReady and nothing runs.
	ready := c.cfg.Spec.Kubernetes.Networking.CNI != config.CNINone
	want := c.nodes()

	var nodes []corev1.Node
	eventually(t, ctx, "every node registered (and Ready when there's a CNI)", func() bool {
		list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil || len(list.Items) < len(want) {
			return false
		}
		nodes = list.Items
		return !ready || !slices.ContainsFunc(nodes, func(n corev1.Node) bool { return !nodeReady(n) })
	})
	checkNodes(t, c, want, nodes)

	// Without a CNI only pods on the host's network could run: the others must exist, Pending.
	for _, pod := range c.present {
		what := pod + " running"
		if !ready {
			what = pod + " created"
		}
		eventually(t, ctx, what, func() bool {
			pods, err := cs.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{})
			return err == nil && slices.ContainsFunc(pods.Items, func(p corev1.Pod) bool {
				return strings.Contains(p.Name, pod) && (!ready || p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodSucceeded)
			})
		})
	}
	pods, err := cs.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range c.absent {
		for _, p := range pods.Items {
			if strings.Contains(p.Name, name) {
				t.Errorf("%s was not asked for, but runs: %s/%s", name, p.Namespace, p.Name)
			}
		}
	}

	checkCNI(t, ctx, cs, c.cfg, len(nodes))

	// Each Helm chart the file asks for is deployed: Helm keeps a release's status on its
	// Secret.
	for _, h := range c.cfg.Spec.Kubernetes.Addons.HelmCharts {
		secrets, err := cs.CoreV1().Secrets(h.Namespace).List(ctx, metav1.ListOptions{LabelSelector: "owner=helm,name=" + h.Name})
		if err != nil || !slices.ContainsFunc(secrets.Items, func(s corev1.Secret) bool { return s.Labels["status"] == "deployed" }) {
			t.Errorf("Helm release %s/%s isn't deployed: %v", h.Namespace, h.Name, err)
		}
	}

	if ready {
		checkStorage(t, ctx, cs, c.cfg, nodes)
		checkServiceLB(t, ctx, cs, c.cfg)
		if c.cfg.Mode != config.ModeLocal {
			checkIngress(t, ctx, cs, c.cfg, c.st.Outputs.Nodes)
			checkGateway(t, ctx, cs)
			checkAccessPorts(t, ctx, cs, c.cfg, c.st.Outputs.Nodes)
		}
		if m := c.cfg.Spec.Kubernetes.MetricsServer.Enabled; m != nil && *m {
			checkMetrics(t, ctx, cs, len(nodes))
		}
		checkManifests(t, ctx, cs, c.cfg)
		helloWorld(t, ctx, cs, c.nodePortHost, nodes)
		checkStatus(t, ctx, c.st.Name)
	}
}

// checkStatus proves 'tow status' sees what the checks saw: a healthy cluster (once the
// checks' own pods have gone).
func checkStatus(t *testing.T, ctx context.Context, name string) {
	t.Helper()
	var last core.Report
	var lastErr error
	healthy := false
	defer func() {
		if !healthy {
			t.Logf("last status: %+v, %v", last, lastErr)
		}
	}()
	eventuallyWithin(t, ctx, 3*time.Minute, "tow status says healthy", func() bool {
		last, lastErr = core.NewOrchestrator(false).Report(ctx, name)
		healthy = lastErr == nil && last.Healthy()
		return healthy
	})
}

// checkCNI proves the CNI tow installed runs on every node, as the file asked: its
// DaemonSet Ready everywhere, flannel's backend, kube-proxy's job taken over when asked,
// WireGuard keys published by every node when asked.
func checkCNI(t *testing.T, ctx context.Context, cs kubernetes.Interface, cfg *config.Config, nodes int) {
	t.Helper()
	n := cfg.Spec.Kubernetes.Networking
	ds := map[string][2]string{config.CNICalico: {"calico-system", "calico-node"}, config.CNICilium: {"kube-system", "cilium"}}[n.CNI]
	if n.CNI == config.CNIFlannel && cfg.Distribution() == config.DistributionKubeadm {
		ds = [2]string{"kube-flannel", "kube-flannel-ds"} // flannel's chart; k3s runs flannel inside itself
	}
	if ds[0] == "" {
		return // flannel runs inside k3s; none runs nothing
	}
	eventually(t, ctx, n.CNI+" ready on every node", func() bool {
		d, err := cs.AppsV1().DaemonSets(ds[0]).Get(ctx, ds[1], metav1.GetOptions{})
		return err == nil && int(d.Status.NumberReady) == nodes && d.Status.DesiredNumberScheduled == d.Status.NumberReady
	})
	raw := func(path string) string {
		out, err := cs.Discovery().RESTClient().Get().AbsPath(path).DoRaw(ctx)
		if err != nil {
			t.Errorf("GET %s: %v", path, err)
		}
		return string(out)
	}
	switch {
	case n.CNI == config.CNIFlannel:
		cm, err := cs.CoreV1().ConfigMaps("kube-flannel").Get(ctx, "kube-flannel-cfg", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		backend := "vxlan"
		if n.Flannel != nil && n.Flannel.Backend != "" {
			backend = n.Flannel.Backend
		}
		var net struct {
			Network string
			Backend struct{ Type string }
		}
		if err := json.Unmarshal([]byte(cm.Data["net-conf.json"]), &net); err != nil || net.Network != n.PodCIDR || net.Backend.Type != backend {
			t.Errorf("flannel's net-conf.json: %q (%v), want network %s, backend %s", cm.Data["net-conf.json"], err, n.PodCIDR, backend)
		}
	case n.CNI == config.CNICilium:
		cm, err := cs.CoreV1().ConfigMaps("kube-system").Get(ctx, "cilium-config", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if got, want := cm.Data["kube-proxy-replacement"] == "true", n.ReplacesKubeProxy(); got != want {
			t.Errorf("cilium kube-proxy-replacement = %q, want %v", cm.Data["kube-proxy-replacement"], want)
		}
		if n.Cilium != nil && n.Cilium.Encryption == "wireguard" {
			var list struct {
				Items []struct {
					Metadata metav1.ObjectMeta `json:"metadata"`
				} `json:"items"`
			}
			json.Unmarshal([]byte(raw("/apis/cilium.io/v2/ciliumnodes")), &list)
			if len(list.Items) != nodes {
				t.Errorf("CiliumNodes: %d, want %d", len(list.Items), nodes)
			}
			for _, cn := range list.Items {
				if cn.Metadata.Annotations["network.cilium.io/wg-pub-key"] == "" {
					t.Errorf("%s has no WireGuard key", cn.Metadata.Name)
				}
			}
		}
	case n.CNI == config.CNICalico && n.ReplacesKubeProxy():
		if out := raw("/apis/operator.tigera.io/v1/installations/default"); !strings.Contains(out, `"linuxDataplane":"BPF"`) {
			t.Errorf("Calico's dataplane isn't eBPF: %.300s", out)
		}
	}
}

// ensureTofu gives a test that creates machines the tofu tow runs: this machine's own
// when it has one tow works with (on PATH or in ~/.local/bin), else the pinned release
// (sha256 checked) in a throwaway folder put first on PATH. Tests run unattended, so tow
// itself can't ask before installing it.
func ensureTofu(t *testing.T) {
	t.Helper()
	if tools.Installed(context.Background(), tools.Tofu) != "" {
		return
	}
	dir := t.TempDir()
	if _, err := tools.Download(context.Background(), tools.Tofu, dir); err != nil {
		t.Fatalf("download tofu: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// ensureTools gives the test the helm and kubectl tow runs, in a throwaway folder put
// first on PATH, never in ~/.local/bin: tests run unattended, so tow itself can't ask.
// helm is downloaded (pinned, sha256 checked) when this machine has none. kubectl, for a
// cluster of release cluster ("" = none needed), is always the newest patch of its minor
// release (sha256 checked against the published one): this machine's own may be of another.
func ensureTools(t *testing.T, cluster string) {
	t.Helper()
	dir := ""
	get := func(tool tools.Tool) {
		if dir == "" {
			dir = t.TempDir()
			t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
		}
		if _, err := tools.Download(context.Background(), tool, dir); err != nil {
			t.Fatalf("download %s: %v", tool.Name, err)
		}
	}
	if _, err := exec.LookPath(tools.Helm.Name); err != nil {
		get(tools.Helm)
	}
	switch cluster {
	case "":
	case config.VersionStable:
		t.Fatal("pin spec.kubernetes.version in test files: with 'stable', the test can't know which kubectl the cluster gets")
	default:
		get(tools.Kubectl(cluster))
	}
}

// clientset reaches the cluster through the kubeconfig tow saved.
func clientset(t *testing.T, st *state.ClusterState) kubernetes.Interface {
	t.Helper()
	rc, err := clientcmd.BuildConfigFromFlags("", st.Outputs.Kubeconfig)
	if err != nil {
		t.Fatalf("kubeconfig %s: %v", st.Outputs.Kubeconfig, err)
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

// checkNodes compares each node with the file: name, role, release, labels, taints, and
// whether workloads may run on the control planes.
func checkNodes(t *testing.T, c clusterCheck, want []wantNode, nodes []corev1.Node) {
	t.Helper()
	if len(nodes) != len(want) {
		var names []string
		for _, n := range nodes {
			names = append(names, n.Name)
		}
		t.Errorf("nodes = %v, want %d", names, len(want))
	}
	release := c.st.K8sVersion // kubeadm's kubelet reports the release itself
	if c.cfg.Distribution() != config.DistributionKubeadm {
		release = k3s.Release(release)
	}
	for _, w := range want {
		i := slices.IndexFunc(nodes, func(n corev1.Node) bool { return n.Name == w.name })
		if i < 0 {
			t.Errorf("node %s didn't register under its name", w.name)
			continue
		}
		n := nodes[i]
		if got := n.Status.NodeInfo.KubeletVersion; got != release {
			t.Errorf("%s runs %s, want %s", n.Name, got, release)
		}
		if _, cp := n.Labels[controlPlaneTaint]; cp != w.controlPlane {
			t.Errorf("%s: control plane = %v, want %v", n.Name, cp, w.controlPlane)
		}
		for k, v := range w.group.Labels {
			if got, ok := n.Labels[k]; !ok || got != v {
				t.Errorf("%s: label %s = %q (set: %v), want %q", n.Name, k, got, ok, v)
			}
		}
		for _, wt := range w.group.Taints {
			if !slices.ContainsFunc(n.Spec.Taints, func(x corev1.Taint) bool {
				return x.Key == wt.Key && x.Value == wt.Value && string(x.Effect) == wt.Effect
			}) {
				t.Errorf("%s: taint %s=%s:%s missing (%v)", n.Name, wt.Key, wt.Value, wt.Effect, n.Spec.Taints)
			}
		}
		if w.controlPlane {
			tainted := slices.ContainsFunc(n.Spec.Taints, func(x corev1.Taint) bool { return x.Key == controlPlaneTaint })
			if tainted == c.cfg.ControlPlaneSchedulable() {
				t.Errorf("%s: control-plane taint = %v, but schedulable = %v", n.Name, tainted, c.cfg.ControlPlaneSchedulable())
			}
		}
	}
}

// helloWorld runs two replicas of a web server, on different nodes when two can take them,
// and calls them from this machine: through a NodePort (or the API's service proxy) to one
// replica, which calls the ClusterIP Service by its DNS name until both replicas answered.
// That proves pod networking across nodes, cluster DNS and Service routing. The throwaway
// namespace is always deleted.
func helloWorld(t *testing.T, ctx context.Context, cs kubernetes.Interface, nodePortHost string, nodes []corev1.Node) {
	t.Helper()
	ns := fmt.Sprintf("tow-check-%d", time.Now().UnixNano())
	if _, err := cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	// Deleted on the way out, failure included (t.Fatal runs defers), while the cluster
	// still exists: tests delete it right after.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		if err := cs.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil {
			t.Errorf("delete namespace %s: %v", ns, err)
		}
	}()

	schedulable := 0
	for _, n := range nodes {
		if !slices.ContainsFunc(n.Spec.Taints, func(x corev1.Taint) bool { return x.Effect != corev1.TaintEffectPreferNoSchedule }) {
			schedulable++
		}
	}
	labels := map[string]string{"app": "hello"}
	pod := corev1.PodSpec{Containers: []corev1.Container{{
		Name: "netexec", Image: agnhost, Args: []string{"netexec", "--http-port=8080"},
		Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
	}}}
	if schedulable >= 2 {
		pod.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				LabelSelector: &metav1.LabelSelector{MatchLabels: labels}, TopologyKey: "kubernetes.io/hostname",
			}},
		}}
	}
	replicas := int32(2)
	_, err := cs.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: pod},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	port := []corev1.ServicePort{{Port: 8080, TargetPort: intstr.FromInt32(8080)}}
	if _, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "hello"},
		Spec: corev1.ServiceSpec{Selector: labels, Ports: port}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	np, err := cs.CoreV1().Services(ns).Create(ctx, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "hello-nodeport"},
		Spec: corev1.ServiceSpec{Type: corev1.ServiceTypeNodePort, Selector: labels, Ports: port}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var running []corev1.Pod
	eventually(t, ctx, "both hello-world replicas running", func() bool {
		list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		running = slices.DeleteFunc(list.Items, func(p corev1.Pod) bool { return p.Status.Phase != corev1.PodRunning })
		return len(running) == 2
	})
	if schedulable >= 2 && running[0].Spec.NodeName == running[1].Spec.NodeName {
		t.Errorf("both replicas run on %s", running[0].Spec.NodeName)
	}

	dial := map[string]string{"request": "hostname", "protocol": "http", "host": "hello." + ns + ".svc.cluster.local", "port": "8080", "tries": "20"}
	call := func(path string, query map[string]string) (string, error) {
		if nodePortHost == "" {
			out, err := cs.CoreV1().Services(ns).ProxyGet("http", "hello", "8080", path, query).DoRaw(ctx)
			return string(out), err
		}
		q := []string{}
		for k, v := range query {
			q = append(q, k+"="+v)
		}
		url := "http://" + net.JoinHostPort(nodePortHost, strconv.Itoa(int(np.Spec.Ports[0].NodePort))) + "/" + path + "?" + strings.Join(q, "&")
		client := http.Client{Timeout: 15 * time.Second}
		resp, err := client.Get(url)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		return string(body), err
	}

	names := []string{running[0].Name, running[1].Name}
	var last string
	eventually(t, ctx, "both replicas answering through the Service's DNS name", func() bool {
		out, err := call("dial", dial)
		if err != nil {
			last = err.Error()
			return false
		}
		last = out
		var got struct {
			Responses []string `json:"responses"`
		}
		if json.Unmarshal([]byte(out), &got) != nil {
			return false
		}
		return slices.Contains(got.Responses, names[0]) && slices.Contains(got.Responses, names[1])
	})
	t.Logf("hello world answered from %v (last reply: %.200s)", names, last)

	// Pod to pod, by pod IP: the replica that answers calls each replica directly, so with
	// replicas on two nodes one call crosses nodes through the CNI alone, no Service.
	for _, p := range running {
		eventually(t, ctx, "pod "+p.Name+" reached by its IP", func() bool {
			out, err := call("dial", map[string]string{"request": "hostname", "protocol": "http", "host": p.Status.PodIP, "port": "8080", "tries": "3"})
			return err == nil && strings.Contains(out, p.Name)
		})
	}
	if nodePortHost != "" {
		if out, err := call("hostname", nil); err != nil || !slices.Contains(names, strings.TrimSpace(out)) {
			t.Errorf("NodePort %s from this machine: %q, %v", nodePortHost, out, err)
		}
	}
}

// checkFailover takes one control plane away (off), then proves the cluster still works
// through the address in front of the control planes, as the user reaches it: the API
// answers, the rest of the control plane sees the missing one NotReady, and a new hello world
// runs and answers (scheduling, DNS, Services, a NodePort). Then the control plane comes back
// (on) and must be Ready again.
func checkFailover(t *testing.T, ctx context.Context, c clusterCheck, victim string, off, on func(context.Context) error) {
	t.Helper()
	cs := clientset(t, c.st)
	// CoreDNS's replicas on different nodes, or losing the one node they share leaves the
	// cluster without DNS.
	var placed []string
	eventually(t, ctx, "CoreDNS's replicas running on different nodes", func() bool {
		pods, err := cs.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=kube-dns"})
		if err != nil {
			return false
		}
		placed = nil
		nodes := map[string]bool{}
		for _, p := range pods.Items {
			if p.DeletionTimestamp == nil && p.Status.Phase == corev1.PodRunning {
				placed = append(placed, p.Name+" on "+p.Spec.NodeName)
				nodes[p.Spec.NodeName] = true
			}
		}
		return len(placed) > 1 && len(nodes) == len(placed)
	})
	t.Logf("CoreDNS before the failover: %s", strings.Join(placed, ", "))
	t.Logf("taking control plane %s away", victim)
	if err := off(ctx); err != nil {
		t.Fatalf("power off %s: %v", victim, err)
	}

	var nodes []corev1.Node
	eventually(t, ctx, "the API answering through "+c.st.Outputs.Endpoint+" without "+victim, func() bool {
		list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		if err != nil {
			return false
		}
		nodes = list.Items
		return true
	})
	eventually(t, ctx, victim+" seen NotReady by the rest of the control plane", func() bool {
		n, err := cs.CoreV1().Nodes().Get(ctx, victim, metav1.GetOptions{})
		return err == nil && !nodeReady(*n)
	})
	// status says so too: not healthy, the victim NotReady.
	if r, err := core.NewOrchestrator(false).Report(ctx, c.st.Name); err != nil || r.Healthy() ||
		!slices.ContainsFunc(r.Nodes, func(n kubeapi.Node) bool { return n.Name == victim && !n.Ready }) {
		t.Errorf("status with %s down: %+v, %v", victim, r, err)
	}
	var up []corev1.Node
	for _, n := range nodes {
		if n.Name != victim && nodeReady(n) {
			up = append(up, n)
		}
	}
	helloWorld(t, ctx, cs, c.nodePortHost, up)

	if err := on(ctx); err != nil {
		t.Fatalf("power on %s: %v", victim, err)
	}
	eventuallyWithin(t, ctx, 6*time.Minute, victim+" Ready again", func() bool {
		n, err := cs.CoreV1().Nodes().Get(ctx, victim, metav1.GetOptions{})
		return err == nil && nodeReady(*n)
	})
	t.Logf("%s is back and Ready", victim)
}

// checkK3sCoreDNS proves a k3s cluster of several control planes keeps two CoreDNS replicas on
// different machines, also after k3s applies its own CoreDNS manifest again: restart (k3s
// restarted on a control plane) makes it do so, seen by its AppliedManifest event.
func checkK3sCoreDNS(t *testing.T, ctx context.Context, c clusterCheck, restart func(context.Context) error) {
	t.Helper()
	cs := clientset(t, c.st)
	spread := func() bool {
		d, err := cs.AppsV1().Deployments("kube-system").Get(ctx, "coredns", metav1.GetOptions{})
		if err != nil || d.Spec.Replicas == nil || *d.Spec.Replicas != 2 || d.Status.ReadyReplicas != 2 {
			return false
		}
		pods, err := cs.CoreV1().Pods("kube-system").List(ctx, metav1.ListOptions{LabelSelector: "k8s-app=kube-dns"})
		if err != nil {
			return false
		}
		nodes := map[string]bool{}
		for _, p := range pods.Items {
			if p.Status.Phase == corev1.PodRunning {
				nodes[p.Spec.NodeName] = true
			}
		}
		return len(nodes) == 2
	}
	eventually(t, ctx, "two CoreDNS replicas Ready on different machines", spread)

	// When k3s last applied its CoreDNS manifest, by the cluster's clock.
	applied := func() time.Time {
		events, err := cs.CoreV1().Events("kube-system").List(ctx, metav1.ListOptions{
			FieldSelector: "involvedObject.kind=Addon,involvedObject.name=coredns,reason=AppliedManifest"})
		var last time.Time
		if err != nil {
			return last
		}
		for _, e := range events.Items {
			for _, at := range []time.Time{e.LastTimestamp.Time, e.EventTime.Time, e.CreationTimestamp.Time} {
				if at.After(last) {
					last = at
				}
			}
		}
		return last
	}
	before := applied()
	if err := restart(ctx); err != nil {
		t.Fatalf("restart k3s: %v", err)
	}
	eventuallyWithin(t, ctx, 5*time.Minute, "k3s applying its CoreDNS manifest again after the restart", func() bool { return applied().After(before) })
	d, err := cs.AppsV1().Deployments("kube-system").Get(ctx, "coredns", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d.Spec.Replicas == nil || *d.Spec.Replicas != 2 {
		t.Fatalf("after k3s applied its manifest again, CoreDNS has %v replicas, want 2", d.Spec.Replicas)
	}
	eventually(t, ctx, "every node Ready and CoreDNS spread again after the restart", func() bool {
		list, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		return err == nil && !slices.ContainsFunc(list.Items, func(n corev1.Node) bool { return !nodeReady(n) }) && spread()
	})
	t.Log("CoreDNS kept its two replicas after k3s applied its manifest again")
}

// systemPods are the kube-system pods a cluster on machines must run, and the ones it must
// not. k3s: CoreDNS, and the components it bundles when the file asks for them. kubeadm: its
// control plane as static pods, CoreDNS, and kube-proxy unless the CNI replaces it; it bundles
// none of k3s's extras, which arrive with their own step.
func systemPods(cfg *config.Config) (present, absent []string) {
	k := cfg.Spec.Kubernetes
	present = []string{"coredns"}
	kubeadm := cfg.Distribution() == config.DistributionKubeadm
	if kubeadm {
		present = append(present, "etcd-", "kube-apiserver-", "kube-controller-manager-", "kube-scheduler-")
		if k.Networking.ReplacesKubeProxy() {
			absent = append(absent, "kube-proxy")
		} else {
			present = append(present, "kube-proxy")
		}
	}
	// k3s's extras, in kube-system. Where kubeadm gets them, tow installs them: metrics-server
	// in kube-system with kubelet-csr-approver, local-path in its own namespace (checked by
	// checkStorage). Ingress and service load balancers on kubeadm arrive later.
	metrics := k.MetricsServer.Enabled != nil && *k.MetricsServer.Enabled
	for _, c := range []struct {
		pod            string
		wanted, onKube bool // asked for; and in kube-system on kubeadm when asked for
	}{
		{"traefik", slices.Contains(k.Ingress.Controllers, config.IngressTraefik), false},
		{"svclb", k.ServiceLoadBalancer.Provider == config.ServiceLBServiceLB, false},
		{"local-path-provisioner", slices.Contains(k.Storage.CSI, config.CSILocalPath), false},
		{"metrics-server", metrics, true},
	} {
		switch {
		case c.wanted && c.pod == "svclb" && k.Networking.CNI == config.CNINone:
			// k3s makes svclb pods for Traefik's Service, and the Helm job that installs
			// Traefik can't run without a network: neither ever comes.
		case c.wanted && (!kubeadm || c.onKube):
			present = append(present, c.pod)
		case c.wanted:
		default:
			absent = append(absent, c.pod)
		}
	}
	if kubeadm && metrics {
		present = append(present, "kubelet-csr-approver")
	} else {
		absent = append(absent, "kubelet-csr-approver")
	}
	return present, absent
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
