package config

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/towproj/tow/internal/providers"
)

// What serves spec.kubernetes.apiEndpoint in front of several control planes.
const (
	APILBKubeVIP  = "kube-vip" // a virtual IP the control planes share
	APILBExternal = "external" // the user's own load balancer (byoh)
)

// Service load balancers: what gives LoadBalancer Services an address. Exactly one.
const (
	ServiceLBServiceLB = "servicelb"
	ServiceLBMetalLB   = "metallb"
	ServiceLBKubeVIP   = "kube-vip"
	ServiceLBCilium    = "cilium"
	ServiceLBAWS       = "aws"
	ServiceLBNone      = "none"

	ModeL2  = "l2"
	ModeBGP = "bgp"
)

type ServiceLoadBalancer struct {
	Provider string          `yaml:"provider,omitempty"` // servicelb | metallb | kube-vip | cilium | aws | none
	MetalLB  *PoolController `yaml:"metallb,omitempty"`
	KubeVIP  *PoolController `yaml:"kube-vip,omitempty"`
	Cilium   *AddressPool    `yaml:"cilium,omitempty"` // its version follows the CNI's
	AWS      *Pinned         `yaml:"aws,omitempty"`
}

// PoolController is a load balancer that hands out addresses from a pool the user owns.
type PoolController struct {
	Version     string `yaml:"version,omitempty"`
	AddressPool `yaml:",inline"`
}

type AddressPool struct {
	Mode      string   `yaml:"mode,omitempty"`      // l2 | bgp; default l2, or bgp when bgp is set
	Addresses []string `yaml:"addresses,omitempty"` // ranges like 192.168.1.200-192.168.1.220 or 192.168.1.240/28
	BGP       *BGP     `yaml:"bgp,omitempty"`
}

type BGP struct {
	LocalASN uint32    `yaml:"localASN,omitempty"`
	Peers    []BGPPeer `yaml:"peers,omitempty"`
}

type BGPPeer struct {
	Address string `yaml:"address,omitempty"`
	ASN     uint32 `yaml:"asn,omitempty"`
}

var serviceLBChoices = map[string]choice{
	ServiceLBServiceLB: {distributions: []string{DistributionK3s, DistributionK3d}, why: "it comes bundled with k3s"},
	ServiceLBMetalLB:   {distributions: remote},
	ServiceLBKubeVIP:   {distributions: remote},
	ServiceLBCilium:    {distributions: remote, cni: CNICilium},
	ServiceLBAWS:       {distributions: remote, providers: []string{ProviderAWS}, why: "it creates AWS load balancers"},
	ServiceLBNone:      {},
}

// validateAPILoadBalancer checks what serves the API address.
func (c *Config) validateAPILoadBalancer() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	k := c.Spec.Kubernetes
	const at = "spec.kubernetes.apiLoadBalancer"
	if k.APILoadBalancer == "" {
		return nil
	}
	if err := oneOf(at, k.APILoadBalancer, APILBKubeVIP, APILBExternal); err != nil {
		return []error{err}
	}
	if k.APIEndpoint == "" {
		add("%s needs spec.kubernetes.apiEndpoint: the address it serves", at)
	}
	// kube-vip only moves an address between the control planes: the API servers answer on
	// it at their own port.
	if host, port, err := net.SplitHostPort(k.APIEndpoint); err == nil && k.APILoadBalancer == APILBKubeVIP {
		if ip, err := netip.ParseAddr(host); err != nil || !ip.Is4() {
			add("spec.kubernetes.apiEndpoint %q: kube-vip holds an IPv4 address, not a name; use a free address on the control planes' network, like 192.168.1.100:6443", k.APIEndpoint)
		}
		if port != "6443" {
			add("spec.kubernetes.apiEndpoint %q: with kube-vip the port is the API server's own, 6443", k.APIEndpoint)
		}
	}
	if c.Provider() == ProviderProxmox && k.APILoadBalancer == APILBExternal {
		add("%s external isn't used on proxmox: the API address is a kube-vip virtual IP (your own load balancer is for byoh)", at)
	}
	return errs
}

