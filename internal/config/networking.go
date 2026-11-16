package config

import (
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"
)

const (
	CNINone = "none" // no CNI: nodes stay NotReady until the user installs one

	DefaultClusterDomain = "cluster.local"
)

type Networking struct {
	PodCIDR       string   `yaml:"podCIDR,omitempty"`
	ServiceCIDR   string   `yaml:"serviceCIDR,omitempty"`
	ClusterDomain string   `yaml:"clusterDomain,omitempty"`
	CNI           string   `yaml:"cni,omitempty"` // flannel | calico | cilium | none
	Flannel       *Flannel `yaml:"flannel,omitempty"`
	Calico        *Calico  `yaml:"calico,omitempty"`
	Cilium        *Cilium  `yaml:"cilium,omitempty"`
}

type Flannel struct {
	Version string `yaml:"version,omitempty"` // not on k3s, which bundles flannel
	Backend string `yaml:"backend,omitempty"` // vxlan | host-gw | wireguard; empty = flannel's default
}

type Calico struct {
	Version       string `yaml:"version,omitempty"`
	Encapsulation string `yaml:"encapsulation,omitempty"` // vxlan (default) | vxlan-cross-subnet | ipip | ipip-cross-subnet | none
	// Dataplane is what applies policies and Services on each node: iptables (Calico's
	// default) or ebpf, which replaces kube-proxy.
	Dataplane string `yaml:"dataplane,omitempty"`
}

// Calico's dataplanes.
const (
	CalicoIptables = "iptables"
	CalicoEBPF     = "ebpf"
)

// ReplacesKubeProxy reports whether the CNI takes over kube-proxy's job: Cilium with
// kubeProxyReplacement, or Calico's eBPF dataplane. The distribution then runs without it.
func (n Networking) ReplacesKubeProxy() bool {
	switch {
	case n.CNI == CNICilium && n.Cilium != nil && n.Cilium.KubeProxyReplacement != nil:
		return *n.Cilium.KubeProxyReplacement
	case n.CNI == CNICalico && n.Calico != nil:
		return n.Calico.Dataplane == CalicoEBPF
	}
	return false
}

type Cilium struct {
	Version              string `yaml:"version,omitempty"`
	KubeProxyReplacement *bool  `yaml:"kubeProxyReplacement,omitempty"` // empty = Cilium's default
	Encryption           string `yaml:"encryption,omitempty"`           // wireguard | none
}

var cniChoices = map[string]choice{
	CNIFlannel: {},
	CNICalico:  {distributions: remote},
	CNICilium:  {distributions: remote},
	CNINone:    {},
}

var dnsSubdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?(\.[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?)*$`)

// validateCNI checks the CNI choice, its options, and the cluster's DNS domain.
func (c *Config) validateCNI() []error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	const at = "spec.kubernetes.networking"
	n := c.Spec.Kubernetes.Networking

	add(c.checkChoice(at+".cni", n.CNI, cniChoices))
	errs = append(errs, checkBlocks(at, []optionBlock{
		{CNIFlannel, n.Flannel != nil}, {CNICalico, n.Calico != nil}, {CNICilium, n.Cilium != nil},
	}, func(name string) bool { return name == n.CNI }, "cni is "+n.CNI)...)

	if f := n.Flannel; f != nil && n.CNI == CNIFlannel {
		add(c.checkVersion(at+".flannel.version", CNIFlannel, f.Version))
		add(oneOf(at+".flannel.backend", f.Backend, "vxlan", "host-gw", "wireguard"))
	}
	if p := n.Calico; p != nil && n.CNI == CNICalico {
		add(c.checkVersion(at+".calico.version", CNICalico, p.Version))
		add(oneOf(at+".calico.encapsulation", p.Encapsulation, "vxlan", "vxlan-cross-subnet", "ipip", "ipip-cross-subnet", "none"))
		add(oneOf(at+".calico.dataplane", p.Dataplane, CalicoIptables, CalicoEBPF))
		if p.Dataplane == CalicoEBPF && strings.HasPrefix(p.Encapsulation, "ipip") {
			add(fmt.Errorf("%s.calico.encapsulation %s doesn't work with dataplane ebpf (Calico docs: \"IPIP is not supported\"); use vxlan, or none on a network that routes pod addresses", at, p.Encapsulation))
		}
	}
	if p := n.Cilium; p != nil && n.CNI == CNICilium {
		add(c.checkVersion(at+".cilium.version", CNICilium, p.Version))
		add(oneOf(at+".cilium.encryption", p.Encryption, "wireguard", "none"))
	}

	if len(n.ClusterDomain) > 253 || !dnsSubdomain.MatchString(n.ClusterDomain) {
		add(fmt.Errorf("%s.clusterDomain %q must be a lowercase DNS name like cluster.local", at, n.ClusterDomain))
	}
	return errs
}

// oneOf refuses a value outside its list; empty means the tool's own default.
func oneOf(path, v string, allowed ...string) error {
	if v == "" || contains(allowed, v) {
		return nil
	}
	return fmt.Errorf("%s %q must be %s", path, v, joinOr(allowed))
}

// validateNetworking checks the pod and service ranges against each other and against every
// address the machines use: Kubernetes would route those to pods instead.
func (c *Config) validateNetworking() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	n := c.Spec.Kubernetes.Networking

	parse := func(field, v string) (netip.Prefix, bool) {
		p, err := netip.ParsePrefix(v)
		if err != nil || !p.Addr().Is4() {
			add("spec.kubernetes.networking.%s %q must be an IPv4 range like 10.244.0.0/16", field, v)
			return netip.Prefix{}, false
		}
		return p.Masked(), true
	}
	pods, okP := parse("podCIDR", n.PodCIDR)
	svcs, okS := parse("serviceCIDR", n.ServiceCIDR)
	if !okP || !okS {
		return errs
	}
	if pods.Overlaps(svcs) {
		add("spec.kubernetes.networking: podCIDR %s and serviceCIDR %s overlap", pods, svcs)
	}

	// The machines' own network.
	var machineNet []struct{ what, v string }
	switch in := c.Spec.Infrastructure; c.Provider() {
	case ProviderAWS, ProviderGCP, ProviderAzure:
		machineNet = append(machineNet, struct{ what, v string }{c.cloudNetworkName(), c.cloudNetwork()})
	case ProviderProxmox:
		if in.Network != nil {
			machineNet = append(machineNet, struct{ what, v string }{"spec.infrastructure.network.subnet", in.Network.Subnet})
		}
	case ProviderBYOH:
		for _, g := range c.groups() {
			for _, m := range g.Machines {
				if ip, err := netip.ParseAddr(m.Host); err == nil {
					machineNet = append(machineNet, struct{ what, v string }{"machine " + m.Host, netip.PrefixFrom(ip, ip.BitLen()).String()})
				}
			}
		}
	}
	if host, _, err := net.SplitHostPort(c.Spec.Kubernetes.APIEndpoint); err == nil {
		if ip, err := netip.ParseAddr(host); err == nil {
			machineNet = append(machineNet, struct{ what, v string }{"spec.kubernetes.apiEndpoint", netip.PrefixFrom(ip, ip.BitLen()).String()})
		}
	}
	for _, m := range machineNet {
		p, err := netip.ParsePrefix(m.v)
		if err != nil {
			continue // reported by the provider's own checks
		}
		for _, r := range []struct {
			name string
			p    netip.Prefix
		}{{"podCIDR", pods}, {"serviceCIDR", svcs}} {
			if r.p.Overlaps(p) {
				add("spec.kubernetes.networking.%s %s overlaps %s (%s); pick a range your machines don't use", r.name, r.p, m.what, p)
			}
		}
	}
	return errs
}

// cloudNetwork is the range of the network a public cloud's module creates: AWS's VPC, GCP's
// subnet, Azure's VNet.
func (c *Config) cloudNetwork() string {
	if n := c.Spec.Infrastructure.Network; n != nil && n.Subnet != "" {
		return n.Subnet
	}
	return DefaultCloudNetwork
}

// cloudNetworkName names it in messages: "the AWS VPC".
func (c *Config) cloudNetworkName() string {
	t := cloudTerms[c.Provider()]
	return "the " + t.name + " " + t.network
}

// addrRange is a run of IPv4 addresses with a name for messages.
type addrRange struct {
	what        string
	first, last netip.Addr
}

func prefixRange(what string, p netip.Prefix) addrRange {
	p = p.Masked()
	return addrRange{what, p.Addr(), lastAddr(p)}
}

func (a addrRange) overlaps(b addrRange) bool { return !a.last.Less(b.first) && !b.last.Less(a.first) }

func (a addrRange) contains(b addrRange) bool { return !b.first.Less(a.first) && !a.last.Less(b.last) }
