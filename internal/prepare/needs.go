// Package prepare gets machines ready for Kubernetes, following the official documentation
// of the distribution and of every component the cluster file selects. Each item is checked
// first and changed only when it's missing; every change is recorded on the machine in
// /etc/tow/prepared.json, with how to undo it.
//
// Sources (read 2026-11-09; RHEL family 2026-11-14):
//   - kubeadm's install page, Red Hat-based: SELinux permissive (setenforce 0 and
//     SELINUX=permissive in /etc/selinux/config)
//   - kubeadm "Before you begin": unique MAC and product_uuid, swap off, required ports
//     https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/install-kubeadm/
//   - Container runtimes: net.ipv4.ip_forward=1 in /etc/sysctl.d/k8s.conf; other settings
//     are the network plugin's to ask for
//     https://kubernetes.io/docs/setup/production-environment/container-runtimes/
//   - k3s requirements: ufw/firewalld rules, Raspberry Pi memory cgroups, kernel-modules-extra
//     on RHEL 10  https://docs.k3s.io/installation/requirements (read 2026-11-14)
//   - Longhorn on RHEL: iscsi-initiator-utils and nfs-utils (its install page, same packages)
//   - flannel: "requires the br_netfilter module" (kubeadm stopped checking in 1.30)
//   - Calico: kernel 5.10+, NetworkManager must leave its interfaces alone, no other iptables
//     manager (firewalld, ufw)  https://docs.tigera.io/calico/latest/getting-started/kubernetes/requirements
//   - Cilium: kernel 5.10+  https://docs.cilium.io/en/stable/operations/system_requirements/
//   - Longhorn: open-iscsi, nfs-common, iscsi_tcp loaded  https://longhorn.io/docs/latest/deploy/install/
//     and multipathd kept off its volumes ("blacklist { devnode "^sd[a-z0-9]+" }", read
//     2026-11-11)  https://longhorn.io/kb/troubleshooting-volume-with-multipath/
package prepare

import (
	"slices"
	"strconv"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
)

// poolAddresses are the address pool of the service load balancer tow installs.
func poolAddresses(lb config.ServiceLoadBalancer) []string {
	switch {
	case lb.Provider == config.ServiceLBMetalLB && lb.MetalLB != nil:
		return lb.MetalLB.Addresses
	case lb.Provider == config.ServiceLBKubeVIP && lb.KubeVIP != nil:
		return lb.KubeVIP.Addresses
	case lb.Provider == config.ServiceLBCilium && lb.Cilium != nil:
		return lb.Cilium.Addresses
	}
	return nil
}

// PoolCIDRs are the service load balancer's addresses as CIDRs, for the firewalls in front
// of the machines (the Proxmox module).
func PoolCIDRs(lb config.ServiceLoadBalancer) []string {
	var out []string
	for _, entry := range poolAddresses(lb) {
		out = append(out, providers.CIDRs(entry)...)
	}
	return out
}

// Needs is what one node must have before Kubernetes goes on it. Why says who asks for it.
type Needs struct {
	Distro    string // the distribution that goes on the node: k3s or kubeadm
	SwapOff   bool   // kubeadm: the kubelet refuses to start when it finds swap
	Packages  []Item // packages, by their name on each family
	Modules   []Item // kernel modules, loaded now and at boot
	Sysctls   []Item // kernel settings, now and at boot (/etc/sysctl.d/k8s.conf)
	MinKernel *Item  // Value: the oldest kernel release that works, like "5.10"

	// SELinuxPermissive: kubeadm's install page sets SELinux to permissive ("required to
	// allow containers to access the host filesystem"), now and at boot.
	SELinuxPermissive bool
	// MinMemoryMB is the memory the machine must show (MemTotal), as kubeadm's preflight
	// checks it on a control plane: caught before anything changes; 0 = no check.
	MinMemoryMB int

	// QuietNetworkManager are interface patterns NetworkManager must leave alone (Calico).
	QuietNetworkManager []string
	// QuietMultipath names a component multipathd must leave alone (Longhorn): its fix
	// blacklists sd* disks, refused where multipath manages devices.
	QuietMultipath string
	// NoFirewallManager names a component that asks for no ufw or firewalld on the node.
	NoFirewallManager string

	// Firewall is what the node's own firewall (ufw or firewalld) must let in, when it's on.
	// Cloud security groups and the Proxmox firewall are set by the OpenTofu modules instead.
	Firewall []Rule
}