// validateServiceLB checks the service load balancer, its options and its address pool.
func (c *Config) validateServiceLB() []error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	addf := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	const at = "spec.kubernetes.serviceLoadBalancer"
	lb := c.Spec.Kubernetes.ServiceLoadBalancer

	add(c.checkChoice(at+".provider", lb.Provider, serviceLBChoices))
	errs = append(errs, checkBlocks(at, []optionBlock{
		{ServiceLBMetalLB, lb.MetalLB != nil}, {ServiceLBKubeVIP, lb.KubeVIP != nil},
		{ServiceLBCilium, lb.Cilium != nil}, {ServiceLBAWS, lb.AWS != nil},
	}, func(name string) bool { return name == lb.Provider }, "provider is "+lb.Provider)...)

	var pool *AddressPool
	switch lb.Provider {
	case ServiceLBMetalLB, ServiceLBKubeVIP:
		p := lb.MetalLB
		if lb.Provider == ServiceLBKubeVIP {
			p = lb.KubeVIP
		}
		if p == nil {
			addf("%s.%s is required with provider %s: the addresses it hands out", at, lb.Provider, lb.Provider)
			break
		}
		add(c.checkVersion(at+"."+lb.Provider+".version", lb.Provider, p.Version))
		pool = &p.AddressPool
		if lb.Provider == ServiceLBKubeVIP && p.Mode == ModeBGP {
			addf("%s.kube-vip.mode bgp isn't supported yet: kube-vip needs a BGP router ID per node; use mode l2, or provider metallb or cilium for BGP", at)
		}
	case ServiceLBCilium:
		if lb.Cilium == nil {
			addf("%s.cilium is required with provider cilium: the addresses it hands out", at)
			break
		}
		pool = lb.Cilium
		// Cilium's L2 announcements and its BGP control plane announce what its kube-proxy
		// replacement serves (Cilium docs, prerequisites).
		if c.Spec.Kubernetes.Networking.CNI == CNICilium && !c.Spec.Kubernetes.Networking.ReplacesKubeProxy() {
			addf("%s provider cilium needs spec.kubernetes.networking.cilium.kubeProxyReplacement: true (Cilium's load balancer works through it)", at)
		}
	case ServiceLBAWS:
		if lb.AWS != nil {
			add(c.checkVersion(at+".aws.version", ServiceLBAWS, lb.AWS.Version))
		}
	}
	if pool != nil {
		errs = append(errs, c.validatePool(at+"."+lb.Provider, pool)...)
	}
	return errs
}

// validatePool checks an address pool: its mode, BGP peers, and that its addresses are free.
func (c *Config) validatePool(at string, p *AddressPool) []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if err := oneOf(at+".mode", p.Mode, ModeL2, ModeBGP); err != nil {
		errs = append(errs, err)
	}
	switch p.Mode {
	case ModeL2:
		if p.BGP != nil {
			add("%s.bgp is set but mode is l2", at)
		}
		switch {
		case c.Provider() == ProviderAWS:
			add("%s.mode l2 can't work on aws: a VPC ignores layer-2 announcements (use provider aws, or mode bgp with your own BGP router)", at)
		case c.PublicCloud():
			add("%s.mode l2 can't work on %s: a VPC ignores layer-2 announcements", at, c.Provider())
		}
	case ModeBGP:
		if b := p.BGP; b == nil || b.LocalASN == 0 || len(b.Peers) == 0 {
			add("%s.bgp is required with mode bgp: localASN and at least one peer (address, asn)", at)
		} else {
			for i, peer := range b.Peers {
				if ip, err := netip.ParseAddr(peer.Address); err != nil || !ip.Is4() {
					add("%s.bgp.peers[%d].address %q must be an IPv4 address", at, i, peer.Address)
				}
				if peer.ASN == 0 {
					add("%s.bgp.peers[%d].asn is required", at, i)
				}
			}
		}
	}

	if len(p.Addresses) == 0 {
		add("%s.addresses is required: the addresses LoadBalancer Services get, like [192.168.1.200-192.168.1.220]", at)
		return errs
	}
	var ranges []addrRange
	for i, a := range p.Addresses {
		r, err := parseAddresses(fmt.Sprintf("%s.addresses[%d] %s", at, i, a), a)
		if err != nil {
			add("%s.addresses[%d] %q must be a range like 192.168.1.200-192.168.1.220 or 192.168.1.240/28", at, i, a)
			continue
		}
		for _, prev := range ranges {
			if r.overlaps(prev) {
				add("%s.addresses[%d] %s overlaps %s", at, i, a, prev.what)
			}
		}
		ranges = append(ranges, r)
	}

	taken := c.takenAddresses()
	var subnet *addrRange
	if n := c.Spec.Infrastructure.Network; c.Provider() == ProviderProxmox && n != nil {
		if s, err := netip.ParsePrefix(n.Subnet); err == nil && s.Addr().Is4() {
			r := prefixRange("spec.infrastructure.network.subnet", s)
			subnet = &r
		}
	}
	for i, r := range ranges {
		for _, t := range taken {
			if r.overlaps(t) {
				add("%s.addresses[%d] %s overlaps %s; pick addresses nothing else uses", at, i, p.Addresses[i], t.what)
			}
		}
		if subnet != nil && p.Mode == ModeL2 && !subnet.contains(r) {
			add("%s.addresses[%d] %s must be inside %s (%s): layer-2 announcements reach only the local network", at, i, p.Addresses[i], subnet.what, c.Spec.Infrastructure.Network.Subnet)
		}
	}
	return errs
}

