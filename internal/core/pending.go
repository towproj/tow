package core

import (
	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
)

// Pending lists what the file asks for that this version doesn't install yet, so nothing
// is silently ignored. Each item arrives with its Phase 2 step; the list shrinks as they land.
func Pending(cfg *config.Config) []string {
	// Every distribution installs Kubernetes, the role labels, the CNI (flannel bundled with
	// k3s; Calico and Cilium, and flannel on kubeadm, installed by tow), the storage
	// drivers of the first batch, metrics-server, the Helm charts and the manifests. k3s and
	// k3d bring a few add-ons themselves; kubeadm brings none. The rest arrives with its own
	// step.
	k := cfg.Spec.Kubernetes
	d := k.Distribution
	var out []string
	for _, c := range k.Storage.CSI {
		if _, installed := addons.StorageClasses[c]; !installed {
			out = append(out, "storage "+c)
		}
	}
	if lb := k.ServiceLoadBalancer.Provider; lb != config.ServiceLBNone && !addons.Bundled(d, lb) && !addons.ServiceLBInstalled(lb) {
		out = append(out, "service load balancer "+lb)
	}
	// Every ingress controller the file accepts (traefik, cilium) is installed.
	return out
}
