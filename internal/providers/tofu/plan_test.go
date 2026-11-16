package tofu

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
)

// One change of each kind the guard knows, as 'tofu show -json' writes it.
const (
	taintedMachine = `{"address": "fake_machine.node[\"demo-cp-0\"]", "type": "fake_machine", "index": "demo-cp-0",
		"action_reason": "replace_because_tainted", "change": {"actions": ["delete", "create"]}}`
	newImage = `{"address": "fake_machine.node[\"demo-general-0\"]", "type": "fake_machine", "index": "demo-general-0",
		"action_reason": "replace_because_cannot_update", "change": {"actions": ["create", "delete"], "replace_paths": [["ami"], ["root_block_device", 0, "volume_size"]]}}`
	goneSubnet = `{"address": "aws_subnet.public[\"eu-west-3a\"]", "type": "aws_subnet", "index": "eu-west-3a",
		"action_reason": "delete_because_each_key", "change": {"actions": ["delete"]}}`
	missingMachine = `{"address": "fake_machine.node[\"demo-general-1\"]", "type": "fake_machine", "index": "demo-general-1", "change": {"actions": ["create"]}}`
	newRule        = `{"address": "aws_vpc_security_group_ingress_rule.ssh[\"203.0.113.7/32\"]", "type": "aws_vpc_security_group_ingress_rule", "change": {"actions": ["create"]}}`
	tagsUpdated    = `{"address": "fake_machine.node[\"demo-cp-0\"]", "type": "fake_machine", "index": "demo-cp-0", "change": {"actions": ["update"]}}`
	dataRead       = `{"address": "data.aws_ami.ubuntu[\"amd64\"]", "type": "aws_ami", "change": {"actions": ["read"]}}`
)

func planOf(changes ...string) []byte {
	return []byte(`{"resource_changes": [` + strings.Join(changes, ",") + `]}`)
}

func TestGuard(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name    string
		level   providers.Guard
		changes []string
		want    []string // nil = allowed
	}{
		{"a new cluster: anything goes", providers.GuardOff, []string{taintedMachine, goneSubnet}, nil},
		{"creates and updates", providers.GuardKeep, []string{missingMachine, newRule, tagsUpdated, dataRead}, nil},
		{"a tainted machine", providers.GuardKeep, []string{taintedMachine},
			[]string{"machine demo-cp-0 would be replaced: it is marked for replacement ('tofu untaint' in " + workdir("demo") + " undoes it)"}},
		{"an attribute that forces a replacement", providers.GuardKeep, []string{newImage},
			[]string{"machine demo-general-0 would be replaced: ami, root_block_device.0.volume_size changed, which can't change in place"}},
		{"anything deleted, not only machines", providers.GuardKeep, []string{goneSubnet},
			[]string{`aws_subnet.public["eu-west-3a"] would be deleted: tow no longer asks for it`}},
		{"a machine missing from a running cluster", providers.GuardRunning, []string{missingMachine, newRule},
			[]string{"machine demo-general-1 would be created: it is missing (deleted outside tow?)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := guard(mustParsePlan(t, planOf(tc.changes...)), tc.level, "fake_machine", "demo", nil)
			if tc.want == nil {
				if err != nil {
					t.Errorf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("not refused")
			}
			for _, w := range append(tc.want, "nothing was changed", "run 'tow apply' again") {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("missing %q in:\n%v", w, err)
				}
			}
			if n := strings.Count(err.Error(), "\n  - "); n != len(tc.want) {
				t.Errorf("%d resources listed, want %d:\n%v", n, len(tc.want), err)
			}
		})
	}
}

// providerWithPlan answers 'tofu show' with plan, and the outputs of one machine.
func providerWithPlan(t *testing.T, plan []byte) (*Provider, *[][]string) {
	t.Helper()
	p, _ := fakeProvider(t, &fakeCloud{})
	writePublicKey(t)
	var ran [][]string
	p.tofu = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		ran = append(ran, args)
		switch args[0] {
		case "show":
			return plan, nil
		case "output":
			return []byte(`{"nodes": {"value": [{"name": "demo-cp-0", "role": "control-plane", "public_ip": "198.51.100.1", "ssh_user": "ubuntu"}]}}`), nil
		}
		return nil, nil
	}
	p.waitReady = func(context.Context, []providers.Node, string) error { return nil }
	return p, &ran
}

// OpenTofu applies the plan the guard checked, never a new one of its own.
func TestProvisionAppliesTheCheckedPlan(t *testing.T) {
	p, ran := providerWithPlan(t, planOf(missingMachine))
	if _, err := p.Provision(context.Background(), awsSpec); err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for _, args := range *ran {
		verbs = append(verbs, args[0])
	}
	if !slices.Equal(verbs, []string{"init", "plan", "show", "apply", "output"}) {
		t.Fatalf("tofu calls = %v", verbs)
	}
	if apply := (*ran)[3]; apply[len(apply)-1] != planFile || slices.Contains(apply, "-auto-approve") {
		t.Errorf("apply args = %v, want the saved plan file", apply)
	}
	if _, err := os.Stat(filepath.Join(workdir("demo"), planFile)); !os.IsNotExist(err) {
		t.Error("the plan file must be removed after apply")
	}

	// Machines Kubernetes may run on: the guard stops before anything is applied.
	p, ran = providerWithPlan(t, planOf(taintedMachine))
	spec := awsSpec
	spec.Guard = providers.GuardKeep
	if _, err := p.Provision(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "machine demo-cp-0 would be replaced") {
		t.Errorf("err = %v", err)
	}
	if last := (*ran)[len(*ran)-1]; last[0] != "show" {
		t.Errorf("tofu ran %v after the guard refused", last)
	}
}

