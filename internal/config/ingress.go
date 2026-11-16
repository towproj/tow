package config

import "fmt"

// Ingress controllers tow installs itself. Several can coexist; each one's class is named
// after it. Only the ones that need tow are here: traefik (k3s bundles it; the presets
// ask for it on kubeadm too) and cilium (part of the CNI's own install). Any other controller
// is a Helm chart: spec.kubernetes.addons.helmCharts, with spec.infrastructure.access.ports
// for the ports it listens on (docs/ingress.md has tested examples).
const (
	IngressTraefik = "traefik"
	IngressCilium  = "cilium" // Cilium's Ingress and Gateway API
)

type Ingress struct {
	Controllers  []string `yaml:"controllers,omitempty"`
	DefaultClass string   `yaml:"defaultClass,omitempty"` // one of controllers; default: the only one listed
	Traefik      *Pinned  `yaml:"traefik,omitempty"`
	// Cilium's ingress has no block: its version follows the CNI's.
}

var ingressChoices = map[string]choice{
	IngressTraefik: {},
	IngressCilium:  {distributions: remote, cni: CNICilium},
}

// asCharts are controllers tow used to plan as built-ins: they install as charts or
// manifests of their own, which tow has nothing to add to.
var asCharts = map[string]string{
	"nginx":         "F5 NGINX Ingress Controller",
	"ingress-nginx": "ingress-nginx (retired upstream)",
	"haproxy":       "HAProxy Kubernetes Ingress",
	"contour":       "Contour",
	"envoy-gateway": "Envoy Gateway",
}

// ingressClasses lists the selected controllers that serve Ingress objects.
func ingressClasses(controllers []string) []string { return controllers }

func (c *Config) validateIngress() []error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	addf := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	const at = "spec.kubernetes.ingress"
	k := c.Spec.Kubernetes
	in := k.Ingress

	selected := map[string]bool{}
	for i, name := range in.Controllers {
		path := fmt.Sprintf("%s.controllers[%d]", at, i)
		if what, ok := asCharts[name]; ok {
			addf("%s: %s installs as a chart of its own, which tow has nothing to add to: put it in spec.kubernetes.addons.helmCharts, and its ports in spec.infrastructure.access.ports (docs/ingress.md has a tested example)", path, what)
			continue
		}
		if selected[name] {
			addf("%s: %s is listed twice", path, name)
			continue
		}
		selected[name] = true
		add(c.checkChoice(path, name, ingressChoices))
	}
	if in.Traefik != nil && selected[IngressTraefik] {
		add(c.checkVersion(at+".traefik.version", IngressTraefik, in.Traefik.Version))
	}
	errs = append(errs, checkBlocks(at, []optionBlock{{IngressTraefik, in.Traefik != nil}},
		func(name string) bool { return selected[name] }, "ingress.controllers doesn't list it")...)

	// Cilium's Ingress and Gateway go through its kube-proxy replacement, and are reached on
	// the addresses its load balancer hands out (Cilium docs).
	if selected[IngressCilium] && k.Networking.CNI == CNICilium {
		if !k.Networking.ReplacesKubeProxy() {
			addf("%s.controllers: cilium needs spec.kubernetes.networking.cilium.kubeProxyReplacement: true (Cilium's ingress works through it)", at)
		}
		if lb := k.ServiceLoadBalancer.Provider; lb == ServiceLBNone || lb == "" {
			addf("%s.controllers: cilium needs a service load balancer (spec.kubernetes.serviceLoadBalancer): its Ingress and Gateways are reached on the addresses it hands out", at)
		}
	}

	switch classes := ingressClasses(in.Controllers); {
	case in.DefaultClass == "":
		if len(classes) > 1 {
			c.warnings = append(c.warnings, "no default IngressClass: Ingresses that name no class are served by no controller\n  → set spec.kubernetes.ingress.defaultClass")
		}
	case !selected[in.DefaultClass]:
		addf("%s.defaultClass %q must be one of ingress.controllers", at, in.DefaultClass)
	case in.DefaultClass != IngressTraefik && selected[IngressTraefik] && c.bundles():
		addf("%s.defaultClass %s: k3s marks its bundled traefik as the default IngressClass, and marks it again each time it starts; make traefik the default, or leave it out of ingress.controllers", at, in.DefaultClass)
	}
	return errs
}
