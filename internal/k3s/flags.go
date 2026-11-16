// Package k3s turns the cluster file into k3s's own flags. The same list becomes --k3s-arg
// flags for k3d now, and keys of /etc/rancher/k3s/config.yaml on machines (step 3). The
// flags tow manages per machine (token, server, node-name, node-ip) are not here: k3d
// sets them itself, and the installer will.
package k3s

import (
	"slices"
	"strconv"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
)

// k3s's own ranges: a flag that would repeat one is left out.
const (
	defaultPodCIDR     = "10.42.0.0/16"
	defaultServiceCIDR = "10.43.0.0/16"

	controlPlaneTaint = "node-role.kubernetes.io/control-plane:NoSchedule"
)

// bundled are the components k3s ships, and the names its --disable flag takes. Each one
// stays only when the file (or its flavor) selects it. k3s ships the Gateway API CRDs for
// its traefik, so they go with it. Not listed, so always kept: coredns (core), and runtimes,
// which only declares RuntimeClasses for the container runtimes k3s finds and runs nothing
// (user, 2026-11-09).
var bundled = []struct{ component, disable string }{
	{config.IngressTraefik, "traefik"},
	{config.ServiceLBServiceLB, "servicelb"},
	{config.CSILocalPath, "local-storage"},
	{config.MetricsServerName, "metrics-server"},
	{gatewayAPICRDs, "gateway-api-crd"},
}

const gatewayAPICRDs = "gateway-api-crds"

// flannelBackends maps the file's backend names to k3s's.
var flannelBackends = map[string]string{"vxlan": "vxlan", "host-gw": "host-gw", "wireguard": "wireguard-native"}

// ServerFlags are the flags of every server: the cluster-wide settings, then the control
// plane's own node settings, then k3s.server.extraArgs.
func ServerFlags(cfg *config.Config) []providers.Flag {
	k := cfg.Spec.Kubernetes
	n := k.Networking
	var f flags
	if n.PodCIDR != defaultPodCIDR {
		f.add("cluster-cidr", n.PodCIDR)
	}
	if n.ServiceCIDR != defaultServiceCIDR {
		f.add("service-cidr", n.ServiceCIDR)
	}
	if n.ClusterDomain != config.DefaultClusterDomain {
		f.add("cluster-domain", n.ClusterDomain)
	}

	switch {
	case n.CNI != config.CNIFlannel:
		// Another CNI (or none): k3s's flannel and its network policy controller step aside.
		f.add("flannel-backend", "none")
		f.add("disable-network-policy", "true")
	case n.Flannel != nil && n.Flannel.Backend != "":
		f.add("flannel-backend", flannelBackends[n.Flannel.Backend])
	}
	// Cilium's kube-proxy replacement and Calico's eBPF dataplane take over kube-proxy's job;
	// running both wastes resources (Calico and Cilium docs). Same on every server (k3s docs).
	if n.ReplacesKubeProxy() {
		f.add("disable-kube-proxy", "true")
	}
	for _, b := range bundled {
		if !selected(k, b.component) {
			f.add("disable", b.disable)
		}
	}

	for _, san := range k.APIServer.CertSANs {
		f.add("tls-san", san)
	}
	f.componentArgs("kube-apiserver-arg", k.APIServer.ExtraArgs)
	f.componentArgs("kube-controller-manager-arg", k.ControllerManager.ExtraArgs)
	f.componentArgs("kube-scheduler-arg", k.Scheduler.ExtraArgs)
	f.componentArgs("etcd-arg", k.Etcd.ExtraArgs)
	// k3s takes etcd snapshots itself (every 12 hours, 5 kept): the file's schedule, when it has one.
	if b := k.Backups; b != nil {
		f.add("etcd-snapshot-schedule-cron", b.Schedule)
		f.add("etcd-snapshot-retention", strconv.Itoa(b.Keep))
	}

	f.node(cfg, &cfg.Spec.ControlPlane.Group, !cfg.ControlPlaneSchedulable())
	f.own(k.K3s.Server.ExtraArgs)
	return f
}

// AgentFlags are the flags of one worker pool's agents: its node settings, then
// k3s.agent.extraArgs.
func AgentFlags(cfg *config.Config, pool *config.WorkerPool) []providers.Flag {
	var f flags
	f.node(cfg, &pool.Group, false)
	f.own(cfg.Spec.Kubernetes.K3s.Agent.ExtraArgs)
	return f
}

// selected reports whether the file asks for one of the components k3s bundles.
func selected(k config.Kubernetes, component string) bool {
	switch component {
	case config.IngressTraefik, gatewayAPICRDs:
		return slices.Contains(k.Ingress.Controllers, config.IngressTraefik)
	case config.ServiceLBServiceLB:
		return k.ServiceLoadBalancer.Provider == component
	case config.CSILocalPath:
		return slices.Contains(k.Storage.CSI, component)
	case config.MetricsServerName:
		return k.MetricsServer.Enabled != nil && *k.MetricsServer.Enabled
	}
	return false
}

type flags []providers.Flag

func (f *flags) add(name, value string) { *f = append(*f, providers.Flag{Name: name, Value: value}) }

// componentArgs passes a component's flags through k3s: kube-apiserver-arg=audit-log-maxage=30.
func (f *flags) componentArgs(name string, args config.Args) {
	for _, key := range sortedKeys(args) {
		v := args[key]
		if v.Env != "" {
			*f = append(*f, providers.Flag{Name: name, Value: key + "=", Env: v.Env})
			continue
		}
		for _, value := range v.Values {
			f.add(name, key+"="+value)
		}
	}
}

// own passes k3s's own flags as written: secrets-encryption=true.
func (f *flags) own(args config.Args) {
	for _, key := range sortedKeys(args) {
		v := args[key]
		if v.Env != "" {
			*f = append(*f, providers.Flag{Name: key, Env: v.Env})
			continue
		}
		for _, value := range v.Values {
			f.add(key, value)
		}
	}
}

// node adds a machine group's kubelet flags, labels and taints. Role labels
// (node-role.kubernetes.io/...) are left out: a kubelet may not set them, so they go
// through the API once the node has joined.
func (f *flags) node(cfg *config.Config, g *config.Group, taintControlPlane bool) {
	f.componentArgs("kubelet-arg", cfg.KubeletArgs(g))
	labels, _ := config.SplitLabels(g.Labels)
	for _, key := range sortedKeys(labels) {
		f.add("node-label", key+"="+labels[key])
	}
	for _, t := range g.Taints {
		taint := t.Key
		if t.Value != "" {
			taint += "=" + t.Value
		}
		f.add("node-taint", taint+":"+t.Effect)
	}
	if taintControlPlane {
		f.add("node-taint", controlPlaneTaint)
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
