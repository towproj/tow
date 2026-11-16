package core

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/secretenv"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// Swappable so tests run without machines.
var (
	installKubeadmCluster = kubeadm.Install
	setSnapshots          = kubeadm.SetSnapshots
)

// kubeadmOptions says how kubeadm goes on the cluster's machines, from the file. The release,
// the machines, their API address and the secrets are filled in at apply time.
func kubeadmOptions(cfg *config.Config) *kubeadm.Options {
	k := cfg.Spec.Kubernetes
	n := k.Networking
	o := &kubeadm.Options{
		Cluster: cfg.Name(), Runtime: k.ContainerRuntime,
		PodCIDR: n.PodCIDR, ServiceCIDR: n.ServiceCIDR, DNSDomain: n.ClusterDomain,
		CertSANs:          k.APIServer.CertSANs,
		APIServer:         flagsOf(k.APIServer.ExtraArgs),
		ControllerManager: flagsOf(k.ControllerManager.ExtraArgs),
		Scheduler:         flagsOf(k.Scheduler.ExtraArgs),
		Etcd:              flagsOf(k.Etcd.ExtraArgs),
		SkipKubeProxy:     n.ReplacesKubeProxy(),
		Schedulable:       cfg.ControlPlaneSchedulable(),
		ControlPlanes:     cfg.ControlPlaneCount(),
		// metrics-server checks the kubelets' serving certificates: the cluster signs them.
		ServerTLSBootstrap: k.MetricsServer.Enabled != nil && *k.MetricsServer.Enabled,
	}
	// The nodes reach the API through an address only when something serves it for them: the
	// user's own load balancer, tested before init, kube-vip's virtual IP, or the load
	// balancer tow creates in front of several control planes on AWS (installKubeadm). A
	// public address (one control plane on AWS) is for this machine only.
	switch k.APILoadBalancer {
	case config.APILBExternal:
		o.ControlPlaneEndpoint, o.CheckEndpoint = k.APIEndpoint, true
	case config.APILBKubeVIP:
		o.ControlPlaneEndpoint, o.KubeVIP = k.APIEndpoint, k.APIHost()
		o.KubeVIPServices = k.ServiceLoadBalancer.Provider == config.ServiceLBKubeVIP
	}
	if k.Containerd != nil {
		o.Containerd = k.Containerd.Version
	}
	if k.CRIO != nil {
		o.CRIO = k.CRIO.Version
	}
	o.Snapshots = snapshotsOf(cfg)
	o.Node = func(n providers.Node) (kubeadm.NodeSettings, bool) {
		g := &cfg.Spec.ControlPlane.Group
		if n.Role != config.RoleControlPlane {
			p := workerPool(cfg, n.Pool)
			if p == nil {
				return kubeadm.NodeSettings{}, false
			}
			g = &p.Group
		}
		labels, roles := config.SplitLabels(g.Labels)
		return kubeadm.NodeSettings{KubeletArgs: flagsOf(cfg.KubeletArgs(g)), Labels: labels, Taints: g.Taints, RoleLabels: roles}, true
	}
	return o
}

func controlPlanes(nodes []providers.Node) int {
	n := 0
	for _, x := range nodes {
		if x.Role == config.RoleControlPlane {
			n++
		}
	}
	return n
}

// workerPool finds a worker pool by name; nodes saved before they carried their pool can only
// be in the single one.
func workerPool(cfg *config.Config, name string) *config.WorkerPool {
	for i := range cfg.Spec.Workers {
		if cfg.Spec.Workers[i].Name == name {
			return &cfg.Spec.Workers[i]
		}
	}
	if name == "" && len(cfg.Spec.Workers) == 1 {
		return &cfg.Spec.Workers[0]
	}
	return nil
}

// flagsOf turns the file's flags into a list, in name order; a list repeats the flag.
func flagsOf(a config.Args) []providers.Flag {
	var out []providers.Flag
	for _, name := range slices.Sorted(maps.Keys(a)) {
		v := a[name]
		if v.Env != "" {
			out = append(out, providers.Flag{Name: name, Env: v.Env})
			continue
		}
		for _, x := range v.Values {
			out = append(out, providers.Flag{Name: name, Value: x})
		}
	}
	return out
}

