package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
)

// useKubeadm swaps in a fake kubeadm setup that records what it was asked and fails with fail;
// the install that follows hands out a kubeconfig.
func useKubeadm(t *testing.T, fail error) *[]kubeadm.Options {
	t.Helper()
	got, _ := useKubeadmInstall(t, fail, nil)
	return got
}

// useKubeadmInstall fakes both parts: the setup fails with setupErr, the install with
// installErr; each records what it was asked.
func useKubeadmInstall(t *testing.T, setupErr, installErr error) (setups, installs *[]kubeadm.Options) {
	t.Helper()
	var s, i []kubeadm.Options
	origSetup, origInstall := setupKubeadm, installKubeadmCluster
	setupKubeadm = func(_ context.Context, o kubeadm.Options) error {
		s = append(s, o)
		return setupErr
	}
	installKubeadmCluster = func(_ context.Context, o kubeadm.Options) ([]byte, error) {
		i = append(i, o)
		if installErr != nil {
			return nil, installErr
		}
		return []byte(k3sKubeconfig), nil
	}
	origSnapshots := setSnapshots
	setSnapshots = func(context.Context, kubeadm.Options) error { return nil }
	t.Cleanup(func() { setupKubeadm, installKubeadmCluster, setSnapshots = origSetup, origInstall, origSnapshots })
	return &s, &i
}

const kubeadmLab = "{flavor: dev-test, infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, controlPlane: {machines: [{host: 192.168.1.10}]}}"

// kubeadm's machines get their runtime and packages once prepared, then the cluster, its
// kubeconfig and the file's CNI.
func TestKubeadmMachinesAreSetUp(t *testing.T) {
	_, prepared := usePrepare(t, nil)
	setups, clusters := useKubeadmInstall(t, nil, nil)
	installs := useInstall(t, nil)
	r := useCharts(t, nil, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", kubeadmLab)); err != nil {
		t.Fatal(err)
	}
	if len(*setups) != 1 || len(*clusters) != 1 || len(*installs) != 0 {
		t.Fatalf("kubeadm setups %d, kubeadm clusters %d, k3s installs %d", len(*setups), len(*clusters), len(*installs))
	}
	c := (*clusters)[0]
	if c.Version != config.DefaultKubernetesVersion || c.PodCIDR != "10.244.0.0/16" || c.ServiceCIDR != "10.96.0.0/12" || c.DNSDomain != "cluster.local" ||
		c.ControlPlaneEndpoint != "" || c.SkipKubeProxy || len(c.Nodes) != 1 || c.Node == nil {
		t.Errorf("install options: %+v", c)
	}
	if _, ok := c.Node(c.Nodes[0]); !ok {
		t.Error("the control plane's settings come from the file")
	}
	// dev-test asks for Calico: installed once the cluster runs, then every node is Ready.
	if len(r.networks) != 1 || r.networks[0].Networking.CNI != config.CNICalico || r.networks[0].APIHost != "192.168.1.10" || len(r.waits) != 1 {
		t.Errorf("networks %+v, waits %v", r.networks, r.waits)
	}
	if b, err := os.ReadFile(state.Dir("lab") + "/kubeconfig"); err != nil || !strings.Contains(string(b), "192.168.1.10:6443") {
		t.Errorf("kubeconfig: %s, %v", b, err)
	}
	o := (*setups)[0]
	if o.Cluster != "lab" || o.Version != config.DefaultKubernetesVersion || o.Runtime != "containerd" || o.Containerd != "" ||
		len(o.Nodes) != 1 || o.Nodes[0].Host != "192.168.1.10" || !strings.HasSuffix(o.KnownHosts, "/clusters/lab/known_hosts") {
		t.Errorf("setup options: %+v", o)
	}
	// The runtime already on a machine is checked with preparation, before any change.
	if (*prepared)[0].Preflight == nil {
		t.Error("preparation must check the machines' container runtime first")
	}
	// dev-test's add-ons follow: local-path and metrics-server, kubelet-csr-approver first
	// (kubeadm's kubelets ask for serving certificates the cluster signs).
	if st, _ := state.Load("lab"); st == nil || st.Stage != state.StageAddons || st.Status != state.StatusReady {
		t.Errorf("state: %+v", st)
	}
	if len(r.installs) != 1 || len(r.installs[0].Nodes) != 1 || r.installs[0].Kubectl != "/fake/kubectl" || !(*clusters)[0].ServerTLSBootstrap {
		t.Errorf("add-ons %+v, kubeadm %+v", r.installs, (*clusters)[0])
	}
	if got := strings.Join(addons.Commands(r.installs[0]), "\n"); !strings.Contains(got, "local-path 0.0.37:") || !strings.Contains(got, "kubelet-csr-approver") || !strings.Contains(got, "metrics-server 3.14.0:") {
		t.Errorf("add-ons:\n%s", got)
	}

	// The file's runtime and releases reach the setup.
	setups = useKubeadm(t, nil)
	crio := strings.Replace(kubeadmLab, "{flavor: dev-test,", "{flavor: dev-test, kubernetes: {version: v1.36.4, containerRuntime: cri-o, cri-o: {version: 1.36.2}},", 1)
	crio = strings.Replace(crio, "192.168.1.10", "192.168.1.11", 1)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "crio", crio)); err != nil {
		t.Fatal(err)
	}
	if o := (*setups)[0]; o.Version != "v1.36.4" || o.Runtime != "cri-o" || o.CRIO != "1.36.2" {
		t.Errorf("setup options: %+v", o)
	}
}

