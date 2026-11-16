package core

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
)

// fakeProvider records calls so tests can assert on the flow without real infra.
type fakeProvider struct {
	name         string
	calls        []string
	spec         providers.Spec
	status       *providers.StatusResult // nil = healthy
	provisionErr error
	teardownErr  error
	// duringProvision runs inside Provision: what an apply killed there leaves behind.
	duringProvision func()
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) PreFlight(context.Context, providers.Spec) error {
	f.calls = append(f.calls, "preflight")
	return nil
}
func (f *fakeProvider) Plan(_ context.Context, s providers.Spec) ([]string, error) {
	f.calls, f.spec = append(f.calls, "plan"), s
	return []string{"would do things"}, nil
}
func (f *fakeProvider) Provision(_ context.Context, s providers.Spec) (*providers.Result, error) {
	f.calls, f.spec = append(f.calls, "provision"), s
	if f.duringProvision != nil {
		f.duringProvision()
	}
	if f.provisionErr != nil {
		return nil, f.provisionErr
	}
	return &providers.Result{KubeContext: "ctx-" + s.Name, Kubeconfig: filepath.Join(state.Dir(s.Name), "kubeconfig")}, nil
}
func (f *fakeProvider) Status(context.Context, providers.Spec) (providers.StatusResult, error) {
	f.calls = append(f.calls, "status")
	if f.status != nil {
		return *f.status, nil
	}
	return providers.StatusResult{Healthy: true, State: "ready"}, nil
}
func (f *fakeProvider) Teardown(_ context.Context, s providers.Spec) error {
	f.calls, f.spec = append(f.calls, "teardown"), s
	return f.teardownErr
}

// useFakes swaps in a fake provider, an offline version resolver and a k3s install that
// touches no machine, isolating HOME (state) and KUBECONFIG.
func useFakes(t *testing.T) (requested *string, fake *fakeProvider) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("KUBECONFIG", filepath.Join(home, ".kube", "config"))
	useInstall(t, nil)
	useCharts(t, nil, nil)
	useKubeadm(t, nil)

	fake = &fakeProvider{}
	requested = new(string)
	origProvider, origResolve := newProvider, resolveVersion
	newProvider = func(name string) (providers.Provider, error) {
		*requested, fake.name = name, name
		return fake, nil
	}
	resolveVersion = func(_ context.Context, _, v string) (string, error) {
		if v == config.VersionStable {
			return "v1.99.0", nil
		}
		return v, nil
	}
	t.Cleanup(func() { newProvider, resolveVersion = origProvider, origResolve })
	// Release channels: no answer, so no newer release noted; every release exists.
	origNewest, origExists := newestPatch, releaseExists
	newestPatch = func(context.Context, string, int) (string, error) { return "", errors.New("offline") }
	releaseExists = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { newestPatch, releaseExists = origNewest, origExists })
	// Certificates: none read (nothing expires).
	origExpiry := certExpiry
	certExpiry = func(context.Context, string) (time.Time, time.Time, error) { return time.Time{}, time.Time{}, nil }
	t.Cleanup(func() { certExpiry = origExpiry })

	// The user's own machines: nothing recorded on them, nothing to undo.
	origRead, origUndo := readLeft, undoMachines
	readLeft = func(_ context.Context, o prepare.UndoOptions) ([]prepare.Left, error) {
		left := make([]prepare.Left, len(o.Nodes))
		for i, n := range o.Nodes {
			left[i].Node = n
		}
		return left, nil
	}
	undoMachines = func(context.Context, prepare.UndoOptions, bool) error { return nil }
	// And no volumes; a running cluster's machines need nothing.
	origList, origCheck := cloudVolumes, checkMachines
	cloudVolumes = func(context.Context, string, []string) ([]kubeapi.Volume, error) { return nil, nil }
	checkMachines = func(context.Context, prepare.Options) ([]string, error) { return nil, nil }
	t.Cleanup(func() { readLeft, undoMachines, cloudVolumes, checkMachines = origRead, origUndo, origList, origCheck })
	// The built-in add-ons run as the file says (useBuiltins says otherwise); helm is there.
	useBuiltins(t, nil)
	origInstalled := installedTool
	installedTool = func(context.Context, tools.Tool) string { return "helm" }
	t.Cleanup(func() { installedTool = origInstalled })
	return requested, fake
}

