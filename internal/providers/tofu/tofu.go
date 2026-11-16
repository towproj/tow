// Package tofu creates machines with OpenTofu (provision mode).
package tofu

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/aws"
	"github.com/towproj/tow/internal/providers/tofu/azure"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
	"github.com/towproj/tow/internal/providers/tofu/gcp"
	"github.com/towproj/tow/internal/providers/tofu/proxmox"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/secretenv"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
	"github.com/towproj/tow/internal/ui"
)

const (
	readyTimeout  = 10 * time.Minute
	leftoverTries = 3
	sshPort       = 22
	// The names OpenTofu itself reads and writes in its working directory.
	varsFile     = "terraform.tfvars.json"
	tfState      = "terraform.tfstate"
	planFile     = "tow.tfplan"
	initLockFile = "tofu-init.lock" // in ~/.tow: one init at a time (lockInit)
)

// clouds must match config.supportedProviders. Each target is its own package (module +
// API calls); only those packages may import a cloud's SDK.
var clouds = map[string]cloud.Cloud{
	"aws":     aws.Cloud{},
	"azure":   azure.Cloud{},
	"gcp":     gcp.Cloud{},
	"proxmox": proxmox.Cloud{},
}

// Provider creates machines with OpenTofu (provision mode). One adapter drives every cloud:
// each cloud package embeds a module with the same inputs and outputs, plus the API calls
// OpenTofu doesn't make (credentials, status, leftovers).
type Provider struct {
	clouds       map[string]cloud.Cloud
	tofu         func(ctx context.Context, dir string, args ...string) ([]byte, error)
	adminCIDR    func(c cloud.Cloud, ctx context.Context, m *providers.MachineRequest) (string, error) // cloud.Cloud.AdminCIDR, swapped in tests
	waitReady    func(ctx context.Context, nodes []providers.Node, knownHosts string) error
	leftoverWait time.Duration
	// installed finds the tofu tow runs: tools.Installed, swapped in tests.
	installed func(ctx context.Context, t tools.Tool) string
}

func New() providers.Provider {
	p := &Provider{
		clouds:       clouds,
		adminCIDR:    cloud.Cloud.AdminCIDR,
		waitReady:    waitReady,
		leftoverWait: 10 * time.Second,
		installed:    tools.Installed,
	}
	p.tofu = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		bin, err := p.bin(ctx)
		if err != nil {
			return nil, err
		}
		return runTofu(ctx, bin, dir, args...)
	}
	return p
}

func (p *Provider) Name() string { return "tofu" }

// Tool is providers.Tooled: OpenTofu, found or installed by the orchestrator.
func (p *Provider) Tool() tools.Tool { return tools.Tofu }

// bin is the tofu to run: a release tow works with, on PATH or in ~/.local/bin. The
// orchestrator finds it, or installs it after a yes, before PreFlight.
func (p *Provider) bin(ctx context.Context) (string, error) {
	if path := p.installed(ctx, tools.Tofu); path != "" {
		return path, nil
	}
	return "", fmt.Errorf("OpenTofu (tofu %s) isn't installed: 'tow apply' offers to install it, or see https://opentofu.org/docs/intro/install/", tools.Tofu.Supports)
}

// workdir holds the module copy, its inputs and OpenTofu's state, next to tow's own
// state and never in the directory tow runs from.
func workdir(name string) string        { return filepath.Join(state.Dir(name), "tofu") }
func knownHostsPath(name string) string { return filepath.Join(state.Dir(name), "known_hosts") }

func (p *Provider) cloudFor(spec providers.Spec) (cloud.Cloud, *providers.MachineRequest, error) {
	m := spec.Machines
	if m == nil {
		return nil, nil, fmt.Errorf("cluster '%s' has no provision block to create machines from", spec.Name)
	}
	c, ok := p.clouds[m.Cloud]
	if !ok {
		return nil, nil, fmt.Errorf("no OpenTofu module for cloud %q", m.Cloud)
	}
	return c, m, nil
}

