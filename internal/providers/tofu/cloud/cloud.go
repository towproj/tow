// Package cloud is the contract between the OpenTofu adapter and each provision target.
// Every target lives in its own package next to this one (aws, proxmox, ...) holding its
// OpenTofu module and the Go code for what OpenTofu doesn't do. Only those packages may
// talk to a cloud's API or import its SDK; the rest of tow sees this interface alone.
package cloud

import (
	"context"
	"errors"
	"fmt"
	"io/fs"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
)

// Permanent marks a credential problem that waiting can't fix, like a token read from the
// environment when tow started: PreFlight then fails at once instead of offering a retry.
func Permanent(err error) error { return permanentError{err} }

func IsPermanent(err error) bool {
	var p permanentError
	return errors.As(err, &p)
}

type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

type Cloud interface {
	// Module is the target's OpenTofu module. It takes the shared inputs (cluster_name,
	// region, machines, ...) and returns the shared outputs (nodes, api_endpoint).
	Module() fs.FS
	// MachineType is the OpenTofu resource type of the module's machines (its
	// resource "<type>" "node"), keyed by machine name.
	MachineType() string
	// AdminRules are the module's firewall rules keyed by an admin address or a port (their
	// OpenTofu addresses, key left out): a run that changes the admin addresses or the ports
	// may replace or delete them. None when the module changes its rules in place.
	AdminRules() []string
	// Vars places the machines, filling each one's Zone or Address, and returns the module's
	// inputs beyond the shared ones. prev is the module's last inputs, nil for a new cluster:
	// a machine listed there keeps its place. Never a credential: those reach OpenTofu
	// through the environment or the cloud's own config files.
	Vars(ctx context.Context, spec providers.Spec, machines []Machine, prev *Inputs) (map[string]any, error)
	// AdminCIDR is the address this machine reaches the new machines from: SSH and the API
	// port are opened to it only.
	AdminCIDR(ctx context.Context, m *providers.MachineRequest) (string, error)
	// BeforeCreate runs the target's own checks before machines are created or changed (apply
	// and its dry run, never delete), e.g. that fixed addresses aren't already taken.
	BeforeCreate(ctx context.Context, spec providers.Spec, machines []Machine) error
	// Identity checks the target's credentials and says who tow acts as.
	Identity(ctx context.Context, m *providers.MachineRequest) (string, error)
	// Instances lists the cluster's machines that still exist, found by the cluster tag.
	Instances(ctx context.Context, m *providers.MachineRequest, cluster string) ([]Instance, error)
	// Leftovers lists every resource still tagged for the cluster: the check after a destroy.
	Leftovers(ctx context.Context, m *providers.MachineRequest, cluster string) ([]string, error)
}

// Machine is one entry of the modules' machines input. Every field is always sent: the
// modules' object type has them all, an empty one meaning the module's default or unused.
type Machine struct {
	Name         string `json:"name"`
	Role         string `json:"role"`
	Pool         string `json:"pool"`
	InstanceType string `json:"instance_type"` // clouds
	CPUs         int    `json:"cpus"`          // hypervisors
	MemoryMB     int    `json:"memory_mb"`     // hypervisors
	DiskGB       int    `json:"disk_gb"`
	Zone         string `json:"zone"`    // AWS: the availability zone
	Address      string `json:"address"` // Proxmox: the static address
}

// Machines lists the machines the pools ask for, named as they always were: control planes
// <cluster>-cp-<i>, workers <cluster>-<pool>-<i>. Control planes come first, then each pool
// in the file's order; the first control plane is the one the others join.
func Machines(cluster string, pools []providers.Pool) []Machine {
	var out []Machine
	for _, role := range []string{config.RoleControlPlane, config.RoleWorker} {
		for _, p := range pools {
			if p.Role != role {
				continue
			}
			for i := range p.Count {
				name := fmt.Sprintf("%s-%s-%d", cluster, p.Name, i)
				if p.Role == config.RoleControlPlane {
					name = fmt.Sprintf("%s-cp-%d", cluster, i)
				}
				out = append(out, Machine{Name: name, Role: p.Role, Pool: p.Name, InstanceType: p.InstanceType,
					CPUs: p.CPUs, MemoryMB: p.MemoryMB, DiskGB: p.DiskGB})
			}
		}
	}
	return out
}

// Inputs is what later runs reuse from a module's last inputs (the inputs file kept next to
// OpenTofu's state): where each machine went, and what was detected or generated when the
// cluster was created.
type Inputs struct {
	Machines            []Machine         `json:"machines"`
	AdminCIDRs          []string          `json:"admin_cidrs"`
	OpenPorts           []int             `json:"open_ports"`
	Tags                map[string]string `json:"tags"`
	SelfDestructMinutes int               `json:"self_destruct_minutes"`
	Zones               []string          `json:"zones"` // AWS
}

// Placed maps each machine of the last inputs to its entry; empty for a new cluster.
func (in *Inputs) Placed() map[string]Machine {
	out := map[string]Machine{}
	if in != nil {
		for _, m := range in.Machines {
			out[m.Name] = m
		}
	}
	return out
}

type Instance struct {
	ID    string
	Role  string // control-plane or worker, from the role tag
	State string // running, pending, stopped, ...
}

// Tags every module puts on its resources; the Go side finds resources by them.
const (
	TagCluster = "tow-cluster"
	TagRole    = "tow-role"
	TagExpires = "tow-expires" // unix seconds; only set when TOW_RESOURCE_TTL is
)
