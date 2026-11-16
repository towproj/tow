package addons

import (
	"fmt"
	"net/netip"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubevip"
	"github.com/towproj/tow/internal/providers"
)

// Sources (read 2026-11-11), followed here:
//   - MetalLB 0.16 with Helm (metallb.github.io/metallb, FRR-K8s mode by default): its
//     namespace labeled pod-security privileged first ("the speaker pod requires elevated
//     permission"), then an IPAddressPool and an L2Advertisement, or BGPPeers (v1beta2) and a
//     BGPAdvertisement  https://metallb.io/installation/ https://metallb.io/configuration/
//   - kube-vip-cloud-provider v0.0.13: its manifest at the tag, and the kubevip ConfigMap in
//     kube-system with range-global (comma-separated ranges)
//     https://github.com/kube-vip/kube-vip-cloud-provider
//   - kube-vip for Services: svc_enable on the kube-vip that serves the API, or a DaemonSet of
//     its own  https://kube-vip.io/docs/usage/cloud-provider/
//   - Cilium 1.20 LB IPAM: CiliumLoadBalancerIPPool (cilium.io/v2), blocks of cidr or
//     start/stop; L2 announcements (l2announcements.enabled, a CiliumL2AnnouncementPolicy,
//     cilium.io/v2alpha1); BGP (bgpControlPlane.enabled, CiliumBGPClusterConfig,
//     CiliumBGPPeerConfig, CiliumBGPAdvertisement, cilium.io/v2). Both need the kube-proxy
//     replacement  https://docs.cilium.io/en/stable/network/lb-ipam/
//     https://docs.cilium.io/en/stable/network/l2-announcements/
//     https://docs.cilium.io/en/stable/network/bgp-control-plane/bgp-control-plane-configuration/

const (
	MetalLBVersion       = "0.16.2"
	KubeVIPCloudProvider = "0.0.13"

	metallbCharts = "https://metallb.github.io/metallb"
	poolName      = "tow" // the address pool tow makes, whichever load balancer serves it
)

// ServiceLBInstalled reports whether tow installs a service load balancer (servicelb is
// k3s's own; AWS's arrives later).
func ServiceLBInstalled(provider string) bool {
	switch provider {
	case config.ServiceLBMetalLB, config.ServiceLBKubeVIP, config.ServiceLBCilium:
		return true
	}
	return false
}

// serviceLB is the file's service load balancer and its address pool. Cilium's own values
// go with the CNI's install (ciliumServiceLBValues); its pool and announcements come here.
func (o Options) serviceLB() []component {
	k := o.Kubernetes
	lb := k.ServiceLoadBalancer
	switch lb.Provider {
	case config.ServiceLBMetalLB:
		return []component{o.metallb(lb.MetalLB)}
	case config.ServiceLBKubeVIP:
		return []component{o.kubeVIPServices(lb.KubeVIP, k.APILoadBalancer == config.APILBKubeVIP)}
	case config.ServiceLBCilium:
		return []component{o.ciliumPool(lb.Cilium)}
	}
	return nil
}

func (o Options) metallb(p *config.PoolController) component {
	c := o.versioned(config.ServiceLBMetalLB, p.Version, MetalLBVersion)
	c.role, c.owns = roleServiceLB, "metallb-system"
	var cidrs []string
	for _, a := range p.Addresses {
		cidrs = append(cidrs, metallbAddress(a))
	}
	objects := fmt.Sprintf(`apiVersion: metallb.io/v1beta1
kind: IPAddressPool
metadata: {name: %s, namespace: metallb-system}
spec:
  addresses: [%s]
`, poolName, strings.Join(cidrs, ", "))
	if p.Mode == config.ModeBGP {
		for i, peer := range p.BGP.Peers {
			objects += fmt.Sprintf(`---
apiVersion: metallb.io/v1beta2
kind: BGPPeer
metadata: {name: tow-%d, namespace: metallb-system}
spec: {myASN: %d, peerASN: %d, peerAddress: %s}
`, i, p.BGP.LocalASN, peer.ASN, peer.Address)
		}
		objects += fmt.Sprintf(`---
apiVersion: metallb.io/v1beta1
kind: BGPAdvertisement
metadata: {name: %s, namespace: metallb-system}
spec: {ipAddressPools: [%s]}
`, poolName, poolName)
	} else {
		objects += fmt.Sprintf(`---
apiVersion: metallb.io/v1beta1
kind: L2Advertisement
metadata: {name: %s, namespace: metallb-system}
spec: {ipAddressPools: [%s]}
`, poolName, poolName)
	}
	c.steps = []step{
		o.apply(`apiVersion: v1
kind: Namespace
metadata:
  name: metallb-system
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/audit: privileged
    pod-security.kubernetes.io/warn: privileged
`),
		o.helmStep("metallb", "metallb", metallbCharts, c.version, "metallb-system", nil),
		o.apply(objects),
	}
	return c
}

