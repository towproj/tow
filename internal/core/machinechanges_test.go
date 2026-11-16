package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
	"github.com/towproj/tow/internal/state"
)

// machinesProvider gives back the machines it's asked for, as the real providers do:
// OpenTofu's by name, the user's own named after their address.
type machinesProvider struct{ *reconcilingProvider }

func (m *machinesProvider) Provision(_ context.Context, s providers.Spec) (*providers.Result, error) {
	m.calls, m.spec = append(m.calls, "provision"), s
	if m.provisionErr != nil {
		return nil, m.provisionErr
	}
	var nodes []providers.Node
	if s.Machines != nil {
		for i, x := range cloud.Machines(s.Name, s.Machines.Pools) {
			nodes = append(nodes, providers.Node{Name: x.Name, Host: fmt.Sprintf("10.0.1.%d", 10+i), Role: x.Role, Pool: x.Pool})
		}
	}
	for _, n := range s.Nodes {
		n.Name = "node-" + n.Host
		nodes = append(nodes, n)
	}
	return &providers.Result{Nodes: nodes}, nil
}

// machineLog records what removing and joining machines did, in order.
type machineLog struct {
	calls    []string
	undo     []prepare.UndoOptions
	undoPrep []bool
	drainErr error
	etcdErr  error
	volumes  []string
}

func useMachineChanges(t *testing.T) (*machinesProvider, *machineLog) {
	t.Helper()
	r := useReconciling(t)
	p := &machinesProvider{r}
	newProvider = func(name string) (providers.Provider, error) {
		r.name = name
		return p, nil
	}
	l := &machineLog{}
	origDrain, origLeave, origDelete, origVols, origWait, origUndo, origFree :=
		drainNode, leaveEtcd, deleteNode, localVolumes, waitNewReady, undoMachines, checkNewHosts
	drainNode = func(_ context.Context, _, _, node string, timeout time.Duration) error {
		l.calls = append(l.calls, fmt.Sprintf("drain %s %s", node, timeout))
		return l.drainErr
	}
	leaveEtcd = func(_ context.Context, n providers.Node, _ string, _ func(context.Context, providers.Node, string) (prepare.Runner, error)) error {
		l.calls = append(l.calls, "leave etcd "+n.Name)
		return nil
	}
	origK3sLeave, origTries, origNodeWait := leaveK3sEtcd, nodeTries, nodeWait
	leaveK3sEtcd = func(_ context.Context, _, _, node string) error {
		l.calls = append(l.calls, "k3s etcd "+node)
		return l.etcdErr
	}
	nodeTries, nodeWait = 2, time.Millisecond
	t.Cleanup(func() { leaveK3sEtcd, nodeTries, nodeWait = origK3sLeave, origTries, origNodeWait })
	deleteNode = func(_ context.Context, _, node string) error {
		l.calls = append(l.calls, "delete node "+node)
		return nil
	}
	localVolumes = func(context.Context, string, string) ([]string, error) { return l.volumes, nil }
	waitNewReady = func(_ context.Context, _ string, nodes []string) error {
		l.calls = append(l.calls, "ready "+strings.Join(nodes, ","))
		return nil
	}
	undoMachines = func(_ context.Context, o prepare.UndoOptions, preparation bool) error {
		l.calls = append(l.calls, "undo "+o.Nodes[0].Name)
		l.undo, l.undoPrep = append(l.undo, o), append(l.undoPrep, preparation)
		return nil
	}
	checkNewHosts = func(*config.Config) error { return nil }
	t.Cleanup(func() {
		drainNode, leaveEtcd, deleteNode, localVolumes, waitNewReady, undoMachines, checkNewHosts = origDrain, origLeave, origDelete, origVols, origWait, origUndo, origFree
	})
	return p, l
}