func load(t *testing.T, example string) *config.Config {
	t.Helper()
	cfg, err := config.Load(filepath.Join("../../examples", example))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// loadAsK3s loads an example of several control planes on kubeadm (production) as k3s: the
// tests that use it are about machines, and kubeadm takes one control plane for now.
func loadAsK3s(t *testing.T, example string) *config.Config {
	t.Helper()
	cfg := load(t, example)
	cfg.Spec.Kubernetes.Distribution, cfg.Spec.Kubernetes.ContainerRuntime = config.DistributionK3s, ""
	return cfg
}

// haProvider's machines are three control planes behind a load balancer, and a worker.
type haProvider struct{ nodesProvider }

func (h *haProvider) Provision(ctx context.Context, s providers.Spec) (*providers.Result, error) {
	h.calls, h.spec = append(h.calls, "provision"), s
	var nodes []providers.Node
	for i := range 3 {
		nodes = append(nodes, providers.Node{Name: fmt.Sprintf("ha-cp-%d", i), Host: fmt.Sprintf("203.0.113.1%d", i), PrivateIP: fmt.Sprintf("10.0.0.1%d", i), Role: config.RoleControlPlane, User: "ubuntu"})
	}
	nodes = append(nodes, providers.Node{Name: "ha-general-0", Host: "203.0.113.20", PrivateIP: "10.0.0.20", Role: config.RoleWorker, Pool: "general", User: "ubuntu"})
	return &providers.Result{Nodes: nodes, Endpoint: "ha-api-0123.elb.eu-west-3.amazonaws.com:6443",
		NodeEndpoint: "internal-ha-nodes-4567.elb.eu-west-3.amazonaws.com:6443"}, nil
}

// Several kubeadm control planes go behind the address in front of them: on AWS the nodes'
// internal load balancer tow created (the kubeconfig keeps the public one); on your
// machines your own, tested from the first control plane before init.
func TestKubeadmSeveralControlPlanes(t *testing.T) {
	usePrepare(t, nil)
	h := &haProvider{}
	newProvider = func(name string) (providers.Provider, error) { h.name = name; return h, nil }
	_, installs := useKubeadmInstall(t, nil, nil)
	aws := "{flavor: dev-test, infrastructure: {provider: aws, aws: {region: eu-west-3}}, controlPlane: {count: 3}, workers: [{name: general, count: 1}]}"
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "ha", aws)); err != nil {
		t.Fatal(err)
	}
	if o := (*installs)[0]; o.ControlPlaneEndpoint != "internal-ha-nodes-4567.elb.eu-west-3.amazonaws.com:6443" || o.Endpoint != "ha-api-0123.elb.eu-west-3.amazonaws.com:6443" ||
		o.CheckEndpoint || o.KubeVIP != "" || len(o.Nodes) != 4 {
		t.Errorf("aws: endpoint %q (kubeconfig %q), check %v, kube-vip %q, %d nodes", o.ControlPlaneEndpoint, o.Endpoint, o.CheckEndpoint, o.KubeVIP, len(o.Nodes))
	}

	_, installs = useKubeadmInstall(t, nil, nil)
	byoh := `{flavor: dev-test, kubernetes: {apiEndpoint: "lb.lab.example:6443", apiLoadBalancer: external},
  infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, controlPlane: {machines: [{host: 192.168.1.10}, {host: 192.168.1.11}, {host: 192.168.1.12}]}}`
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", byoh)); err != nil {
		t.Fatal(err)
	}
	if o := (*installs)[0]; o.ControlPlaneEndpoint != "lb.lab.example:6443" || !o.CheckEndpoint {
		t.Errorf("byoh: endpoint %q, check %v", o.ControlPlaneEndpoint, o.CheckEndpoint)
	}

	// kube-vip: its virtual IP for the nodes and the kubeconfig, nothing to test before init.
	_, installs = useKubeadmInstall(t, nil, nil)
	vip := strings.NewReplacer(`apiEndpoint: "lb.lab.example:6443", apiLoadBalancer: external`, `apiEndpoint: "192.168.1.100:6443"`, "192.168.1.1", "192.168.1.3").Replace(byoh)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "vip", vip)); err != nil {
		t.Fatal(err)
	}
	if o := (*installs)[0]; o.ControlPlaneEndpoint != "192.168.1.100:6443" || o.KubeVIP != "192.168.1.100" || o.CheckEndpoint {
		t.Errorf("kube-vip: endpoint %q, virtual IP %q, check %v", o.ControlPlaneEndpoint, o.KubeVIP, o.CheckEndpoint)
	}
}