// Item is one package, module or setting, and who needs it.
type Item struct {
	Name, Value, Why string

	// Packages only. RPM is the package's name on RHEL-family machines; "" = Name. Only
	// limits it to one family (OnlyDebian, OnlyRHEL) or release (OnlyRHEL10); "" = every
	// machine. RunningKernel: the build for the running kernel (kernel-modules-extra-$(uname -r)).
	RPM           string
	Only          string
	RunningKernel bool
}

// Item.Only values.
const (
	OnlyDebian = "debian"
	OnlyRHEL   = "rhel"
	OnlyRHEL10 = "rhel10" // RHEL 10 and newer
)

// Rule lets traffic in from one kind of source. Port "" means every port and protocol.
type Rule struct {
	From  string // nodes | pods | services | admin | anywhere
	Port  string // 6443, or a range like 30000:32767
	Proto string // tcp | udp
	To    string // a destination CIDR, any port (LoadBalancer Services' addresses); then no Port
	Why   string
}

// Rule sources.
const (
	FromNodes    = "nodes"    // the cluster's other machines
	FromPods     = "pods"     // spec.kubernetes.networking.podCIDR
	FromServices = "services" // spec.kubernetes.networking.serviceCIDR
	FromAdmin    = "admin"    // the machine running tow, as the node sees it
	FromAnywhere = "anywhere"
)

// calicoInterfaces are the ones Calico's docs tell NetworkManager to leave alone.
var calicoInterfaces = []string{
	"interface-name:cali*", "interface-name:tunl*", "interface-name:vxlan.calico",
	"interface-name:vxlan-v6.calico", "interface-name:wireguard.cali", "interface-name:wg-v6.cali",
}