// runningMachines is running, with the machines OpenTofu would have made.
func runningMachines(t *testing.T, cfg *config.Config) []kubeapi.NodeMeta {
	t.Helper()
	running(t, cfg)
	st, _ := state.Load(cfg.Name())
	st.Outputs.Nodes = nil
	var metas []kubeapi.NodeMeta
	for i, m := range cloud.Machines(cfg.Name(), st.Spec.Machines.Pools) {
		st.Outputs.Nodes = append(st.Outputs.Nodes, providers.Node{Name: m.Name, Host: fmt.Sprintf("10.0.1.%d", 10+i), Role: m.Role, Pool: m.Pool})
		meta := kubeapi.NodeMeta{Name: m.Name}
		if m.Role == config.RoleControlPlane {
			meta.Taints = []kubeapi.Taint{controlPlaneTaint}
		}
		metas = append(metas, meta)
	}
	state.Save(cfg.Name(), *st)
	return metas
}

func stateNodes(t *testing.T, name string) []string {
	t.Helper()
	st, _ := state.Load(name)
	var out []string
	for _, n := range st.Outputs.Nodes {
		out = append(out, n.Name)
	}
	return out
}

// Machines tow creates: a worker added is created (OpenTofu's run naming it), the
// machines prepared, and Kubernetes joins it; then the same worker leaving is drained, taken
// out and its Node deleted, and only then destroyed (OpenTofu's second run naming it).
func TestTofuMachinesJoinAndLeave(t *testing.T) {
	p, l := useMachineChanges(t)
	cfg := loadAsK3s(t, "production-aws.yaml")
	useNodes(t, runningMachines(t, cfg)...)
	k3sRuns := useInstall(t, nil)
	made := useMachines(t, lines())

	more := loadAsK3s(t, "production-aws.yaml")
	more.Spec.Workers[0].Count = 4
	p.changes = []string{"Would create 1 resource in aws eu-west-3: aws_instance ×1"}
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), more); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(p.spec.Adding, []string{"prod-aws-general-3"}) || p.spec.Guard != providers.GuardRunning {
		t.Errorf("OpenTofu's run: adding %v, guard %v", p.spec.Adding, p.spec.Guard)
	}
	if len(*k3sRuns) != 1 || len((*k3sRuns)[0].Nodes) != 7 {
		t.Fatalf("k3s runs %d", len(*k3sRuns))
	}
	if last := (*made)[len(*made)-1]; len(last.Nodes) != 7 || !last.Confirmed {
		t.Errorf("every machine is prepared for the new one: %+v", last)
	}
	if want := []string{"ready prod-aws-general-3"}; !slices.Equal(l.calls, want) {
		t.Errorf("calls %v, want %v", l.calls, want)
	}
	if got := stateNodes(t, "prod-aws"); len(got) != 7 || !slices.Contains(got, "prod-aws-general-3") {
		t.Errorf("state's nodes %v", got)
	}

	// Back to 3: the highest-numbered leaves, last.
	l.calls, p.calls = nil, nil
	l.volumes = []string{"shop/data-0"}
	useNodes(t, runningMetas(t, "prod-aws")...)
	if err := NewOrchestrator(false).AssumeYes().WithDrainTimeout(3*time.Minute).Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	want := []string{"drain prod-aws-general-3 3m0s", "undo prod-aws-general-3", "delete node prod-aws-general-3"}
	if !slices.Equal(l.calls, want) {
		t.Errorf("calls %v, want %v", l.calls, want)
	}
	if l.undo[0].Firewall || l.undoPrep[0] {
		t.Error("a machine tow made is destroyed: only Kubernetes is undone")
	}
	if !slices.Equal(p.spec.Removing, []string{"prod-aws-general-3"}) || p.calls[len(p.calls)-1] != "provision" {
		t.Errorf("OpenTofu's second run: removing %v, calls %v", p.spec.Removing, p.calls)
	}
	if got := stateNodes(t, "prod-aws"); len(got) != 6 || slices.Contains(got, "prod-aws-general-3") {
		t.Errorf("state's nodes %v", got)
	}
	if st, _ := state.Load("prod-aws"); st.Config["spec.workers[general].count"] != "3" {
		t.Errorf("the file isn't recorded: %v", st.Config["spec.workers[general].count"])
	}
}

