// Package azure is the Azure provision target: its OpenTofu module plus the Azure Resource
// Manager calls OpenTofu doesn't make. It is the only package that talks to Azure's APIs.
package azure

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"net/http"
	"slices"
	"sort"
	"strconv"
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

// API versions of the Resource Manager calls.
const (
	skusVersion      = "2021-07-01"
	computeVersion   = "2024-07-01"
	networkVersion   = "2024-05-01"
	resourcesVersion = "2021-04-01"
	subsVersion      = "2022-12-01"
)

// Cloud uses the credentials OpenTofu's azurerm provider does (api.go), so whatever works for
// one works for the other.
type Cloud struct {
	// newAPI is the client for a subscription: Azure's (nil), or a test's.
	newAPI func(subscription string) (*api, error)
}

func (c Cloud) api(subscription string) (*api, error) {
	if c.newAPI != nil {
		return c.newAPI(subscription)
	}
	return newAPI(subscription)
}

func (Cloud) Module() fs.FS {
	sub, err := fs.Sub(module, "module")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

// DefaultVMSize sizes pools that name no VM size: 2 vCPUs and 8 GiB, enough for a kubeadm
// control plane on AlmaLinux too.
const DefaultVMSize = "Standard_B2s_v2"

func vmSize(s string) string {
	if s != "" {
		return s
	}
	return DefaultVMSize
}

// ResourceGroup holds everything of a cluster: deleting it deletes them all.
func ResourceGroup(cluster string) string { return "tow-" + cluster }

func (Cloud) MachineType() string { return "azurerm_linux_virtual_machine" }

// AdminRules: none. The NSG's rules for the admin addresses take them all at once, and change
// in place.
func (Cloud) AdminRules() []string { return nil }

// sku is a VM size as Azure's resource SKUs list describes it, in one location.
type sku struct {
	ResourceType string `json:"resourceType"`
	Name         string `json:"name"`
	LocationInfo []struct {
		Location string   `json:"location"`
		Zones    []string `json:"zones"`
	} `json:"locationInfo"`
	Capabilities []struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	} `json:"capabilities"`
	Restrictions []struct {
		Type            string   `json:"type"` // Location or Zone
		Values          []string `json:"values"`
		RestrictionInfo struct {
			Zones []string `json:"zones"`
		} `json:"restrictionInfo"`
	} `json:"restrictions"`
}