// PreFlight checks OpenTofu and the cloud credentials. When credentials are missing or
// expired and someone is at the terminal, it waits for them to log in instead of failing,
// unless waiting can't help (a token read from the environment at start).
func (p *Provider) PreFlight(ctx context.Context, spec providers.Spec) error {
	if _, err := p.bin(ctx); err != nil {
		return err
	}
	c, m, err := p.cloudFor(spec)
	if err != nil {
		return err
	}
	for {
		id, err := c.Identity(ctx, m)
		if err == nil {
			ui.Info("Using " + id)
			return nil
		}
		if cloud.IsPermanent(err) || !ui.Retry(err.Error()) {
			return err
		}
	}
}

// run is what one OpenTofu run works with.
type run struct {
	c     cloud.Cloud
	m     *providers.MachineRequest
	vars  map[string]any // the module's inputs
	cidrs []string       // who the firewall lets in: the file's adminCIDRs, or this machine
	prev  *cloud.Inputs  // the module's last inputs; nil for a new cluster
	// moved says this machine's address (the file naming none) differs from the last
	// inputs': "X → Y", for the plan.
	moved string
	// firewall says the admin addresses or the ports differ from the last inputs: the
	// module's rules keyed by them (Cloud.AdminRules) may then be replaced or deleted.
	firewall bool
	// adding and removing are the machines this run creates or destroys on purpose
	// (Spec.Adding, Spec.Removing); known, the cluster's, which may be created again
	// (Spec.Known).
	adding, removing, known []string
}

// allowed is the guard's exceptions: a machine this run adds; a machine this run removes,
// with what's keyed by its name (its firewall, its load balancer attachments: "<lb>-<name>");
// a firewall rule keyed by an admin address or a port, in a run that changes those. Nothing
// else is ever created on a running cluster, or replaced or deleted, unplanned.
func (r run) allowed(rc resourceChange) bool {
	key, _ := rc.Index.(string)
	switch rc.verb() {
	case "create":
		return slices.Contains(r.adding, key) || slices.Contains(r.known, key)
	case "delete":
		for _, name := range r.removing {
			if key == name || strings.HasSuffix(key, "-"+name) {
				return true
			}
		}
	}
	if !r.firewall {
		return false
	}
	for _, prefix := range r.c.AdminRules() {
		if strings.HasPrefix(rc.Address, prefix+"[") {
			return true
		}
	}
	return false
}

// inputs gathers the module's inputs: the shared ones (among them the SSH public key to
// install and the one address allowed to reach SSH and the API), where each machine goes,
// then the cloud's own. Machines OpenTofu already created keep their place: their zone or
// address comes from the module's last inputs.
func (p *Provider) inputs(ctx context.Context, spec providers.Spec) (*run, error) {
	c, m, err := p.cloudFor(spec)
	if err != nil {
		return nil, err
	}
	pub, err := remote.PublicKey(m.SSHKeyPath)
	if err != nil {
		return nil, err
	}
	prev, err := readInputs(workdir(spec.Name))
	if err != nil {
		return nil, err
	}
	r := &run{c: c, m: m, prev: prev, adding: spec.Adding, removing: spec.Removing, known: spec.Known}
	// The file's adminCIDRs win; otherwise this machine's address, as the machines will see
	// it, detected every time: when it changed, the plan says so and the firewall follows.
	r.cidrs = m.AdminCIDRs
	if len(r.cidrs) == 0 {
		cidr, err := p.adminCIDR(c, ctx, m)
		if err != nil {
			return nil, err
		}
		r.cidrs = []string{cidr}
		if prev != nil && len(prev.AdminCIDRs) > 0 && !sameSet(prev.AdminCIDRs, r.cidrs) {
			r.moved = strings.Join(prev.AdminCIDRs, ", ") + " → " + cidr
		}
	}
	if prev != nil {
		r.firewall = !sameSet(prev.AdminCIDRs, r.cidrs) || !sameSet(intStrings(prev.OpenPorts), intStrings(spec.OpenPorts))
	}
	if r.vars, err = moduleVars(spec, pub, r.cidrs, time.Now()); err != nil {
		return nil, err
	}
	// A running cluster keeps a test run's expiry from when it was created.
	if spec.Guard == providers.GuardRunning && prev != nil {
		tags := prev.Tags
		if tags == nil {
			tags = map[string]string{} // never null: the modules merge it
		}
		r.vars["tags"], r.vars["self_destruct_minutes"] = tags, prev.SelfDestructMinutes
	}
	machines := cloud.Machines(spec.Name, m.Pools)
	extra, err := c.Vars(ctx, spec, machines, prev)
	if err != nil {
		return nil, err
	}
	for k, v := range extra {
		r.vars[k] = v
	}
	r.vars["machines"] = machines
	if err := c.BeforeCreate(ctx, spec, machines); err != nil {
		return nil, err
	}
	return r, nil
}