// What the file says reaches kubeadm: the API address the nodes use only when the user's own
// load balancer serves it, kube-proxy skipped when the CNI replaces it, flags with secrets by
// name, and each group's labels, role labels, taints and kubelet flags (a pool's win).
func TestKubeadmOptionsFromTheFile(t *testing.T) {
	file := `{kubernetes: {distribution: kubeadm, apiEndpoint: lb.lab.example:6443, apiLoadBalancer: external,
    networking: {cni: cilium, cilium: {kubeProxyReplacement: true}, podCIDR: 10.50.0.0/16, serviceCIDR: 10.60.0.0/16, clusterDomain: lab.local},
    apiServer: {certSANs: [k8s.lab.example], extraArgs: {audit-log-maxage: "30", oidc-client-secret: {env: OIDC_SECRET}}},
    controllerManager: {extraArgs: {node-monitor-grace-period: 40s}},
    kubelet: {extraArgs: {max-pods: 150}}},
  infrastructure: {provider: byoh, access: {ssh: {user: pi}}},
  controlPlane: {schedulable: true, machines: [{host: 192.168.1.10}], labels: {node-role.kubernetes.io/infra: ""}},
  workers: [{name: gpu, machines: [{host: 192.168.1.20}], labels: {gpu: "true", node-role.kubernetes.io/gpu: ""},
             taints: [{key: gpu, value: "true", effect: NoSchedule}], kubelet: {extraArgs: {max-pods: 250}}}]}`
	o := kubeadmOptions(parse(t, "lab", file))
	if o.ControlPlaneEndpoint != "lb.lab.example:6443" || !o.CheckEndpoint || o.KubeVIP != "" || !o.SkipKubeProxy || !o.Schedulable ||
		o.PodCIDR != "10.50.0.0/16" || o.ServiceCIDR != "10.60.0.0/16" || o.DNSDomain != "lab.local" || !slices.Equal(o.CertSANs, []string{"k8s.lab.example"}) {
		t.Errorf("options: %+v", o)
	}
	if got := fmt.Sprint(o.APIServer, o.ControllerManager); got != "[{audit-log-maxage 30 } {oidc-client-secret  OIDC_SECRET}] [{node-monitor-grace-period 40s }]" {
		t.Errorf("flags: %s", got)
	}

	cp, ok := o.Node(providers.Node{Role: config.RoleControlPlane})
	if !ok || len(cp.Labels) != 0 || fmt.Sprint(cp.RoleLabels) != "map[node-role.kubernetes.io/infra:]" || fmt.Sprint(cp.KubeletArgs) != "[{max-pods 150 }]" {
		t.Errorf("control plane: %+v", cp)
	}
	gpu, ok := o.Node(providers.Node{Role: config.RoleWorker, Pool: "gpu"})
	if !ok || fmt.Sprint(gpu.Labels) != "map[gpu:true]" || fmt.Sprint(gpu.RoleLabels) != "map[node-role.kubernetes.io/gpu:]" ||
		len(gpu.Taints) != 1 || fmt.Sprint(gpu.KubeletArgs) != "[{max-pods 250 }]" {
		t.Errorf("gpu pool: %+v", gpu)
	}
	if _, ok := o.Node(providers.Node{Role: config.RoleWorker, Pool: "gone"}); ok {
		t.Error("a pool the file doesn't have must be reported")
	}

	// kube-vip: the nodes use its virtual IP; nothing to test before init, kube-vip comes with it.
	vip := strings.Replace(file, "apiEndpoint: lb.lab.example:6443, apiLoadBalancer: external", "apiEndpoint: 192.168.1.100:6443, apiLoadBalancer: kube-vip", 1)
	if o := kubeadmOptions(parse(t, "lab", vip)); o.ControlPlaneEndpoint != "192.168.1.100:6443" || o.KubeVIP != "192.168.1.100" || o.CheckEndpoint {
		t.Errorf("kube-vip: endpoint %q, virtual IP %q, check %v", o.ControlPlaneEndpoint, o.KubeVIP, o.CheckEndpoint)
	}
}

