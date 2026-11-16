// Package gcp is the GCP provision target: its OpenTofu module plus the Compute Engine API
// calls OpenTofu doesn't make. It is the only package that talks to Google's APIs.
package gcp

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
)

// Hidden files (.terraform from local runs) are excluded by go:embed, except the lock file,
// named: it pins the provider release tow is tested with, and lets 'tofu init' reuse the
// shared provider cache without writing it again.
//
//go:embed module module/.terraform.lock.hcl
var module embed.FS

// Cloud uses Google's Application Default Credentials, as OpenTofu's google provider does, so
// whatever works for one works for the other.
type Cloud struct {
	// newAPI is the client for a project: Google's (nil), or a test's.
	newAPI func(ctx context.Context, project string) (*api, error)
}

func (c Cloud) api(ctx context.Context, project string) (*api, error) {
	if c.newAPI != nil {
		return c.newAPI(ctx, project)
	}
	return newAPI(ctx, project)
}

func (Cloud) Module() fs.FS {
	sub, err := fs.Sub(module, "module")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

// DefaultMachineType sizes pools that name no machine type: 2 vCPUs and 4 GB, enough for a
// kubeadm control plane on AlmaLinux too.
const DefaultMachineType = "e2-medium"

func machineType(t string) string {
	if t != "" {
		return t
	}
	return DefaultMachineType
}

func (Cloud) MachineType() string { return "google_compute_instance" }

// AdminRules: none. The firewall rule for the admin addresses takes them all at once, and
// changes in place.
func (Cloud) AdminRules() []string { return nil }

// typeInfo is a machine type as Compute Engine describes it.
type typeInfo struct {
	Name         string `json:"name"`
	GuestCPUs    int    `json:"guestCpus"`
	MemoryMB     int    `json:"memoryMb"`
	Architecture string `json:"architecture"` // X86_64, ARM64; empty for older types
}

// arch is the type's CPU architecture as the module names it: amd64 or arm64.
func (t typeInfo) arch() string {
	switch {
	case t.Architecture == "ARM64":
		return "arm64"
	case t.Architecture == "" && (strings.HasPrefix(t.Name, "t2a-") || strings.HasPrefix(t.Name, "c4a-")):
		return "arm64"
	}
	return "amd64"
}

// machineTypes asks every zone of the region for each machine type: the zones that offer it,
// and what it is.
func (a *api) machineTypes(ctx context.Context, region string, types []string) (offers map[string][]string, info map[string]typeInfo, err error) {
	var r struct {
		Zones []string `json:"zones"` // their URLs
	}
	if err := a.get(ctx, fmt.Sprintf("%s/projects/%s/regions/%s", a.compute, a.project, region), &r); err != nil {
		if isNotFound(err) {
			return nil, nil, fmt.Errorf("region %s doesn't exist for project %s ('gcloud compute regions list' lists them)", region, a.project)
		}
		return nil, nil, fmt.Errorf("read region %s of project %s: %w", region, a.project, err)
	}
	offers, info = map[string][]string{}, map[string]typeInfo{}
	for _, zoneURL := range r.Zones {
		zone := path.Base(zoneURL)
		for _, t := range types {
			var ti typeInfo
			err := a.get(ctx, fmt.Sprintf("%s/projects/%s/zones/%s/machineTypes/%s", a.compute, a.project, zone, t), &ti)
			switch {
			case isNotFound(err):
				continue
			case err != nil:
				return nil, nil, fmt.Errorf("check machine type %s in %s: %w", t, zone, err)
			}
			offers[t] = append(offers[t], zone)
			info[t] = ti
		}
	}
	return offers, info, nil
}

// Vars places each machine in a zone, and returns the zones, the machine types' CPU
// architectures (the image follows them), the project, the default machine type, the
// machines' operating system, whether the PD CSI driver needs its account, and the subnet's
// range when the file sets one.
func (c Cloud) Vars(ctx context.Context, spec providers.Spec, machines []cloud.Machine, prev *cloud.Inputs) (map[string]any, error) {
	m := spec.Machines
	a, err := c.api(ctx, m.Project)
	if err != nil {
		return nil, err
	}
	var info map[string]typeInfo
	p := cloud.Placement{Region: m.Region, DefaultSize: DefaultMachineType, Kind: "machine type",
		Offerings: func(ctx context.Context, types []string) (map[string][]string, error) {
			offers, i, err := a.machineTypes(ctx, m.Region, types)
			info = i
			return offers, err
		}}
	zones, err := p.Place(ctx, machines, prev)
	if err != nil {
		return nil, err
	}
	// Every type the machines use gets its architecture: those just placed were described
	// while placing them; the others are asked about here.
	var types []string
	for _, mc := range machines {
		if t := machineType(mc.InstanceType); !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	sort.Strings(types)
	var missing []string
	for _, t := range types {
		if _, ok := info[t]; !ok {
			missing = append(missing, t)
		}
	}
	if len(missing) > 0 {
		offers, more, err := a.machineTypes(ctx, m.Region, missing)
		if err != nil {
			return nil, err
		}
		for _, t := range missing {
			if len(offers[t]) == 0 {
				return nil, fmt.Errorf("machine type %s is not offered in any zone of %s; check its spelling, or pick another instanceType", t, m.Region)
			}
		}
		if info == nil {
			info = map[string]typeInfo{}
		}
		for t, i := range more {
			info[t] = i
		}
	}
	archs := map[string]string{}
	for _, t := range types {
		archs[t] = info[t].arch()
	}
	os := m.OS
	if os == "" {
		os = config.DefaultOS
	}
	vars := map[string]any{"zones": zones, "project": m.Project, "machine_type_archs": archs, "default_instance_type": DefaultMachineType,
		"pd_csi": slices.Contains(spec.Storage, config.CSIGCPPD), "os": os}
	if n := m.Network; n != nil && n.Subnet != "" {
		vars["subnet_cidr"] = n.Subnet
	}
	return vars, nil
}

// BeforeCreate checks each pool's machine type against what the distribution needs for the
// pool's role (CPUs, memory, CPU architecture), before anything is created.
func (c Cloud) BeforeCreate(ctx context.Context, spec providers.Spec, _ []cloud.Machine) error {
	m := spec.Machines
	var types []string
	for _, p := range m.Pools {
		if t := machineType(p.InstanceType); !slices.Contains(types, t) {
			types = append(types, t)
		}
	}
	a, err := c.api(ctx, m.Project)
	if err != nil {
		return err
	}
	offers, info, err := a.machineTypes(ctx, m.Region, types)
	if err != nil {
		return err
	}
	var problems []string
	for _, p := range m.Pools {
		t := machineType(p.InstanceType)
		if len(offers[t]) == 0 {
			problems = append(problems, fmt.Sprintf("pool %s: machine type %s is not offered in any zone of %s; check its spelling", p.Name, t, m.Region))
			continue
		}
		min, ok := cloud.Minimum(spec.Engine, p.Role, m.OS)
		if !ok {
			continue
		}
		ti := info[t]
		if ti.GuestCPUs < min.CPUs || ti.MemoryMB < min.MemoryMB {
			problems = append(problems, fmt.Sprintf("pool %s: %s has %d vCPUs and %d MiB; a %s needs at least %d vCPUs and %d MiB",
				p.Name, t, ti.GuestCPUs, ti.MemoryMB, min.What, min.CPUs, min.MemoryMB))
		}
		uname := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[ti.arch()]
		if !slices.Contains(min.Archs, uname) {
			problems = append(problems, fmt.Sprintf("pool %s: %s's CPU architecture %s has no %s builds", p.Name, t, uname, min.What))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("machine types too small or unsupported:\n  - %s\nPick bigger instanceType values", strings.Join(problems, "\n  - "))
	}
	return nil
}

// Identity checks the credentials against the project: Compute Engine must answer for it.
func (c Cloud) Identity(ctx context.Context, m *providers.MachineRequest) (string, error) {
	a, err := c.api(ctx, m.Project)
	if err != nil {
		return "", err
	}
	if err := a.get(ctx, fmt.Sprintf("%s/projects/%s", a.compute, m.Project), nil); err != nil {
		return "", fmt.Errorf("could not reach Compute Engine in GCP project %s: %w\n"+
			"The project needs the Compute Engine and IAM APIs: gcloud services enable compute.googleapis.com iam.googleapis.com --project %s\n%s",
			m.Project, err, m.Project, credentialHelp)
	}
	if who := a.account(ctx); who != "" {
		return fmt.Sprintf("GCP project %s as %s", m.Project, who), nil
	}
	return "GCP project " + m.Project + " with your Application Default Credentials", nil
}

// AdminCIDR is this machine's public address: the machines have public addresses, so that is
// the address they see.
func (Cloud) AdminCIDR(ctx context.Context, _ *providers.MachineRequest) (string, error) {
	return cloud.PublicCIDR(ctx)
}

// instance is a Compute Engine machine, as far as tow reads one.
type instance struct {
	Name              string            `json:"name"`
	ID                string            `json:"id"`
	Status            string            `json:"status"`
	Zone              string            `json:"zone"` // its URL
	Labels            map[string]string `json:"labels"`
	NetworkInterfaces []struct {
		AccessConfigs []struct {
			NatIP string `json:"natIP"`
		} `json:"accessConfigs"`
	} `json:"networkInterfaces"`
}

func (i instance) publicIP() string {
	for _, n := range i.NetworkInterfaces {
		for _, ac := range n.AccessConfigs {
			if ac.NatIP != "" {
				return ac.NatIP
			}
		}
	}
	return ""
}

// state is the machine's status in tow's words (AWS's): running, pending, stopping,
// stopped.
func (i instance) state() string {
	switch i.Status {
	case "RUNNING":
		return "running"
	case "PROVISIONING", "STAGING", "REPAIRING":
		return "pending"
	case "STOPPING", "SUSPENDING":
		return "stopping"
	}
	return "stopped" // TERMINATED (stopped, on GCP), SUSPENDED
}

// labeled lists a kind of resource in every zone and keeps the cluster's, by its label. (The
// cluster's resources are picked here rather than by a list filter: a filter on a label whose
// key has a hyphen is easy to get wrong, and silently matches nothing.)
func labeled[T interface{ cluster() string }](ctx context.Context, a *api, kind, cluster string) ([]T, error) {
	all, err := aggregated[T](ctx, a, fmt.Sprintf("%s/projects/%s/aggregated/%s", a.compute, a.project, kind), kind, "")
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(r T) bool { return r.cluster() != cluster }), nil
}

func (i instance) cluster() string { return i.Labels[cloud.TagCluster] }

// instances lists the cluster's machines, in any state: a deleted one is gone from the list.
func (a *api) instances(ctx context.Context, cluster string) ([]instance, error) {
	return labeled[instance](ctx, a, "instances", cluster)
}

func (c Cloud) Instances(ctx context.Context, m *providers.MachineRequest, cluster string) ([]cloud.Instance, error) {
	a, err := c.api(ctx, m.Project)
	if err != nil {
		return nil, err
	}
	insts, err := a.instances(ctx, cluster)
	if err != nil {
		return nil, err
	}
	var out []cloud.Instance
	for _, i := range insts {
		out = append(out, cloud.Instance{ID: i.Name, Role: i.Labels[cloud.TagRole], State: i.state()})
	}
	return out, nil
}

// NotTerminated lists the cluster's machines that still exist: what the end-to-end tests check
// after deleting a cluster. (Compute Engine lists a deleted machine no more.)
func (c Cloud) NotTerminated(ctx context.Context, project, cluster string) ([]cloud.Instance, error) {
	return c.Instances(ctx, &providers.MachineRequest{Project: project}, cluster)
}

// named is any resource, as far as the leftover check reads one.
type named struct {
	Name    string            `json:"name"`
	Network string            `json:"network"` // its URL, for subnets and firewall rules
	Labels  map[string]string `json:"labels"`
}

func (n named) cluster() string { return n.Labels[cloud.TagCluster] }

// pdCSIAccount and pdCSIRole are the module's names for the PD CSI driver's service account and
// custom role.
func pdCSIAccount(project, cluster string) string {
	id := cluster
	if len(id) > 24 {
		id = id[:24]
	}
	return fmt.Sprintf("%s-pdcsi@%s.iam.gserviceaccount.com", id, project)
}

func pdCSIRole(cluster string) string {
	return "tow_" + strings.ReplaceAll(cluster, "-", "_") + "_pdcsi"
}

// Leftovers checks every kind of resource the module creates: by the cluster's label where
// the resource takes labels (machines, disks, addresses, forwarding rules), else by the
// names the module gives them, or the cluster's network they're in.
func (c Cloud) Leftovers(ctx context.Context, m *providers.MachineRequest, cluster string) ([]string, error) {
	a, err := c.api(ctx, m.Project)
	if err != nil {
		return nil, err
	}
	p, region := a.compute+"/projects/"+a.project, m.Region
	var left []string
	add := func(kind string, names ...string) {
		for _, n := range names {
			left = append(left, kind+" "+n)
		}
	}
	names := func(items []named, keep func(named) bool) []string {
		var out []string
		for _, i := range items {
			if keep(i) {
				out = append(out, i.Name)
			}
		}
		return out
	}

	insts, err := a.instances(ctx, cluster)
	if err != nil {
		return nil, err
	}
	for _, i := range insts {
		add("instance", i.Name)
	}
	disks, err := labeled[named](ctx, a, "disks", cluster)
	if err != nil {
		return nil, err
	}
	add("disk", names(disks, func(named) bool { return true })...)

	// The load balancers' parts, by the names the module gives them.
	lbNames := []string{cluster + "-api", cluster + "-api-nodes"}
	isLB := func(n named) bool { return slices.Contains(lbNames, n.Name) }
	for _, kind := range []struct{ what, url string }{
		{"forwarding rule", p + "/regions/" + region + "/forwardingRules"},
		{"address", p + "/regions/" + region + "/addresses"},
		{"backend service", p + "/regions/" + region + "/backendServices"},
		{"health check", p + "/regions/" + region + "/healthChecks"},
	} {
		items, err := list[named](ctx, a, kind.url, "")
		if err != nil {
			return nil, err
		}
		add(kind.what, names(items, isLB)...)
	}
	// One instance group per control plane, named after it.
	groups, err := aggregated[named](ctx, a, p+"/aggregated/instanceGroups", "instanceGroups", "")
	if err != nil {
		return nil, err
	}
	add("instance group", names(groups, func(n named) bool {
		rest, ok := strings.CutPrefix(n.Name, cluster+"-cp-")
		return ok && rest != "" && strings.Trim(rest, "0123456789") == ""
	})...)

	// The cluster's network, and what's in it.
	network := cluster + "-vpc"
	inNetwork := func(n named) bool { return path.Base(n.Network) == network }
	firewalls, err := list[named](ctx, a, p+"/global/firewalls", "")
	if err != nil {
		return nil, err
	}
	add("firewall rule", names(firewalls, inNetwork)...)
	subnets, err := list[named](ctx, a, p+"/regions/"+region+"/subnetworks", "")
	if err != nil {
		return nil, err
	}
	add("subnet", names(subnets, inNetwork)...)
	switch err := a.get(ctx, p+"/global/networks/"+network, nil); {
	case err == nil:
		add("network", network)
	case !isNotFound(err):
		return nil, err
	}

	// The PD CSI driver's service account and role (a deleted role lingers, marked deleted,
	// for 7 days before Google purges it: that is gone already).
	account := pdCSIAccount(a.project, cluster)
	switch err := a.get(ctx, fmt.Sprintf("%s/projects/%s/serviceAccounts/%s", a.iam, a.project, account), nil); {
	case err == nil:
		add("service account", account)
	case !isNotFound(err):
		return nil, err
	}
	var role struct {
		Deleted bool `json:"deleted"`
	}
	switch err := a.get(ctx, fmt.Sprintf("%s/projects/%s/roles/%s", a.iam, a.project, pdCSIRole(cluster)), &role); {
	case err == nil && !role.Deleted:
		add("custom role", pdCSIRole(cluster))
	case err != nil && !isNotFound(err):
		return nil, err
	}
	return left, nil
}

// PowerOff stops the cluster's machine called name and waits until it's stopped; PowerOn
// starts it, waits until it runs, and returns its public address, a new one: its address is
// ephemeral. Tests take a control plane away with them.
func (c Cloud) PowerOff(ctx context.Context, project, cluster, name string) error {
	_, err := c.power(ctx, project, cluster, name, false)
	return err
}

func (c Cloud) PowerOn(ctx context.Context, project, cluster, name string) (publicIP string, err error) {
	return c.power(ctx, project, cluster, name, true)
}

func (c Cloud) power(ctx context.Context, project, cluster, name string, on bool) (string, error) {
	a, err := c.api(ctx, project)
	if err != nil {
		return "", err
	}
	insts, err := a.instances(ctx, cluster)
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(insts, func(i instance) bool { return i.Name == name })
	if i < 0 {
		return "", fmt.Errorf("cluster %s has no machine called %s", cluster, name)
	}
	u := fmt.Sprintf("%s/projects/%s/zones/%s/instances/%s", a.compute, project, path.Base(insts[i].Zone), name)
	verb, want := "stop", "TERMINATED"
	if on {
		verb, want = "start", "RUNNING"
	}
	if err := a.do(ctx, "POST", u+"/"+verb, nil, nil); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for {
		var got instance
		if err := a.get(ctx, u, &got); err != nil {
			return "", err
		}
		if got.Status == want {
			if !on {
				return "", nil
			}
			if ip := got.publicIP(); ip != "" {
				return ip, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%s: still %s after 10 minutes, waiting for %s", name, got.Status, want)
		case <-time.After(5 * time.Second):
		}
	}
}