func TestApplyRoutesModeToProvider(t *testing.T) {
	tests := map[string]string{
		"local.yaml":        ProviderK3d,
		"homelab.yaml":      ProviderBYOH,
		"dev-test-aws.yaml": ProviderTofu,
	}
	for example, want := range tests {
		t.Run(example, func(t *testing.T) {
			requested, _ := useFakes(t)
			if err := NewOrchestrator(true).Apply(context.Background(), load(t, example)); err != nil {
				t.Fatal(err)
			}
			if *requested != want {
				t.Errorf("provider = %q, want %q", *requested, want)
			}
		})
	}
}

func TestApplyDryRunOnlyPlans(t *testing.T) {
	_, fake := useFakes(t)
	if err := NewOrchestrator(true).Apply(context.Background(), load(t, "local.yaml")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"preflight", "plan"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
	if state.Exists("dev") {
		t.Error("dry run must not write state")
	}
}

func TestApplySavesStateOwnerOnly(t *testing.T) {
	_, fake := useFakes(t)
	if err := NewOrchestrator(false).Apply(context.Background(), load(t, "local.yaml")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"preflight", "provision"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}

	st, err := state.Load("dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.Provider != ProviderK3d || st.K8sVersion != config.DefaultKubernetesVersion || st.Mode != config.ModeLocal {
		t.Errorf("state = %+v, want provider k3d, mode local and the release tow tests", st)
	}
	if st.ConfigHash == "" || st.Config["spec.flavor"] != config.FlavorLocal || st.Outputs == nil || st.Outputs.KubeContext != "ctx-dev" {
		t.Errorf("state missing fingerprint or outputs: %+v", st)
	}

	info, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".tow/clusters/dev/state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("state file mode = %o, want 600", perm)
	}
}

func TestReapplyUnchangedIsNoop(t *testing.T) {
	_, fake := useFakes(t)
	cfg := load(t, "local.yaml")
	orch := NewOrchestrator(false)
	if err := orch.Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	before, _ := state.Load("dev")

	fake.calls = nil
	if err := orch.Apply(context.Background(), cfg); err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if want := []string{"status"}; !slices.Equal(fake.calls, want) {
		t.Errorf("second apply calls = %v, want only %v", fake.calls, want)
	}
	if after, _ := state.Load("dev"); !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Error("no-op apply rewrote state")
	}
}