func sameSet(a, b []string) bool {
	x, y := slices.Clone(a), slices.Clone(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(slices.Compact(x), slices.Compact(y))
}

func intStrings(ns []int) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = strconv.Itoa(n)
	}
	return out
}

// readInputs reads the module's last inputs in dir; nil when OpenTofu never ran there.
func readInputs(dir string) (*cloud.Inputs, error) {
	data, err := os.ReadFile(filepath.Join(dir, varsFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var in cloud.Inputs
	if err := json.Unmarshal(data, &in); err != nil {
		return nil, fmt.Errorf("read %s: %w", filepath.Join(dir, varsFile), err)
	}
	return &in, nil
}

// Plan runs 'tofu plan' in a throwaway directory, so a dry run leaves no files behind.
// When the cluster's OpenTofu state exists (a retried apply, a running cluster), it is
// copied in: the plan then shows only what would change, as the real run would. The guard
// refuses the same plans it refuses there.
func (p *Provider) Plan(ctx context.Context, spec providers.Spec) ([]string, error) {
	plan, r, err := p.plan(ctx, spec)
	if err != nil {
		return nil, err
	}
	return summarize(plan, spec, r.cidrs), nil
}

// Changes is providers.Reconciler: what Provision would change on a cluster's existing
// infrastructure, nil when nothing. A new address of this machine comes first.
func (p *Provider) Changes(ctx context.Context, spec providers.Spec) (providers.Planned, error) {
	plan, r, err := p.plan(ctx, spec)
	if err != nil {
		return providers.Planned{}, err
	}
	lines := plan.changeLines(where(spec))
	if r.moved != "" && len(lines) > 0 {
		lines = append([]string{"This machine's address changed since the last apply: " + r.moved + "; SSH, the API and NodePorts follow it"}, lines...)
	}
	return providers.Planned{Lines: lines, Creating: plan.creating(r.c.MachineType())}, nil
}

func (p *Provider) plan(ctx context.Context, spec providers.Spec) (*tfPlan, *run, error) {
	r, err := p.inputs(ctx, spec)
	if err != nil {
		return nil, nil, err
	}
	dir, err := os.MkdirTemp("", "tow-plan-")
	if err != nil {
		return nil, nil, err
	}
	defer os.RemoveAll(dir)

	if err := writeModule(dir, r.c.Module(), r.vars); err != nil {
		return nil, nil, fmt.Errorf("prepare OpenTofu files: %w", err)
	}
	if data, err := os.ReadFile(filepath.Join(workdir(spec.Name), tfState)); err == nil {
		if err := os.WriteFile(filepath.Join(dir, tfState), data, 0600); err != nil {
			return nil, nil, err
		}
	}
	if _, err := p.tofu(ctx, dir, "init", "-input=false", "-no-color"); err != nil {
		return nil, nil, err
	}
	plan, err := p.planIn(ctx, dir)
	if err != nil {
		return nil, nil, err
	}
	if err := guard(plan, spec.Guard, r.c.MachineType(), spec.Name, r.allowed); err != nil {
		return nil, nil, err
	}
	return plan, r, nil
}

// planIn plans in dir, saving the plan to planFile, and reads it back.
func (p *Provider) planIn(ctx context.Context, dir string) (*tfPlan, error) {
	if _, err := p.tofu(ctx, dir, "plan", "-input=false", "-no-color", "-out="+planFile); err != nil {
		return nil, err
	}
	raw, err := p.tofu(ctx, dir, "show", "-json", planFile)
	if err != nil {
		return nil, err
	}
	return parsePlan(raw)
}

func where(spec providers.Spec) string { return spec.Machines.Cloud + " " + spec.Machines.Region }

// summarize turns OpenTofu's plan into a few lines: the machines, what changes, by resource
// type, and what the firewall opens to whom.
func summarize(plan *tfPlan, spec providers.Spec, cidrs []string) []string {
	changes := plan.changeLines(where(spec))
	if len(changes) == 0 {
		return []string{"Nothing to change in " + where(spec) + ": everything already exists"}
	}
	lines := append([]string{"Machines: " + describePools(spec.Machines.Pools)}, changes...)
	open := "SSH, the API (6443) and NodePort Services (30000-32767)"
	if len(spec.OpenPorts) > 0 {
		var ports []string
		for _, p := range spec.OpenPorts {
			ports = append(ports, strconv.Itoa(p))
		}
		open = fmt.Sprintf("SSH, the API (6443), NodePort Services (30000-32767) and TCP %s", strings.Join(ports, ", "))
	}
	if len(spec.ServiceLBCIDRs) > 0 && spec.Machines != nil && spec.Machines.Cloud == "proxmox" {
		open += ", and the service load balancer's addresses " + strings.Join(spec.ServiceLBCIDRs, ", ")
	}
	return append(lines, fmt.Sprintf("%s would be open to %s only", open, strings.Join(cidrs, ", ")))
}

// describePools says what each pool gets: "1 control plane (t3.small) + 3 general (t3.large, 50 GB disk)".
func describePools(pools []providers.Pool) string {
	var parts []string
	for _, p := range pools {
		what := pluralize(p.Count, "control plane")
		if p.Role != "control-plane" {
			what = fmt.Sprintf("%d %s", p.Count, p.Name)
		}
		var size []string
		if p.InstanceType != "" {
			size = append(size, p.InstanceType)
		}
		if p.CPUs > 0 {
			size = append(size, fmt.Sprintf("%d CPUs, %d MB memory", p.CPUs, p.MemoryMB))
		}
		if p.DiskGB > 0 {
			size = append(size, fmt.Sprintf("%d GB disk", p.DiskGB))
		}
		if len(size) == 0 {
			size = append(size, "default size")
		}
		parts = append(parts, fmt.Sprintf("%s (%s)", what, strings.Join(size, ", ")))
	}
	return strings.Join(parts, " + ")
}

func pluralize(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// Provision creates the machines, then waits until each one accepts SSH and has finished
// booting. Re-running after a failure is safe: OpenTofu reuses what already exists.
// OpenTofu applies exactly the plan the guard checked.
func (p *Provider) Provision(ctx context.Context, spec providers.Spec) (*providers.Result, error) {
	r, err := p.inputs(ctx, spec)
	if err != nil {
		return nil, err
	}
	m := r.m

	dir := workdir(spec.Name)
	if err := writeModule(dir, r.c.Module(), r.vars); err != nil {
		return nil, fmt.Errorf("prepare OpenTofu files: %w", err)
	}
	if _, err := p.tofu(ctx, dir, "init", "-input=false", "-no-color"); err != nil {
		return nil, err
	}
	defer os.Remove(filepath.Join(dir, planFile))
	plan, err := p.planIn(ctx, dir)
	if err != nil {
		return nil, provisionFailed(spec.Name, err)
	}
	if err := guard(plan, spec.Guard, r.c.MachineType(), spec.Name, r.allowed); err != nil {
		return nil, err
	}
	if _, err := p.tofu(ctx, dir, "apply", "-input=false", "-no-color", planFile); err != nil {
		return nil, provisionFailed(spec.Name, err)
	}
	raw, err := p.tofu(ctx, dir, "output", "-json", "-no-color")
	if err != nil {
		return nil, provisionFailed(spec.Name, err)
	}
	res, err := parseOutputs(raw, m.SSHKeyPath)
	if err != nil {
		return nil, provisionFailed(spec.Name, err)
	}
	// A machine this run created may have an address another one had: its old host key goes.
	var fresh []string
	for _, n := range res.Nodes {
		if slices.Contains(spec.Adding, n.Name) {
			fresh = append(fresh, n.Host)
		}
	}
	if err := remote.Forget(knownHostsPath(spec.Name), fresh); err != nil {
		return nil, provisionFailed(spec.Name, err)
	}
	if err := p.waitReady(ctx, res.Nodes, knownHostsPath(spec.Name)); err != nil {
		if m.Cloud == config.ProviderProxmox && (m.CPUType == "" || m.CPUType == config.DefaultProxmoxCPUType) {
			// The kernel of RHEL 10 and its rebuilds stops at boot on an older CPU model,
			// before SSH ever starts.
			err = fmt.Errorf("%w\n→ a VM that never answers may not boot its template's system: RHEL 10 and its rebuilds (Rocky, AlmaLinux) need spec.infrastructure.proxmox.cpuType: x86-64-v3 (or host)", err)
		}
		return nil, provisionFailed(spec.Name, err)
	}
	return res, nil
}

// provisionFailed says how to recover: OpenTofu's state remembers what it already created.
func provisionFailed(name string, err error) error {
	hint := "Fix the problem and run 'tow apply' again: machines already created are reused, not duplicated."
	if l := strings.ToLower(err.Error()); strings.Contains(l, "expired") && (strings.Contains(l, "token") || strings.Contains(l, "credential")) {
		hint = "Your cloud credentials expired during provisioning. Log in again and run 'tow apply' again: machines already created are reused."
	}
	return fmt.Errorf("%w\n%s\nTo remove everything instead: tow delete --name %s", err, hint, name)
}

func (p *Provider) Status(ctx context.Context, spec providers.Spec) (providers.StatusResult, error) {
	c, m, err := p.cloudFor(spec)
	if err != nil {
		return providers.StatusResult{}, err
	}
	insts, err := c.Instances(ctx, m, spec.Name)
	if err != nil {
		return providers.StatusResult{State: "error", Message: err.Error()}, nil
	}
	if len(insts) == 0 {
		return providers.StatusResult{State: "not_found"}, nil
	}

	var cps, workers int
	for _, i := range insts {
		if i.State != "running" {
			continue
		}
		switch i.Role {
		case config.RoleControlPlane:
			cps++
		case config.RoleWorker:
			workers++
		}
	}
	res := providers.StatusResult{
		ControlPlanes: fmt.Sprintf("%d/%d", cps, m.ControlPlanes()),
		Workers:       fmt.Sprintf("%d/%d", workers, m.Workers()),
		State:         "degraded",
		Message:       "not every machine is running",
	}
	if cps == m.ControlPlanes() && workers == m.Workers() {
		res.Healthy, res.State, res.Message = true, "ready", ""
	}
	return res, nil
}

// Teardown destroys what OpenTofu created, then asks the cloud for anything still tagged
// with the cluster's name: a resource OpenTofu lost track of would keep billing.
func (p *Provider) Teardown(ctx context.Context, spec providers.Spec) error {
	c, m, err := p.cloudFor(spec)
	if err != nil {
		return err
	}

	// No inputs file means provisioning stopped before OpenTofu ever ran.
	dir := workdir(spec.Name)
	if _, err := os.Stat(filepath.Join(dir, varsFile)); err == nil {
		if _, err := p.tofu(ctx, dir, "init", "-input=false", "-no-color"); err != nil {
			return err
		}
		if _, err := p.tofu(ctx, dir, "destroy", "-auto-approve", "-input=false", "-no-color"); err != nil {
			return err
		}
	}

	// Cloud APIs can lag a few seconds behind a destroy; recheck before calling it a leak.
	var left []string
	for try := 1; ; try++ {
		if left, err = c.Leftovers(ctx, m, spec.Name); err != nil {
			return fmt.Errorf("check for leftover resources: %w", err)
		}
		if len(left) == 0 || try == leftoverTries {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(p.leftoverWait):
		}
	}
	if len(left) > 0 {
		return fmt.Errorf("these resources of cluster %s still exist in %s %s; delete them by hand (cloud console or web UI):\n  - %s",
			spec.Name, m.Cloud, m.Region, strings.Join(left, "\n  - "))
	}
	return nil
}

// moduleVars are the module's shared inputs but the machines: identifiers and a public key,
// never a credential.
func moduleVars(spec providers.Spec, publicKey string, adminCIDRs []string, now time.Time) (map[string]any, error) {
	m := spec.Machines
	tags := map[string]string{}
	selfDestruct := 0
	// For CI and tests only: machines delete themselves after the TTL, and an expiry tag lets
	// a cleanup job find anything else a crashed run left. Real clusters never set this.
	if ttl := os.Getenv("TOW_RESOURCE_TTL"); ttl != "" {
		d, err := time.ParseDuration(ttl)
		if err != nil || d < time.Minute {
			return nil, fmt.Errorf("TOW_RESOURCE_TTL=%q must be a duration of at least a minute, like 1h", ttl)
		}
		tags[cloud.TagExpires] = strconv.FormatInt(now.Add(d).Unix(), 10)
		selfDestruct = int(d.Minutes())
	}
	return map[string]any{
		"self_destruct_minutes": selfDestruct,
		"cluster_name":          spec.Name,
		"region":                m.Region,
		"ssh_user":              m.SSHUser,
		"ssh_public_key":        publicKey,
		"admin_cidrs":           adminCIDRs,
		"open_ports":            append([]int{}, spec.OpenPorts...), // never null: modules iterate over it
		"tags":                  tags,
	}, nil
}

// writeModule lays out the cloud's module and its inputs in dir. Module files are rewritten on
// every apply so fixes ship with tow; OpenTofu's state next to them is left alone.
func writeModule(dir string, module fs.FS, vars map[string]any) error {
	entries, err := fs.ReadDir(module, ".")
	if err != nil {
		return fmt.Errorf("read module: %w", err)
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	stale, _ := filepath.Glob(filepath.Join(dir, "*.tf"))
	for _, f := range stale {
		if err := os.Remove(f); err != nil {
			return err
		}
	}
	for _, e := range entries {
		data, err := fs.ReadFile(module, e.Name())
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0600); err != nil {
			return err
		}
	}

	data, err := json.MarshalIndent(vars, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, varsFile), data, 0600)
}

func parseOutputs(raw []byte, keyPath string) (*providers.Result, error) {
	var out struct {
		Nodes struct {
			Value []struct {
				Name      string `json:"name"`
				Role      string `json:"role"`
				Pool      string `json:"pool"`
				PublicIP  string `json:"public_ip"`
				PrivateIP string `json:"private_ip"`
				SSHUser   string `json:"ssh_user"`
			} `json:"value"`
		} `json:"nodes"`
		APIEndpoint struct {
			Value string `json:"value"`
		} `json:"api_endpoint"`
		APINodeEndpoint struct {
			Value string `json:"value"`
		} `json:"api_node_endpoint"` // optional
		APINodeName struct {
			Value string `json:"value"`
		} `json:"api_node_name"` // optional
		StorageConfig struct {
			Value map[string]string `json:"value"`
		} `json:"storage_config"` // optional
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("read OpenTofu outputs: %w", err)
	}
	if len(out.Nodes.Value) == 0 {
		return nil, errors.New("OpenTofu outputs list no nodes")
	}

	res := &providers.Result{Endpoint: out.APIEndpoint.Value, NodeEndpoint: out.APINodeEndpoint.Value, NodeAPIName: out.APINodeName.Value}
	if len(out.StorageConfig.Value) > 0 {
		res.StorageConfig = out.StorageConfig.Value
	}
	for _, n := range out.Nodes.Value {
		res.Nodes = append(res.Nodes, providers.Node{
			Name:      n.Name,
			Host:      n.PublicIP,
			PrivateIP: n.PrivateIP,
			Role:      n.Role,
			Pool:      n.Pool,
			User:      n.SSHUser,
			KeyPath:   keyPath,
			Port:      sshPort,
		})
	}
	return res, nil
}

// waitReady blocks until every node accepts SSH and cloud-init has finished, so the next
// stage gets machines it can use rather than ones still booting.
func waitReady(ctx context.Context, nodes []providers.Node, knownHosts string) error {
	ctx, cancel := context.WithTimeout(ctx, readyTimeout)
	defer cancel()

	errs := make([]error, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := remote.WaitDial(ctx, remote.Target{Host: n.Host, Port: n.Port, User: n.User, KeyPath: n.KeyPath}, knownHosts)
			if err != nil {
				errs[i] = fmt.Errorf("%s: %w", n.Name, err)
				return
			}
			defer c.Close()
			// Exit code 2 means cloud-init finished with recoverable errors; the machine is usable.
			// Some images have none (GCE's AlmaLinux: Google's guest agent creates the user from
			// the key): SSH answering is then all there is to wait for.
			if out, err := c.Run(ctx, "if command -v cloud-init >/dev/null 2>&1; then cloud-init status --wait; fi"); err != nil && remote.ExitCode(err) != 2 {
				errs[i] = fmt.Errorf("%s: cloud-init did not finish: %w\n%s", n.Name, err, strings.TrimSpace(out))
			}
			ui.Debug("%s (%s) is ready", n.Name, n.Host)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// runTofu runs one command of the tofu at bin in dir. Credentials reach OpenTofu only
// through the inherited environment and the cloud's own config files, never arguments or
// files of ours. The variables holding the cluster file's secrets are left out: OpenTofu never
// needs them. OpenTofu reads Terraform's TF_ variables.
func runTofu(ctx context.Context, bin, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(secretenv.Environ(), "TF_IN_AUTOMATION=1")
	if os.Getenv("TF_PLUGIN_CACHE_DIR") == "" {
		// One provider download shared by every cluster instead of hundreds of MB each. The
		// modules ship their lock file, so init reuses a cached provider instead of writing it
		// again while another apply runs it.
		cache := filepath.Join(state.Root(), "plugin-cache")
		if err := os.MkdirAll(cache, 0700); err == nil {
			cmd.Env = append(cmd.Env, "TF_PLUGIN_CACHE_DIR="+cache)
		}
	}
	if args[0] == "init" {
		// Only one init at a time adds to the cache.
		if err := os.MkdirAll(state.Root(), 0700); err != nil {
			return nil, err
		}
		unlock, err := lockInit(ctx, filepath.Join(state.Root(), initLockFile))
		if err != nil {
			return nil, fmt.Errorf("wait for another tofu init: %w", err)
		}
		defer unlock()
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	// Verbose streams OpenTofu's output live, through redaction. Not for 'show -json': that
	// is a machine-readable dump of the whole plan, read by tow, not meant for people.
	if ui.Verbose() && args[0] != "show" {
		out, errOut := ui.NewRedactingWriter(os.Stdout), ui.NewRedactingWriter(os.Stderr)
		defer out.Flush()
		defer errOut.Flush()
		cmd.Stdout = io.MultiWriter(&stdout, out)
		cmd.Stderr = io.MultiWriter(&stderr, errOut)
	}

	ui.Debug("exec: %s %s (in %s)", bin, strings.Join(args, " "), dir)
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tofu %s failed: %w\n%s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
