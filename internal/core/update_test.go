package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	"k8s.io/client-go/tools/clientcmd"
)

// fakeNodes is the cluster's API: nodes as given, every change recorded.
type fakeNodes struct {
	live  []kubeapi.NodeMeta
	calls []string
	fail  string // the node whose change fails
}

func (f *fakeNodes) NodeMetas(context.Context) ([]kubeapi.NodeMeta, error) { return f.live, nil }
func (f *fakeNodes) Relabel(_ context.Context, node string, set map[string]string, remove []string) error {
	f.calls = append(f.calls, fmt.Sprintf("relabel %s %v %v", node, set, remove))
	if node == f.fail {
		return errors.New("the API refused")
	}
	return nil
}
func (f *fakeNodes) Retaint(_ context.Context, node string, add []kubeapi.Taint, remove []string) error {
	f.calls = append(f.calls, fmt.Sprintf("retaint %s %v %v", node, add, remove))
	return nil
}

func useNodes(t *testing.T, live ...kubeapi.NodeMeta) *fakeNodes {
	t.Helper()
	f := &fakeNodes{live: live}
	orig := nodeAPI
	nodeAPI = func(string) (nodeClient, error) { return f, nil }
	t.Cleanup(func() { nodeAPI = orig })
	return f
}

const lab = `{infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, kubernetes: {distribution: k3s},
  controlPlane: {machines: [{host: 10.0.0.1}]%s},
  workers: [{name: general, machines: [{host: 10.0.0.2}]%s}]}`

// running saves cfg as a cluster that runs on two machines, cp and w1, with a kubeconfig.
func running(t *testing.T, cfg *config.Config) {
	t.Helper()
	fields, spec := cfg.Fields(), specFromConfig(cfg, "v1.37.2")
	kubeconfig := filepath.Join(state.Dir(cfg.Name()), "kubeconfig")
	os.MkdirAll(filepath.Dir(kubeconfig), 0700)
	os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: lab, cluster: {server: "https://10.0.0.1:6443"}}]