func TestReapplyRefusesDrift(t *testing.T) {
	_, fake := useFakes(t)
	orch := NewOrchestrator(false)
	if err := orch.Apply(context.Background(), load(t, "local.yaml")); err != nil {
		t.Fatal(err)
	}

	changed := load(t, "local.yaml")
	changed.Spec.Kubernetes.Version = "v1.35.0"
	fake.calls = nil
	err := orch.Apply(context.Background(), changed)
	if err == nil {
		t.Fatal("expected drift error")
	}
	for _, want := range []string{"the file changed in ways it can't follow; nothing was changed", "  - spec.kubernetes.version: (none) → v1.35.0: " + strings.ReplaceAll(whyLocal, "%s", "dev")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
	if len(fake.calls) != 0 {
		t.Errorf("drift must not touch the provider, got calls %v", fake.calls)
	}
}

func TestReapplyDetectsDeletedCluster(t *testing.T) {
	_, fake := useFakes(t)
	orch := NewOrchestrator(false)
	if err := orch.Apply(context.Background(), load(t, "local.yaml")); err != nil {
		t.Fatal(err)
	}

	fake.status = &providers.StatusResult{State: "not_found"}
	err := orch.Apply(context.Background(), load(t, "local.yaml"))
	if err == nil || !strings.Contains(err.Error(), "can't find it") {
		t.Errorf("err = %v, want stale-state error", err)
	}
}

func TestFailedProvisionIsSavedAndRetried(t *testing.T) {
	_, fake := useFakes(t)
	fake.provisionErr = errors.New("quota exceeded")
	orch := NewOrchestrator(false)
	if err := orch.Apply(context.Background(), loadAsK3s(t, "production-aws.yaml")); err == nil {
		t.Fatal("expected provision error")
	}
	st, err := state.Load("prod-aws")
	if err != nil || st.Status != state.StatusFailed || st.Spec == nil || st.Spec.Machines == nil {
		t.Fatalf("failed provision must leave state for retry and delete: %+v, %v", st, err)
	}

	// A fixed config retries instead of being refused as drift.
	fake.provisionErr, fake.calls = nil, nil
	fixed := loadAsK3s(t, "production-aws.yaml")
	fixed.Spec.Workers[0].InstanceType = "t3.xlarge"
	if err := orch.Apply(context.Background(), fixed); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if want := []string{"preflight", "provision"}; !slices.Equal(fake.calls, want) {
		t.Errorf("retry calls = %v, want %v", fake.calls, want)
	}
	if st, _ := state.Load("prod-aws"); st.Status != state.StatusReady || st.Config["spec.workers[general].instanceType"] != "t3.xlarge" {
		t.Errorf("retry should record the new config as ready: %+v", st)
	}
}

// An apply killed while OpenTofu runs has already saved the cluster as failed, with what
// delete needs to tear it down.
func TestStateSavedBeforeProvision(t *testing.T) {
	_, fake := useFakes(t)
	var during *state.ClusterState
	fake.duringProvision = func() { during, _ = state.Load("prod-aws") }
	orch := NewOrchestrator(false)
	if err := orch.Apply(context.Background(), loadAsK3s(t, "production-aws.yaml")); err != nil {
		t.Fatal(err)
	}
	if during == nil || during.Status != state.StatusFailed || during.Spec == nil || during.Spec.Machines == nil {
		t.Fatalf("state while provisioning: %+v", during)
	}

	// The apply dies there: delete still finds the cluster and tears it down.
	if err := state.Save("prod-aws", *during); err != nil {
		t.Fatal(err)
	}
	fake.calls = nil
	if err := orch.Delete(context.Background(), "prod-aws", func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(fake.calls, "teardown") {
		t.Errorf("delete calls = %v", fake.calls)
	}
	if _, err := state.Load("prod-aws"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("state after delete: %v", err)
	}
}

// reconcilingProvider is a fake with infrastructure of its own, like OpenTofu.
type reconcilingProvider struct {
	*fakeProvider
	changes  []string
	creating []string // machines OpenTofu would create
}

// tooledProvider runs a program on this machine, like OpenTofu.
type tooledProvider struct{ *reconcilingProvider }

func (tooledProvider) Tool() tools.Tool { return tools.Tofu }

// The tofu a provider runs is found, or installed after a yes, before its PreFlight: when
// apply creates the cluster, when it checks a running one, and when delete destroys it.
// --install-tools installs without asking; a dry run only says what would be installed. A
// tofu that isn't there and isn't installed stops everything before PreFlight.
func TestProviderToolFoundFirst(t *testing.T) {
	r := useReconciling(t)
	newProvider = func(name string) (providers.Provider, error) { r.name = name; return tooledProvider{r}, nil }
	var opts []tools.Options
	var missing error
	orig := findTool
	findTool = func(_ context.Context, tl tools.Tool, o tools.Options) (string, error) {
		if tl.Name != "tofu" {
			t.Errorf("looked for %s", tl.Name)
		}
		r.calls, opts = append(r.calls, "find tofu"), append(opts, o)
		return "/home/u/.local/bin/tofu", missing
	}
	t.Cleanup(func() { findTool = orig })
	ctx, cfg := context.Background(), loadAsK3s(t, "production-aws.yaml")

	if err := NewOrchestrator(true).Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := NewOrchestrator(false).InstallTools().Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := NewOrchestrator(false).Apply(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := NewOrchestrator(false).Delete(ctx, "prod-aws", func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	want := []string{"find tofu", "preflight", "plan", "find tofu", "preflight", "provision", "find tofu", "preflight", "status", "changes", "find tofu", "preflight", "teardown"}
	if !slices.Equal(r.calls, want) {
		t.Errorf("calls = %v, want %v", r.calls, want)
	}
	if len(opts) != 4 || !opts[0].DryRun || opts[0].Install || !opts[1].Install || opts[2].Install || opts[3].Install || opts[3].DryRun {
		t.Errorf("options = %+v: dry run, --install-tools, then asked (apply and delete)", opts)
	}

	r.calls, missing = nil, errors.New("tofu isn't installed, so nothing was done")
	if err := NewOrchestrator(false).Apply(ctx, cfg); err == nil || !slices.Equal(r.calls, []string{"find tofu"}) {
		t.Errorf("no tofu: %v, calls %v", err, r.calls)
	}
}

func (r *reconcilingProvider) Changes(_ context.Context, s providers.Spec) (providers.Planned, error) {
	r.calls, r.spec = append(r.calls, "changes"), s
	return providers.Planned{Lines: r.changes, Creating: r.creating}, nil
}

func useReconciling(t *testing.T) *reconcilingProvider {
	t.Helper()
	_, fake := useFakes(t)
	r := &reconcilingProvider{fakeProvider: fake}
	newProvider = func(name string) (providers.Provider, error) {
		fake.name = name
		return r, nil
	}
	return r
}

// Re-applying an unchanged cluster plans its infrastructure again: nothing to do says up to
// date; something changed outside tow is put back after a yes, guarded as a running
// cluster.
func TestReapplyRechecksInfrastructure(t *testing.T) {
	r := useReconciling(t)
	cfg := loadAsK3s(t, "production-aws.yaml")
	if err := NewOrchestrator(false).Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	before, _ := state.Load("prod-aws")

	r.calls = nil
	if err := NewOrchestrator(false).Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if want := []string{"preflight", "status", "changes"}; !slices.Equal(r.calls, want) {
		t.Errorf("calls = %v, want %v", r.calls, want)
	}
	if r.spec.Guard != providers.GuardRunning {
		t.Errorf("guard = %v, want GuardRunning", r.spec.Guard)
	}

	r.changes, r.calls = []string{"Would create 1 resource in aws eu-west-3: aws_vpc_security_group_ingress_rule ×1"}, nil
	if err := NewOrchestrator(true).Apply(context.Background(), cfg); err != nil || slices.Contains(r.calls, "provision") {
		t.Errorf("a dry run changes nothing: %v, calls %v", err, r.calls)
	}
	// Nobody to ask in a test: refused without --yes.
	if err := NewOrchestrator(false).Apply(context.Background(), cfg); err == nil || slices.Contains(r.calls, "provision") {
		t.Errorf("without --yes: %v, calls %v", err, r.calls)
	}
	r.calls = nil
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if want := []string{"preflight", "status", "changes", "provision"}; !slices.Equal(r.calls, want) || r.spec.Guard != providers.GuardRunning {
		t.Errorf("calls = %v (guard %v), want %v", r.calls, r.spec.Guard, want)
	}
	if after, _ := state.Load("prod-aws"); !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Error("putting the infrastructure back doesn't change the cluster's state")
	}
}

// A retried apply keeps machines Kubernetes may run on, also after OpenTofu refused once.
func TestRetryGuardsMachinesWithKubernetes(t *testing.T) {
	_, fake := useFakes(t)
	fake.provisionErr = errors.New("OpenTofu would replace machine prod-aws-cp-0")
	cfg := loadAsK3s(t, "production-aws.yaml")
	orch := NewOrchestrator(false)
	orch.Apply(context.Background(), cfg)
	if fake.spec.Guard != providers.GuardOff {
		t.Errorf("a new cluster: guard = %v", fake.spec.Guard)
	}

	st, _ := state.Load("prod-aws")
	st.Stage = state.StageKubernetes
	if err := state.Save("prod-aws", *st); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		orch.Apply(context.Background(), cfg)
		if fake.spec.Guard != providers.GuardKeep {
			t.Errorf("guard = %v, want GuardKeep", fake.spec.Guard)
		}
		if st, _ := state.Load("prod-aws"); st.Stage != state.StageKubernetes {
			t.Errorf("stage = %q: a refused run must keep it, or the next one would replace the machines", st.Stage)
		}
	}

	// Machines nothing was installed on may still change.
	st, _ = state.Load("prod-aws")
	st.Stage = state.StageMachines
	state.Save("prod-aws", *st)
	orch.Apply(context.Background(), cfg)
	if fake.spec.Guard != providers.GuardOff {
		t.Errorf("guard = %v, want GuardOff", fake.spec.Guard)
	}
}

func TestFailedLocalProvisionLeavesNoState(t *testing.T) {
	_, fake := useFakes(t)
	fake.provisionErr = errors.New("docker down")
	NewOrchestrator(false).Apply(context.Background(), load(t, "local.yaml"))
	if state.Exists("dev") {
		t.Error("k3d creates nothing to clean up on failure; no state expected")
	}
}

func TestDeleteTearsDownAndRemovesState(t *testing.T) {
	_, fake := useFakes(t)
	orch := NewOrchestrator(false)
	if err := orch.Apply(context.Background(), loadAsK3s(t, "production-aws.yaml")); err != nil {
		t.Fatal(err)
	}

	var summary string
	fake.calls = nil
	err := orch.Delete(context.Background(), "prod-aws", func(s string) bool { summary = s; return true })
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"preflight", "teardown"}; !slices.Equal(fake.calls, want) {
		t.Errorf("calls = %v, want %v", fake.calls, want)
	}
	if fake.spec.Machines == nil || fake.spec.Machines.Region != "eu-west-3" {
		t.Errorf("teardown needs the saved spec, got %+v", fake.spec)
	}
	if !strings.Contains(summary, "aws eu-west-3 (6 machine(s))") {
		t.Errorf("summary = %q", summary)
	}
	if _, err := os.Stat(state.Dir("prod-aws")); !os.IsNotExist(err) {
		t.Error("cluster directory should be gone")
	}
}

// Volumes on EBS or Proxmox disks outlive the machines: without -v the cluster isn't deleted;
// with it, they go first, through Kubernetes, then the machines.
func TestDeleteVolumes(t *testing.T) {
	_, fake := useFakes(t)
	ebs := "{flavor: dev-test, kubernetes: {storage: {csi: [aws-ebs]}}, infrastructure: {provider: aws, aws: {region: eu-west-3}}}"
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "vol", ebs)); err != nil {
		t.Fatal(err)
	}
	vols := []kubeapi.Volume{{Name: "pvc-1", Claim: "shop/data", Size: "1Gi", Driver: "ebs.csi.aws.com"}}
	var listErr error
	var deleted [][]kubeapi.Volume
	origList, origDelete := cloudVolumes, deleteVolumes
	cloudVolumes = func(_ context.Context, _ string, drivers []string) ([]kubeapi.Volume, error) {
		if slices.Contains(drivers, "rancher.io/local-path") {
			t.Error("local volumes die with the machines tow created: only cloud volumes count")
		}
		return vols, listErr
	}
	deleteVolumes = func(_ context.Context, _ string, v []kubeapi.Volume) error {
		deleted = append(deleted, v)
		fake.calls = append(fake.calls, "volumes")
		return nil
	}
	t.Cleanup(func() { cloudVolumes, deleteVolumes = origList, origDelete })
	yes := func(string) bool { return true }

	fake.calls = nil
	err := NewOrchestrator(false).Delete(context.Background(), "vol", yes)
	if err == nil || !strings.Contains(err.Error(), "pvc-1 (claim shop/data), 1Gi on ebs.csi.aws.com") || !strings.Contains(err.Error(), "'tow delete --name vol -v'") {
		t.Errorf("err = %v", err)
	}
	if slices.Contains(fake.calls, "teardown") || !state.Exists("vol") {
		t.Errorf("nothing may be deleted without -v: %v", fake.calls)
	}

	// Can't list them: refused without -v too; with -v, deleted anyway.
	listErr = errors.New("connection refused")
	if err := NewOrchestrator(false).Delete(context.Background(), "vol", yes); err == nil || !strings.Contains(err.Error(), "couldn't be listed") {
		t.Errorf("err = %v", err)
	}
	listErr = nil

	var summary string
	fake.calls = nil
	if err := NewOrchestrator(false).WithVolumes().Delete(context.Background(), "vol", func(s string) bool { summary = s; return true }); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.calls, []string{"preflight", "volumes", "teardown"}) || len(deleted) != 1 {
		t.Errorf("calls %v, deleted %v", fake.calls, deleted)
	}
	if !strings.Contains(summary, "Its 1 volume goes too, with their data (delete -v):\n  pvc-1") {
		t.Errorf("summary:\n%s", summary)
	}
}

