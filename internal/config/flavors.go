package config

import (
	"sort"
	"strings"
)

const (
	FlavorLocal      = "local"
	FlavorHomelab    = "homelab"
	FlavorDevTest    = "dev-test"
	FlavorProduction = "production"

	DistributionK3d     = "k3d"
	DistributionK3s     = "k3s"
	DistributionKubeadm = "kubeadm"

	CNIFlannel = "flannel"
	CNICalico  = "calico"
	CNICilium  = "cilium"
)

// Flavor is an optional, ready-made starting point for people who don't know what to pick.
// It only fills in what the file leaves out: every value can be overridden, and it adds no
// rule of its own. Without a flavor you still get Kubernetes itself, just none of the optional
// components (CNI, storage, ingress, load balancers, metrics-server) the file doesn't name.
type Flavor struct {
	Description   string
	Provider      string // only local picks one; the others work with any provider
	Distribution  string
	CNI           string
	Storage       []string
	Ingress       []string
	ServiceLB     string
	MetricsServer bool
	Schedulable   string // empty = auto
	ControlPlanes int
	Workers       int // provisioned and local clusters; byoh lists its machines
}

// local and homelab are k3d and k3s exactly as they ship. dev-test and production have no
// service load balancer: it needs an address pool only the user knows.
var flavors = map[string]Flavor{
	FlavorLocal: {
		Description:   "Throwaway cluster on this machine (k3d in Docker, as it ships), for development",
		Provider:      ProviderLocal,
		Distribution:  DistributionK3d,
		CNI:           CNIFlannel,
		Storage:       []string{CSILocalPath},
		Ingress:       []string{IngressTraefik},
		ServiceLB:     ServiceLBServiceLB,
		MetricsServer: true,
		Schedulable:   SchedulableTrue,
		ControlPlanes: 1,
	},
	FlavorHomelab: {
		Description:   "Lightweight k3s cluster as it ships, e.g. on Raspberry Pis",
		Distribution:  DistributionK3s,
		CNI:           CNIFlannel,
		Storage:       []string{CSILocalPath},
		Ingress:       []string{IngressTraefik},
		ServiceLB:     ServiceLBServiceLB,
		MetricsServer: true,
		Schedulable:   SchedulableTrue,
		ControlPlanes: 1,
		Workers:       1,
	},
	FlavorDevTest: {
		Description:   "Upstream Kubernetes for staging and CI, no high availability",
		Distribution:  DistributionKubeadm,
		CNI:           CNICalico,
		Storage:       []string{CSILocalPath},
		Ingress:       []string{IngressTraefik},
		MetricsServer: true,
		ControlPlanes: 1,
		Workers:       1,
	},
	FlavorProduction: {
		Description:   "Highly available upstream Kubernetes with eBPF networking and replicated storage",
		Distribution:  DistributionKubeadm,
		CNI:           CNICilium,
		Storage:       []string{CSILonghorn},
		Ingress:       []string{IngressTraefik},
		MetricsServer: true,
		ControlPlanes: 3,
		Workers:       2,
	},
}

// Distribution is where a Kubernetes distribution runs, and its own default ranges.
type Distribution struct {
	RemoteNodes bool // installs onto provisioned or listed machines over SSH; false = local
	PodCIDR     string
	ServiceCIDR string
}

var distributions = map[string]Distribution{
	DistributionK3d: {PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16"},
	DistributionK3s: {RemoteNodes: true, PodCIDR: "10.42.0.0/16", ServiceCIDR: "10.43.0.0/16"},
	// Calico's own default pod range (192.168.0.0/16) clashes with home networks; 10.244/16 doesn't.
	DistributionKubeadm: {RemoteNodes: true, PodCIDR: "10.244.0.0/16", ServiceCIDR: "10.96.0.0/12"},
}

func flavorNames() string { return strings.Join(sortedKeys(flavors), ", ") }

func distributionNames() []string { return sortedKeys(distributions) }

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// joinOr lists choices for a message: "a, b or c".
func joinOr(list []string) string {
	switch len(list) {
	case 0:
		return ""
	case 1:
		return list[0]
	}
	return strings.Join(list[:len(list)-1], ", ") + " or " + list[len(list)-1]
}