// readKubeadmSecrets reads the variables kubeadm's flags take their values from, before
// anything is created: an unset one stops apply, by name. Each value is hidden from all
// output.
func readKubeadmSecrets(cfg *config.Config, o *kubeadm.Options) (map[string]string, error) {
	flags := slices.Concat(o.APIServer, o.ControllerManager, o.Scheduler, o.Etcd)
	for _, g := range append([]*config.Group{&cfg.Spec.ControlPlane.Group}, groupsOf(cfg)...) {
		flags = append(flags, flagsOf(cfg.KubeletArgs(g))...)
	}
	secrets := map[string]string{}
	for _, f := range flags {
		if f.Env == "" || secrets[f.Env] != "" {
			continue
		}
		v, err := secretenv.Read(f.Env)
		if err != nil {
			return nil, err
		}
		secrets[f.Env] = v
	}
	return secrets, nil
}

func groupsOf(cfg *config.Config) []*config.Group {
	var out []*config.Group
	for i := range cfg.Spec.Workers {
		out = append(out, &cfg.Spec.Workers[i].Group)
	}
	return out
}

// cloudEndpoint: several control planes on a public cloud. The nodes' (internal) load balancer
// tow created, or the name the machines resolve to it (GCP), is the address kubeadm puts
// in front of them, known once it exists. The user's public one stays in Endpoint: the
// kubeconfig and the API's certificate.
func cloudEndpoint(o *kubeadm.Options, res *providers.Result) {
	if o.ControlPlaneEndpoint != "" || controlPlanes(res.Nodes) < 2 {
		return
	}
	o.ControlPlaneEndpoint = res.NodeAPI()
	if res.NodeAPIName != "" {
		o.APIName, o.APIAddress = res.NodeAPIName, res.NodeAPIAddress()
	}
}

// runningKubeadm is kubeadmOptions for a cluster that runs: its release, its machines and
// the address in front of its control planes, as installKubeadm set them.
func runningKubeadm(cfg *config.Config, prev *state.ClusterState) *kubeadm.Options {
	o := kubeadmOptions(cfg)
	o.Version, o.Nodes, o.Endpoint = prev.K8sVersion, prev.Outputs.Nodes, prev.Outputs.Endpoint
	o.KnownHosts = filepath.Join(state.Dir(cfg.Name()), "known_hosts")
	cloudEndpoint(o, prev.Outputs)
	return o
}

// installKubeadm gets the prepared machines their container runtime and packages, sets the
// cluster up with kubeadm, then hands out the kubeconfig, like k3s's. The state is saved
// before each part: a failure is resumed by the next apply.
func installKubeadm(ctx context.Context, spec providers.Spec, st *state.ClusterState, res *providers.Result, o *kubeadm.Options) error {
	if err := state.Save(spec.Name, *st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	o.Nodes, o.Endpoint = res.Nodes, res.Endpoint
	o.KnownHosts = filepath.Join(state.Dir(spec.Name), "known_hosts")
	cloudEndpoint(o, res)
	if err := setupKubeadm(ctx, *o); err != nil {
		return fmt.Errorf("%w%s", err, again)
	}
	kubeconfig, err := installKubeadmCluster(ctx, *o)
	if err != nil {
		return fmt.Errorf("%w%s", err, again)
	}
	// The file's etcd snapshots, on every control plane (one that just joined too).
	if o.Snapshots != nil {
		done := ui.Step("Scheduling etcd snapshots on the control planes")
		if err := setSnapshots(ctx, *o); err != nil {
			fmt.Println("failed")
			return fmt.Errorf("%w%s", err, again)
		}
		done()
	}
	return handOut(spec, st, res, kubeconfig, fmt.Sprintf("Kubernetes %s runs on %s (kubeadm)", spec.Version, ui.Plural(len(res.Nodes), "machine")))
}
