package core

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// Sources (read 2026-11-13), followed here:
//   - k3s's critical configuration values must be the same on every server, or servers fail
//     to join: they can't change one server at a time  https://docs.k3s.io/cli/server
//     (--disable=servicelb is one; other packaged components are disabled per server)
//   - k3s's --disable deletes a packaged component already deployed (k3s server --help)
//   - restarting k3s or the kubelet keeps the node's pods running

// k3sCritical are k3s's critical configuration values ("disable=servicelb" for one disable
// value): a running cluster keeps them. data-dir holds the node's data.
var k3sCritical = []string{"agent-token", "cluster-cidr", "cluster-dns", "cluster-domain", "disable-cloud-controller",
	"disable-helm-controller", "disable-network-policy", "disable=servicelb", "egress-selector-mode", "embedded-registry",
	"flannel-backend", "flannel-external-ip", "flannel-ipv6-masq", "secrets-encryption", "secrets-encryption-provider",
	"service-cidr", "data-dir"}

// rendered is a flag list as text: "kube-apiserver-arg=audit-log-maxage=30"; a secret
// names its variable, never its value.
func rendered(flags []providers.Flag) []string {
	var out []string
	for _, f := range flags {
		v := f.Value
		if f.Env != "" {
			v = "{env: " + f.Env + "}"
		}
		out = append(out, f.Name+"="+v)
	}
	return out
}

// flagChanges are the flags added and removed between two lists.
func flagChanges(was, is []string) (added, removed []string) {
	for _, f := range is {
		if !slices.Contains(was, f) {
			added = append(added, f)
		}
	}
	for _, f := range was {
		if !slices.Contains(is, f) {
			removed = append(removed, f)
		}
	}
	return added, removed
}

// critical names the k3s critical value a flag sets, "" when none.
func critical(flag string) string {
	name, value, _ := strings.Cut(flag, "=")
	for _, c := range k3sCritical {
		if c == name || c == name+"="+value {
			return c
		}
	}
	return ""
}

// refusedSettings lists the component settings a running cluster can't take: kubeadm's for
// now, and k3s's critical values (and data-dir).
func refusedSettings(old, cfg *config.Config, fileChanges []string) []string {
	if old == nil || cfg.Mode == config.ModeLocal {
		return nil
	}
	var out []string
	if cfg.Distribution() == config.DistributionKubeadm {
		return nil // every setting rolls: planKubeadmSettings
	}
	lists := [][2][]providers.Flag{{k3s.ServerFlags(old), k3s.ServerFlags(cfg)}}
	for _, p := range cfg.Spec.Workers {
		if op := workerPool(old, p.Name); op != nil {
			lists = append(lists, [2][]providers.Flag{k3s.AgentFlags(old, op), k3s.AgentFlags(cfg, &p)})
		}
	}
	seen := map[string]bool{}
	for _, l := range lists {
		added, removed := flagChanges(rendered(l[0]), rendered(l[1]))
		for _, f := range append(added, removed...) {
			if c := critical(f); c != "" && !seen[c] {
				seen[c] = true
				out = append(out, fmt.Sprintf("k3s's %s must be the same on every server (k3s's docs: servers whose value differs fail to join), so a running cluster keeps it: build a new cluster with it and move your workloads", c))
			}
		}
	}
	slices.Sort(out)
	return out
}