// A secret a kubeadm flag reads from the environment is read before anything starts, reaches
// the install, and is never saved.
func TestKubeadmSecretsAreReadFirst(t *testing.T) {
	p, _ := usePrepare(t, nil)
	t.Setenv("OIDC_SECRET", "")
	os.Unsetenv("OIDC_SECRET")
	withSecret := strings.Replace(kubeadmLab, "{flavor: dev-test,", "{flavor: dev-test, kubernetes: {apiServer: {extraArgs: {oidc-client-secret: {env: OIDC_SECRET}}}},", 1)
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", withSecret))
	if err == nil || !strings.Contains(err.Error(), "OIDC_SECRET isn't set") || slices.Contains(p.calls, "provision") {
		t.Fatalf("err = %v, calls %v", err, p.calls)
	}

	t.Setenv("OIDC_SECRET", "oidc-value-from-env")
	_, clusters := useKubeadmInstall(t, nil, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", withSecret)); err != nil {
		t.Fatal(err)
	}
	if got := (*clusters)[0].Secrets["OIDC_SECRET"]; got != "oidc-value-from-env" {
		t.Errorf("secret = %q", got)
	}
	if st, _ := state.Load("lab"); strings.Contains(stateText(t, st), "oidc-value-from-env") {
		t.Error("the secret's value reached the state")
	}
}

// A failed setup keeps the machines, prepared; the next apply picks up from there.
func TestKubeadmSetupResumes(t *testing.T) {
	p, _ := usePrepare(t, nil)
	useKubeadm(t, errors.New("192.168.1.10: installing containerd 2.3.7 failed: sha256sum: WARNING: 1 computed checksum did NOT match"))
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", kubeadmLab))
	if err == nil || !strings.Contains(err.Error(), "did NOT match") || !strings.Contains(err.Error(), "pick up from here") {
		t.Fatalf("err = %v", err)
	}
	if st, _ := state.Load("lab"); st == nil || st.Status != state.StatusFailed || st.Stage != state.StagePrepared {
		t.Fatalf("state after a failed setup: %+v", st)
	}
	setups := useKubeadm(t, nil)
	p.calls = nil
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", kubeadmLab)); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(p.calls, "provision") || len(*setups) != 1 {
		t.Errorf("machines reused, set up again: calls %v, setups %d", p.calls, len(*setups))
	}
}