func runningMetas(t *testing.T, name string) []kubeapi.NodeMeta {
	t.Helper()
	st, _ := state.Load(name)
	var out []kubeapi.NodeMeta
	for _, n := range st.Outputs.Nodes {
		m := kubeapi.NodeMeta{Name: n.Name}
		if n.Role == config.RoleControlPlane {
			m.Taints = []kubeapi.Taint{controlPlaneTaint}
		}
		out = append(out, m)
	}
	return out
}

// The user's own machines: a new one is checked alone (the others run Kubernetes), then
// joined; one leaving keeps its preparation, but not the cluster's firewall rules, unless
// --reset.
func TestBYOHMachinesJoinAndLeave(t *testing.T) {
	p, l := useMachineChanges(t)
	two := `{name: general, machines: [{host: 10.0.0.2}, {host: 10.0.0.3}]}`
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	k3sRuns := useInstall(t, nil)
	useMachines(t, lines())

	added := parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "{name: general, machines: [{host: 10.0.0.2}]}", two, 1))
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), added); err != nil {
		t.Fatal(err)
	}
	if len(p.spec.Nodes) != 1 || p.spec.Nodes[0].Host != "10.0.0.3" {
		t.Errorf("only the new machine is checked: %+v", p.spec.Nodes)
	}
	if len(*k3sRuns) != 1 || len((*k3sRuns)[0].Nodes) != 3 || !slices.Equal(l.calls, []string{"ready node-10.0.0.3"}) {
		t.Errorf("k3s runs %d, calls %v", len(*k3sRuns), l.calls)
	}

	// It leaves again: Kubernetes and the firewall rules go, the rest stays.
	l.calls = nil
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"}, kubeapi.NodeMeta{Name: "node-10.0.0.3"})
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), parse(t, "lab", fmt.Sprintf(lab, "", ""))); err != nil {
		t.Fatal(err)
	}
	if want := []string{"drain node-10.0.0.3 10m0s", "undo node-10.0.0.3", "delete node node-10.0.0.3"}; !slices.Equal(l.calls, want) {
		t.Errorf("calls %v, want %v", l.calls, want)
	}
	if !l.undo[0].Firewall || l.undoPrep[0] {
		t.Errorf("firewall %v, preparation %v", l.undo[0].Firewall, l.undoPrep[0])
	}
	if got := stateNodes(t, "lab"); len(got) != 2 {
		t.Errorf("state's nodes %v", got)
	}

	// With --reset, all of its preparation goes.
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), added); err != nil {
		t.Fatal(err)
	}
	l.undo, l.undoPrep = nil, nil
	if err := NewOrchestrator(false).AssumeYes().WithReset().Apply(context.Background(), parse(t, "lab", fmt.Sprintf(lab, "", ""))); err != nil {
		t.Fatal(err)
	}
	if !l.undoPrep[0] {
		t.Error("--reset undoes the preparation too")
	}
}

// A drain that doesn't finish stops the run: the machine stays, in the cluster and in state.
func TestMachineThatCantBeDrained(t *testing.T) {
	_, l := useMachineChanges(t)
	running(t, parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "{host: 10.0.0.2}", "{host: 10.0.0.2}, {host: 10.0.0.3}", 1)))
	st, _ := state.Load("lab")
	st.Outputs.Nodes = append(st.Outputs.Nodes, providers.Node{Name: "w2", Host: "10.0.0.3", Role: config.RoleWorker, Pool: "general"})
	state.Save("lab", *st)
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"}, kubeapi.NodeMeta{Name: "w2"})
	useMachines(t, lines())
	l.drainErr = errors.New("w2 couldn't be drained: a PodDisruptionBudget allows no disruption")

	err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), parse(t, "lab", fmt.Sprintf(lab, "", "")))
	wantErr(t, err, "PodDisruptionBudget")
	if slices.ContainsFunc(l.calls, func(c string) bool { return strings.HasPrefix(c, "undo") || strings.HasPrefix(c, "delete") }) {
		t.Errorf("nothing after the drain: %v", l.calls)
	}
	if got := stateNodes(t, "lab"); !slices.Contains(got, "w2") {
		t.Errorf("state's nodes %v", got)
	}
}