// planSettings plans k3s's new settings: each machine whose config.yaml changes gets it and
// restarts k3s, one at a time, control planes first, its pods kept running, checked before
// the next. kube-vip's manifest follows the file on every control plane.
func (o *Orchestrator) planSettings(ctx context.Context, plan *updatePlan, cfg, old *config.Config, prev *state.ClusterState, up *upgrade) error {
	if up != nil || old == nil || cfg.Mode == config.ModeLocal || prev.Outputs == nil {
		return nil
	}
	if cfg.Distribution() == config.DistributionKubeadm {
		return o.planKubeadmSettings(ctx, plan, cfg, old, prev)
	}
	// What changes, per role and pool.
	var lines, cps []string
	describe := func(who string, added, removed []string) {
		var parts []string
		for _, f := range added {
			parts = append(parts, "+ "+f)
		}
		for _, f := range removed {
			parts = append(parts, "− "+f)
		}
		lines = append(lines, who+": "+strings.Join(parts, ", "))
	}
	for _, n := range prev.Outputs.Nodes {
		if n.Role == config.RoleControlPlane {
			cps = append(cps, n.Name)
		}
	}
	if added, removed := flagChanges(rendered(k3s.ServerFlags(old)), rendered(k3s.ServerFlags(cfg))); len(added)+len(removed) > 0 {
		describe("control planes "+strings.Join(cps, ", "), added, removed)
	}
	for _, p := range cfg.Spec.Workers {
		op := workerPool(old, p.Name)
		if op == nil {
			continue
		}
		added, removed := flagChanges(rendered(k3s.AgentFlags(old, op)), rendered(k3s.AgentFlags(cfg, &p)))
		if len(added)+len(removed) == 0 {
			continue
		}
		var names []string
		for _, n := range prev.Outputs.Nodes {
			if n.Role != config.RoleControlPlane && n.Pool == p.Name {
				names = append(names, n.Name)
			}
		}
		describe(fmt.Sprintf("workers of pool %s (%s)", p.Name, strings.Join(names, ", ")), added, removed)
	}
	kv := config.ServiceLBKubeVIP
	vipChanged := cfg.Spec.Kubernetes.APILoadBalancer == config.APILBKubeVIP &&
		(old.Spec.Kubernetes.ServiceLoadBalancer.Provider == kv) != (cfg.Spec.Kubernetes.ServiceLoadBalancer.Provider == kv)
	if vipChanged {
		lines = append(lines, fmt.Sprintf("kube-vip's manifest on every control plane: it announces LoadBalancer Services too: %v", cfg.Spec.Kubernetes.ServiceLoadBalancer.Provider == kv))
	}
	if len(lines) == 0 {
		return nil
	}

	// k3s's bundled add-ons turned off: storage with volumes is refused, an ingress in use named.
	if err := o.bundledOff(ctx, plan, cfg, old, prev); err != nil {
		return err
	}
	inst := k3sOptions(cfg)
	if !o.dryRun {
		var err error
		if inst.Secrets, err = readK3sSecrets(inst); err != nil {
			return err
		}
	}
	release := nodeRelease(cfg.Distribution(), prev.K8sVersion)
	head := "k3s's new settings, one machine at a time (control planes first), k3s restarted with them: no drain, its pods keep running; a control plane is checked (the API ready on every one) before the next"
	plan.cluster = append(plan.cluster, planStep{lines: append([]string{head}, lines...), run: func(ctx context.Context) error {
		out := prev.Outputs
		inst.Version, inst.Nodes, inst.Endpoint, inst.NodeEndpoint, inst.APIName = prev.K8sVersion, out.Nodes, out.Endpoint, out.NodeEndpoint, out.NodeAPIName
		inst.KnownHosts = filepath.Join(state.Dir(cfg.Name()), "known_hosts")
		var done []string
		inst.Rolling = func(ctx context.Context, n providers.Node, change func() error) error {
			if err := ui.StepRun(fmt.Sprintf("Restarting k3s on %s with its new settings", n.Label()), change); err != nil {
				return err
			}
			if err := o.nodeReady(ctx, cfg, prev, release, n); err != nil {
				return err
			}
			done = append(done, n.Name)
			return nil
		}
		if _, err := installK3s(ctx, *inst); err != nil {
			if len(done) > 0 {
				err = fmt.Errorf("%w\nAlready on the new settings, so skipped when you run it again: %s", err, strings.Join(done, ", "))
			}
			return err
		}
		return nil
	}})
	return nil
}

// bundledOff checks k3s's bundled add-ons the file turns off: local-path storage that
// still has volumes is refused; Ingresses Traefik serves are named.
func (o *Orchestrator) bundledOff(ctx context.Context, plan *updatePlan, cfg, old *config.Config, prev *state.ClusterState) error {
	ok, nk := old.Spec.Kubernetes, cfg.Spec.Kubernetes
	localPath := slices.Contains(ok.Storage.CSI, config.CSILocalPath) && !slices.Contains(nk.Storage.CSI, config.CSILocalPath)
	traefik := slices.Contains(ok.Ingress.Controllers, config.IngressTraefik) && !slices.Contains(nk.Ingress.Controllers, config.IngressTraefik)
	if !localPath && !traefik {
		return nil
	}
	api, err := newBuiltinClient(addons.Options{Kubeconfig: prev.Outputs.Kubeconfig, Dir: state.Dir(cfg.Name()), Kubernetes: nk, Cluster: cfg.Name()})
	if err != nil {
		return err
	}
	if localPath {
		vols, err := api.Volumes(ctx, []string{addons.VolumeDrivers[config.CSILocalPath]})
		if err != nil {
			return err
		}
		if len(vols) > 0 {
			return refusal{[]string{fmt.Sprintf("spec.kubernetes.storage.csi: local-path still has %s (%s): delete them (their claims) first, or keep it in the file",
				ui.Plural(len(vols), "volume"), describeNames(vols))}}
		}
	}
	if traefik {
		ings, err := api.Ingresses(ctx, config.IngressTraefik, ok.Ingress.DefaultClass == config.IngressTraefik)
		if err != nil {
			return err
		}
		if len(ings) > 0 {
			plan.notes = append(plan.notes, fmt.Sprintf("the Ingresses Traefik serves lose their controller (%d): %s", len(ings), describeNames(ings)))
		}
	}
	return nil
}

// nodeReady waits for a node to be Ready at its release; after a control plane, the API must
// be ready on every control plane.
func (o *Orchestrator) nodeReady(ctx context.Context, cfg *config.Config, prev *state.ClusterState, release string, n providers.Node) error {
	if err := ui.StepRun(fmt.Sprintf("Waiting for %s to be Ready at %s", n.Name, release), func() error {
		return waitNodeAt(ctx, prev.Outputs.Kubeconfig, n.Name, release)
	}); err != nil {
		return err
	}
	if n.Role != config.RoleControlPlane {
		return nil
	}
	var cps []providers.Node
	for _, m := range prev.Outputs.Nodes {
		if m.Role == config.RoleControlPlane {
			cps = append(cps, m)
		}
	}
	return ui.StepRun("Checking the API on every control plane", func() error {
		return controlPlanesHealthy(ctx, cfg, cps, filepath.Join(state.Dir(cfg.Name()), "known_hosts"))
	})
}
