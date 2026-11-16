package providers

import (
	"cmp"
	"context"
	"errors"
	"net"

	"github.com/towproj/tow/internal/tools"
)

// Provider gets infrastructure ready behind a single contract. One adapter per mode:
// k3d builds a whole local cluster, tofu creates machines, byoh uses the user's own.
type Provider interface {
	Name() string
	// PreFlight checks tools and credentials before anything is created or destroyed.
	PreFlight(ctx context.Context, spec Spec) error
	// Plan describes what Provision would do, without side effects. Backs --dry-run.
	Plan(ctx context.Context, spec Spec) ([]string, error)
	Provision(ctx context.Context, spec Spec) (*Result, error)
	Status(ctx context.Context, spec Spec) (StatusResult, error)
	Teardown(ctx context.Context, spec Spec) error
}

// Tooled is a provider that runs a program on this machine (OpenTofu). Before PreFlight, the
// orchestrator finds a release of it that works, or installs one after a yes.
type Tooled interface {
	Tool() tools.Tool
}

// Reconciler is a provider with infrastructure of its own (OpenTofu), which can drift from
// what it created: on a running cluster tow asks it what differs, and Provision puts it
// back.
type Reconciler interface {
	// Changes says what Provision would change on infrastructure that already exists. The
	// spec's Guard applies, as for Provision.
	Changes(ctx context.Context, spec Spec) (Planned, error)
}

// Planned is what Provision would change: one line per kind of change (nil when nothing
// would), and the machines it would create, by name.
type Planned struct {
	Lines    []string
	Creating []string
}

// Guard says what a provider may not change on infrastructure that already exists. The
// orchestrator sets it for each run; it is never saved.
type Guard int

const (
	// GuardOff: a new cluster, or machines nothing was installed on yet. Anything may change.
	GuardOff Guard = iota
	// GuardKeep: Kubernetes may run on the machines. Nothing is replaced or deleted.
	GuardKeep
	// GuardRunning: a running cluster whose file didn't change. Nothing is replaced, deleted
	// or added, and what was detected when it was created (this machine's address, a test
	// run's expiry) is kept.
	GuardRunning
)

// ErrNotImplemented marks adapters whose Phase 1 step hasn't landed yet.
var ErrNotImplemented = errors.New("not implemented yet")

type Spec struct {
	Name     string `json:"name"`
	Version  string `json:"k8s_version,omitempty"` // concrete release; "stable" is resolved before providers see it
	Engine   string `json:"engine,omitempty"`      // the distribution: k3s or kubeadm on machines, k3d locally
	CNI      string `json:"cni,omitempty"`         // remote nodes: decides which ports nodes must reach on each other
	Endpoint string `json:"endpoint,omitempty"`    // API address in front of several control planes (byoh, proxmox)
	// ServiceLBCIDRs are the addresses LoadBalancer Services get (the file's pool, as CIDRs):
	// the machines' firewall lets the admin addresses reach them (proxmox).
	ServiceLBCIDRs []string `json:"service_lb_cidrs,omitempty"`
	// OpenPorts are TCP ports the machines' firewall opens to the admin addresses: 80 and 443
	// for the cluster's ingress, and the file's access.ports.
	OpenPorts []int `json:"open_ports,omitempty"`
	// Storage are the file's storage drivers: the aws module gives the EBS CSI driver its
	// permissions; proxmox checks what its CSI plugin needs.
	Storage     []string        `json:"storage,omitempty"`
	PodCIDR     string          `json:"pod_cidr,omitempty"`     // for Phase 2; Phase 1 only checks overlaps
	ServiceCIDR string          `json:"service_cidr,omitempty"` // same
	Nodes       []Node          `json:"nodes,omitempty"`        // byoh: existing machines to use
	Machines    *MachineRequest `json:"machines,omitempty"`     // provision: machines to create
	Local       *LocalRequest   `json:"local,omitempty"`        // local: k3d servers and agents
	Kubernetes  *KubernetesSpec `json:"kubernetes,omitempty"`   // what k3d installs, and where every cluster's kubeconfig goes; nil = k3d as it ships

	// SecretEnv names the variables holding the file's secrets: kept from every program
	// tow starts. Names only, never values.
	SecretEnv []string `json:"secret_env,omitempty"`

	// Guard is what this run may not change on existing infrastructure; not saved.
	Guard Guard `json:"-"`
	// Adding and Removing name the machines this run creates or destroys on purpose: the
	// guard lets exactly those through on a running cluster. Not saved.
	Adding   []string `json:"-"`
	Removing []string `json:"-"`
	// Known names the cluster's machines: one missing (deleted outside tow) may be created
	// again under its name, for the cluster to take it back. Not saved.
	Known []string `json:"-"`
}

