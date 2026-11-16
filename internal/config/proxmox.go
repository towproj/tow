package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"

	"github.com/towproj/tow/internal/providers"
)

const (
	DefaultProxmoxStorage = "local-lvm"
	DefaultProxmoxBridge  = "vmbr0"
)

// Default VM size: the smallest kubeadm accepts (2 CPUs, ~1.7 GB) with room for a CNI and a
// few pods; it suits k3s too.
var (
	DefaultProxmoxResources = Resources{CPU: 2, Memory: "2Gi"}
	DefaultProxmoxDisk      = "20Gi"
)

func (c *Config) proxmoxDefaults() {
	p := c.Spec.Infrastructure.Proxmox
	if p == nil {
		return // reported by validateProxmox
	}
	if p.Storage == "" {
		p.Storage = DefaultProxmoxStorage
	}
	if p.Bridge == "" {
		p.Bridge = DefaultProxmoxBridge
	}
	if p.CPUType == "" {
		p.CPUType = DefaultProxmoxCPUType
	}
	for _, g := range c.groups() {
		if g.Resources == nil {
			g.Resources = &Resources{}
		}
		if g.Resources.CPU == 0 {
			g.Resources.CPU = DefaultProxmoxResources.CPU
		}
		if g.Resources.Memory == "" {
			g.Resources.Memory = DefaultProxmoxResources.Memory
		}
		if g.Disk == "" {
			g.Disk = DefaultProxmoxDisk
		}
	}
}

func (c *Config) validateProxmox() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	in := c.Spec.Infrastructure
	p := in.Proxmox

	if p == nil {
		add("spec.infrastructure.proxmox is required: url, node and template")
	} else {
		if u, err := url.Parse(p.URL); p.URL == "" {
			add("spec.infrastructure.proxmox.url is required: the Proxmox API address, like https://192.168.1.10:8006")
		} else if err != nil || u.Scheme != "https" || u.Hostname() == "" || (u.Path != "" && u.Path != "/") {
			add("spec.infrastructure.proxmox.url %q must be the Proxmox API address, like https://192.168.1.10:8006", p.URL)
		}
		if p.Node == "" {
			add("spec.infrastructure.proxmox.node is required: the Proxmox server the VMs run on (shown under Datacenter in the web UI, e.g. pve)")
		}
		if p.Template < 100 {
			add("spec.infrastructure.proxmox.template is required: the ID (100 or more) of a cloud-init template VM to clone, e.g. 9000")
		}
	}

	// VM sizes against the distribution's minimum for each role.
	for _, g := range c.namedGroups() {
		if g.Resources == nil {
			continue
		}
		m, ok := MinimumFor(c.Distribution(), g.role)
		if !ok {
			continue
		}
		if g.Resources.CPU < m.CPUs {
			add("%s.resources.cpu is %d; a %s needs at least %d", g.path, g.Resources.CPU, m.What, m.CPUs)
		}
		if mib, err := MiB(g.Resources.Memory); err != nil {
			add("%s.resources.memory: %v", g.path, err)
		} else if mib < m.MemoryMB {
			add("%s.resources.memory is %dMi; a %s needs at least %dMi", g.path, mib, m.What, m.MemoryMB)
		}
	}
	errs = append(errs, c.validateDisks()...)

	var first, last netip.Addr
	if in.Network == nil {
		add("spec.infrastructure.network is required on proxmox: VMs get fixed addresses (subnet, addresses, gateway, dns)")
	} else {
		var nerrs []error
		first, last, nerrs = validateNetwork(in.Network, c.MachineCount())
		errs = append(errs, nerrs...)
	}

	if host, _, err := net.SplitHostPort(c.Spec.Kubernetes.APIEndpoint); err == nil && first.IsValid() {
		if ip, err := netip.ParseAddr(host); err == nil && !ip.Less(first) && !last.Less(ip) {
			add("spec.kubernetes.apiEndpoint %s is inside spec.infrastructure.network.addresses, which tow gives to VMs; pick an address outside the range", host)
		}
		// kube-vip announces it on the VMs' network (ARP), which doesn't cross routers.
		if ip, err := netip.ParseAddr(host); err == nil {
			if subnet, err := netip.ParsePrefix(in.Network.Subnet); err == nil && !subnet.Masked().Contains(ip) {
				add("spec.kubernetes.apiEndpoint %s isn't in spec.infrastructure.network.subnet %s: kube-vip announces it on the VMs' network; pick a free address there, outside the VM range", host, subnet)
			}
		}
	}
	return errs
}

func validateNetwork(n *Network, machines int) (first, last netip.Addr, errs []error) {
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	const at = "spec.infrastructure.network"

	subnet, err := netip.ParsePrefix(n.Subnet)
	if err != nil || !subnet.Addr().Is4() {
		add("%s.subnet %q must be an IPv4 network like 192.168.1.0/24", at, n.Subnet)
		return first, last, errs
	}
	subnet = subnet.Masked()
	broadcast := lastAddr(subnet)

	first, last, err = providers.ParseRange(n.Addresses)
	if err != nil {
		add("%s.addresses: %v", at, err)
		return netip.Addr{}, netip.Addr{}, errs
	}
	switch {
	case !subnet.Contains(first) || !subnet.Contains(last):
		add("%s.addresses %s must be inside subnet %s", at, n.Addresses, subnet)
	case first == subnet.Addr() || last == broadcast:
		add("%s.addresses %s includes the subnet's network or broadcast address", at, n.Addresses)
	case providers.RangeSize(first, last) < machines:
		add("%s.addresses %s holds %d addresses, but %d VMs need one each", at, n.Addresses, providers.RangeSize(first, last), machines)
	}

	if gw, err := netip.ParseAddr(n.Gateway); err != nil || !gw.Is4() {
		add("%s.gateway %q must be an IPv4 address", at, n.Gateway)
	} else if !subnet.Contains(gw) {
		add("%s.gateway %s is outside subnet %s", at, gw, subnet)
	} else if !gw.Less(first) && !last.Less(gw) {
		add("%s.gateway %s is inside the VM address range %s", at, gw, n.Addresses)
	}

	if len(n.DNS) == 0 {
		add("%s.dns needs at least one DNS server, like [1.1.1.1]", at)
	}
	for _, d := range n.DNS {
		if _, err := netip.ParseAddr(d); err != nil {
			add("%s.dns: %q is not an IP address", at, d)
		}
	}
	return first, last, errs
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Addr().As4()
	for i := p.Bits(); i < 32; i++ {
		b[i/8] |= 1 << (7 - i%8)
	}
	return netip.AddrFrom4(b)
}
