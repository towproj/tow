package tofu

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
	"github.com/towproj/tow/internal/tools"
	"golang.org/x/crypto/ssh"
)

// Every cloud module must expose the shared contract tow reads.
func TestModulesHonorContract(t *testing.T) {
	for name, c := range clouds {
		t.Run(name, func(t *testing.T) {
			vars, err := fs.ReadFile(c.Module(), "variables.tf")
			if err != nil {
				t.Fatalf("module not embedded: %v", err)
			}
			outs, _ := fs.ReadFile(c.Module(), "outputs.tf")
			for _, v := range []string{"cluster_name", "region", "machines", "ssh_user", "ssh_public_key", "admin_cidrs", "tags", "self_destruct_minutes"} {
				if !strings.Contains(string(vars), `variable "`+v+`"`) {
					t.Errorf("missing input %s", v)
				}
			}
			for _, o := range []string{"nodes", "api_endpoint"} {
				if !strings.Contains(string(outs), `output "`+o+`"`) {
					t.Errorf("missing output %s", o)
				}
			}
		})
	}
}

// AWS refuses a security group whose description has other characters than these (an
// apostrophe broke the first HA run); the cluster's name, a DNS label, always fits.
func TestAWSDescriptionsAreAccepted(t *testing.T) {
	main, err := fs.ReadFile(clouds["aws"].Module(), "main.tf")
	if err != nil {
		t.Fatal(err)
	}
	allowed := regexp.MustCompile(`^[a-zA-Z0-9 ._\-:/()#,@\[\]+=&;{}!$*]*$`)
	found := regexp.MustCompile(`(?m)^\s*description\s*=\s*"(.*)"\s*$`).FindAllStringSubmatch(string(main), -1)
	if len(found) == 0 {
		t.Fatal("no description found: the pattern is out of date")
	}
	for _, m := range found {
		if text := regexp.MustCompile(`\$\{[^}]*\}`).ReplaceAllString(m[1], "x"); !allowed.MatchString(text) {
			t.Errorf("description %q has a character AWS refuses", m[1])
		}
	}
}

var awsSpec = providers.Spec{Name: "demo", Machines: &providers.MachineRequest{
	Cloud: "aws", Region: "eu-west-3", SSHUser: "ubuntu", Pools: []providers.Pool{
		{Name: "control-plane", Role: "control-plane", Count: 1},
		{Name: "general", Role: "worker", Count: 2, InstanceType: "t3.large", DiskGB: 50},
	},
}}