func TestDeleteKeepsStateWhenTeardownFails(t *testing.T) {
	_, fake := useFakes(t)
	orch := NewOrchestrator(false)
	orch.Apply(context.Background(), loadAsK3s(t, "production-aws.yaml"))

	fake.teardownErr = errors.New("vpc still in use")
	err := orch.Delete(context.Background(), "prod-aws", func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "run 'tow delete --name prod-aws' again") {
		t.Errorf("err = %v", err)
	}
	if !state.Exists("prod-aws") {
		t.Error("state must survive a failed teardown so delete can be re-run")
	}
}

func TestDeleteCancelled(t *testing.T) {
	_, fake := useFakes(t)
	orch := NewOrchestrator(false)
	orch.Apply(context.Background(), load(t, "local.yaml"))

	fake.calls = nil
	if err := orch.Delete(context.Background(), "dev", func(string) bool { return false }); err == nil {
		t.Fatal("expected cancellation error")
	}
	if slices.Contains(fake.calls, "teardown") || !state.Exists("dev") {
		t.Error("a declined confirmation must not touch anything")
	}
}

func TestDeleteUnknownCluster(t *testing.T) {
	useFakes(t)
	err := NewOrchestrator(false).Delete(context.Background(), "nope", func(string) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "no cluster named 'nope'") {
		t.Errorf("err = %v", err)
	}
}

