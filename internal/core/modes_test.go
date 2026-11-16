package core

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
)

// parse builds a config from a name and the spec block's YAML.
func parse(t *testing.T, name, spec string) *config.Config {
	t.Helper()
	cfg, err := config.Parse([]byte("apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: " + name + "}\nspec: " + spec + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func wantErr(t *testing.T, err error, parts ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error containing %q", parts)
	}
	for _, p := range parts {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error missing %q:\n%v", p, err)
		}
	}
}

func TestModeSwitchIsRefusedWithReason(t *testing.T) {
	_, fake := useFakes(t)
	orch := NewOrchestrator(false)
	if err := orch.Apply(context.Background(), loadAsK3s(t, "production-aws.yaml")); err != nil {
		t.Fatal(err)
	}

	// The same name, now listing the user's own machines: OpenTofu's servers would be forgotten.
	byoh := loadAsK3s(t, "production.yaml")
	byoh.Metadata.Name = "prod-aws"
	fake.calls = nil
	wantErr(t, orch.Apply(context.Background(), byoh),
		"cluster 'prod-aws' was built from machines OpenTofu created (aws eu-west-3, 6 machines)",
		"this file now describes your own machines (5 machines listed)",
		"can't change mode in place",
		"give this one a new name",
		"tow delete --name prod-aws, then apply again")
	if len(fake.calls) != 0 {
		t.Errorf("a refused switch must not touch the provider, got %v", fake.calls)
	}
}

func TestModeSwitchAfterFailedApply(t *testing.T) {
	_, fake := useFakes(t)
	fake.provisionErr = errors.New("quota exceeded")
	orch := NewOrchestrator(false)
	orch.Apply(context.Background(), loadAsK3s(t, "production-aws.yaml"))

	byoh := loadAsK3s(t, "production.yaml")
	byoh.Metadata.Name = "prod-aws"
	wantErr(t, orch.Apply(context.Background(), byoh),
		"cluster 'prod-aws' failed partway while building it from machines OpenTofu created",
		"tow delete --name prod-aws")
}

func TestLabelsFitTheMode(t *testing.T) {
	lab := load(t, "homelab.yaml")
	aws := load(t, "production-aws.yaml")
	tests := map[string]string{
		applyLabel(config.ModeLocal, "dev", nil):                              "Creating k3d cluster 'dev' on this machine",
		applyLabel(config.ModeProvision, aws.Name(), aws):                     "Creating 6 machines for 'prod-aws' in aws eu-west-3",
		applyLabel(config.ModeBYOH, lab.Name(), lab):                          "Checking your 3 machines for 'homelab'",
		deleteLabel(&state.ClusterState{Name: "dev", Mode: config.ModeLocal}): "Deleting k3d cluster 'dev'",
		deleteLabel(&state.ClusterState{Name: "lab", Mode: config.ModeBYOH}):  "Forgetting cluster 'lab'",
		deleteLabel(&state.ClusterState{Name: "p", Mode: config.ModeProvision, Spec: &providers.Spec{Machines: &providers.MachineRequest{Cloud: "aws", Region: "eu-west-3"}}}): "Destroying the machines of 'p' in aws eu-west-3",
	}
	for got, want := range tests {
		if got != want {
			t.Errorf("label = %q, want %q", got, want)
		}
	}
}

func TestMachineCanOnlyBeInOneCluster(t *testing.T) {
	useFakes(t)
	orch := NewOrchestrator(false)
	// homelab.yaml adopts 192.168.1.10-12 on port 22.
	if err := orch.Apply(context.Background(), load(t, "homelab.yaml")); err != nil {
		t.Fatal(err)
	}
	// A cluster OpenTofu built owns 203.0.113.5.
	if err := state.Save("cloud", state.ClusterState{
		Name: "cloud", Mode: config.ModeProvision, Status: state.StatusReady, ConfigHash: "x",
		Outputs: &providers.Result{Nodes: []providers.Node{{Host: "203.0.113.5", PrivateIP: "10.0.1.5", Port: 22}}},
	}); err != nil {
		t.Fatal(err)
	}

	byoh := func(name, host string, port int) *config.Config {
		return parse(t, name, "{kubernetes: {distribution: k3s}, infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, controlPlane: {machines: [{host: "+host+", ssh: {port: "+strconv.Itoa(port)+"}}]}}")
	}
	tests := []struct {
		name string
		cfg  *config.Config
		want []string // empty = allowed
	}{
		{"same host and port", byoh("lab2", "192.168.1.11", 22),
			[]string{"192.168.1.11:22 already belongs to cluster 'homelab' (one of its machines)", "tow delete --name homelab"}},
		{"byoh on both sides: another port is another machine", byoh("lab2", "192.168.1.11", 2222), nil},
		{"a server OpenTofu created", byoh("lab2", "203.0.113.5", 22),
			[]string{"already belongs to cluster 'cloud' (a server OpenTofu created for it)"}},
		{"a server OpenTofu created is one machine, whatever the port", byoh("lab2", "203.0.113.5", 2222),
			[]string{"203.0.113.5:2222 already belongs to cluster 'cloud'"}},
		{"private IPs aren't compared", byoh("lab2", "10.0.1.5", 22), nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := orch.Apply(context.Background(), tt.cfg)
			if tt.want == nil {
				if err != nil {
					t.Errorf("should be allowed: %v", err)
				}
				state.Remove(tt.cfg.Name())
				return
			}
			wantErr(t, err, tt.want...)
			if state.Exists(tt.cfg.Name()) {
				t.Error("a refused apply must not save state")
			}
		})
	}

	t.Run("the cluster's own machines don't count", func(t *testing.T) {
		if err := orch.Apply(context.Background(), load(t, "homelab.yaml")); err != nil {
			t.Errorf("re-applying homelab: %v", err)
		}
	})
}

func TestMachineSame(t *testing.T) {
	if !(machine{host: "PI.local.", port: 22, byoh: true}).same(machine{host: "pi.local", port: 22, byoh: true}) {
		t.Error("hostnames are case-insensitive and may end in a dot")
	}
	// A machine tow created on one side: the address is one machine.
	if !(machine{host: "h", port: 22, byoh: true}).same(machine{host: "h", port: 2222}) {
		t.Error("ports only tell machines apart when both clusters list their own machines")
	}
}