// Changes is empty when OpenTofu has nothing to do, so apply can say "up to date".
func TestChanges(t *testing.T) {
	spec := awsSpec
	spec.Guard = providers.GuardRunning
	p, _ := providerWithPlan(t, planOf(dataRead, `{"address": "aws_vpc.this", "type": "aws_vpc", "change": {"actions": ["no-op"]}}`))
	if planned, err := p.Changes(context.Background(), spec); err != nil || planned.Lines != nil || planned.Creating != nil {
		t.Errorf("nothing to change: %v, %v", planned, err)
	}
	p, _ = providerWithPlan(t, planOf(newRule))
	planned, err := p.Changes(context.Background(), spec)
	if err != nil || !slices.Equal(planned.Lines, []string{"Would create 1 resource in aws eu-west-3: aws_vpc_security_group_ingress_rule ×1"}) {
		t.Errorf("lines = %v, %v", planned.Lines, err)
	}
	// A machine of the cluster's that's missing may be created again; its name is said.
	known := spec
	known.Known = []string{"demo-general-1"}
	p, _ = providerWithPlan(t, planOf(missingMachine))
	if planned, err := p.Changes(context.Background(), known); err != nil || !slices.Equal(planned.Creating, []string{"demo-general-1"}) {
		t.Errorf("creating %v, %v", planned.Creating, err)
	}
	p, _ = providerWithPlan(t, planOf(missingMachine))
	if _, err := p.Changes(context.Background(), spec); err == nil {
		t.Error("the guard applies to Changes too")
	}
}

// writeLast saves the module's last inputs, as an earlier apply left them.
func writeLast(t *testing.T, last cloud.Inputs) {
	t.Helper()
	data, _ := json.Marshal(last)
	os.MkdirAll(workdir("demo"), 0700)
	os.WriteFile(filepath.Join(workdir("demo"), varsFile), data, 0600)
}