// KubernetesSpec is what a provider that installs Kubernetes itself (k3d) needs from the
// cluster file: k3s's own flags and the role labels. Every cluster's spec carries where its
// kubeconfig goes, so delete can remove the context without the file.
type KubernetesSpec struct {
	ServerFlags      []Flag            `json:"server_flags,omitempty"`       // every server (control plane)
	AgentFlags       []Flag            `json:"agent_flags,omitempty"`        // every agent (worker)
	ServerRoleLabels map[string]string `json:"server_role_labels,omitempty"` // node-role.kubernetes.io/... set through the API
	AgentRoleLabels  map[string]string `json:"agent_role_labels,omitempty"`
	Context          string            `json:"context"` // kubeconfig context to create
	Merge            bool              `json:"merge"`   // into the user's kubeconfig; false = the state folder's copy only
}

// Flag is one k3s flag. With Env, the value continues with that variable's content, read
// when it's applied; the state only ever holds the variable's name.
type Flag struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
	Env   string `json:"env,omitempty"`
}

// LocalRequest sizes a local cluster: k3d's --servers and --agents.
type LocalRequest struct {
	Servers int `json:"servers"`
	Agents  int `json:"agents"`
}

type Node struct {
	Name      string `json:"name,omitempty"`       // node name; filled in by the provider, never by the user
	Host      string `json:"host"`                 // address tow connects to over SSH
	PrivateIP string `json:"private_ip,omitempty"` // address the other nodes reach this one on
	Role      string `json:"role"`                 // control-plane or worker
	Pool      string `json:"pool,omitempty"`       // worker pool name from the file
	User      string `json:"user"`
	KeyPath   string `json:"key_path,omitempty"` // empty = ssh-agent, then default ~/.ssh keys
	Port      int    `json:"port"`
}

// Label names the machine in messages: its address, and its node name when that differs.
func (n Node) Label() string {
	if n.Name != "" && n.Name != n.Host {
		return n.Host + " (" + n.Name + ")"
	}
	return n.Host
}

// MachineRequest asks a provision target for machines, pool by pool: the control plane
// first, then each worker pool in file order.
type MachineRequest struct {
	Cloud   string `json:"cloud"`
	Region  string `json:"region"`            // the cloud's region, or the Proxmox node
	Project string `json:"project,omitempty"` // GCP: the project the machines go in
	// Subscription is the Azure subscription the machines go in.
	Subscription string   `json:"subscription,omitempty"`
	Pools        []Pool   `json:"pools"`
	SSHUser      string   `json:"ssh_user"`
	SSHKeyPath   string   `json:"ssh_key_path,omitempty"`
	AdminCIDRs   []string `json:"admin_cidrs,omitempty"` // empty = this machine's address, detected
	OS           string   `json:"os,omitempty"`          // clouds: the image's system (config.OSUbuntu2404, ...)

	// Hypervisors (Proxmox) have no managed networks, so the request spells out where the VMs
	// go and their addresses. On AWS, Network carries only the VPC's subnet.
	URL      string         `json:"url,omitempty"`      // API address
	Template int            `json:"template,omitempty"` // ID of the template VM to clone
	Storage  string         `json:"storage,omitempty"`
	Bridge   string         `json:"bridge,omitempty"`
	CPUType  string         `json:"cpu_type,omitempty"` // the VMs' CPU model
	Network  *StaticNetwork `json:"network,omitempty"`
}

