// Package config reads tow.yaml: a Kubernetes-style description of a cluster and of the
// machines that host it. The file decides the whole architecture; a flavor is only an optional
// preset that fills in what the file leaves out.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/towproj/tow/internal/runtimes"
	"gopkg.in/yaml.v3"
)

const (
	APIVersion      = "towproj.github.io/v1alpha1"
	Kind            = "Cluster"
	DefaultFileName = "tow.yaml"
	DefaultSSHPort  = 22
	DefaultSSHUser  = "ubuntu" // user baked into the cloud images tow provisions

	// VersionStable resolves to the latest stable Kubernetes release at apply time.
	VersionStable = "stable"
	// DefaultKubernetesVersion is the release tow tests, installed when the file names
	// none (k3s: its first build, v1.37.2+k3s1).
	DefaultKubernetesVersion = "v1.37.2"

	RoleControlPlane = "control-plane"
	RoleWorker       = "worker"

	// DefaultPoolName names a worker pool the file doesn't name (one pool only).
	DefaultPoolName = "workers"

	// Modes say who owns the machines; they follow from the provider.
	ModeLocal     = "local"     // the distribution creates its own nodes on this machine (k3d)
	ModeProvision = "provision" // OpenTofu creates the machines
	ModeBYOH      = "byoh"      // bring your own hosts: the file lists existing machines
)

// Providers: where the machines come from.
const (
	ProviderLocal   = "local"
	ProviderBYOH    = "byoh"
	ProviderAWS     = "aws"
	ProviderGCP     = "gcp"
	ProviderAzure   = "azure"
	ProviderProxmox = "proxmox"
)

var supportedProviders = []string{ProviderAWS, ProviderGCP, ProviderAzure, ProviderProxmox, ProviderBYOH, ProviderLocal}

// cloudTerms are the public clouds (machines in a region's zones, with public addresses, and
// a load balancer tow creates in front of several control planes) and how each one names
// its parts, for messages.
var cloudTerms = map[string]struct{ name, network, sizeExample string }{
	ProviderAWS:   {"AWS", "VPC", "t3.large"},
	ProviderGCP:   {"GCP", "VPC", "e2-standard-4"},
	ProviderAzure: {"Azure", "VNet", "Standard_B2ms"},
}

// PublicCloud reports whether the file's provider is a public cloud (AWS, GCP, Azure).
func (c *Config) PublicCloud() bool {
	_, ok := cloudTerms[c.Provider()]
	return ok
}

// CloudName is how a public cloud is written in messages ("AWS"); the provider itself for
// the others.
func CloudName(provider string) string {
	if t, ok := cloudTerms[provider]; ok {
		return t.name
	}
	return provider
}

