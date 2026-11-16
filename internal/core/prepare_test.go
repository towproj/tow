package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
)

// nodesProvider is a fake provider whose machines exist, so they get prepared.
type nodesProvider struct{ fakeProvider }

func (n *nodesProvider) Provision(ctx context.Context, s providers.Spec) (*providers.Result, error) {
	n.calls, n.spec = append(n.calls, "provision"), s
	if n.provisionErr != nil {
		return nil, n.provisionErr
	}
	return &providers.Result{Nodes: []providers.Node{{Host: "192.168.1.10", PrivateIP: "192.168.1.10", Role: "control-plane", User: "pi"}}}, nil
}

const k3sKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: default, cluster: {server: "https://192.168.1.10:6443"}}]
contexts: [{name: default, context: {cluster: default, user: default}}]
current-context: default
users: [{name: default, user: {token: abc}}]
`

// useInstall swaps in a fake k3s install that records what it was asked and fails with fail.
func useInstall(t *testing.T, fail error) *[]k3s.Options {
	t.Helper()
	var got []k3s.Options
	orig := installK3s
	installK3s = func(_ context.Context, o k3s.Options) ([]byte, error) {
		got = append(got, o)
		if fail != nil {
			return nil, fail
		}
		return []byte(k3sKubeconfig), nil
	}
	t.Cleanup(func() { installK3s = orig })
	return &got
}

// usePrepare swaps in a fake machine preparation that records what it was asked.
func usePrepare(t *testing.T, fail error) (*nodesProvider, *[]prepare.Options) {
	t.Helper()
	useFakes(t)
	p := &nodesProvider{}
	newProvider = func(name string) (providers.Provider, error) { p.name = name; return p, nil }
	var got []prepare.Options
	orig := prepareMachines
	prepareMachines = func(_ context.Context, o prepare.Options) error {
		got = append(got, o)
		return fail
	}
	t.Cleanup(func() { prepareMachines = orig })
	return p, &got
}

const homelab = "{flavor: homelab, infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, controlPlane: {machines: [{host: 192.168.1.10}]}}"

func TestMachinesArePrepared(t *testing.T) {
	_, got := usePrepare(t, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", homelab)); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 1 {
		t.Fatalf("prepare calls = %d", len(*got))
	}
	o := (*got)[0]
	if o.Cluster != "lab" || len(o.Nodes) != 1 || o.Nodes[0].PrivateIP != "192.168.1.10" || o.PodCIDR != "10.42.0.0/16" || !strings.HasSuffix(o.KnownHosts, "/clusters/lab/known_hosts") {
		t.Errorf("options: %+v", o)
	}
	if o.Confirm == nil {
		t.Error("your own machines change only after a yes")
	}
	// homelab is k3s, which goes on the machines next.
	st, err := state.Load("lab")
	if err != nil || st.Status != state.StatusReady || st.Stage != state.StageKubernetes {
		t.Errorf("state: %+v, %v", st, err)
	}

	// --yes: no question.
	_, got = usePrepare(t, nil)
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), parse(t, "lab", homelab)); err != nil {
		t.Fatal(err)
	}
	if (*got)[0].Confirm != nil {
		t.Error("--yes means no question")
	}

	// Machines tow created: no question either.
	_, got = usePrepare(t, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), load(t, "dev-test-aws.yaml")); err != nil {
		t.Fatal(err)
	}
	if (*got)[0].Confirm != nil {
		t.Error("tow's own machines are prepared without asking")
	}

	// Local clusters: k3d brings everything, nothing to prepare.
	_, got = usePrepare(t, nil)
	if err := NewOrchestrator(false).Apply(context.Background(), load(t, "local.yaml")); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Error("k3d clusters have no machines to prepare")
	}
}

// A failed preparation keeps the machines; the next apply picks up without recreating them.
func TestPrepareResumes(t *testing.T) {
	p, got := usePrepare(t, errors.New("apt-get: no space left"))
	err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", homelab))
	if err == nil || !strings.Contains(err.Error(), "Done and kept: the machines. Run it again to pick up from here") {
		t.Fatalf("err = %v", err)
	}
	st, _ := state.Load("lab")
	if st == nil || st.Status != state.StatusFailed || st.Stage != state.StageMachines {
		t.Fatalf("state after a failed preparation: %+v", st)
	}

	orig := prepareMachines
	prepareMachines = func(_ context.Context, o prepare.Options) error { *got = append(*got, o); return nil }
	t.Cleanup(func() { prepareMachines = orig })
	p.calls = nil
	if err := NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", homelab)); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(p.calls, "provision") {
		t.Errorf("the machines must be reused, not provisioned again: %v", p.calls)
	}
	if st, _ := state.Load("lab"); st.Status != state.StatusReady || st.Stage != state.StageKubernetes {
		t.Errorf("state after resuming: %+v", st)
	}
}

// When the file now asks for other machines, the unfinished ones aren't reused.
func TestChangedMachinesAreNotReused(t *testing.T) {
	p, _ := usePrepare(t, errors.New("boom"))
	NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", homelab))
	if st, _ := state.Load("lab"); st == nil || st.Stage != state.StageMachines {
		t.Fatalf("setup: %+v", st)
	}
	p.calls = nil
	other := "{flavor: homelab, infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, controlPlane: {machines: [{host: 192.168.1.11}]}}"
	NewOrchestrator(false).Apply(context.Background(), parse(t, "lab", other))
	if !slices.Contains(p.calls, "provision") {
		t.Errorf("changed machines must be provisioned (checked) again: %v", p.calls)
	}
}

func TestDryRunDescribesPreparation(t *testing.T) {
	_, got := usePrepare(t, nil)
	if err := NewOrchestrator(true).Apply(context.Background(), parse(t, "lab", homelab)); err != nil {
		t.Fatal(err)
	}
	if len(*got) != 0 {
		t.Error("a dry run prepares nothing")
	}
	if _, err := state.Load("lab"); err == nil {
		t.Error("a dry run saves no state")
	}
}
