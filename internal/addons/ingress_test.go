package addons

import (
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
)

// Traefik on kubeadm: a pool address with a service load balancer; otherwise every node's
// ports 80 and 443, one node at a time on updates. Default class only when the file says so.
func TestTraefik(t *testing.T) {
	k := config.Kubernetes{Distribution: config.DistributionKubeadm, Ingress: config.Ingress{Controllers: []string{config.IngressTraefik}, DefaultClass: config.IngressTraefik}}
	o := Options{Kubernetes: k, Kubeconfig: "/state/kc"}
	first, _ := o.components()
	if len(first) != 1 || first[0].name != "traefik" || first[0].version != TraefikVersion {
		t.Fatalf("%+v", first)
	}
	s := first[0].steps[0]
	if !strings.Contains(strings.Join(s.args, " "), "upgrade --install traefik traefik --repo https://traefik.github.io/charts --version 42.3.2 --namespace traefik") {
		t.Errorf("args %v", s.args)
	}
	v := s.values
	if at(v, "ingressClass", "isDefaultClass") != true || at(v, "deployment", "kind") != "DaemonSet" || at(v, "ports", "web", "hostPort") != 80 ||
		at(v, "ports", "websecure", "hostPort") != 443 || at(v, "service", "spec", "type") != "ClusterIP" || at(v, "updateStrategy", "rollingUpdate", "maxSurge") != 0 {
		t.Errorf("no load balancer: %v", v)
	}

	k.Ingress.DefaultClass = ""
	k.ServiceLoadBalancer = config.ServiceLoadBalancer{Provider: config.ServiceLBMetalLB, MetalLB: &config.PoolController{AddressPool: config.AddressPool{Addresses: []string{"10.0.0.200/29"}}}}
	o.Kubernetes = k
	first, _ = o.components()
	v = first[len(first)-1].steps[0].values
	if at(v, "ingressClass", "isDefaultClass") != false || v["deployment"] != nil || v["ports"] != nil {
		t.Errorf("with MetalLB: %v", v)
	}
	if first[0].name != "metallb" {
		t.Errorf("the load balancer goes first, so Traefik gets an address: %s", first[0].name)
	}

	// k3s brings its own.
	o.Kubernetes.Distribution = config.DistributionK3s
	if first, _ := o.components(); slices.ContainsFunc(first, func(c component) bool { return c.name == "traefik" }) {
		t.Error("k3s bundles traefik")
	}
}

// Cilium's Ingress and Gateway: its values with Cilium's install, the Gateway API CRDs first.
func TestCiliumIngress(t *testing.T) {
	useRun(t, "")
	tr := true
	n := network(t.TempDir(), config.Networking{CNI: config.CNICilium, Cilium: &config.Cilium{KubeProxyReplacement: &tr}})
	n.Ingress = config.Ingress{Controllers: []string{config.IngressCilium}, DefaultClass: config.IngressCilium}
	steps := n.steps()
	if len(steps) != 2 || steps[0].program != "kubectl" || !strings.HasSuffix(steps[0].args[len(steps[0].args)-1], "/gateway-api/releases/download/v1.6.3/standard-install.yaml") {
		t.Fatalf("steps %+v", steps)
	}
	v := steps[1].values
	if at(v, "ingressController", "enabled") != true || at(v, "ingressController", "default") != true || at(v, "ingressController", "loadbalancerMode") != "shared" || at(v, "gatewayAPI", "enabled") != true {
		t.Errorf("values %v", v)
	}
	if !n.NeedsKubectl() {
		t.Error("the CRDs need kubectl")
	}
	n.Ingress = config.Ingress{}
	if steps := n.steps(); len(steps) != 1 || steps[0].values["gatewayAPI"] != nil || n.NeedsKubectl() {
		t.Errorf("without its ingress: %+v", steps)
	}
}