// Config mirrors tow.yaml.
type Config struct {
	APIVersion string   `yaml:"apiVersion"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	Spec       Spec     `yaml:"spec"`

	// Derived from the file, never read from it.
	Mode     string   `yaml:"-"`
	warnings []string `yaml:"-"`
	dir      string   // the cluster file's folder: local manifest paths are relative to it
}

type Metadata struct {
	Name string `yaml:"name"` // DNS label; names the machines, the state and the cloud tags
}

type Spec struct {
	Flavor         string         `yaml:"flavor,omitempty"` // optional preset
	Kubernetes     Kubernetes     `yaml:"kubernetes"`
	Infrastructure Infrastructure `yaml:"infrastructure"`
	ControlPlane   ControlPlane   `yaml:"controlPlane"`
	Workers        []WorkerPool   `yaml:"workers,omitempty"`
}

// Kubernetes is the cluster tow installs: exactly what this part of the file (or the
// flavor) asks for, and nothing else.
type Kubernetes struct {
	Distribution        string              `yaml:"distribution,omitempty"`     // k3s | kubeadm | k3d
	Version             string              `yaml:"version,omitempty"`          // a release; "stable"; empty = DefaultKubernetesVersion
	ContainerRuntime    string              `yaml:"containerRuntime,omitempty"` // kubeadm: containerd (default) | cri-o
	Containerd          *Containerd         `yaml:"containerd,omitempty"`
	CRIO                *CRIO               `yaml:"cri-o,omitempty"`
	APIEndpoint         string              `yaml:"apiEndpoint,omitempty"`     // host:port in front of several control planes
	APILoadBalancer     string              `yaml:"apiLoadBalancer,omitempty"` // what serves apiEndpoint: kube-vip | external
	APIServer           APIServer           `yaml:"apiServer,omitempty"`
	ControllerManager   Component           `yaml:"controllerManager,omitempty"`
	Scheduler           Component           `yaml:"scheduler,omitempty"`
	Etcd                Component           `yaml:"etcd,omitempty"`
	Kubelet             Component           `yaml:"kubelet,omitempty"` // every node; pools add their own
	K3s                 K3s                 `yaml:"k3s,omitempty"`
	Networking          Networking          `yaml:"networking,omitempty"`
	Storage             Storage             `yaml:"storage,omitempty"`
	ServiceLoadBalancer ServiceLoadBalancer `yaml:"serviceLoadBalancer,omitempty"`
	Ingress             Ingress             `yaml:"ingress,omitempty"`
	MetricsServer       MetricsServer       `yaml:"metricsServer,omitempty"`
	Addons              Addons              `yaml:"addons,omitempty"`
	Kubeconfig          Kubeconfig          `yaml:"kubeconfig,omitempty"`
	Backups             *Backups            `yaml:"backups,omitempty"` // etcd snapshots on a schedule; none = tow adds none
}

// Kubeconfig says where the cluster's credentials go on this machine.
type Kubeconfig struct {
	Context string `yaml:"context,omitempty"` // default: metadata.name
	Merge   *bool  `yaml:"merge,omitempty"`   // into ~/.kube/config (or $KUBECONFIG); false = only the copy in the state folder
}

type Infrastructure struct {
	Provider string `yaml:"provider,omitempty"`
	// OS is the operating system of the machines tow creates on a cloud (os.go); default
	// ubuntu-24.04.
	OS      string   `yaml:"os,omitempty"`
	AWS     *AWS     `yaml:"aws,omitempty"`
	GCP     *GCP     `yaml:"gcp,omitempty"`
	Azure   *Azure   `yaml:"azure,omitempty"`
	Proxmox *Proxmox `yaml:"proxmox,omitempty"`
	Network *Network `yaml:"network,omitempty"`
	Access  Access   `yaml:"access,omitempty"`
}

type AWS struct {
	Region string `yaml:"region,omitempty"`
}

// GCP: the credentials are Google's Application Default Credentials, never this file.
type GCP struct {
	Project string `yaml:"project,omitempty"` // the project ID, like my-project-123456
	Region  string `yaml:"region,omitempty"`  // like europe-west1
}

// Azure: the credentials are Azure's own (az login, a service principal or workload identity
// in the environment, a managed identity), never this file.
type Azure struct {
	Subscription string `yaml:"subscription,omitempty"` // the subscription's ID, a GUID
	Location     string `yaml:"location,omitempty"`     // the region, like westeurope
}

// Proxmox: the API token comes from PROXMOX_VE_API_TOKEN, never from this file.
type Proxmox struct {
	URL      string `yaml:"url,omitempty"`      // API address, e.g. https://192.168.1.10:8006
	Node     string `yaml:"node,omitempty"`     // the Proxmox server the VMs run on
	Template int    `yaml:"template,omitempty"` // ID of the cloud-init template VM to clone
	Storage  string `yaml:"storage,omitempty"`  // default local-lvm
	Bridge   string `yaml:"bridge,omitempty"`   // default vmbr0
	// CPUType is the VMs' CPU model; default x86-64-v2-AES. A RHEL 10-based template (Rocky,
	// AlmaLinux) needs x86-64-v3 or host.
	CPUType string `yaml:"cpuType,omitempty"`
}

// Network is the machines' network. Proxmox: VMs get fixed addresses from the range; each
// keeps its own, and a new VM gets the lowest one free. Public clouds: only subnet, the
// range of the network tow creates (VPC, VNet).
type Network struct {
	Subnet    string   `yaml:"subnet,omitempty"`
	Addresses string   `yaml:"addresses,omitempty"` // first-last
	Gateway   string   `yaml:"gateway,omitempty"`
	DNS       []string `yaml:"dns,omitempty"`
}

type Access struct {
	SSH        SSH      `yaml:"ssh,omitempty"`
	AdminCIDRs []string `yaml:"adminCIDRs,omitempty"` // who may reach SSH and the API; default: this machine
	// Ports are TCP ports the machines' firewalls (the clouds', Proxmox's, ufw, firewalld)
	// open to AdminCIDRs, for what you install yourself (an ingress controller from
	// addons.helmCharts). tow's own components open theirs.
	Ports []int `yaml:"ports,omitempty"`
}

type SSH struct {
	User    string `yaml:"user,omitempty"`    // byoh: required; provisioned: default ubuntu
	KeyPath string `yaml:"keyPath,omitempty"` // empty = ssh-agent, then ~/.ssh default keys (like ssh itself)
	Port    int    `yaml:"port,omitempty"`    // byoh only; default 22
}

// Group is a set of machines with one role: created by count (aws, proxmox, local) or listed
// one by one (byoh).
type Group struct {
	Count        int        `yaml:"count,omitempty"`
	InstanceType string     `yaml:"instanceType,omitempty"` // clouds: the provider's catalog name
	Resources    *Resources `yaml:"resources,omitempty"`    // hypervisors: CPU and memory
	Disk         string     `yaml:"disk,omitempty"`         // e.g. 40Gi
	Machines     []Machine  `yaml:"machines,omitempty"`     // byoh
	NodeSettings `yaml:",inline"`
}

type WorkerPool struct {
	Name  string `yaml:"name,omitempty"`
	Group `yaml:",inline"`
}

type Resources struct {
	CPU    int    `yaml:"cpu,omitempty"`
	Memory string `yaml:"memory,omitempty"` // e.g. 4Gi
}

// Machine is one existing machine (byoh).
type Machine struct {
	Host string `yaml:"host"`
	SSH  SSH    `yaml:"ssh,omitempty"` // overrides infrastructure.access.ssh
}

// Load reads, defaults and validates a config file in one pass.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return parse(data, filepath.Dir(path))
}

// Parse decodes a file's content, refuses fields its provider doesn't use, fills defaults
// (file > flavor > distribution), then checks real technical limits. Every problem is
// reported at once. Local manifest paths are relative to the current folder.
func Parse(data []byte) (*Config, error) { return parse(data, ".") }

// ParseIn is Parse for content that belongs to a file in dir: its local manifests resolve
// there (tests that change a cluster file before applying it).
func ParseIn(data []byte, dir string) (*Config, error) { return parse(data, dir) }

func parse(data []byte, dir string) (*Config, error) {
	var raw yaml.Node
	if err := yaml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := rejectCredentials(&raw); err != nil {
		return nil, err
	}

	c := Config{dir: dir}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // typos like "contolPlane:" fail loudly instead of being ignored
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	if c.APIVersion != APIVersion {
		return nil, fmt.Errorf("apiVersion %q is not supported (expected %s)", c.APIVersion, APIVersion)
	}
	if c.Kind != Kind {
		return nil, fmt.Errorf("kind %q is not supported (expected %s)", c.Kind, Kind)
	}
	if err := c.resolveProvider(); err != nil {
		return nil, err
	}
	// Fields the provider doesn't use are refused as written, before defaults fill anything.
	if errs := c.checkFieldsForProvider(); len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// resolveProvider settles the flavor and the provider, which every later rule depends on.
func (c *Config) resolveProvider() error {
	s := &c.Spec
	if s.Flavor != "" {
		f, ok := flavors[s.Flavor]
		if !ok {
			return fmt.Errorf("spec.flavor %q is unknown (available: %s); a flavor is optional, you can describe everything yourself", s.Flavor, flavorNames())
		}
		if s.Infrastructure.Provider == "" {
			s.Infrastructure.Provider = f.Provider
		}
	}
	p := s.Infrastructure.Provider
	switch p {
	case "":
		return fmt.Errorf("spec.infrastructure.provider is required: where the machines come from (%s)", joinOr(supportedProviders))
	case ProviderLocal:
		c.Mode = ModeLocal
	case ProviderBYOH:
		c.Mode = ModeBYOH
	case ProviderAWS, ProviderGCP, ProviderAzure, ProviderProxmox:
		c.Mode = ModeProvision
	default:
		return fmt.Errorf("spec.infrastructure.provider %q is not supported (%s)", p, joinOr(supportedProviders))
	}
	return nil
}

// applyDefaults layers: what the file says > the flavor's preset > the distribution's default.
func (c *Config) applyDefaults() error {
	s := &c.Spec
	k := &s.Kubernetes
	var f Flavor
	if s.Flavor != "" {
		f = flavors[s.Flavor]
	}

	if k.Distribution == "" {
		k.Distribution = f.Distribution
	}
	if k.Distribution == "" && c.Mode == ModeLocal {
		k.Distribution = DistributionK3d
	}
	if k.Distribution == "" {
		return fmt.Errorf("spec.kubernetes.distribution is required without a flavor (%s)", joinOr(distributionNames()))
	}
	d, ok := distributions[k.Distribution]
	if !ok {
		return fmt.Errorf("spec.kubernetes.distribution %q is not supported (%s)", k.Distribution, joinOr(distributionNames()))
	}

	// The Kubernetes release stays empty when the file names none: it means the one tow
	// tests at apply time, like the add-ons' versions, so a newer tow isn't drift.
	c.kubernetesDefaults(f, d)

	// Machine counts: byoh counts what it lists; the others create what the file (or the
	// flavor) asks for.
	if c.Mode != ModeBYOH {
		if s.ControlPlane.Count == 0 {
			s.ControlPlane.Count = max(f.ControlPlanes, 1)
		}
		if s.Workers == nil && f.Workers > 0 {
			s.Workers = []WorkerPool{{Name: DefaultPoolName, Group: Group{Count: f.Workers}}}
		}
	}
	if len(s.Workers) == 1 && s.Workers[0].Name == "" {
		s.Workers[0].Name = DefaultPoolName
	}

	a := &s.Infrastructure.Access
	switch c.Mode {
	case ModeProvision:
		if c.PublicCloud() && s.Infrastructure.OS == "" {
			s.Infrastructure.OS = DefaultOS
		}
		if a.SSH.User == "" {
			a.SSH.User = DefaultSSHUser
			if c.PublicCloud() {
				a.SSH.User = LoginUser(s.Infrastructure.Provider, s.Infrastructure.OS)
			}
		}
	case ModeBYOH:
		if a.SSH.Port == 0 {
			a.SSH.Port = DefaultSSHPort
		}
		for _, g := range c.groups() {
			for i := range g.Machines {
				m := &g.Machines[i]
				if m.SSH.User == "" {
					m.SSH.User = a.SSH.User
				}
				if m.SSH.KeyPath == "" {
					m.SSH.KeyPath = a.SSH.KeyPath
				}
				if m.SSH.Port == 0 {
					m.SSH.Port = a.SSH.Port
				}
			}
		}
	}

	if s.Infrastructure.Provider == ProviderProxmox {
		c.proxmoxDefaults()
	}
	return nil
}

// kubernetesDefaults picks the optional components: the file's choice, else the flavor's,
// else none. Kubernetes itself is always installed.
func (c *Config) kubernetesDefaults(f Flavor, d Distribution) {
	k := &c.Spec.Kubernetes
	if k.Distribution == DistributionKubeadm && k.ContainerRuntime == "" {
		k.ContainerRuntime = runtimes.Containerd
	}
	if k.Backups != nil && k.Backups.Keep == 0 {
		k.Backups.Keep = DefaultBackupsKeep
	}
	n := &k.Networking
	if n.PodCIDR == "" {
		n.PodCIDR = d.PodCIDR
	}
	if n.ServiceCIDR == "" {
		n.ServiceCIDR = d.ServiceCIDR
	}
	if n.ClusterDomain == "" {
		n.ClusterDomain = DefaultClusterDomain
	}
	// Local clusters are for trying things, which needs a working network: flannel unless the
	// file names another choice (user, 2026-11-09). Elsewhere, no CNI unless asked.
	localCNI := ""
	if k.Distribution == DistributionK3d {
		localCNI = CNIFlannel
	}
	n.CNI = firstOf(n.CNI, f.CNI, localCNI, CNINone)

	// Lists: one written in the file replaces the flavor's, and [] means none.
	s := &k.Storage
	if s.CSI == nil {
		s.CSI = f.Storage
	}
	if s.DefaultClass == "" && len(s.CSI) == 1 {
		s.DefaultClass = s.CSI[0]
	}
	in := &k.Ingress
	if in.Controllers == nil {
		in.Controllers = f.Ingress
	}
	if classes := ingressClasses(in.Controllers); in.DefaultClass == "" && len(classes) == 1 {
		in.DefaultClass = classes[0]
	}

	lb := &k.ServiceLoadBalancer
	lb.Provider = firstOf(lb.Provider, f.ServiceLB, ServiceLBNone)
	for _, p := range []*AddressPool{poolOf(lb.MetalLB), poolOf(lb.KubeVIP), lb.Cilium} {
		if p != nil && p.Mode == "" {
			p.Mode = ModeL2
			if p.BGP != nil {
				p.Mode = ModeBGP
			}
		}
	}

	if k.MetricsServer.Enabled == nil {
		on := f.MetricsServer
		k.MetricsServer.Enabled = &on
	}
	for i := range k.Addons.HelmCharts {
		if k.Addons.HelmCharts[i].Namespace == "" {
			k.Addons.HelmCharts[i].Namespace = k.Addons.HelmCharts[i].Name // its own, never "default"
		}
	}

	if k.APILoadBalancer == "" && k.APIEndpoint != "" && (c.Mode == ModeBYOH || c.Provider() == ProviderProxmox) {
		k.APILoadBalancer = APILBKubeVIP
	}
	if k.Kubeconfig.Context == "" {
		k.Kubeconfig.Context = c.Metadata.Name
	}
	if k.Kubeconfig.Merge == nil {
		merge := true
		k.Kubeconfig.Merge = &merge
	}
	c.Spec.ControlPlane.Schedulable = firstOf(c.Spec.ControlPlane.Schedulable, f.Schedulable, SchedulableAuto)
}

func poolOf(p *PoolController) *AddressPool {
	if p == nil {
		return nil
	}
	return &p.AddressPool
}

// firstOf returns the first non-empty value.
func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// groups lists every machine group, control plane first, as pointers into the spec.
func (c *Config) groups() []*Group {
	out := []*Group{&c.Spec.ControlPlane.Group}
	for i := range c.Spec.Workers {
		out = append(out, &c.Spec.Workers[i].Group)
	}
	return out
}

// Name is the cluster's name.
func (c *Config) Name() string { return c.Metadata.Name }

// Provider says where the machines come from.
func (c *Config) Provider() string { return c.Spec.Infrastructure.Provider }

// Distribution is the Kubernetes distribution the machines are meant for.
func (c *Config) Distribution() string { return c.Spec.Kubernetes.Distribution }

// APIHost is apiEndpoint's host: kube-vip's virtual IP when kube-vip serves it.
func (k Kubernetes) APIHost() string {
	host, _, err := net.SplitHostPort(k.APIEndpoint)
	if err != nil {
		return ""
	}
	return host
}

// Release is the Kubernetes release the file asks for: its own, "stable" (resolved at apply
// time), or the one tow tests when it names none.
func (k Kubernetes) Release() string {
	if k.Version == "" {
		return DefaultKubernetesVersion
	}
	return k.Version
}

// ControlPlaneCount is how many control-plane machines the cluster has.
func (c *Config) ControlPlaneCount() int {
	if c.Mode == ModeBYOH {
		return len(c.Spec.ControlPlane.Machines)
	}
	return c.Spec.ControlPlane.Count
}

// WorkerCount is how many worker machines the cluster has, over every pool.
func (c *Config) WorkerCount() int {
	n := 0
	for _, p := range c.Spec.Workers {
		if c.Mode == ModeBYOH {
			n += len(p.Machines)
		} else {
			n += p.Count
		}
	}
	return n
}

func (c *Config) MachineCount() int { return c.ControlPlaneCount() + c.WorkerCount() }

// Location is where provisioned machines are created: the cloud's region or the Proxmox node.
func (c *Config) Location() string {
	i := c.Spec.Infrastructure
	switch {
	case i.AWS != nil:
		return i.AWS.Region
	case i.GCP != nil:
		return i.GCP.Region
	case i.Azure != nil:
		return i.Azure.Location
	case i.Proxmox != nil:
		return i.Proxmox.Node
	}
	return ""
}

// Warnings are choices that work but deserve a second look; apply prints them.
func (c *Config) Warnings() []string { return c.warnings }