func TestSpecFromConfig(t *testing.T) {
	aws := specFromConfig(load(t, "production-aws.yaml"), "v1.37.1")
	if m := aws.Machines; m == nil || m.Cloud != "aws" || m.Region != "eu-west-3" || m.SSHUser != config.DefaultSSHUser || len(aws.Nodes) != 0 {
		t.Fatalf("provision spec = %+v, nodes %v", aws.Machines, aws.Nodes)
	}
	wantPools := []providers.Pool{
		{Name: "control-plane", Role: "control-plane", Count: 3},
		{Name: "general", Role: "worker", Count: 3, InstanceType: "t3.large", DiskGB: 50},
	}
	if !slices.Equal(aws.Machines.Pools, wantPools) || aws.Engine != config.DistributionKubeadm || aws.CNI != config.CNICilium || aws.PodCIDR != "10.244.0.0/16" {
		t.Errorf("aws pools %+v, engine %s, cni %s, pods %s", aws.Machines.Pools, aws.Engine, aws.CNI, aws.PodCIDR)
	}

	pve := specFromConfig(load(t, "production-proxmox.yaml"), "v1.37.1")
	m := pve.Machines
	if m == nil || m.Cloud != "proxmox" || m.Region != "pve" || m.URL != "https://192.168.1.10:8006" || m.Template != 9000 ||
		m.ControlPlanes() != 3 || m.Workers() != 3 || m.Storage != "local-lvm" || m.Bridge != "vmbr0" {
		t.Fatalf("proxmox spec = %+v", m)
	}
	if m.Pools[0] != (providers.Pool{Name: "control-plane", Role: "control-plane", Count: 3, CPUs: 2, MemoryMB: 4096, DiskGB: 40}) ||
		m.Pools[1] != (providers.Pool{Name: "general", Role: "worker", Count: 3, CPUs: 4, MemoryMB: 8192, DiskGB: 50}) {
		t.Errorf("proxmox pools %+v", m.Pools)
	}
	if m.Network.Addresses != "192.168.1.110-192.168.1.129" || m.Network.DNS[0] != "192.168.1.1" || pve.Endpoint != "192.168.1.100:6443" {
		t.Errorf("proxmox network %+v, endpoint %q", m.Network, pve.Endpoint)
	}

	lab := specFromConfig(load(t, "homelab.yaml"), "v1.36.5+k3s1")
	if lab.Machines != nil || len(lab.Nodes) != 3 {
		t.Fatalf("byoh spec = %+v", lab)
	}
	if n := lab.Nodes[0]; n.Host != "192.168.1.10" || n.Role != config.RoleControlPlane || n.User != "pi" || n.Port != 22 {
		t.Errorf("node[0] = %+v", n)
	}
	if n := lab.Nodes[2]; n.Role != config.RoleWorker || n.Pool != "pis" {
		t.Errorf("node[2] = %+v", n)
	}

	local := specFromConfig(parse(t, "dev", "{flavor: local, controlPlane: {count: 3}, workers: [{count: 2}]}"), "v1.36.5+k3s1")
	if local.Local == nil || *local.Local != (providers.LocalRequest{Servers: 3, Agents: 2}) || local.Machines != nil {
		t.Errorf("local spec = %+v", local)
	}
}