func (s sku) capability(name string) string {
	for _, c := range s.Capabilities {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// sizeInfo is what tow needs of a VM size: its CPUs, memory and architecture, and the
// location's zones that offer it to this subscription ("" alone: offered, in a location
// without zones).
type sizeInfo struct {
	CPUs     int
	MemoryMB int
	Arch     string // amd64 or arm64
	Zones    []string
}

// sizes describes the location's VM sizes this subscription may use.
func (a *api) sizes(ctx context.Context, location string) (map[string]sizeInfo, error) {
	skus, err := list[sku](ctx, a, fmt.Sprintf("/subscriptions/%s/providers/Microsoft.Compute/skus?$filter=location%%20eq%%20'%s'", a.subscription, location), skusVersion)
	if err != nil {
		return nil, fmt.Errorf("list the VM sizes of %s: %w", location, err)
	}
	out := map[string]sizeInfo{}
	for _, s := range skus {
		if s.ResourceType != "virtualMachines" {
			continue
		}
		var zones, restricted []string
		offered := false
		for _, li := range s.LocationInfo {
			if strings.EqualFold(li.Location, location) {
				offered, zones = true, li.Zones
			}
		}
		for _, r := range s.Restrictions {
			switch r.Type {
			case "Location":
				if slices.ContainsFunc(r.Values, func(v string) bool { return strings.EqualFold(v, location) }) {
					offered = false
				}
			case "Zone":
				restricted = append(restricted, r.RestrictionInfo.Zones...)
			}
		}
		if !offered {
			continue
		}
		zones = slices.DeleteFunc(slices.Clone(zones), func(z string) bool { return slices.Contains(restricted, z) })
		if len(s.LocationInfo) > 0 && len(zones) == 0 && len(restricted) == 0 {
			zones = []string{""} // a location without zones
		}
		cpus, _ := strconv.Atoi(s.capability("vCPUs"))
		mem, _ := strconv.ParseFloat(s.capability("MemoryGB"), 64)
		arch := "amd64"
		if s.capability("CpuArchitectureType") == "Arm64" {
			arch = "arm64"
		}
		// Azure may list a size more than once (entries for different sets of locations): the
		// zones any of them offers count.
		if prev, ok := out[s.Name]; ok {
			for _, z := range prev.Zones {
				if !slices.Contains(zones, z) {
					zones = append(zones, z)
				}
			}
			if len(zones) > 1 {
				zones = slices.DeleteFunc(zones, func(z string) bool { return z == "" })
			}
			slices.Sort(zones)
		}
		out[s.Name] = sizeInfo{CPUs: cpus, MemoryMB: int(mem * 1024), Arch: arch, Zones: zones}
	}
	return out, nil
}

// Vars places each machine in a zone, and returns the zones, the VM sizes' CPU architectures
// (the image follows them), the subscription, the default VM size, the machines' operating
// system, whether the Azure Disk CSI driver needs its identity, and the VNet's range when the
// file sets one.
func (c Cloud) Vars(ctx context.Context, spec providers.Spec, machines []cloud.Machine, prev *cloud.Inputs) (map[string]any, error) {
	m := spec.Machines
	a, err := c.api(m.Subscription)
	if err != nil {
		return nil, err
	}
	info, err := a.sizes(ctx, m.Region)
	if err != nil {
		return nil, err
	}
	p := cloud.Placement{Region: m.Region, DefaultSize: DefaultVMSize, Kind: "VM size",
		Offerings: func(_ context.Context, sizes []string) (map[string][]string, error) {
			offers := map[string][]string{}
			for _, s := range sizes {
				offers[s] = info[s].Zones
			}
			return offers, nil
		}}
	zones, err := p.Place(ctx, machines, prev)
	if err != nil {
		return nil, err
	}
	archs := map[string]string{}
	var sizes []string
	for _, mc := range machines {
		if s := vmSize(mc.InstanceType); !slices.Contains(sizes, s) {
			sizes = append(sizes, s)
		}
	}
	sort.Strings(sizes)
	for _, s := range sizes {
		i, ok := info[s]
		if !ok {
			return nil, fmt.Errorf("VM size %s is not offered in %s to this subscription; check its spelling, or pick another instanceType", s, m.Region)
		}
		archs[s] = i.Arch
	}
	os := m.OS
	if os == "" {
		os = config.DefaultOS
	}
	vars := map[string]any{"zones": zones, "subscription": m.Subscription, "machine_type_archs": archs, "default_instance_type": DefaultVMSize,
		"disk_csi": slices.Contains(spec.Storage, config.CSIAzureDisk), "os": os}
	if n := m.Network; n != nil && n.Subnet != "" {
		vars["subnet_cidr"] = n.Subnet
	}
	return vars, nil
}

// BeforeCreate checks each pool's VM size against what the distribution needs for the pool's
// role (CPUs, memory, CPU architecture), and the disk against the images' 30 GB, before
// anything is created.
func (c Cloud) BeforeCreate(ctx context.Context, spec providers.Spec, _ []cloud.Machine) error {
	m := spec.Machines
	a, err := c.api(m.Subscription)
	if err != nil {
		return err
	}
	info, err := a.sizes(ctx, m.Region)
	if err != nil {
		return err
	}
	var problems []string
	for _, p := range m.Pools {
		s := vmSize(p.InstanceType)
		si, ok := info[s]
		if !ok {
			problems = append(problems, fmt.Sprintf("pool %s: VM size %s is not offered in %s to this subscription; check its spelling", p.Name, s, m.Region))
			continue
		}
		if p.DiskGB > 0 && p.DiskGB < 30 {
			problems = append(problems, fmt.Sprintf("pool %s: a %d GB disk is smaller than Azure's Ubuntu and AlmaLinux images (30 GB)", p.Name, p.DiskGB))
		}
		min, ok := cloud.Minimum(spec.Engine, p.Role, m.OS)
		if !ok {
			continue
		}
		if si.CPUs < min.CPUs || si.MemoryMB < min.MemoryMB {
			problems = append(problems, fmt.Sprintf("pool %s: %s has %d vCPUs and %d MiB; a %s needs at least %d vCPUs and %d MiB",
				p.Name, s, si.CPUs, si.MemoryMB, min.What, min.CPUs, min.MemoryMB))
		}
		uname := map[string]string{"amd64": "x86_64", "arm64": "aarch64"}[si.Arch]
		if !slices.Contains(min.Archs, uname) {
			problems = append(problems, fmt.Sprintf("pool %s: %s's CPU architecture %s has no %s builds", p.Name, s, uname, min.What))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("VM sizes too small or unsupported:\n  - %s\nPick bigger instanceType values", strings.Join(problems, "\n  - "))
	}
	return nil
}

// Identity checks the credentials against the subscription.
func (c Cloud) Identity(ctx context.Context, m *providers.MachineRequest) (string, error) {
	a, err := c.api(m.Subscription)
	if err != nil {
		return "", err
	}
	var sub struct {
		DisplayName string `json:"displayName"`
		State       string `json:"state"`
	}
	if err := a.get(ctx, "/subscriptions/"+m.Subscription, subsVersion, &sub); err != nil {
		return "", fmt.Errorf("could not reach Azure subscription %s: %w\n%s", m.Subscription, err, credentialHelp)
	}
	if sub.State != "" && sub.State != "Enabled" {
		return "", cloud.Permanent(fmt.Errorf("Azure subscription %s (%s) is %s: it can't create anything", m.Subscription, sub.DisplayName, sub.State))
	}
	what := fmt.Sprintf("Azure subscription %s (%s)", sub.DisplayName, m.Subscription)
	if who := a.account(ctx); who != "" {
		return what + " as " + who, nil
	}
	return what, nil
}

// AdminCIDR is this machine's public address: the machines have public addresses, so that is
// the address they see.
func (Cloud) AdminCIDR(ctx context.Context, _ *providers.MachineRequest) (string, error) {
	return cloud.PublicCIDR(ctx)
}

// vm is an Azure VM, as far as tow reads one.
type vm struct {
	Name       string            `json:"name"`
	Tags       map[string]string `json:"tags"`
	Properties struct {
		InstanceView struct {
			Statuses []struct {
				Code string `json:"code"`
			} `json:"statuses"`
		} `json:"instanceView"`
	} `json:"properties"`
}

// power is the VM's PowerState: running, starting, stopping, stopped, deallocating,
// deallocated; "" before Azure reports one.
func (v vm) power() string {
	for _, s := range v.Properties.InstanceView.Statuses {
		if p, ok := strings.CutPrefix(s.Code, "PowerState/"); ok {
			return p
		}
	}
	return ""
}

// state is the VM's power state in tow's words (AWS's): running, pending, stopping,
// stopped.
func (v vm) state() string {
	switch v.power() {
	case "running":
		return "running"
	case "starting", "":
		return "pending"
	case "stopping", "deallocating":
		return "stopping"
	}
	return "stopped"
}

// vms lists the cluster's VMs with their power state; none when its resource group is gone.
// A resource group's list carries no power states (its $expand=instanceView needs a scale
// set filter: Azure answers 400 without one), so each VM's instance view is read after it.
func (a *api) vms(ctx context.Context, cluster string) ([]vm, error) {
	group := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s/providers/Microsoft.Compute/virtualMachines", a.subscription, ResourceGroup(cluster))
	all, err := list[vm](ctx, a, group, computeVersion)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []vm
	for _, v := range all {
		if v.Tags[cloud.TagCluster] != cluster {
			continue
		}
		err := a.get(ctx, group+"/"+v.Name+"/instanceView", computeVersion, &v.Properties.InstanceView)
		switch {
		case isNotFound(err): // deleted since the list
			continue
		case err != nil:
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (c Cloud) Instances(ctx context.Context, m *providers.MachineRequest, cluster string) ([]cloud.Instance, error) {
	a, err := c.api(m.Subscription)
	if err != nil {
		return nil, err
	}
	vms, err := a.vms(ctx, cluster)
	if err != nil {
		return nil, err
	}
	var out []cloud.Instance
	for _, v := range vms {
		out = append(out, cloud.Instance{ID: v.Name, Role: v.Tags[cloud.TagRole], State: v.state()})
	}
	return out, nil
}

// NotTerminated lists the cluster's VMs that still exist: what the end-to-end tests check after
// deleting a cluster.
func (c Cloud) NotTerminated(ctx context.Context, subscription, cluster string) ([]cloud.Instance, error) {
	return c.Instances(ctx, &providers.MachineRequest{Subscription: subscription}, cluster)
}

// Leftovers: everything of the cluster's lives in its resource group, so what's left is the
// group itself and whatever is still in it.
func (c Cloud) Leftovers(ctx context.Context, m *providers.MachineRequest, cluster string) ([]string, error) {
	a, err := c.api(m.Subscription)
	if err != nil {
		return nil, err
	}
	rg := ResourceGroup(cluster)
	base := fmt.Sprintf("/subscriptions/%s/resourcegroups/%s", a.subscription, rg)
	switch err := a.get(ctx, base, resourcesVersion, nil); {
	case isNotFound(err):
		return nil, nil
	case err != nil:
		return nil, err
	}
	type resource struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	in, err := list[resource](ctx, a, base+"/resources", resourcesVersion)
	if err != nil {
		return nil, err
	}
	left := []string{"resource group " + rg}
	for _, r := range in {
		left = append(left, r.Type+" "+r.Name)
	}
	return left, nil
}

// PowerOff deallocates the cluster's VM called name (compute stops billing) and waits until
// it's deallocated; PowerOn starts it, waits until it runs, and returns its public address,
// the same one (static). Tests take a control plane away with them.
func (c Cloud) PowerOff(ctx context.Context, subscription, cluster, name string) error {
	_, err := c.power(ctx, subscription, cluster, name, false)
	return err
}

func (c Cloud) PowerOn(ctx context.Context, subscription, cluster, name string) (publicIP string, err error) {
	return c.power(ctx, subscription, cluster, name, true)
}

func (c Cloud) power(ctx context.Context, subscription, cluster, name string, on bool) (string, error) {
	a, err := c.api(subscription)
	if err != nil {
		return "", err
	}
	group := fmt.Sprintf("/subscriptions/%s/resourceGroups/%s", subscription, ResourceGroup(cluster))
	u := group + "/providers/Microsoft.Compute/virtualMachines/" + name
	verb, want := "/deallocate", "deallocated"
	if on {
		verb, want = "/start", "running"
	}
	if err := a.do(ctx, http.MethodPost, u+verb, computeVersion, nil); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	for {
		var got vm
		if err := a.get(ctx, u+"/instanceView", computeVersion, &got.Properties.InstanceView); err != nil {
			return "", err
		}
		if got.power() == want {
			break
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%s: still %q after 10 minutes, waiting for %s", name, got.power(), want)
		case <-time.After(5 * time.Second):
		}
	}
	if !on {
		return "", nil
	}
	var ip struct {
		Properties struct {
			IPAddress string `json:"ipAddress"`
		} `json:"properties"`
	}
	if err := a.get(ctx, group+"/providers/Microsoft.Network/publicIPAddresses/"+name, networkVersion, &ip); err != nil {
		return "", err
	}
	return ip.Properties.IPAddress, nil
}