users: [{name: lab, user: {token: fake}}]
contexts: [{name: lab, context: {cluster: lab, user: lab}}]
current-context: lab
`), 0600)
	err := state.Save(cfg.Name(), state.ClusterState{Name: cfg.Name(), Provider: providerForMode[cfg.Mode], Mode: cfg.Mode, K8sVersion: "v1.37.2",
		Status: state.StatusReady, Stage: state.StageAddons, ConfigHash: config.HashFields(fields), Config: fields, File: cfg.Effective(), Spec: &spec,
		Outputs: &providers.Result{Kubeconfig: kubeconfig, KubeContext: "lab", Nodes: []providers.Node{
			{Name: "cp", Host: "10.0.0.1", Role: config.RoleControlPlane, Pool: "control-plane"},
			{Name: "w1", Host: "10.0.0.2", Role: config.RoleWorker, Pool: "general"},
		}}})
	if err != nil {
		t.Fatal(err)
	}
}

// The file's labels and taints reach the nodes through the API; those it never had stay, a
// node tow didn't make is only reported, and the run is recorded once it all worked.
func TestUpdateLabelsAndTaints(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", ", labels: {tier: web, old: x}, taints: [{key: a, effect: NoSchedule}]")))
	api := useNodes(t,
		kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}},
		kubeapi.NodeMeta{Name: "w1", Labels: map[string]string{"tier": "web", "old": "x", "cloud.example/zone": "z"},
			Taints: []kubeapi.Taint{{Key: "a", Effect: "NoSchedule"}, {Key: "other", Effect: "NoExecute"}}},
		kubeapi.NodeMeta{Name: "gpu-1"})
	changed := parse(t, "lab", fmt.Sprintf(lab, ", schedulable: \"true\"", ", labels: {tier: batch}, taints: [{key: b, value: v, effect: NoExecute}]"))

	// A dry run, and a run with nobody to ask, change nothing.
	if err := NewOrchestrator(true).Apply(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if err := NewOrchestrator(false).Apply(context.Background(), changed); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Errorf("without --yes: %v", err)
	}
	if len(api.calls) != 0 {
		t.Fatalf("changed without a yes: %v", api.calls)
	}

	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"relabel cp map[] []",
		"retaint cp [] [node-role.kubernetes.io/control-plane:NoSchedule]",
		"relabel w1 map[tier:batch] [old]",
		"retaint w1 [b=v:NoExecute] [a:NoSchedule]",
	}
	if !slices.Equal(api.calls, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(api.calls, "\n  "), strings.Join(want, "\n  "))
	}
	st, _ := state.Load("lab")
	if st.Config["spec.workers[general].labels.tier"] != "batch" || st.ConfigHash != config.HashFields(changed.Fields()) {
		t.Errorf("the new file isn't recorded: %v", st.Config)
	}

	// The next run finds nothing to do.
	api.live[0].Taints, api.live[1].Labels, api.live[1].Taints = nil, map[string]string{"tier": "batch", "cloud.example/zone": "z"}, []kubeapi.Taint{{Key: "b", Value: "v", Effect: "NoExecute"}, {Key: "other", Effect: "NoExecute"}}
	api.calls = nil
	if err := NewOrchestrator(false).Apply(context.Background(), changed); err != nil || len(api.calls) != 0 {
		t.Errorf("up to date: %v, calls %v", err, api.calls)
	}
}

func TestPlanShowsBothSections(t *testing.T) {
	var lines []string
	plan := updatePlan{
		fileChanges: []string{"spec.workers[general].labels.tier: web → batch"},
		cluster:     []planStep{{lines: []string{"w1: label tier=batch"}}},
		notes:       []string{"node gpu-1 isn't one tow made (joined outside tow?): left alone"},
	}
	plan.print(func(l string) { lines = append(lines, l) }, "lab")
	want := []string{
		"Plan for cluster 'lab':",
		"  The file changed since the last apply:",
		"    spec.workers[general].labels.tier: web → batch",
		"  Infrastructure (OpenTofu, firewalls):",
		"    none",
		"  Cluster (Kubernetes and add-ons):",
		"    - w1: label tier=batch",
		"    - node gpu-1 isn't one tow made (joined outside tow?): left alone",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}
}

// A change that can't be made stops everything before anything is asked or touched.
func TestUpdateRefusesFirst(t *testing.T) {
	_, fake := useFakes(t)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	api := useNodes(t)
	changed := parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ", labels: {tier: web}"), "distribution: k3s", "distribution: k3s, networking: {podCIDR: 10.50.0.0/16}", 1))
	err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), changed)
	if err == nil || !strings.Contains(err.Error(), "spec.kubernetes.networking.podCIDR: 10.42.0.0/16 → 10.50.0.0/16: "+whyNetwork) {
		t.Errorf("err = %v", err)
	}
	if len(api.calls) != 0 || len(fake.calls) != 0 {
		t.Errorf("touched: %v %v", api.calls, fake.calls)
	}
}

// A run that stops says what it had done; the file isn't recorded, so the next run plans the
// rest from what's there.
func TestUpdateStopsPartway(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	api := useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	api.fail = "w1"
	changed := parse(t, "lab", fmt.Sprintf(lab, ", labels: {zone: a}", ", labels: {tier: web}"))
	err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), changed)
	if err == nil || !strings.Contains(err.Error(), "the API refused") || !strings.Contains(err.Error(), "Done before it stopped, so left out of the plan when you run it again:\n  - cp: label zone=a") {
		t.Errorf("err = %v", err)
	}
	if st, _ := state.Load("lab"); st.Config["spec.workers[general].labels.tier"] != "" {
		t.Error("a run that stopped must not record the new file")
	}
}

// A new context name renames the cluster's kubeconfig, here and in the user's, and leaves
// the user on the cluster they were on.
func TestUpdateKubeconfig(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	st, _ := state.Load("lab")
	data, _ := os.ReadFile(st.Outputs.Kubeconfig)
	if err := os.MkdirAll(filepath.Dir(os.Getenv("KUBECONFIG")), 0700); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(os.Getenv("KUBECONFIG"), data, 0600)

	renamed := parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: k3s, kubeconfig: {context: lab-2}", 1))
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), renamed); err != nil {
		t.Fatal(err)
	}
	user, err := clientcmd.LoadFromFile(os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	if user.Contexts["lab-2"] == nil || user.Contexts["lab"] != nil || user.CurrentContext != "lab-2" {
		t.Errorf("your kubeconfig: contexts %v, current %q", user.Contexts, user.CurrentContext)
	}
	own, _ := clientcmd.LoadFromFile(st.Outputs.Kubeconfig)
	if own.Contexts["lab-2"] == nil || own.CurrentContext != "lab-2" {
		t.Errorf("the state folder's copy: %v", own.Contexts)
	}
	if st, _ = state.Load("lab"); st.Outputs.KubeContext != "lab-2" || st.Spec.Kubernetes.Context != "lab-2" {
		t.Errorf("delete must find the new context: %+v %+v", st.Outputs, st.Spec.Kubernetes)
	}
}

// Two runs on one cluster: the second stops at once.
func TestApplyTakesTheLock(t *testing.T) {
	_, fake := useFakes(t)
	unlock, err := state.Lock("dev")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if err := NewOrchestrator(false).Apply(context.Background(), load(t, "local.yaml")); err == nil || !strings.Contains(err.Error(), "another tow run is changing cluster 'dev'") {
		t.Errorf("err = %v", err)
	}
	if err := NewOrchestrator(true).Apply(context.Background(), load(t, "local.yaml")); err != nil {
		t.Errorf("a dry run changes nothing, so it doesn't wait: %v", err)
	}
	if slices.Contains(fake.calls, "provision") {
		t.Errorf("calls = %v", fake.calls)
	}
}

// useMachines makes the machines' read-only check answer with each of answers in turn (the
// last repeats), and records the preparations made.
func useMachines(t *testing.T, answers ...func() ([]string, error)) *[]prepare.Options {
	t.Helper()
	calls, made := 0, &[]prepare.Options{}
	origCheck, origRun := checkMachines, prepareMachines
	checkMachines = func(context.Context, prepare.Options) ([]string, error) {
		a := answers[min(calls, len(answers)-1)]
		calls++
		return a()
	}
	prepareMachines = func(_ context.Context, o prepare.Options) error { *made = append(*made, o); return nil }
	t.Cleanup(func() { checkMachines, prepareMachines = origCheck, origRun })
	return made
}

func lines(l ...string) func() ([]string, error) { return func() ([]string, error) { return l, nil } }

// Every apply checks the machines: what they need is planned (here, a port the file no
// longer opens), and made under the plan's own yes, without preparation asking again.
func TestUpdateChecksTheMachines(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "access: {ssh: {user: pi}}", "access: {ssh: {user: pi}, ports: [8080, 9090]}", 1)))
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	made := useMachines(t, lines("w1: close ufw rules no longer needed: allow from 203.0.113.7 to any port 9090 proto tcp"))

	changed := parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "access: {ssh: {user: pi}}", "access: {ssh: {user: pi}, ports: [8080]}", 1))
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if len(*made) != 1 || !(*made)[0].Confirmed || (*made)[0].Confirm != nil || len((*made)[0].Nodes) != 2 {
		t.Fatalf("preparations: %+v", *made)
	}
	if st, _ := state.Load("lab"); st.Config["spec.infrastructure.access.ports"] != "8080" {
		t.Errorf("the new ports aren't recorded: %v", st.Config["spec.infrastructure.access.ports"])
	}
}

// This machine can't reach the machines (its address moved; the firewall follows it once
// OpenTofu runs): they're planned and changed right after OpenTofu, in the same run, under
// the same yes. With nothing for OpenTofu to do, that's an error.
func TestUpdateChecksTheMachinesAfterTofu(t *testing.T) {
	r := useReconciling(t)
	running(t, loadAsK3s(t, "production-aws.yaml"))
	st, _ := state.Load("prod-aws")
	st.Outputs.Nodes = nil
	var metas []kubeapi.NodeMeta // the machines as OpenTofu names them
	for _, m := range cloud.Machines("prod-aws", st.Spec.Machines.Pools) {
		st.Outputs.Nodes = append(st.Outputs.Nodes, providers.Node{Name: m.Name, Role: m.Role, Pool: m.Pool})
		meta := kubeapi.NodeMeta{Name: m.Name}
		if m.Role == config.RoleControlPlane {
			meta.Taints = []kubeapi.Taint{controlPlaneTaint}
		}
		metas = append(metas, meta)
	}
	state.Save("prod-aws", *st)
	useNodes(t, metas...)
	blocked := func() ([]string, error) { return nil, errors.New("dial tcp 10.0.0.1:22: i/o timeout") }

	useMachines(t, blocked)
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), loadAsK3s(t, "production-aws.yaml")); err == nil || !strings.Contains(err.Error(), "is SSH open to this machine's address") {
		t.Errorf("nothing for OpenTofu to do: %v", err)
	}

	// The firewall takes a moment: the first try after OpenTofu still finds them closed.
	origWait := reachWait
	reachWait = time.Millisecond
	t.Cleanup(func() { reachWait = origWait })
	r.changes, r.calls = []string{"This machine's address changed since the last apply: 198.51.100.1/32 → 203.0.113.7/32; SSH, the API and NodePorts follow it"}, nil
	made := useMachines(t, blocked, blocked, lines("w1: open ufw (it's on): allow from 203.0.113.7 to any port 22 proto tcp"))
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), loadAsK3s(t, "production-aws.yaml")); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.calls, "provision") || len(*made) != 1 || !(*made)[0].Confirmed {
		t.Errorf("calls %v, preparations %+v", r.calls, *made)
	}
}