// NeedsFor lists what a node of this role needs for the cluster the file describes.
func NeedsFor(cfg *config.Config, role string) Needs {
	k := cfg.Spec.Kubernetes
	n := Needs{Distro: k.Distribution}

	switch k.Distribution {
	case config.DistributionK3s:
		// k3s loads its own kernel modules and sets its own sysctls when it starts. On RHEL 10
		// the modules it loads come in a package of their own (k3s docs, requirements), built
		// for each kernel: the running one's.
		const why = "the k3s installer (get.k3s.io)"
		n.Packages = append(n.Packages, Item{Name: "ca-certificates", Why: why}, Item{Name: "curl", Why: why})
	case config.DistributionKubeadm:
		const why = "the Kubernetes package repository (pkgs.k8s.io)"
		// dnf checks a repository's key itself; apt needs gpg to store it.
		n.Packages = append(n.Packages, Item{Name: "ca-certificates", Why: why}, Item{Name: "curl", Why: why}, Item{Name: "gpg", Why: why, Only: OnlyDebian})
		n.SwapOff, n.SELinuxPermissive = true, true
		if role == config.RoleControlPlane {
			n.MinMemoryMB = 1700 // kubeadm's preflight: "the system RAM is less than the minimum 1700 MB"
		}
		n.Sysctls = append(n.Sysctls, Item{Name: "net.ipv4.ip_forward", Value: "1", Why: "IPv4 forwarding (Kubernetes container runtime docs)"})
		if !k.Networking.ReplacesKubeProxy() {
			// kube-proxy sets nf_conntrack_max when it starts and exits when the setting isn't
			// there; it only is once the module is loaded, which a firewall or iptables rules do
			// as a side effect. A machine without either (AlmaLinux 10 on GCP) never loads it.
			// k3s loads it itself.
			n.Modules = append(n.Modules, Item{Name: "nf_conntrack", Why: "kube-proxy sets its connection tracking limits when it starts"})
		}
		if k.Networking.CNI == config.CNICalico || k.Networking.CNI == config.CNIFlannel {
			// Both chain the portmap plugin (hostPort), which runs the machine's own iptables;
			// k3s brings its own. Most images have it; AlmaLinux 10's on GCP doesn't.
			n.Packages = append(n.Packages, Item{Name: "iptables", RPM: "iptables-nft", Why: "hostPort: the CNI's portmap plugin runs iptables"})
		}
		if k.Networking.CNI == config.CNIFlannel {
			const why = "flannel requires the br_netfilter module"
			n.Modules = append(n.Modules, Item{Name: "br_netfilter", Why: why})
			n.Sysctls = append(n.Sysctls,
				Item{Name: "net.bridge.bridge-nf-call-iptables", Value: "1", Why: "pod traffic on flannel's bridge must go through iptables"},
				Item{Name: "net.bridge.bridge-nf-call-ip6tables", Value: "1", Why: "pod traffic on flannel's bridge must go through iptables"})
		}
	}

	// RHEL 10 moved the kernel modules the network plugins and kube-proxy load (nft_compat,
	// the xt_ matches, ip_set, br_netfilter) into a package of its own, built for each kernel:
	// the running one's (k3s docs, requirements; the same modules with k3s or kubeadm).
	if k.Distribution == config.DistributionK3s || k.Distribution == config.DistributionKubeadm {
		n.Packages = append(n.Packages, Item{Name: "kernel-modules-extra", Why: "the network plugins' kernel modules on RHEL 10 (k3s docs)", Only: OnlyRHEL10, RunningKernel: true})
	}

	switch k.Networking.CNI {
	case config.CNICalico:
		n.MinKernel = &Item{Name: "kernel", Value: "5.10", Why: "Calico"}
		n.QuietNetworkManager = calicoInterfaces
		n.NoFirewallManager = "Calico"
	case config.CNICilium:
		n.MinKernel = &Item{Name: "kernel", Value: "5.10", Why: "Cilium"}
	}

	if slices.Contains(k.Storage.CSI, config.CSILonghorn) {
		const why = "Longhorn"
		n.Packages = append(n.Packages, Item{Name: "open-iscsi", RPM: "iscsi-initiator-utils", Why: why + " volumes (iSCSI)"},
			Item{Name: "nfs-common", RPM: "nfs-utils", Why: why + " shared volumes and backups (NFSv4)"})
		n.Modules = append(n.Modules, Item{Name: "iscsi_tcp", Why: why + " (loaded before iscsid starts)"})
		n.QuietMultipath = why
	}

	// The node's own firewall. The cluster's machines trust each other fully, as the AWS and
	// Proxmox modules already do: the control plane, kubelets, etcd and every CNI's overlay
	// (VXLAN, IP-in-IP, WireGuard, BGP) talk between them. Pods and Services reach the nodes,
	// as k3s's docs ask. This machine reaches the API and NodePort Services.
	n.Firewall = []Rule{
		{From: FromNodes, Why: "the cluster's nodes talk to each other (API, kubelet, etcd, the CNI)"},
		{From: FromPods, Why: "pods reach the nodes (k3s docs)"},
		{From: FromServices, Why: "Services reach the nodes (k3s docs)"},
	}
	if role == config.RoleControlPlane {
		n.Firewall = append(n.Firewall, Rule{From: FromAdmin, Port: "6443", Proto: "tcp", Why: "the Kubernetes API, from this machine"})
	}
	n.Firewall = append(n.Firewall,
		Rule{From: FromAdmin, Port: "30000:32767", Proto: "tcp", Why: "NodePort Services, from this machine"},
		Rule{From: FromAdmin, Port: "30000:32767", Proto: "udp", Why: "NodePort Services, from this machine"})
	// The addresses LoadBalancer Services get from the pool, any port, from this machine: the
	// node that announces one receives its traffic.
	for _, entry := range poolAddresses(k.ServiceLoadBalancer) {
		for _, cidr := range providers.CIDRs(entry) {
			n.Firewall = append(n.Firewall, Rule{From: FromAdmin, To: cidr, Why: "LoadBalancer Services' addresses, from this machine"})
		}
	}
	// The ports the cluster's own ingress controllers and the file's access.ports listen on,
	// from the admin addresses (widened with adminCIDRs).
	for _, p := range OpenPorts(cfg) {
		n.Firewall = append(n.Firewall, Rule{From: FromAdmin, Port: strconv.Itoa(p), Proto: "tcp", Why: "port " + strconv.Itoa(p) + " (ingress, or spec.infrastructure.access.ports), from the admin addresses"})
	}
	return n
}

// OpenPorts are the TCP ports the machines' firewalls open to the admin addresses: 80 and 443
// for the cluster's ingress controllers, then the file's access.ports.
func OpenPorts(cfg *config.Config) []int {
	var out []int
	if len(cfg.Spec.Kubernetes.Ingress.Controllers) > 0 {
		out = append(out, 80, 443)
	}
	for _, p := range cfg.Spec.Infrastructure.Access.Ports {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}