// A failed kubeadm init or join keeps the machines; the next apply runs the setup (which
// finds everything there) and the install again, on the same machines, then the CNI.
func TestKubeadmInstallResumes(t *testing.T) {
	p, _ := usePrepare(t, nil)
	r := useCharts(t, nil, nil)
	useKubeadmInstall(t, nil, errors.New("192.168.1.10 (lab-cp-0): kubeadm init failed: exit status 1\nerror execution phase wait-control-plane"))
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", kubeadmLab))
	if err == nil || !strings.Contains(err.Error(), "wait-control-plane") || !strings.Contains(err.Error(), "pick up from here") {
		t.Fatalf("err = %v", err)
	}
	if st, _ := state.Load("lab"); st == nil || st.Status != state.StatusFailed || st.Stage != state.StagePrepared {
		t.Fatalf("state after a failed install: %+v", st)
	}
	if len(r.networks) != 0 {
		t.Error("no CNI on a cluster that didn't start")
	}

	p.calls = nil
	setups, clusters := useKubeadmInstall(t, nil, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", kubeadmLab)); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(p.calls, "provision") || len(*setups) != 1 || len(*clusters) != 1 || len(r.networks) != 1 {
		t.Errorf("calls %v, setups %d, installs %d, networks %d", p.calls, len(*setups), len(*clusters), len(r.networks))
	}
	if st, _ := state.Load("lab"); st.Status != state.StatusReady || st.Stage != state.StageAddons {
		t.Errorf("state: %+v", st)
	}
}

// "stable" is checked against the runtime once it's resolved, before anything is created.
func TestKubeadmStableCheckedAgainstRuntime(t *testing.T) {
	p, _ := usePrepare(t, nil)
	resolveVersion = func(context.Context, string, string) (string, error) { return "v1.34.2", nil }
	stable := strings.Replace(kubeadmLab, "{flavor: dev-test,", "{flavor: dev-test, kubernetes: {version: stable},", 1)
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", stable))
	if err == nil || !strings.Contains(err.Error(), "Kubernetes v1.34.2: containerd 2.3.7 isn't one containerd recommends") {
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(p.calls, "provision") {
		t.Error("nothing may be created")
	}
}

// Tests may stop once the machines are prepared: no runtime, no Kubernetes on them.
func TestStopAfterPrepared(t *testing.T) {
	_, prepared := usePrepare(t, nil)
	setups := useKubeadm(t, nil)
	installs := useInstall(t, nil)
	r := useCharts(t, errors.New("helm isn't installed, and nobody can answer here"), nil)
	if err := NewOrchestrator(false).StopAfter(state.StagePrepared).Apply(context.Background(), parse(t, "lab", kubeadmLab)); err != nil {
		t.Fatal(err)
	}
	if len(*prepared) != 1 || len(*setups) != 0 || (*prepared)[0].Preflight != nil {
		t.Errorf("prepared %d, setups %d, preflight %v", len(*prepared), len(*setups), (*prepared)[0].Preflight != nil)
	}
	// dev-test asks for Calico, which the run never gets to: no helm needed.
	if len(r.finds)+len(r.kubectl)+len(r.networks) != 0 {
		t.Errorf("tools looked for: %+v", r)
	}
	if err := NewOrchestrator(false).StopAfter(state.StagePrepared).Apply(context.Background(), parse(t, "k3s", strings.Replace(homelab, "192.168.1.10", "192.168.1.11", 1))); err != nil {
		t.Fatal(err)
	}
	if len(*installs) != 0 {
		t.Error("no k3s either")
	}
}

func TestKubeadmDryRun(t *testing.T) {
	_, prepared := usePrepare(t, nil)
	setups := useKubeadm(t, nil)
	if err := NewOrchestrator(true).Apply(context.Background(), parse(t, "lab", kubeadmLab)); err != nil {
		t.Fatal(err)
	}
	if len(*prepared) != 0 || len(*setups) != 0 {
		t.Error("a dry run changes nothing")
	}
}