// Refused before anything changes: the first control plane leaving, a machine changing its
// role, kubeadm control planes with no address in front of them, a second control plane on
// AWS's single one.
func TestMachineChangesRefused(t *testing.T) {
	useMachineChanges(t)
	useMachines(t, lines())
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	err := applyYes(t, parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "{host: 10.0.0.1}", "{host: 10.0.0.5}", 1)))
	wantErr(t, err, "nothing was changed", "the first control plane stays")
	err = applyYes(t, parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "name: general", "name: gpu", 1)))
	wantErr(t, err, "10.0.0.2: a machine keeps its role and its pool")

	// One control plane on AWS: no load balancer in front of the API yet. (Elsewhere the file
	// can't add a second without an apiEndpoint, which a running cluster can't take.)
	for _, d := range []string{config.DistributionK3s, config.DistributionKubeadm} {
		single := load(t, "dev-test-aws.yaml")
		single.Spec.Kubernetes.Distribution = d
		if d == config.DistributionK3s {
			single.Spec.Kubernetes.ContainerRuntime = ""
		}
		useNodes(t, runningMachines(t, single)...)
		two := *single
		two.Spec.ControlPlane.Count = 2
		err = applyYes(t, &two)
		want := "on AWS a second control plane brings the load balancer"
		if d == config.DistributionKubeadm {
			want = "kubeadm's control planes join through an address in front of them"
		}
		wantErr(t, err, "nothing was changed", want)
	}
}

// A machine deleted outside tow: what's left of it is cleared (its Node, a kubeadm
// control plane's etcd member), OpenTofu makes it again under its name, and it joins. The
// first control plane is never made again; an unhealthy cluster with nothing to make again is
// refused as before.
func TestGoneMachinesAreMadeAgain(t *testing.T) {
	p, l := useMachineChanges(t)
	cfg := load(t, "production-aws.yaml") // kubeadm, 3 control planes behind AWS's load balancer
	useNodes(t, runningMachines(t, cfg)...)
	useKubeadm(t, nil)
	useMachines(t, lines())
	var forgot []string
	origForget := forgetEtcdMember
	forgetEtcdMember = func(_ context.Context, _, _, via, member string) error {
		forgot = append(forgot, via+" forgets "+member)
		return nil
	}
	t.Cleanup(func() { forgetEtcdMember = origForget })
	p.status = &providers.StatusResult{State: "degraded", Message: "not every machine is running"}
	// The machines that are gone don't answer: they're checked only once they're made again.
	origCheck := checkMachines
	checkMachines = func(_ context.Context, o prepare.Options) ([]string, error) {
		for _, n := range o.Nodes {
			if n.Name == "prod-aws-cp-1" || n.Name == "prod-aws-general-0" {
				return nil, errors.New("dial tcp " + n.Host + ":22: connect: no route to host")
			}
		}
		return nil, nil
	}
	t.Cleanup(func() { checkMachines = origCheck })

	p.changes, p.creating = []string{"Would create 2 resources in aws eu-west-3: aws_instance ×2"}, []string{"prod-aws-cp-1", "prod-aws-general-0"}
	if err := applyYes(t, cfg); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(forgot, []string{"prod-aws-cp-0 forgets prod-aws-cp-1"}) {
		t.Errorf("etcd members forgotten: %v", forgot)
	}
	if want := []string{"delete node prod-aws-cp-1", "delete node prod-aws-general-0", "ready prod-aws-cp-1,prod-aws-general-0"}; !slices.Equal(l.calls, want) {
		t.Errorf("calls %v, want %v", l.calls, want)
	}
	if !slices.Equal(p.spec.Adding, []string{"prod-aws-cp-1", "prod-aws-general-0"}) || len(p.spec.Known) != 6 {
		t.Errorf("OpenTofu's run: adding %v, known %v", p.spec.Adding, p.spec.Known)
	}

	p.creating = []string{"prod-aws-cp-0"}
	wantErr(t, applyYes(t, cfg), "nothing was changed", "the first control plane, is gone")
	p.changes, p.creating = nil, nil
	wantErr(t, applyYes(t, cfg), "isn't healthy: degraded")
}

