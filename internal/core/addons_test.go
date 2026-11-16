package core

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
)

// chartRun is what the fake helm side saw: where helm and kubectl were looked for, each
// chart install, each CNI install and each wait for Ready nodes.
type chartRun struct {
	finds    []tools.Options
	kubectl  []tools.Tool
	installs []addons.Options
	networks []addons.Network
	waits    [][]string
}

// useCharts swaps in tool finders, a chart and CNI installer, and a Ready wait that touch
// nothing; findErr and installErr make the helm finder and the installers fail.
func useCharts(t *testing.T, findErr, installErr error) *chartRun {
	t.Helper()
	r := &chartRun{}
	origFind, origKubectl, origInstall, origCNI, origWait := findHelm, findKubectl, installCharts, installCNI, waitReady
	findHelm = func(_ context.Context, _ tools.Tool, o tools.Options) (string, error) {
		r.finds = append(r.finds, o)
		return "/fake/helm", findErr
	}
	findKubectl = func(_ context.Context, k tools.Tool, _ tools.Options) (string, error) {
		r.kubectl = append(r.kubectl, k)
		return "/fake/kubectl", nil
	}
	installCharts = func(_ context.Context, o addons.Options) error {
		r.installs = append(r.installs, o)
		return installErr
	}
	installCNI = func(_ context.Context, n addons.Network) error {
		r.networks = append(r.networks, n)
		return installErr
	}
	waitReady = func(_ context.Context, _ string, nodes []string) error {
		r.waits = append(r.waits, nodes)
		return nil
	}
	t.Cleanup(func() {
		findHelm, findKubectl, installCharts, installCNI, waitReady = origFind, origKubectl, origInstall, origCNI, origWait
	})
	return r
}

// Calico and Cilium go on right after k3s, before any chart; then every node must be Ready.
func TestCNIOnMachines(t *testing.T) {
	cfg := func(networking string) string {
		return strings.Replace(homelab, "{flavor: homelab,", "{flavor: homelab, kubernetes: {networking: "+networking+"},", 1)
	}

	usePrepare(t, nil)
	installs := useInstall(t, nil)
	r := useCharts(t, nil, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", cfg("{cni: calico, calico: {dataplane: ebpf}}"))); err != nil {
		t.Fatal(err)
	}
	if len(*installs) != 1 || (*installs)[0].WaitReady || len(r.networks) != 1 || len(r.kubectl) != 1 || len(r.waits) != 1 {
		t.Fatalf("k3s %d, networks %d, kubectl %d, waits %d", len(*installs), len(r.networks), len(r.kubectl), len(r.waits))
	}
	n := r.networks[0]
	if n.Helm != "/fake/helm" || n.Kubectl != "/fake/kubectl" || n.KubeMinor != 37 || n.Networking.CNI != "calico" ||
		n.APIHost != "192.168.1.10" || n.APIPort != "6443" || n.Nodes != 1 || n.Kubeconfig != state.Dir("lab")+"/kubeconfig" {
		t.Errorf("network: %+v", n)
	}
	if r.kubectl[0].Supports != "1.37 (the cluster's minor release, any patch)" || r.kubectl[0].Pinned != "" {
		t.Errorf("kubectl: %q, %s", r.kubectl[0].Pinned, r.kubectl[0].Supports)
	}

	// An older cluster gets kubectl of its own minor release.
	usePrepare(t, nil)
	useInstall(t, nil)
	r = useCharts(t, nil, nil)
	older := strings.Replace(homelab, "{flavor: homelab,", "{flavor: homelab, kubernetes: {version: v1.34.3, networking: {cni: calico}},", 1)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "old", older)); err != nil {
		t.Fatal(err)
	}
	if len(r.kubectl) != 1 || r.kubectl[0].Supports != "1.34 (the cluster's minor release, any patch)" {
		t.Errorf("kubectl for a 1.34 cluster: %+v", r.kubectl)
	}

	// Cilium needs no kubectl.
	usePrepare(t, nil)
	r = useCharts(t, nil, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", cfg("{cni: cilium}"))); err != nil {
		t.Fatal(err)
	}
	if len(r.networks) != 1 || len(r.kubectl) != 0 {
		t.Errorf("networks %d, kubectl %d", len(r.networks), len(r.kubectl))
	}

	// A failed CNI keeps the machines and resumes; flannel needs nothing from here.
	usePrepare(t, nil)
	useCharts(t, nil, errors.New("calico: kubectl apply: exit status 1"))
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "cni", cfg("{cni: calico}")))
	if err == nil || !strings.Contains(err.Error(), "pick up from here") {
		t.Fatalf("err = %v", err)
	}
	if st, _ := state.Load("cni"); st.Status != state.StatusFailed || st.Stage != state.StageKubernetes {
		t.Errorf("state: %+v", st)
	}
	usePrepare(t, nil)
	r = useCharts(t, nil, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "flannel", homelab)); err != nil {
		t.Fatal(err)
	}
	if len(r.finds)+len(r.networks)+len(r.waits) != 0 {
		t.Errorf("flannel comes with k3s: %+v", r)
	}
}