// kubeVIPServices: kube-vip-cloud-provider hands out the pool's addresses; the kube-vip that
// serves the API announces them (set up with Kubernetes), or a DaemonSet of its own does.
func (o Options) kubeVIPServices(p *config.PoolController, apiKubeVIP bool) component {
	c := o.versioned("kube-vip-cloud-provider", p.Version, KubeVIPCloudProvider)
	c.role, c.deployment = roleServiceLB, "kube-system/kube-vip-cloud-provider"
	v := c.version
	var ranges []string
	for _, a := range p.Addresses {
		first, last := bounds(a)
		ranges = append(ranges, first+"-"+last)
	}
	steps := []step{
		{program: "kubectl", args: []string{"apply", "--server-side", "--kubeconfig", o.Kubeconfig, "-f",
			fmt.Sprintf("https://raw.githubusercontent.com/kube-vip/kube-vip-cloud-provider/v%s/manifest/kube-vip-cloud-controller.yaml", v)}},
		o.apply(fmt.Sprintf(`apiVersion: v1
kind: ConfigMap
metadata: {name: kubevip, namespace: kube-system}
data:
  range-global: %q
`, strings.Join(ranges, ","))),
	}
	if !apiKubeVIP {
		ds, _ := kubevip.ServicesDaemonSet() // plain structs: marshaling can't fail
		steps = append(steps, o.apply(string(ds)))
	}
	steps = append(steps, step{program: "kubectl", args: []string{"rollout", "status", "deployment/kube-vip-cloud-provider", "--namespace", "kube-system", "--kubeconfig", o.Kubeconfig, "--timeout", chartTimeout}})
	if !apiKubeVIP {
		steps = append(steps, step{program: "kubectl", args: []string{"rollout", "status", "daemonset/kube-vip-svc", "--namespace", "kube-system", "--kubeconfig", o.Kubeconfig, "--timeout", chartTimeout}})
	}
	c.steps = steps
	return c
}

// ciliumPool is Cilium's address pool and how it's announced: L2, or BGP to the file's peers.
func (o Options) ciliumPool(p *config.AddressPool) component {
	var blocks []string
	for _, a := range p.Addresses {
		if pfx, err := netip.ParsePrefix(a); err == nil {
			blocks = append(blocks, fmt.Sprintf("  - cidr: %q", pfx.Masked().String()))
			continue
		}
		first, last := bounds(a)
		blocks = append(blocks, fmt.Sprintf("  - start: %q\n    stop: %q", first, last))
	}
	objects := fmt.Sprintf(`apiVersion: cilium.io/v2
kind: CiliumLoadBalancerIPPool
metadata: {name: %s}
spec:
  blocks:
%s
`, poolName, strings.Join(blocks, "\n"))
	if p.Mode == config.ModeBGP {
		var peers []string
		for i, peer := range p.BGP.Peers {
			peers = append(peers, fmt.Sprintf("    - name: peer-%d\n      peerASN: %d\n      peerAddress: %q\n      peerConfigRef: {name: %s}", i, peer.ASN, peer.Address, poolName))
		}
		objects += fmt.Sprintf(`---
apiVersion: cilium.io/v2
kind: CiliumBGPClusterConfig
metadata: {name: %[1]s}
spec:
  bgpInstances:
  - name: %[1]s
    localASN: %[2]d
    peers:
%[3]s
---
apiVersion: cilium.io/v2
kind: CiliumBGPPeerConfig
metadata: {name: %[1]s}
spec:
  families:
  - afi: ipv4
    safi: unicast
    advertisements:
      matchLabels: {advertise: %[1]s}
---
apiVersion: cilium.io/v2
kind: CiliumBGPAdvertisement
metadata:
  name: %[1]s
  labels: {advertise: %[1]s}
spec:
  advertisements:
  - advertisementType: Service
    service:
      addresses: [LoadBalancerIP]
    selector:
      matchExpressions:
      - {key: towproj.github.io/never, operator: NotIn, values: ["never"]}
`, poolName, p.BGP.LocalASN, strings.Join(peers, "\n"))
	} else {
		objects += fmt.Sprintf(`---
apiVersion: cilium.io/v2alpha1
kind: CiliumL2AnnouncementPolicy
metadata: {name: %s}
spec:
  loadBalancerIPs: true
`, poolName)
	}
	return component{name: "cilium load balancer pool", role: roleServiceLB, steps: []step{o.apply(objects)}}
}

// CiliumServiceLBValues are the Cilium values its load balancer needs: L2 announcements, or
// the BGP control plane. nil when Cilium isn't the service load balancer.
func CiliumServiceLBValues(lb config.ServiceLoadBalancer) map[string]any {
	if lb.Provider != config.ServiceLBCilium || lb.Cilium == nil {
		return nil
	}
	if lb.Cilium.Mode == config.ModeBGP {
		return map[string]any{"bgpControlPlane": map[string]any{"enabled": true}}
	}
	return map[string]any{"l2announcements": map[string]any{"enabled": true}}
}

// bounds are an address pool entry's first and last address: a range, a CIDR or one
// address (config checked them).
func bounds(a string) (string, string) {
	if first, last, err := providers.ParseRange(a); err == nil {
		return first.String(), last.String()
	}
	if p, err := netip.ParsePrefix(a); err == nil && p.Addr().Is4() {
		p = p.Masked()
		b := p.Addr().As4()
		host := uint32(1)<<(32-p.Bits()) - 1
		n := (uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])) | host
		last := netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
		return p.Addr().String(), last.String()
	}
	return a, a
}

// metallbAddress is a pool entry as MetalLB takes it: a range or a CIDR (one address = /32).
func metallbAddress(a string) string {
	if ip, err := netip.ParseAddr(a); err == nil {
		return netip.PrefixFrom(ip, ip.BitLen()).String()
	}
	return a
}