func TestWriteModule(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tofu")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "removed-in-new-version.tf"), nil, 0600)
	os.WriteFile(filepath.Join(dir, "terraform.tfstate"), []byte("{}"), 0600)

	vars, err := moduleVars(awsSpec, "ssh-ed25519 AAAA test", []string{"203.0.113.7/32"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	vars["machines"] = cloud.Machines("demo", awsSpec.Machines.Pools)
	if err := writeModule(dir, clouds["aws"].Module(), vars); err != nil {
		t.Fatal(err)
	}

	for _, f := range []string{"main.tf", "variables.tf", "outputs.tf", "versions.tf", "cloud-init.yaml.tftpl", ".terraform.lock.hcl", varsFile, "terraform.tfstate"} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		if perm := info.Mode().Perm(); perm != 0600 {
			t.Errorf("%s mode = %o, want 600", f, perm)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "removed-in-new-version.tf")); !os.IsNotExist(err) {
		t.Error("stale module files must be removed")
	}
	// Each module ships its lock file, with checksums for every platform tow runs on, so
	// init reuses the shared provider cache instead of writing it again. Its providers come
	// from OpenTofu's registry, which gives the checksums of every platform it publishes.
	for name, c := range clouds {
		lock, err := fs.ReadFile(c.Module(), ".terraform.lock.hcl")
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if n := strings.Count(string(lock), `"h1:`); n < 4 {
			t.Errorf("%s lock file: %d h1 checksums, want at least 4 (linux and darwin, amd64 and arm64)", name, n)
		}
		locked := regexp.MustCompile(`(?m)^provider "([^"]+)"`).FindAllStringSubmatch(string(lock), -1)
		if len(locked) == 0 {
			t.Errorf("%s lock file names no provider", name)
		}
		for _, p := range locked {
			if !strings.HasPrefix(p[1], "registry.opentofu.org/") {
				t.Errorf("%s lock file: %s isn't from registry.opentofu.org", name, p[1])
			}
		}
	}

	var got map[string]any
	data, _ := os.ReadFile(filepath.Join(dir, varsFile))
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if got["cluster_name"] != "demo" || got["admin_cidrs"].([]any)[0] != "203.0.113.7/32" {
		t.Errorf("inputs = %v", got)
	}
	// Every machine reaches the module with every field the modules' object type has, empty
	// meaning the module's default or unused: OpenTofu refuses an object missing one.
	machines := got["machines"].([]any)
	if len(machines) != 3 {
		t.Fatalf("machines = %v", machines)
	}
	for _, m := range machines {
		if len(m.(map[string]any)) != 9 {
			t.Errorf("machine %v: want all 9 fields", m)
		}
	}
	worker := machines[2].(map[string]any)
	if worker["name"] != "demo-general-1" || worker["pool"] != "general" || worker["instance_type"] != "t3.large" ||
		worker["disk_gb"] != 50.0 || worker["cpus"] != 0.0 || worker["zone"] != "" {
		t.Errorf("machine = %v", worker)
	}
}

// Machines keep the names they always had; control planes come first whatever the pools'
// order, since the first one is the one the others join.
func TestMachines(t *testing.T) {
	got := cloud.Machines("demo", []providers.Pool{
		{Name: "gpu", Role: "worker", Count: 1},
		{Name: "control-plane", Role: "control-plane", Count: 2},
		{Name: "general", Role: "worker", Count: 2},
	})
	var names []string
	for _, m := range got {
		names = append(names, m.Name)
	}
	if want := []string{"demo-cp-0", "demo-cp-1", "demo-gpu-0", "demo-general-0", "demo-general-1"}; !slices.Equal(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if got[0].Pool != "control-plane" || got[0].Role != "control-plane" || got[2].Pool != "gpu" {
		t.Errorf("machines = %+v", got)
	}
}

func TestModuleVarsExpiryTag(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	t.Setenv("TOW_RESOURCE_TTL", "2h")
	vars, err := moduleVars(awsSpec, "k", []string{"c"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got := vars["tags"].(map[string]string)[cloud.TagExpires]; got != "1007200" {
		t.Errorf("expiry tag = %q, want now+2h in unix seconds", got)
	}
	if got := vars["self_destruct_minutes"]; got != 120 {
		t.Errorf("self_destruct_minutes = %v, want 120", got)
	}

	t.Setenv("TOW_RESOURCE_TTL", "")
	if vars, _ := moduleVars(awsSpec, "k", []string{"c"}, now); vars["self_destruct_minutes"] != 0 {
		t.Error("real clusters must never self-destruct")
	}

	t.Setenv("TOW_RESOURCE_TTL", "soon")
	if _, err := moduleVars(awsSpec, "k", []string{"c"}, now); err == nil {
		t.Error("a bad TTL must fail before anything is created")
	}
}

func TestParseOutputs(t *testing.T) {
	raw := []byte(`{
	  "api_endpoint": {"value": "198.51.100.1:6443"},
	  "nodes": {"value": [
	    {"name": "demo-cp-0", "role": "control-plane", "public_ip": "198.51.100.1", "private_ip": "10.250.0.10", "ssh_user": "ubuntu"},
	    {"name": "demo-worker-0", "role": "worker", "public_ip": "198.51.100.2", "private_ip": "10.250.1.10", "ssh_user": "ubuntu"}
	  ]}
	}`)
	res, err := parseOutputs(raw, "~/.ssh/k")
	if err != nil {
		t.Fatal(err)
	}
	want := providers.Node{Name: "demo-worker-0", Host: "198.51.100.2", PrivateIP: "10.250.1.10", Role: "worker", User: "ubuntu", KeyPath: "~/.ssh/k", Port: 22}
	if res.Endpoint != "198.51.100.1:6443" || len(res.Nodes) != 2 || res.Nodes[1] != want {
		t.Errorf("result = %+v", res)
	}

	if res.NodeEndpoint != "" {
		t.Errorf("no api_node_endpoint output: the nodes use api_endpoint, got %q", res.NodeEndpoint)
	}

	// An HA cluster on AWS: the nodes have their own (internal) load balancer.
	ha := strings.Replace(string(raw), `"api_endpoint": {"value": "198.51.100.1:6443"},`,
		`"api_endpoint": {"value": "demo-api-1.elb.eu-west-3.amazonaws.com:6443"}, "api_node_endpoint": {"value": "internal-demo-nodes-2.elb.eu-west-3.amazonaws.com:6443"},`, 1)
	if res, err := parseOutputs([]byte(ha), "~/.ssh/k"); err != nil || res.Endpoint != "demo-api-1.elb.eu-west-3.amazonaws.com:6443" ||
		res.NodeEndpoint != "internal-demo-nodes-2.elb.eu-west-3.amazonaws.com:6443" {
		t.Errorf("ha result = %+v, %v", res, err)
	}

	if _, err := parseOutputs([]byte(`{"nodes": {"value": []}}`), ""); err == nil {
		t.Error("no nodes must be an error")
	}
}

type fakeCloud struct {
	leftovers    [][]string // one answer per call; the last repeats
	calls        int
	instances    []cloud.Instance
	vars         map[string]any
	beforeCreate error
	prev         *cloud.Inputs // what Vars was given
}

func (f *fakeCloud) Module() fs.FS {
	return fstest.MapFS{"main.tf": {Data: []byte("# fake")}}
}
func (f *fakeCloud) MachineType() string  { return "fake_machine" }
func (f *fakeCloud) AdminRules() []string { return []string{"fake_rule.admin"} }

// Vars puts every machine in zone-a, or where it was.
func (f *fakeCloud) Vars(_ context.Context, _ providers.Spec, machines []cloud.Machine, prev *cloud.Inputs) (map[string]any, error) {
	f.prev = prev
	for i, m := range machines {
		machines[i].Zone = "zone-a"
		if p, ok := prev.Placed()[m.Name]; ok {
			machines[i].Zone = p.Zone
		}
	}
	return f.vars, nil
}
func (f *fakeCloud) BeforeCreate(context.Context, providers.Spec, []cloud.Machine) error {
	return f.beforeCreate
}
func (f *fakeCloud) AdminCIDR(context.Context, *providers.MachineRequest) (string, error) {
	return "203.0.113.7/32", nil
}
func (f *fakeCloud) Identity(context.Context, *providers.MachineRequest) (string, error) {
	return "test account", nil
}
func (f *fakeCloud) Instances(context.Context, *providers.MachineRequest, string) ([]cloud.Instance, error) {
	return f.instances, nil
}
func (f *fakeCloud) Leftovers(context.Context, *providers.MachineRequest, string) ([]string, error) {
	i := min(f.calls, len(f.leftovers)-1)
	f.calls++
	return f.leftovers[i], nil
}

func fakeProvider(t *testing.T, c *fakeCloud) (*Provider, *[]string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	var ran []string
	return &Provider{
		clouds:    map[string]cloud.Cloud{"aws": c},
		adminCIDR: cloud.Cloud.AdminCIDR,
		tofu: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			ran = append(ran, args[0])
			return nil, nil
		},
		installed: func(context.Context, tools.Tool) string { return "/usr/local/bin/tofu" },
	}, &ran
}

// PreFlight stops before any cloud call when no tofu tow works with is installed, and says
// how to get one; the real runner never starts another program in its place.
func TestPreFlightNeedsTofu(t *testing.T) {
	p, _ := fakeProvider(t, &fakeCloud{})
	if err := p.PreFlight(context.Background(), awsSpec); err != nil {
		t.Errorf("tofu installed: %v", err)
	}
	p.installed = func(_ context.Context, tl tools.Tool) string {
		if tl.Name != "tofu" {
			t.Errorf("looked for %s", tl.Name)
		}
		return ""
	}
	err := p.PreFlight(context.Background(), awsSpec)
	if err == nil || !strings.Contains(err.Error(), "OpenTofu (tofu 1.12 or a newer 1.x) isn't installed") || !strings.Contains(err.Error(), "tow apply' offers to install it") {
		t.Errorf("err = %v", err)
	}

	runner := New().(*Provider)
	runner.installed = p.installed
	if _, err := runner.tofu(context.Background(), t.TempDir(), "init"); err == nil || !strings.Contains(err.Error(), "isn't installed") {
		t.Errorf("runner without tofu: %v", err)
	}
}

func TestTeardownDestroysThenChecksLeftovers(t *testing.T) {
	c := &fakeCloud{leftovers: [][]string{{"vpc vpc-123"}, nil}}
	p, ran := fakeProvider(t, c)
	os.MkdirAll(workdir("demo"), 0700)
	os.WriteFile(filepath.Join(workdir("demo"), varsFile), []byte("{}"), 0600)

	if err := p.Teardown(context.Background(), awsSpec); err != nil {
		t.Fatalf("a leftover that disappears on recheck is not a leak: %v", err)
	}
	if !slices.Equal(*ran, []string{"init", "destroy"}) {
		t.Errorf("tofu calls = %v", *ran)
	}
}

func TestTeardownReportsLeftovers(t *testing.T) {
	c := &fakeCloud{leftovers: [][]string{{"instance i-abc", "vpc vpc-123"}}}
	p, ran := fakeProvider(t, c)

	err := p.Teardown(context.Background(), awsSpec)
	if err == nil || !strings.Contains(err.Error(), "instance i-abc") || !strings.Contains(err.Error(), "cluster demo still exist in aws eu-west-3") {
		t.Errorf("err = %v, want the leftover resources listed", err)
	}
	if len(*ran) != 0 {
		t.Errorf("no workdir means OpenTofu never ran, so nothing to destroy; got %v", *ran)
	}
	if c.calls != leftoverTries {
		t.Errorf("leftover checks = %d, want %d", c.calls, leftoverTries)
	}
}

func TestStatusCountsRunningMachines(t *testing.T) {
	c := &fakeCloud{instances: []cloud.Instance{
		{ID: "i-1", Role: "control-plane", State: "running"},
		{ID: "i-2", Role: "worker", State: "stopped"},
	}}
	p, _ := fakeProvider(t, c)
	res, err := p.Status(context.Background(), awsSpec)
	if err != nil {
		t.Fatal(err)
	}
	if res.Healthy || res.ControlPlanes != "1/1" || res.Workers != "0/2" {
		t.Errorf("status = %+v, want unhealthy with the stopped worker missing", res)
	}

	c.instances = nil
	if res, _ := p.Status(context.Background(), awsSpec); res.State != "not_found" {
		t.Errorf("no machines: state = %s, want not_found", res.State)
	}
}

func TestSummarizePlan(t *testing.T) {
	raw := []byte(`{"resource_changes": [
		{"type": "aws_instance", "change": {"actions": ["create"]}},
		{"type": "aws_instance", "change": {"actions": ["create"]}},
		{"type": "aws_vpc", "change": {"actions": ["create"]}},
		{"type": "aws_security_group", "change": {"actions": ["delete", "create"]}},
		{"type": "aws_ami", "change": {"actions": ["read"]}},
		{"type": "aws_subnet", "change": {"actions": ["no-op"]}}
	]}`)
	lines := summarize(mustParsePlan(t, raw), awsSpec, []string{"203.0.113.7/32"})
	want := []string{
		"Machines: 1 control plane (default size) + 2 general (t3.large, 50 GB disk)",
		"Would create 3 resources in aws eu-west-3: aws_instance ×2, aws_vpc ×1",
		"Would replace 1 resource in aws eu-west-3: aws_security_group ×1",
		"SSH, the API (6443) and NodePort Services (30000-32767) would be open to 203.0.113.7/32 only",
	}
	if !slices.Equal(lines, want) {
		t.Errorf("got:\n%s\nwant:\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	none := summarize(mustParsePlan(t, []byte(`{"resource_changes": [{"type": "aws_vpc", "change": {"actions": ["no-op"]}}]}`)), awsSpec, []string{"x"})
	if len(none) != 1 || !strings.HasPrefix(none[0], "Nothing to change") {
		t.Errorf("nothing to do: %v", none)
	}
}

func mustParsePlan(t *testing.T, raw []byte) *tfPlan {
	t.Helper()
	plan, err := parsePlan(raw)
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

// A dry run plans in a throwaway directory: nothing is left under ~/.tow, and a failed
// cluster's OpenTofu state is used so the plan shows only what is missing.
func TestPlanLeavesNoFiles(t *testing.T) {
	p, ran := fakeProvider(t, &fakeCloud{})
	writePublicKey(t)

	var planDir string
	sawState := false
	p.tofu = func(_ context.Context, dir string, args ...string) ([]byte, error) {
		*ran = append(*ran, args[0])
		planDir = dir
		if _, err := os.Stat(filepath.Join(dir, tfState)); err == nil {
			sawState = true
		}
		if args[0] == "show" {
			return []byte(`{"resource_changes": [{"type": "aws_instance", "change": {"actions": ["create"]}}]}`), nil
		}
		return nil, nil
	}

	lines, err := p.Plan(context.Background(), awsSpec)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(*ran, []string{"init", "plan", "show"}) || len(lines) == 0 {
		t.Errorf("calls %v, lines %v", *ran, lines)
	}
	if _, err := os.Stat(planDir); !os.IsNotExist(err) {
		t.Errorf("plan directory %s should be removed", planDir)
	}
	if _, err := os.Stat(workdir(awsSpec.Name)); !os.IsNotExist(err) {
		t.Error("a dry run must not create the cluster's tofu directory")
	}
	if sawState {
		t.Error("a new cluster has no OpenTofu state to copy")
	}

	// Retrying a failed cluster: its state is copied into the plan directory, never modified.
	os.MkdirAll(workdir(awsSpec.Name), 0700)
	os.WriteFile(filepath.Join(workdir(awsSpec.Name), tfState), []byte(`{"version": 4}`), 0600)
	if _, err := p.Plan(context.Background(), awsSpec); err != nil {
		t.Fatal(err)
	}
	if !sawState {
		t.Error("the failed cluster's OpenTofu state should be planned against")
	}
	if entries, _ := os.ReadDir(workdir(awsSpec.Name)); len(entries) != 1 {
		t.Errorf("the cluster's tofu directory must be left as it was, got %v", entries)
	}
}

func writePublicKey(t *testing.T) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(os.Getenv("HOME"), ".ssh")
	os.MkdirAll(dir, 0700)
	os.WriteFile(filepath.Join(dir, "id_ed25519.pub"), ssh.MarshalAuthorizedKey(k), 0644)
}

// The cloud's own inputs join the shared ones; the shared ones can't be lost on the way.
func TestInputsMergeCloudVars(t *testing.T) {
	p, _ := fakeProvider(t, &fakeCloud{vars: map[string]any{"template_id": 9000}})
	writePublicKey(t)
	r, err := p.inputs(context.Background(), awsSpec)
	if err != nil {
		t.Fatal(err)
	}
	if r.vars["template_id"] != 9000 || r.vars["cluster_name"] != "demo" || !slices.Equal(r.cidrs, []string{"203.0.113.7/32"}) {
		t.Errorf("inputs = %v, cidrs %v", r.vars, r.cidrs)
	}
}

func TestSummarizePlanSize(t *testing.T) {
	m := &providers.MachineRequest{Cloud: "proxmox", Region: "pve", Pools: []providers.Pool{
		{Name: "control-plane", Role: "control-plane", Count: 1, CPUs: 2, MemoryMB: 2048, DiskGB: 20},
		{Name: "big", Role: "worker", Count: 3, CPUs: 4, MemoryMB: 8192, DiskGB: 60},
	}}
	lines := summarize(mustParsePlan(t, []byte(`{"resource_changes": [{"type": "proxmox_virtual_environment_vm", "change": {"actions": ["create"]}}]}`)),
		providers.Spec{Machines: m, OpenPorts: []int{80, 443}, ServiceLBCIDRs: []string{"192.168.100.200/29"}}, []string{"192.168.100.1/32"})
	if want := "Machines: 1 control plane (2 CPUs, 2048 MB memory, 20 GB disk) + 3 big (4 CPUs, 8192 MB memory, 60 GB disk)"; lines[0] != want {
		t.Errorf("size line = %q, want %q", lines[0], want)
	}
	if want := "SSH, the API (6443), NodePort Services (30000-32767) and TCP 80, 443, and the service load balancer's addresses 192.168.100.200/29 would be open to 192.168.100.1/32 only"; lines[len(lines)-1] != want {
		t.Errorf("firewall line = %q", lines[len(lines)-1])
	}
}

// adminCIDRs from the file replace this machine's detected address.
func TestAdminCIDRsFromFile(t *testing.T) {
	p, _ := fakeProvider(t, &fakeCloud{})
	writePublicKey(t)
	spec := awsSpec
	m := *awsSpec.Machines
	m.AdminCIDRs = []string{"198.51.100.0/24", "203.0.113.9/32"}
	spec.Machines = &m
	p.adminCIDR = func(cloud.Cloud, context.Context, *providers.MachineRequest) (string, error) {
		t.Error("detection must not run when the file sets adminCIDRs")
		return "", nil
	}
	r, err := p.inputs(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(r.cidrs, m.AdminCIDRs) || !slices.Equal(r.vars["admin_cidrs"].([]string), m.AdminCIDRs) {
		t.Errorf("cidrs = %v, vars %v", r.cidrs, r.vars["admin_cidrs"])
	}
}

// Cloud SDKs stay inside their target's package: the rest of tow is cloud-agnostic.
func TestOnlyTargetPackagesImportCloudSDKs(t *testing.T) {
	sdks := map[string]string{ // import prefix → the only directory allowed to use it
		"github.com/aws/":            "internal/providers/tofu/aws",
		"cloud.google.com/":          "internal/providers/tofu/gcp",
		"golang.org/x/oauth2/google": "internal/providers/tofu/gcp",
		"github.com/Azure/":          "internal/providers/tofu/azure",
	}
	root := "../../.."
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || d.Name() == "vendor") {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, filepath.Dir(path))
		for _, imp := range f.Imports {
			pkg := strings.Trim(imp.Path.Value, `"`)
			for prefix, allowed := range sdks {
				if strings.HasPrefix(pkg, prefix) && filepath.ToSlash(rel) != allowed {
					t.Errorf("%s imports %s; only %s may", path, pkg, allowed)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A target's own check stops apply and its dry run before OpenTofu runs.
func TestBeforeCreateStopsApply(t *testing.T) {
	p, ran := fakeProvider(t, &fakeCloud{beforeCreate: errors.New("192.168.100.20 is taken")})
	writePublicKey(t)
	if _, err := p.Plan(context.Background(), awsSpec); err == nil || !strings.Contains(err.Error(), "is taken") {
		t.Errorf("plan err = %v", err)
	}
	if _, err := p.Provision(context.Background(), awsSpec); err == nil {
		t.Error("provision must stop too")
	}
	if len(*ran) != 0 {
		t.Errorf("tofu ran: %v", *ran)
	}
}