// Machines keep the place they had, and a running cluster a test run's expiry. This
// machine's address is detected every time: when it moved, the run says so and may replace
// the admin rules.
func TestInputsReuseTheLastOnes(t *testing.T) {
	c := &fakeCloud{}
	p, _ := fakeProvider(t, c)
	writePublicKey(t)
	t.Setenv("TOW_RESOURCE_TTL", "2h")
	writeLast(t, cloud.Inputs{
		Machines:            []cloud.Machine{{Name: "demo-cp-0", Zone: "zone-c"}},
		AdminCIDRs:          []string{"198.51.100.1/32"},
		Tags:                map[string]string{cloud.TagExpires: "123"},
		SelfDestructMinutes: 60,
	})

	spec := awsSpec
	spec.Guard = providers.GuardRunning
	r, err := p.inputs(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	machines := r.vars["machines"].([]cloud.Machine)
	if c.prev == nil || machines[0].Zone != "zone-c" || machines[1].Zone != "zone-a" {
		t.Errorf("machines = %+v", machines)
	}
	if r.vars["tags"].(map[string]string)[cloud.TagExpires] != "123" || r.vars["self_destruct_minutes"] != 60 {
		t.Errorf("vars = %v", r.vars)
	}
	if !slices.Equal(r.cidrs, []string{"203.0.113.7/32"}) || r.moved != "198.51.100.1/32 → 203.0.113.7/32" || !r.firewall {
		t.Errorf("cidrs %v, moved %q, firewall %v", r.cidrs, r.moved, r.firewall)
	}

	// A retried apply gets a new expiry, as before.
	spec.Guard = providers.GuardKeep
	if r, err = p.inputs(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if r.vars["tags"].(map[string]string)[cloud.TagExpires] == "123" {
		t.Errorf("vars = %v", r.vars)
	}
}

// The guard lets a run that changes the admin addresses or the ports replace or delete the
// admin rules, and nothing else; a run that changes neither, not even those.
func TestGuardAllowsTheAdminRulesAFirewallChangeReplaces(t *testing.T) {
	const oldRule = `{"address": "fake_rule.admin[\"198.51.100.1/32\"]", "type": "fake_rule", "index": "198.51.100.1/32",
		"action_reason": "delete_because_each_key", "change": {"actions": ["delete"]}}`
	const nodesRule = `{"address": "fake_rule.nodes", "type": "fake_rule", "action_reason": "replace_because_cannot_update", "change": {"actions": ["delete", "create"]}}`
	moved := run{c: &fakeCloud{}, firewall: true}
	if err := guard(mustParsePlan(t, planOf(oldRule)), providers.GuardRunning, "fake_machine", "demo", moved.allowed); err != nil {
		t.Errorf("the old address's rule: %v", err)
	}
	if err := guard(mustParsePlan(t, planOf(oldRule, nodesRule)), providers.GuardRunning, "fake_machine", "demo", moved.allowed); err == nil || !strings.Contains(err.Error(), "fake_rule.nodes would be replaced") {
		t.Errorf("the rule between nodes is never allowed: %v", err)
	}
	if err := guard(mustParsePlan(t, planOf(oldRule, taintedMachine)), providers.GuardRunning, "fake_machine", "demo", moved.allowed); err == nil {
		t.Error("nor a machine")
	}
	still := run{c: &fakeCloud{}}
	if err := guard(mustParsePlan(t, planOf(oldRule)), providers.GuardRunning, "fake_machine", "demo", still.allowed); err == nil {
		t.Error("a run that changes no address or port may delete no rule")
	}
}

// A run that adds or removes machines may create or destroy exactly those: a removed one with
// what's keyed by its name (its firewall, its load balancer attachments); nothing else.
func TestGuardAllowsThePlannedMachines(t *testing.T) {
	const (
		goneMachine = `{"address": "fake_machine.node[\"demo-general-2\"]", "type": "fake_machine", "index": "demo-general-2", "change": {"actions": ["delete"]}}`
		goneFW      = `{"address": "fake_fw.node[\"demo-general-2\"]", "type": "fake_fw", "index": "demo-general-2", "change": {"actions": ["delete"]}}`
		goneAttach  = `{"address": "fake_attach.api[\"nodes-demo-general-2\"]", "type": "fake_attach", "index": "nodes-demo-general-2", "change": {"actions": ["delete"]}}`
		otherGone   = `{"address": "fake_machine.node[\"demo-general-0\"]", "type": "fake_machine", "index": "demo-general-0", "change": {"actions": ["delete"]}}`
	)
	r := run{c: &fakeCloud{}, adding: []string{"demo-general-1"}, removing: []string{"demo-general-2"}}
	if err := guard(mustParsePlan(t, planOf(missingMachine, goneMachine, goneFW, goneAttach)), providers.GuardRunning, "fake_machine", "demo", r.allowed); err != nil {
		t.Errorf("the planned machines: %v", err)
	}
	err := guard(mustParsePlan(t, planOf(otherGone, taintedMachine)), providers.GuardRunning, "fake_machine", "demo", r.allowed)
	if err == nil || !strings.Contains(err.Error(), "machine demo-general-0 would be deleted") || !strings.Contains(err.Error(), "machine demo-cp-0 would be replaced") {
		t.Errorf("only those: %v", err)
	}
	none := run{c: &fakeCloud{}}
	if err := guard(mustParsePlan(t, planOf(missingMachine)), providers.GuardRunning, "fake_machine", "demo", none.allowed); err == nil {
		t.Error("a machine nobody planned isn't created on a running cluster")
	}

	// A machine of the cluster's made again: what's keyed by its name is replaced with it
	// (Proxmox's firewall takes the new VM's id); another machine's isn't.
	const (
		itsFW   = `{"address": "fake_fw.node[\"demo-general-1\"]", "type": "fake_fw", "index": "demo-general-1", "action_reason": "replace_because_cannot_update", "change": {"actions": ["delete", "create"]}}`
		otherFW = `{"address": "fake_fw.node[\"demo-general-0\"]", "type": "fake_fw", "index": "demo-general-0", "action_reason": "replace_because_cannot_update", "change": {"actions": ["delete", "create"]}}`
	)
	known := run{c: &fakeCloud{}, known: []string{"demo-general-0", "demo-general-1"}}
	if err := guard(mustParsePlan(t, planOf(missingMachine, itsFW)), providers.GuardRunning, "fake_machine", "demo", known.allowed); err != nil {
		t.Errorf("made again with its firewall: %v", err)
	}
	if err := guard(mustParsePlan(t, planOf(missingMachine, otherFW)), providers.GuardRunning, "fake_machine", "demo", known.allowed); err == nil || !strings.Contains(err.Error(), `fake_fw.node["demo-general-0"] would be replaced`) {
		t.Errorf("another machine's firewall: %v", err)
	}
}

// When this machine's address moved, the plan starts with it.
func TestChangesSayTheAddressMoved(t *testing.T) {
	spec := awsSpec
	spec.Guard = providers.GuardRunning
	p, _ := providerWithPlan(t, planOf(newRule))
	writeLast(t, cloud.Inputs{AdminCIDRs: []string{"198.51.100.1/32"}})
	planned, err := p.Changes(context.Background(), spec)
	if lines := planned.Lines; err != nil || len(lines) != 2 || lines[0] != "This machine's address changed since the last apply: 198.51.100.1/32 → 203.0.113.7/32; SSH, the API and NodePorts follow it" {
		t.Errorf("lines = %q, %v", lines, err)
	}
}