// A k3s control plane leaves etcd through k3s's member controller while it still runs, before
// k3s is removed from it; when it can't, the run stops with nothing else changed on it.
func TestK3sControlPlaneLeavesEtcdFirst(t *testing.T) {
	_, l := useMachineChanges(t)
	useMachines(t, lines())
	three := strings.Replace(strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: k3s, apiEndpoint: \"10.0.0.100:6443\"", 1),
		"machines: [{host: 10.0.0.1}]", "machines: [{host: 10.0.0.1}, {host: 10.0.0.4}]", 1)
	running(t, parse(t, "lab", three))
	st, _ := state.Load("lab")
	st.Outputs.Nodes = append(st.Outputs.Nodes[:1], providers.Node{Name: "cp-2", Host: "10.0.0.4", Role: config.RoleControlPlane}, st.Outputs.Nodes[1])
	state.Save("lab", *st)
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "cp-2", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	one := parse(t, "lab", strings.Replace(three, ", {host: 10.0.0.4}", "", 1))

	l.etcdErr = errors.New("k3s didn't take cp-2 out of etcd within 2m0s")
	wantErr(t, applyYes(t, one), "out of etcd", "cp-2 is drained (it stays cordoned); nothing else changed on it")
	if want := []string{"drain cp-2 10m0s", "k3s etcd cp-2"}; !slices.Equal(l.calls, want) {
		t.Errorf("calls %v, want %v", l.calls, want)
	}

	l.calls, l.etcdErr = nil, nil
	if err := applyYes(t, one); err != nil {
		t.Fatal(err)
	}
	if want := []string{"drain cp-2 10m0s", "k3s etcd cp-2", "undo cp-2", "delete node cp-2"}; !slices.Equal(l.calls, want) {
		t.Errorf("calls %v, want %v", l.calls, want)
	}
}

// A kubeadm control plane leaving takes itself out of etcd first.
func TestKubeadmControlPlaneLeavesEtcd(t *testing.T) {
	_, l := useMachineChanges(t)
	useMachines(t, lines())
	three := strings.Replace(strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: kubeadm, apiEndpoint: \"10.0.0.100:6443\", apiLoadBalancer: kube-vip", 1),
		"machines: [{host: 10.0.0.1}]", "machines: [{host: 10.0.0.1}, {host: 10.0.0.4}, {host: 10.0.0.5}]", 1)
	cfg, err := config.Parse([]byte("apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: lab}\nspec: " + three + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	running(t, cfg)
	st, _ := state.Load("lab")
	st.Outputs.Nodes = append(st.Outputs.Nodes[:1], providers.Node{Name: "cp-2", Host: "10.0.0.4", Role: config.RoleControlPlane},
		providers.Node{Name: "cp-3", Host: "10.0.0.5", Role: config.RoleControlPlane}, st.Outputs.Nodes[1])
	state.Save("lab", *st)
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "cp-2", Taints: []kubeapi.Taint{controlPlaneTaint}},
		kubeapi.NodeMeta{Name: "cp-3", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	useKubeadm(t, nil)

	err = applyYes(t, parse(t, "lab", strings.Replace(three, ", {host: 10.0.0.5}", "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"drain cp-3 10m0s", "leave etcd cp-3", "undo cp-3", "delete node cp-3"}; !slices.Equal(l.calls, want) {
		t.Errorf("calls %v, want %v", l.calls, want)
	}
}
