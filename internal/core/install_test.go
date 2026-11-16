package core

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
)

// k3s goes on the prepared machines; the kubeconfig ends up in the state folder and in the
// user's kubeconfig, under the file's context name.
func TestK3sIsInstalled(t *testing.T) {
	usePrepare(t, nil)
	installs := useInstall(t, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", homelab)); err != nil {
		t.Fatal(err)
	}
	if len(*installs) != 1 {
		t.Fatalf("installs = %d", len(*installs))
	}
	o := (*installs)[0]
	if o.Cluster != "lab" || o.Version != config.DefaultKubernetesVersion || len(o.Nodes) != 1 || o.Nodes[0].Host != "192.168.1.10" ||
		!o.WaitReady || o.KubeVIP != "" || o.CoreDNSReplicas != 0 || !strings.HasSuffix(o.KnownHosts, "/clusters/lab/known_hosts") {
		t.Errorf("install options: %+v", o)
	}

	st, err := state.Load("lab")
	if err != nil || st.Status != state.StatusReady || st.Stage != state.StageKubernetes || st.Outputs.KubeContext != "lab" {
		t.Fatalf("state: %+v, %v", st, err)
	}
	info, err := os.Stat(st.Outputs.Kubeconfig)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig in the state folder: %v, %v", info, err)
	}
	if ok, err := kubecfg.HasContext("lab"); !ok || err != nil {
		t.Errorf("context lab merged: %v, %v", ok, err)
	}

	// Delete removes the context it added.
	if err := NewOrchestrator(false).Delete(context.Background(), "lab", func(string) bool { return true }); err != nil {
		t.Fatal(err)
	}
	if ok, _ := kubecfg.HasContext("lab"); ok {
		t.Error("delete left the context behind")
	}
}

// A failed install keeps the machines; the next apply reuses them and the release it started
// with, though "stable" moved on meanwhile.
func TestK3sInstallResumes(t *testing.T) {
	p, prepared := usePrepare(t, nil)
	useInstall(t, errors.New("lab-cp-0: installing k3s failed: etcd join failed"))
	stable := strings.Replace(homelab, "{flavor: homelab,", "{flavor: homelab, kubernetes: {version: stable},", 1)
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", stable))
	if err == nil || !strings.Contains(err.Error(), "etcd join failed") || !strings.Contains(err.Error(), "pick up from here") {
		t.Fatalf("err = %v", err)
	}
	st, _ := state.Load("lab")
	if st == nil || st.Status != state.StatusFailed || st.Stage != state.StagePrepared || st.Outputs.KubeContext != "" {
		t.Fatalf("state after a failed install: %+v", st)
	}

	resolveVersion = func(context.Context, string, string) (string, error) { return "v1.100.0", nil }
	installs := useInstall(t, nil)
	p.calls, *prepared = nil, nil
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", stable)); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(p.calls, "provision") || len(*prepared) != 1 {
		t.Errorf("machines reused and checked again: calls %v, prepared %d", p.calls, len(*prepared))
	}
	if len(*installs) != 1 || (*installs)[0].Version != "v1.99.0" {
		t.Errorf("the release the install started with: %+v", *installs)
	}
	if st, _ := state.Load("lab"); st.Status != state.StatusReady || st.Stage != state.StageKubernetes || st.K8sVersion != "v1.99.0" {
		t.Errorf("state after resuming: %+v", st)
	}
}

// A context of the user's kubeconfig is never overwritten: it may reach another cluster.
func TestContextClashIsRefusedBeforeAnything(t *testing.T) {
	p, prepared := usePrepare(t, nil)
	data, err := kubecfg.Rename([]byte(k3sKubeconfig), "lab")
	if err != nil {
		t.Fatal(err)
	}
	if err := kubecfg.Merge(data); err != nil {
		t.Fatal(err)
	}
	err = NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", homelab))
	if err == nil || !strings.Contains(err.Error(), "already has a context named 'lab'") {
		t.Fatalf("err = %v", err)
	}
	if slices.Contains(p.calls, "provision") || len(*prepared) != 0 {
		t.Errorf("nothing may start: calls %v", p.calls)
	}
}

// A secret the file reads from the environment is read before anything starts.
func TestK3sSecretsAreReadFirst(t *testing.T) {
	p, _ := usePrepare(t, nil)
	t.Setenv("S3_KEY", "")
	os.Unsetenv("S3_KEY")
	withSecret := "{flavor: homelab, kubernetes: {k3s: {server: {extraArgs: {etcd-s3-secret-key: {env: S3_KEY}}}}}, infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, controlPlane: {machines: [{host: 192.168.1.10}]}}"
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", withSecret))
	if err == nil || !strings.Contains(err.Error(), "S3_KEY isn't set") || slices.Contains(p.calls, "provision") {
		t.Fatalf("err = %v, calls %v", err, p.calls)
	}

	t.Setenv("S3_KEY", "s3-secret-value")
	installs := useInstall(t, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", withSecret)); err != nil {
		t.Fatal(err)
	}
	if got := (*installs)[0].Secrets["S3_KEY"]; got != "s3-secret-value" {
		t.Errorf("secret = %q", got)
	}
	if st, _ := state.Load("lab"); strings.Contains(stateText(t, st), "s3-secret-value") {
		t.Error("the secret's value reached the state")
	}
}

func TestK3sOptionsFromTheFile(t *testing.T) {
	cfg := parse(t, "lab", `{kubernetes: {distribution: k3s, networking: {cni: none}, apiEndpoint: 192.168.1.100:6443},
  infrastructure: {provider: byoh, access: {ssh: {user: pi}}},
  controlPlane: {machines: [{host: 192.168.1.10}, {host: 192.168.1.11}, {host: 192.168.1.12}], labels: {node-role.kubernetes.io/infra: ""}},
  workers: [{name: gpu, machines: [{host: 192.168.1.20}], labels: {gpu: "true", node-role.kubernetes.io/gpu: ""}},
            {name: general, machines: [{host: 192.168.1.21}]}]}`)
	o := k3sOptions(cfg)
	if o.KubeVIP != "192.168.1.100" || o.CoreDNSReplicas != 2 || o.WaitReady || !slices.Equal(o.Pools, []string{"gpu", "general"}) {
		t.Errorf("options: %+v", o)
	}
	flags, ok := o.AgentFlags("gpu")
	if !ok || len(flags) != 1 || flags[0].Name != "node-label" || flags[0].Value != "gpu=true" {
		t.Errorf("gpu flags: %v, %v", flags, ok)
	}
	if _, ok := o.AgentFlags("gone"); ok {
		t.Error("an unknown pool must be reported")
	}
	if l := o.RoleLabels(providers.Node{Role: config.RoleWorker, Pool: "gpu"}); len(l) != 1 {
		t.Errorf("gpu role labels: %v", l)
	}
	if l := o.RoleLabels(providers.Node{Role: config.RoleControlPlane}); len(l) != 1 {
		t.Errorf("control-plane role labels: %v", l)
	}
	if l := o.RoleLabels(providers.Node{Role: config.RoleWorker, Pool: "general"}); len(l) != 0 {
		t.Errorf("general role labels: %v", l)
	}
}

func stateText(t *testing.T, st *state.ClusterState) string {
	t.Helper()
	data, err := os.ReadFile(state.Dir(st.Name) + "/state.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