// Pool is a set of identical machines with one role. Zero sizes mean the target's default.
type Pool struct {
	Name         string `json:"name"` // "control-plane" or the worker pool's name
	Role         string `json:"role"`
	Count        int    `json:"count"`
	InstanceType string `json:"instance_type,omitempty"` // clouds
	CPUs         int    `json:"cpus,omitempty"`          // hypervisors
	MemoryMB     int    `json:"memory_mb,omitempty"`     // hypervisors
	DiskGB       int    `json:"disk_gb,omitempty"`
}

func (m *MachineRequest) count(role string) int {
	n := 0
	for _, p := range m.Pools {
		if p.Role == role || role == "" {
			n += p.Count
		}
	}
	return n
}

// ControlPlanes, Workers and Total count machines over the pools.
func (m *MachineRequest) ControlPlanes() int { return m.count("control-plane") }
func (m *MachineRequest) Workers() int       { return m.count("worker") }
func (m *MachineRequest) Total() int         { return m.count("") }

// StaticNetwork hands out fixed addresses from the range: each machine keeps the one it got,
// and a new machine gets the lowest one free.
type StaticNetwork struct {
	Subnet    string   `json:"subnet"`              // e.g. 192.168.1.0/24
	Addresses string   `json:"addresses,omitempty"` // first-last, e.g. 192.168.1.20-192.168.1.29
	Gateway   string   `json:"gateway,omitempty"`
	DNS       []string `json:"dns,omitempty"`
}

// Result is what every provider hands to the next stage, whether it created or adopted the machines.
type Result struct {
	Nodes        []Node `json:"nodes,omitempty"`         // empty for local engines that manage their own nodes
	Endpoint     string `json:"endpoint,omitempty"`      // API server address, when the provider knows it
	NodeEndpoint string `json:"node_endpoint,omitempty"` // the nodes' API address, when not Endpoint: AWS HA's internal load balancer (Endpoint, the public one, is for the user)
	// NodeAPIName is the name the nodes reach the API by, when a machine behind the internal
	// load balancer can't reach it (GCP): each machine resolves it in its /etc/hosts, to
	// NodeEndpoint's address on a worker, to itself on a control plane (prepare.HostsScript).
	NodeAPIName string `json:"node_api_name,omitempty"`
	// StorageConfig is what the cloud's storage driver needs from the machines' module, known
	// once they exist (Azure Disk: the tenant and the managed identity's client ID). IDs only,
	// never a secret.
	StorageConfig map[string]string `json:"storage_config,omitempty"`
	KubeContext   string            `json:"kube_context,omitempty"` // kubeconfig context the provider created, if any
	Kubeconfig    string            `json:"kubeconfig,omitempty"`   // path of the cluster's kubeconfig in the state folder
}

// NodeAPI is where the nodes reach the API in front of several control planes: the API name
// at NodeEndpoint's port, else NodeEndpoint, else Endpoint ("" when none).
func (r *Result) NodeAPI() string {
	if r.NodeAPIName != "" {
		port := "6443"
		if _, p, err := net.SplitHostPort(r.NodeEndpoint); err == nil {
			port = p
		}
		return net.JoinHostPort(r.NodeAPIName, port)
	}
	return cmp.Or(r.NodeEndpoint, r.Endpoint)
}

// NodeAPIAddress is the address the API name stands for on a worker: NodeEndpoint's host.
func (r *Result) NodeAPIAddress() string {
	if host, _, err := net.SplitHostPort(r.NodeEndpoint); err == nil {
		return host
	}
	return r.NodeEndpoint
}

// StatusResult is the machines as the provider sees them: how many of each role run.
type StatusResult struct {
	Healthy       bool   `json:"healthy"`
	State         string `json:"state"`          // ready, degraded, stopped, not_found, error
	ControlPlanes string `json:"control_planes"` // running/wanted: "3/3"
	Workers       string `json:"workers"`        // "0/0"
	Message       string `json:"message"`        // why it isn't healthy
}