func TestAPIAddress(t *testing.T) {
	cp := func(host, ip string) providers.Node {
		return providers.Node{Host: host, PrivateIP: ip, Role: "control-plane"}
	}
	one := &providers.Result{Endpoint: "203.0.113.10:6443", Nodes: []providers.Node{cp("203.0.113.10", "10.0.1.10")}}
	if h, p := apiAddress(one); h != "10.0.1.10" || p != "6443" {
		t.Errorf("one control plane: the private address, not the public one: %s:%s", h, p)
	}
	three := &providers.Result{Endpoint: "lab-nlb.example.com:6443", Nodes: []providers.Node{cp("a", "10.0.1.10"), cp("b", "10.0.1.11"), cp("c", "10.0.1.12")}}
	if h, _ := apiAddress(three); h != "lab-nlb.example.com" {
		t.Errorf("several control planes: the address in front: %s", h)
	}
	// AWS: the nodes have their own internal load balancer; the public one is the user's.
	three.NodeEndpoint = "internal-lab-nodes.example.com:6443"
	if h, _ := apiAddress(three); h != "internal-lab-nodes.example.com" {
		t.Errorf("the nodes' own address in front: %s", h)
	}
	if minorOf("v1.37.2+k3s1") != 37 || minorOf("v1.36.0") != 36 {
		t.Error("minorOf")
	}
}

const localWithChart = `{flavor: local, kubernetes: {addons: {helmCharts: [{name: podinfo, repo: "https://stefanprodan.github.io/podinfo", chart: podinfo, version: 6.17.0, namespace: demo, values: {replicaCount: 2}}]}}}`

func TestChartsGoOnTheCluster(t *testing.T) {
	useFakes(t)
	r := useCharts(t, nil, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "dev", localWithChart)); err != nil {
		t.Fatal(err)
	}
	if len(r.finds) != 1 || r.finds[0].Install || r.finds[0].DryRun || len(r.installs) != 1 {
		t.Fatalf("finds %+v, installs %d", r.finds, len(r.installs))
	}
	o := r.installs[0]
	if o.Helm != "/fake/helm" || o.Kubeconfig != state.Dir("dev")+"/kubeconfig" || o.Dir != state.Dir("dev") || len(o.Charts) != 1 || o.Charts[0].Name != "podinfo" {
		t.Errorf("install options: %+v", o)
	}
	if st, _ := state.Load("dev"); st.Status != state.StatusReady || st.Stage != state.StageAddons {
		t.Errorf("state: %+v", st)
	}

	// --install-tools reaches the finder.
	useFakes(t)
	r = useCharts(t, nil, nil)
	if err := NewOrchestrator(false).InstallTools().Apply(context.Background(), parse(t, "dev", localWithChart)); err != nil {
		t.Fatal(err)
	}
	if !r.finds[0].Install {
		t.Error("--install-tools must install without asking")
	}

	// No charts: helm isn't looked for at all.
	useFakes(t)
	r = useCharts(t, nil, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "dev", "{flavor: local}")); err != nil {
		t.Fatal(err)
	}
	if len(r.finds)+len(r.installs) != 0 {
		t.Errorf("helm wasn't needed: %+v", r)
	}
}

// A chart that fails keeps the k3d cluster; the next apply installs the charts again, without
// creating the cluster again.
func TestFailedChartKeepsTheCluster(t *testing.T) {
	_, fake := useFakes(t)
	useCharts(t, nil, errors.New("helm chart podinfo: exit status 1"))
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "dev", localWithChart))
	if err == nil || !strings.Contains(err.Error(), "The cluster is kept") || !strings.Contains(err.Error(), "tow delete --name dev") {
		t.Fatalf("err = %v", err)
	}
	st, _ := state.Load("dev")
	if st == nil || st.Status != state.StatusFailed || st.Stage != state.StageKubernetes {
		t.Fatalf("state after a failed chart: %+v", st)
	}

	r := useCharts(t, nil, nil)
	fake.calls = nil
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "dev", localWithChart)); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(fake.calls, "provision") || len(r.installs) != 1 {
		t.Errorf("calls %v, chart installs %d", fake.calls, len(r.installs))
	}
	if st, _ := state.Load("dev"); st.Status != state.StatusReady || st.Stage != state.StageAddons {
		t.Errorf("state after resuming: %+v", st)
	}

	// A changed k3d cluster isn't reused: it's built again.
	useCharts(t, nil, errors.New("boom"))
	os.RemoveAll(state.Dir("dev"))
	NewOrchestrator(false).Apply(context.Background(), parse(t, "dev", localWithChart))
	fake.calls = nil
	useCharts(t, nil, nil)
	changed := strings.Replace(localWithChart, "{flavor: local,", "{flavor: local, workers: [{count: 1}],", 1)
	NewOrchestrator(false).Apply(context.Background(), parse(t, "dev", changed))
	if !slices.Contains(fake.calls, "provision") {
		t.Errorf("a k3d cluster the file changed must be created again: %v", fake.calls)
	}
}

