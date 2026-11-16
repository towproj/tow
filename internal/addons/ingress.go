package addons

import (
	"github.com/towproj/tow/internal/config"
)

// Sources (read 2026-11-11), followed here:
//   - Traefik's chart 42.3.2 (traefik.github.io/charts, Traefik 3.7.17): ingressClass.isDefaultClass
//     (true unless told otherwise), deployment.kind DaemonSet with ports.web/websecure.hostPort,
//     and a DaemonSet with host ports updated one node at a time (maxSurge 0)
//     https://github.com/traefik/traefik-helm-chart
//   - Cilium 1.20 Ingress and Gateway API: ingressController.enabled/default/loadbalancerMode,
//     gatewayAPI.enabled, both through the kube-proxy replacement; the Gateway API CRDs (standard
//     channel, TLSRoute included) installed first
//     https://docs.cilium.io/en/stable/network/servicemesh/ingress/
//     https://docs.cilium.io/en/stable/network/servicemesh/gateway-api/gateway-api/

const (
	TraefikVersion    = "42.3.2"
	GatewayAPIVersion = "1.6.3" // Cilium 1.20 is built on it

	traefikCharts = "https://traefik.github.io/charts"
)

// traefik is Traefik on kubeadm (k3s brings its own): on a pool address when the cluster has a
// service load balancer, otherwise on every node's ports 80 and 443.
func (o Options) traefik() component {
	k := o.Kubernetes
	c := o.versioned(config.IngressTraefik, fileVersion(k.Ingress.Traefik), TraefikVersion)
	c.role, c.class, c.owns = roleIngress, config.IngressTraefik, "traefik"
	values := map[string]any{
		"ingressClass": map[string]any{"enabled": true, "isDefaultClass": k.Ingress.DefaultClass == config.IngressTraefik},
	}
	if !ServiceLBInstalled(k.ServiceLoadBalancer.Provider) {
		values["deployment"] = map[string]any{"kind": "DaemonSet"}
		values["updateStrategy"] = map[string]any{"type": "RollingUpdate", "rollingUpdate": map[string]any{"maxUnavailable": 1, "maxSurge": 0}}
		values["ports"] = map[string]any{"web": map[string]any{"hostPort": 80}, "websecure": map[string]any{"hostPort": 443}}
		values["service"] = map[string]any{"spec": map[string]any{"type": "ClusterIP"}}
	}
	c.steps = []step{o.helmStep("traefik", "traefik", traefikCharts, c.version, "traefik", values)}
	return c
}

// CiliumIngress reports whether Cilium serves Ingress and the Gateway API: its CRDs go in
// before Cilium, and its values with Cilium's install.
func CiliumIngress(in config.Ingress) bool {
	for _, c := range in.Controllers {
		if c == config.IngressCilium {
			return true
		}
	}
	return false
}

// ciliumIngressValues are Cilium's Ingress and Gateway API values: one shared load balancer
// address for every Ingress; its IngressClass the default when the file says so.
func ciliumIngressValues(in config.Ingress) map[string]any {
	if !CiliumIngress(in) {
		return nil
	}
	return map[string]any{
		"ingressController": map[string]any{"enabled": true, "default": in.DefaultClass == config.IngressCilium, "loadbalancerMode": "shared"},
		"gatewayAPI":        map[string]any{"enabled": true},
	}
}

// gatewayAPICRDs is Gateway API's standard channel, applied server side.
func gatewayAPICRDs(kubeconfig string) step {
	return step{program: "kubectl", args: []string{"apply", "--server-side", "--kubeconfig", kubeconfig, "-f",
		"https://github.com/kubernetes-sigs/gateway-api/releases/download/v" + GatewayAPIVersion + "/standard-install.yaml"}}
}
