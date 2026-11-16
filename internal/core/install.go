package core

import (
	"cmp"
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/secretenv"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
	"github.com/towproj/tow/internal/ui"
)

// Swappable so tests run without machines, helm or downloads.
var (
	installK3s    = k3s.Install
	installCharts = addons.Install
	installCNI    = addons.InstallCNI
	findHelm      = tools.Find
	findKubectl   = tools.Find
	findTool      = tools.Find // the program the provider runs: tofu
	// waitReady waits from this machine until every node is Ready.
	waitReady = func(ctx context.Context, kubeconfig string, nodes []string) error {
		data, err := os.ReadFile(kubeconfig)
		if err != nil {
			return err
		}
		api, err := kubeapi.New(data)
		if err != nil {
			return err
		}
		return api.WaitNodes(ctx, nodes, true)
	}
)

// minorOf is the minor release of a Kubernetes version: 37 for v1.37.2+k3s1.
func minorOf(version string) int {
	_, rest, _ := strings.Cut(version, ".")
	minor, _, _ := strings.Cut(rest, ".")
	n, _ := strconv.Atoi(minor)
	return n
}

// installNetwork installs the CNI right after Kubernetes, then waits for every node to be
// Ready. The cluster is saved first: a failure is resumed by the next apply.
func installNetwork(ctx context.Context, spec providers.Spec, st *state.ClusterState, res *providers.Result, n *addons.Network) error {
	if err := state.Save(spec.Name, *st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	n.Kubeconfig, n.Dir, n.Nodes = res.Kubeconfig, state.Dir(spec.Name), len(res.Nodes)
	n.APIHost, n.APIPort = apiAddress(res)
	again := pickUp(state.StageKubernetes)
	if err := installCNI(ctx, *n); err != nil {
		return fmt.Errorf("%w%s", err, again)
	}
	names := make([]string, len(res.Nodes))
	for i, node := range res.Nodes {
		names[i] = node.Name
	}
	done := ui.Step(fmt.Sprintf("Waiting from this machine for %s to be Ready", ui.Plural(len(names), "node")))
	if err := waitReady(ctx, res.Kubeconfig, names); err != nil {
		fmt.Println("failed")
		return fmt.Errorf("%s runs, but %w%s", n.Networking.CNI, err, again)
	}
	done()
	return nil
}

// apiAddress is where a CNI that replaces kube-proxy reaches the API server, from every
// node: the nodes' address in front of several control planes (on AWS the internal load
// balancer, not the user's public one; kube-vip's virtual IP; the user's load balancer),
// otherwise the first control plane's private address (a public one may not be reachable
// from inside, as on AWS).
func apiAddress(res *providers.Result) (host, port string) {
	var cps []providers.Node
	for _, n := range res.Nodes {
		if n.Role == config.RoleControlPlane {
			cps = append(cps, n)
		}
	}
	if e := res.NodeAPI(); len(cps) > 1 && e != "" {
		if h, p, err := net.SplitHostPort(e); err == nil {
			return h, p
		}
	}
	if len(cps) == 0 {
		return "", "6443"
	}
	return cmp.Or(cps[0].PrivateIP, cps[0].Host), "6443"
}

// readSecrets reads secret variables before anything is created: an unset one stops apply,
// by name. Each value is hidden from all output.
func readSecrets(names []string) (map[string]string, error) {
	out := map[string]string{}
	for _, name := range names {
		v, err := secretenv.Read(name)
		if err != nil {
			return nil, err
		}
		out[name] = v
	}
	return out, nil
}

// installAddons installs the file's Helm charts once Kubernetes runs. The cluster is saved
// first: when a chart fails, it's kept, and the next apply installs the charts again.
func installAddons(ctx context.Context, spec providers.Spec, st *state.ClusterState, res *providers.Result, ad *addons.Options) error {
	st.Status, st.Stage = state.StatusFailed, state.StageKubernetes
	if err := state.Save(spec.Name, *st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	ad.Kubeconfig, ad.Dir, ad.Nodes, ad.CloudStorage = res.Kubeconfig, state.Dir(spec.Name), res.Nodes, res.StorageConfig
	ad.Applied = func(path string, objs []addons.Object) {
		if st.Manifests == nil {
			st.Manifests = map[string][]addons.Object{}
		}
		st.Manifests[path] = objs
	}
	if err := installCharts(ctx, *ad); err != nil {
		return fmt.Errorf("%w\nThe cluster is kept, Kubernetes running: run it again to pick up from here, with the add-ons.\nTo remove the cluster instead: tow delete --name %s", err, spec.Name)
	}
	st.Stage = state.StageAddons
	return nil
}

// k3sOptions says how k3s goes on the cluster's machines, from the file. The machines, the
// version and the secrets are filled in at apply time.
func k3sOptions(cfg *config.Config) *k3s.Options {
	k := cfg.Spec.Kubernetes
	pool := func(name string) *config.WorkerPool { return workerPool(cfg, name) }
	o := &k3s.Options{
		Cluster:     cfg.Name(),
		ServerFlags: k3s.ServerFlags(cfg),
		AgentFlags: func(name string) ([]providers.Flag, bool) {
			p := pool(name)
			if p == nil {
				return nil, false
			}
			return k3s.AgentFlags(cfg, p), true
		},
		RoleLabels: func(n providers.Node) map[string]string {
			g := &cfg.Spec.ControlPlane.Group
			if n.Role != config.RoleControlPlane {
				p := pool(n.Pool)
				if p == nil {
					return nil
				}
				g = &p.Group
			}
			_, roles := config.SplitLabels(g.Labels)
			return roles
		},
		// k3s brings flannel itself; any other CNI arrives later, so nodes stay NotReady.
		WaitReady: k.Networking.CNI == config.CNIFlannel,
	}
	if k.APILoadBalancer == config.APILBKubeVIP {
		o.KubeVIP = k.APIHost()
		o.KubeVIPServices = k.ServiceLoadBalancer.Provider == config.ServiceLBKubeVIP
	}
	// Several control planes are asked for to survive losing one: cluster DNS too, as with
	// kubeadm, which runs two CoreDNS replicas.
	if cfg.ControlPlaneCount() > 1 {
		o.CoreDNSReplicas = 2
	}
	for _, w := range cfg.Spec.Workers {
		o.Pools = append(o.Pools, w.Name)
	}
	return o
}

// readK3sSecrets reads the variables k3s's flags take their values from, before anything is
// created: an unset one stops apply, by name. Each value is hidden from all output.
func readK3sSecrets(o *k3s.Options) (map[string]string, error) {
	flags := o.ServerFlags
	for _, p := range o.Pools {
		more, _ := o.AgentFlags(p)
		flags = append(flags, more...)
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

// installKubernetes puts k3s on the prepared machines, then hands out the kubeconfig: kept in
// the state folder and merged into the user's under the file's context name.
func (o *Orchestrator) installKubernetes(ctx context.Context, spec providers.Spec, st *state.ClusterState, res *providers.Result, inst *k3s.Options) error {
	if err := state.Save(spec.Name, *st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	inst.Version, inst.Nodes, inst.Endpoint, inst.NodeEndpoint, inst.APIName = spec.Version, res.Nodes, res.Endpoint, res.NodeEndpoint, res.NodeAPIName
	inst.KnownHosts = filepath.Join(state.Dir(spec.Name), "known_hosts")
	kubeconfig, err := installK3s(ctx, *inst)
	if err != nil {
		return fmt.Errorf("%w%s", err, again)
	}
	return handOut(spec, st, res, kubeconfig, fmt.Sprintf("k3s %s runs on %s", k3s.Release(spec.Version), ui.Plural(len(res.Nodes), "machine")))
}

// again ends an install that stopped: the machines are kept, prepared.
var again = pickUp(state.StagePrepared)

// handOut keeps the cluster's kubeconfig in the state folder and merges it into the user's
// under the file's context name. The context is remembered before the user's kubeconfig
// changes, so delete removes it even when what follows fails.
func handOut(spec providers.Spec, st *state.ClusterState, res *providers.Result, kubeconfig []byte, done string) error {
	k := spec.Kubernetes
	res.KubeContext, res.Kubeconfig = k.Context, filepath.Join(state.Dir(spec.Name), "kubeconfig")
	if err := state.Save(spec.Name, *st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	if err := kubecfg.Store(kubeconfig, k.Context, res.Kubeconfig, k.Merge); err != nil {
		return fmt.Errorf("Kubernetes runs, but its kubeconfig couldn't be saved: %w%s", err, pickUp(state.StageKubernetes))
	}
	st.Stage = state.StageKubernetes
	ui.Info(done)
	return nil
}

// ownContext reports whether an unfinished apply already put this context in the user's
// kubeconfig: it's this cluster's, so it may be written again.
func ownContext(unfinished *state.ClusterState, name string) bool {
	return unfinished != nil && unfinished.Outputs != nil && unfinished.Outputs.KubeContext == name
}

// removeContext takes the context tow added out of the user's kubeconfig. k3d's teardown
// does this itself; machine clusters come here.
func removeContext(st *state.ClusterState) {
	if st.Mode == config.ModeLocal || st.Outputs == nil || st.Outputs.KubeContext == "" || st.Spec == nil || st.Spec.Kubernetes == nil || !st.Spec.Kubernetes.Merge {
		return
	}
	if err := kubecfg.Remove(st.Outputs.KubeContext); err != nil {
		ui.Warn(fmt.Sprintf("the kubeconfig context '%s' remains: %v", st.Outputs.KubeContext, err))
	}
}