// parseAddresses reads a pool entry: first-last, a CIDR, or a single address.
func parseAddresses(what, v string) (addrRange, error) {
	if strings.Contains(v, "-") {
		first, last, err := providers.ParseRange(v)
		return addrRange{what, first, last}, err
	}
	if strings.Contains(v, "/") {
		p, err := netip.ParsePrefix(v)
		if err != nil || !p.Addr().Is4() {
			return addrRange{}, fmt.Errorf("not an IPv4 range")
		}
		return prefixRange(what, p), nil
	}
	ip, err := netip.ParseAddr(v)
	if err != nil || !ip.Is4() {
		return addrRange{}, fmt.Errorf("not an IPv4 address")
	}
	return addrRange{what, ip, ip}, nil
}

// takenAddresses are the addresses a load balancer pool must leave alone: the pod and service
// ranges, the machines and their network, and the API address.
func (c *Config) takenAddresses() []addrRange {
	var out []addrRange
	n := c.Spec.Kubernetes.Networking
	for _, r := range []struct{ name, v string }{{"podCIDR", n.PodCIDR}, {"serviceCIDR", n.ServiceCIDR}} {
		if p, err := netip.ParsePrefix(r.v); err == nil && p.Addr().Is4() {
			out = append(out, prefixRange("spec.kubernetes.networking."+r.name, p))
		}
	}
	single := func(what, v string) {
		if ip, err := netip.ParseAddr(v); err == nil && ip.Is4() {
			out = append(out, addrRange{what, ip, ip})
		}
	}
	in := c.Spec.Infrastructure
	switch c.Provider() {
	case ProviderAWS, ProviderGCP, ProviderAzure:
		if p, err := netip.ParsePrefix(c.cloudNetwork()); err == nil && p.Addr().Is4() {
			out = append(out, prefixRange(c.cloudNetworkName(), p))
		}
	case ProviderProxmox:
		if in.Network != nil {
			if first, last, err := providers.ParseRange(in.Network.Addresses); err == nil {
				out = append(out, addrRange{"spec.infrastructure.network.addresses (the VMs)", first, last})
			}
			single("spec.infrastructure.network.gateway", in.Network.Gateway)
		}
	case ProviderBYOH:
		for _, g := range c.groups() {
			for _, m := range g.Machines {
				single("machine "+m.Host, m.Host)
			}
		}
	}
	if host, _, err := net.SplitHostPort(c.Spec.Kubernetes.APIEndpoint); err == nil {
		single("spec.kubernetes.apiEndpoint", host)
	}
	return out
}