// No helm (and no yes to install it) stops apply before anything is created.
func TestNoHelmStopsBeforeAnything(t *testing.T) {
	_, fake := useFakes(t)
	useCharts(t, errors.New("helm isn't installed, so nothing was done"), nil)
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "dev", localWithChart))
	if err == nil || !strings.Contains(err.Error(), "nothing was done") || len(fake.calls) != 0 {
		t.Errorf("err = %v, calls %v", err, fake.calls)
	}
}

func TestChartSecretsAreReadFirst(t *testing.T) {
	_, fake := useFakes(t)
	r := useCharts(t, nil, nil)
	t.Setenv("PODINFO_MESSAGE", "")
	os.Unsetenv("PODINFO_MESSAGE")
	withSecret := strings.Replace(localWithChart, "values: {replicaCount: 2}", "valuesFromEnv: {ui.message: PODINFO_MESSAGE}", 1)
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "dev", withSecret))
	if err == nil || !strings.Contains(err.Error(), "PODINFO_MESSAGE isn't set") || slices.Contains(fake.calls, "provision") {
		t.Fatalf("err = %v, calls %v", err, fake.calls)
	}

	t.Setenv("PODINFO_MESSAGE", "hello from a secret")
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "dev", withSecret)); err != nil {
		t.Fatal(err)
	}
	if got := r.installs[0].Secrets["PODINFO_MESSAGE"]; got != "hello from a secret" {
		t.Errorf("secret = %q", got)
	}
	if st, _ := state.Load("dev"); strings.Contains(stateText(t, st), "hello from a secret") {
		t.Error("the secret's value reached the state")
	}
}

// On machines, charts go on once Kubernetes and its network run: k3s's or kubeadm's.
func TestChartsOnMachines(t *testing.T) {
	usePrepare(t, nil)
	installs := useInstall(t, nil)
	r := useCharts(t, nil, nil)
	withChart := strings.Replace(homelab, "{flavor: homelab,", `{flavor: homelab, kubernetes: {addons: {helmCharts: [{name: podinfo, repo: "https://stefanprodan.github.io/podinfo", chart: podinfo, version: 6.17.0}]}},`, 1)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", withChart)); err != nil {
		t.Fatal(err)
	}
	if len(*installs) != 1 || len(r.installs) != 1 || r.installs[0].Kubeconfig != state.Dir("lab")+"/kubeconfig" {
		t.Errorf("k3s installs %d, chart installs %+v", len(*installs), r.installs)
	}
	if st, _ := state.Load("lab"); st.Stage != state.StageAddons || st.Status != state.StatusReady {
		t.Errorf("state: %+v", st)
	}

	usePrepare(t, nil)
	r = useCharts(t, nil, nil)
	kubeadm := `{flavor: dev-test, kubernetes: {addons: {helmCharts: [{name: podinfo, repo: "https://stefanprodan.github.io/podinfo", chart: podinfo, version: 6.17.0}]}}, infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, controlPlane: {machines: [{host: 192.168.1.10}]}, workers: [{machines: [{host: 192.168.1.11}]}]}`
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "kadm", kubeadm)); err != nil {
		t.Fatal(err)
	}
	if len(r.finds) != 1 || len(r.networks) != 1 || len(r.installs) != 1 || r.installs[0].Kubeconfig != state.Dir("kadm")+"/kubeconfig" {
		t.Errorf("kubeadm: one helm lookup, Calico, then the chart: %+v", r)
	}
	if st, _ := state.Load("kadm"); st.Stage != state.StageAddons || st.Status != state.StatusReady {
		t.Errorf("state: %+v", st)
	}
}

func TestDryRunShowsTheCharts(t *testing.T) {
	_, fake := useFakes(t)
	r := useCharts(t, nil, nil)
	if err := NewOrchestrator(true).Apply(context.Background(), parse(t, "dev", localWithChart)); err != nil {
		t.Fatal(err)
	}
	if len(r.finds) != 1 || !r.finds[0].DryRun || len(r.installs) != 0 || slices.Contains(fake.calls, "provision") {
		t.Errorf("finds %+v, installs %d, calls %v", r.finds, len(r.installs), fake.calls)
	}
}
