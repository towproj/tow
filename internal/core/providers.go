package core

import (
	"context"
	"fmt"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/byoh"
	"github.com/towproj/tow/internal/providers/k3d"
	"github.com/towproj/tow/internal/providers/tofu"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
)

// The only place adapters are named; the CLI never imports them.
const (
	ProviderK3d  = "k3d"
	ProviderTofu = "tofu"
	ProviderBYOH = "byoh"
)

var providerForMode = map[string]string{
	config.ModeLocal:     ProviderK3d,
	config.ModeProvision: ProviderTofu,
	config.ModeBYOH:      ProviderBYOH,
}

// newProvider is a var so tests can swap in fakes.
var newProvider = func(name string) (providers.Provider, error) {
	switch name {
	case ProviderK3d:
		return k3d.New(), nil
	case ProviderTofu:
		return tofu.New(), nil
	case ProviderBYOH:
		return byoh.New(), nil
	default:
		return nil, fmt.Errorf("unsupported provider: %s", name)
	}
}

// findProviderTool makes sure the program a provider runs on this machine (OpenTofu) is
// there before its PreFlight: found, or installed after a yes (apply --install-tools installs
// without asking; a dry run only says what would be installed).
func (o *Orchestrator) findProviderTool(ctx context.Context, p providers.Provider) error {
	t, ok := p.(providers.Tooled)
	if !ok {
		return nil
	}
	_, err := findTool(ctx, t.Tool(), tools.Options{Install: o.installTools, DryRun: o.dryRun})
	return err
}

// specFromState rebuilds what the provider needs for status and teardown; a state saved
// without one gives just the name, which is all k3d needs.
func specFromState(st *state.ClusterState) providers.Spec {
	if st.Spec != nil {
		return *st.Spec
	}
	return providers.Spec{Name: st.Name}
}

// specFromConfig translates the user-facing config into the provider contract. The config is
// validated, so sizes parse.
func specFromConfig(cfg *config.Config, version string) providers.Spec {
	k := cfg.Spec.Kubernetes
	spec := providers.Spec{Name: cfg.Name(), Version: version, Endpoint: k.APIEndpoint, SecretEnv: cfg.SecretEnv(),
		ServiceLBCIDRs: prepare.PoolCIDRs(k.ServiceLoadBalancer), OpenPorts: prepare.OpenPorts(cfg), Storage: k.Storage.CSI}

	switch cfg.Mode {
	case config.ModeLocal:
		spec.Local = &providers.LocalRequest{Servers: cfg.ControlPlaneCount(), Agents: cfg.WorkerCount()}
		// k3d installs Kubernetes itself, so it gets the file's settings as k3s flags.
		ks := &providers.KubernetesSpec{ServerFlags: k3s.ServerFlags(cfg), Context: k.Kubeconfig.Context, Merge: *k.Kubeconfig.Merge}
		_, ks.ServerRoleLabels = config.SplitLabels(cfg.Spec.ControlPlane.Labels)
		if len(cfg.Spec.Workers) > 0 {
			ks.AgentFlags = k3s.AgentFlags(cfg, &cfg.Spec.Workers[0])
			_, ks.AgentRoleLabels = config.SplitLabels(cfg.Spec.Workers[0].Labels)
		}
		spec.Kubernetes = ks
		return spec
	}
	// Machines: the installer gets its settings from the file; the spec keeps where the
	// kubeconfig goes, for delete.
	spec.Kubernetes = &providers.KubernetesSpec{Context: k.Kubeconfig.Context, Merge: *k.Kubeconfig.Merge}
	spec.Engine, spec.CNI = k.Distribution, k.Networking.CNI
	spec.PodCIDR, spec.ServiceCIDR = k.Networking.PodCIDR, k.Networking.ServiceCIDR

	if cfg.Mode == config.ModeBYOH {
		add := func(role, pool string, ms []config.Machine) {
			for _, m := range ms {
				spec.Nodes = append(spec.Nodes, providers.Node{
					Host: m.Host, Role: role, Pool: pool, User: m.SSH.User, KeyPath: m.SSH.KeyPath, Port: m.SSH.Port,
				})
			}
		}
		add(config.RoleControlPlane, "", cfg.Spec.ControlPlane.Machines)
		for _, w := range cfg.Spec.Workers {
			add(config.RoleWorker, w.Name, w.Machines)
		}
		return spec
	}

	in := cfg.Spec.Infrastructure
	m := &providers.MachineRequest{
		Cloud:      in.Provider,
		Region:     cfg.Location(),
		SSHUser:    in.Access.SSH.User,
		SSHKeyPath: in.Access.SSH.KeyPath,
		AdminCIDRs: in.Access.AdminCIDRs,
		OS:         in.OS,
	}
	m.Pools = append(m.Pools, pool(config.RoleControlPlane, config.RoleControlPlane, cfg.Spec.ControlPlane.Group))
	for _, w := range cfg.Spec.Workers {
		m.Pools = append(m.Pools, pool(w.Name, config.RoleWorker, w.Group))
	}
	if p := in.Proxmox; p != nil {
		m.URL, m.Template, m.Storage, m.Bridge, m.CPUType = p.URL, p.Template, p.Storage, p.Bridge, p.CPUType
	}
	if g := in.GCP; g != nil {
		m.Project = g.Project
	}
	if a := in.Azure; a != nil {
		m.Subscription = a.Subscription
	}
	if n := in.Network; n != nil {
		m.Network = &providers.StaticNetwork{Subnet: n.Subnet, Addresses: n.Addresses, Gateway: n.Gateway, DNS: n.DNS}
	}
	spec.Machines = m
	return spec
}

func pool(name, role string, g config.Group) providers.Pool {
	p := providers.Pool{Name: name, Role: role, Count: g.Count, InstanceType: g.InstanceType}
	if g.Resources != nil {
		p.CPUs = g.Resources.CPU
		p.MemoryMB, _ = config.MiB(g.Resources.Memory)
	}
	if g.Disk != "" {
		p.DiskGB, _ = config.GiB(g.Disk)
	}
	return p
}
