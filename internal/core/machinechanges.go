package core

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
	"github.com/towproj/tow/internal/ui"
)

// Sources (read 2026-11-12), followed here:
//   - kubectl drain cordons the node, then evicts its pods through the eviction API, which
//     respects PodDisruptionBudgets; --ignore-daemonsets, --delete-emptydir-data, --timeout
//     https://kubernetes.io/docs/tasks/administer-cluster/safely-drain-node/
//   - kubeadm reset phase remove-etcd-member: a control plane leaves etcd from the machine
//     itself  https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-reset-phase/
//   - k3s's managed etcd member controller: the etcd.k3s.cattle.io/remove annotation on a
//     server's Node removes its member, then etcd.k3s.cattle.io/removed-node-name is set
//     https://github.com/k3s-io/k3s/blob/master/pkg/etcd/member_controller.go

// DefaultDrainTimeout is how long a machine leaving the cluster gets to move its pods away.
const DefaultDrainTimeout = 10 * time.Minute

// k3s's member controller: a Node annotated with k3sRemoveAnnotation has its etcd member
// removed, then carries k3sRemovedAnnotation (k3s's pkg/etcd/member_controller.go).
const (
	k3sRemoveAnnotation  = "etcd.k3s.cattle.io/remove"
	k3sRemovedAnnotation = "etcd.k3s.cattle.io/removed-node-name"
	k3sLeaveTimeout      = 2 * time.Minute
)

// Swappable so tests run without a cluster.
var (
	drainNode = func(ctx context.Context, kubectl, kubeconfig, node string, timeout time.Duration) error {
		// First, the workloads with a pod here must have their other pods Ready: a node
		// drained right after another came back would otherwise take a second pod of theirs
		// away before the first one returned (a database's replica still catching up).
		if api, err := kubeapiFromFile(kubeconfig); err == nil {
			if waiting, err := api.WaitSettled(ctx, node, timeout); err != nil {
				if len(waiting) == 0 {
					return fmt.Errorf("check the pods before draining %s: %w", node, err)
				}
				return fmt.Errorf("%s wasn't drained: within %s, these workloads with a pod on it still had another pod that isn't Ready, so draining it would take a second one away:\n  - %s\nNothing was changed on it. Let them recover (or fix them), then apply again",
					node, timeout, strings.Join(waiting, "\n  - "))
			}
		}
		out, err := exec.CommandContext(ctx, kubectl, "drain", node, "--ignore-daemonsets", "--delete-emptydir-data",
			"--timeout", timeout.String(), "--kubeconfig", kubeconfig).CombinedOutput()
		switch {
		case err == nil:
			return nil
		case strings.Contains(string(out), "not found"):
			return nil // gone already: a run that stopped after deleting it
		}
		return fmt.Errorf("%s couldn't be drained within %s: %w\n%s\nNothing was forced: it stays cordoned, its pods where they are. Let them move (a PodDisruptionBudget that allows no disruption, a pod nothing manages), then apply again; or 'kubectl uncordon %s' to keep it",
			node, timeout, err, ui.LastLines(ui.Redact(string(out)), 10), node)
	}
	leaveEtcd = kubeadm.LeaveEtcd
	// leaveK3sEtcd asks k3s to take a server out of its embedded etcd while it still runs: k3s's
	// member controller removes it when the Node is annotated, then marks it removed.
	leaveK3sEtcd = func(ctx context.Context, kubectl, kubeconfig, node string) error {
		run := func(args ...string) (string, error) {
			out, err := exec.CommandContext(ctx, kubectl, append(args, "--kubeconfig", kubeconfig)...).CombinedOutput()
			return strings.TrimSpace(string(out)), err
		}
		if out, err := run("annotate", "node", node, k3sRemoveAnnotation+"=true", "--overwrite"); err != nil {
			return fmt.Errorf("ask k3s to take %s out of etcd: %w\n%s", node, err, out)
		}
		deadline := time.Now().Add(k3sLeaveTimeout)
		for {
			out, err := run("get", "node", node, "--output", "jsonpath={.metadata.annotations."+strings.ReplaceAll(k3sRemovedAnnotation, ".", `\.`)+"}")
			if err == nil && out != "" {
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("k3s didn't take %s out of etcd within %s (the other control planes' k3s log says why)", node, k3sLeaveTimeout)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	// forgetEtcdMember removes a gone control plane's member from kubeadm's etcd, through the
	// etcd of another control plane (its static pod, which has etcdctl and the certificates).
	forgetEtcdMember = func(ctx context.Context, kubectl, kubeconfig, via, member string) error {
		etcdctl := []string{"--kubeconfig", kubeconfig, "--namespace", "kube-system", "exec", "etcd-" + via, "--", "etcdctl",
			"--endpoints=https://127.0.0.1:2379", "--cacert=/etc/kubernetes/pki/etcd/ca.crt",
			"--cert=/etc/kubernetes/pki/etcd/server.crt", "--key=/etc/kubernetes/pki/etcd/server.key"}
		out, err := exec.CommandContext(ctx, kubectl, append(etcdctl, "member", "list")...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("etcd's members, through %s: %w\n%s", via, err, ui.Redact(string(out)))
		}
		// "8e9e05c52164694d, started, lab-cp-1, https://10.0.0.11:2380, https://10.0.0.11:2379, false"
		for _, line := range strings.Split(string(out), "\n") {
			fields := strings.Split(line, ", ")
			if len(fields) < 3 || fields[2] != member {
				continue
			}
			if out, err := exec.CommandContext(ctx, kubectl, append(etcdctl, "member", "remove", fields[0])...).CombinedOutput(); err != nil {
				return fmt.Errorf("remove %s's etcd member: %w\n%s", member, err, ui.Redact(string(out)))
			}
		}
		return nil
	}
	deleteNode = func(ctx context.Context, kubeconfig, node string) error {
		return withAPI(kubeconfig, func(c apiClient) error { return c.DeleteNode(ctx, node) })
	}
	localVolumes = func(ctx context.Context, kubeconfig, node string) ([]string, error) {
		return localVolumesOn(ctx, kubeconfig, node)
	}
	waitNewReady = func(ctx context.Context, kubeconfig string, nodes []string) error {
		return waitReady(ctx, kubeconfig, nodes)
	}
	checkNewHosts = checkMachinesFree
)

// apiClient is what removing a machine asks of the API.
type apiClient interface {
	DeleteNode(ctx context.Context, name string) error
	LocalVolumesOn(ctx context.Context, node string, drivers []string) ([]string, error)
}

func withAPI(kubeconfig string, f func(apiClient) error) error {
	c, err := kubeapiFromFile(kubeconfig)
	if err != nil {
		return err
	}
	return f(c)
}

func localVolumesOn(ctx context.Context, kubeconfig, node string) ([]string, error) {
	var out []string
	err := withAPI(kubeconfig, func(c apiClient) error {
		var err error
		out, err = c.LocalVolumesOn(ctx, node, []string{addons.VolumeDrivers[config.CSILocalPath], addons.VolumeDrivers[config.CSIOpenEBS]})
		return err
	})
	return out, err
}

// machinePlan is what the file asks of the machines, compared with those that run: the new
// ones are created (or checked, on the user's own), prepared and joined first; the ones
// leaving are drained and taken out one at a time, last, then destroyed.
type machinePlan struct {
	added, removed []providers.Node
	gone           []providers.Node // among added: the cluster's own, deleted outside tow
	// first is OpenTofu's first run: the file's machines and the leaving ones, the new ones
	// named; last is its second, the leaving ones named. Zero without OpenTofu.
	first, last providers.Spec
	before      []providers.Node // the nodes when the run started
	notes       []string
	join, leave []planStep // the new machines (first in the cluster section); the leaving ones (last)
	checkNew    *planStep  // the user's own new machines, checked as at creation (infrastructure)
	stale       []planStep // what's left of machines deleted outside tow, cleared before they're made again
}

func (mp *machinePlan) changes() bool { return mp != nil && len(mp.added)+len(mp.removed) > 0 }

// diffMachines lists the machines the file adds and removes: machines tow creates by
// their names (a shrinking pool loses its highest-numbered ones), the user's own by address.
// A machine of the user's whose role or pool changed is refused.
func diffMachines(cfg *config.Config, spec providers.Spec, prev *state.ClusterState) (added, removed []providers.Node, refused []string) {
	current := prev.Outputs.Nodes
	switch {
	case spec.Machines != nil:
		pools := spec.Machines.Pools
		want := map[string]bool{}
		have := map[string]bool{}
		for _, n := range current {
			have[n.Name] = true
		}
		for _, m := range cloud.Machines(cfg.Name(), pools) {
			want[m.Name] = true
			if !have[m.Name] {
				added = append(added, providers.Node{Name: m.Name, Role: m.Role, Pool: m.Pool})
			}
		}
		for _, n := range current {
			if !want[n.Name] {
				removed = append(removed, n)
			}
		}
	case cfg.Mode == config.ModeBYOH:
		byHost := map[string]providers.Node{}
		for _, n := range current {
			byHost[n.Host] = n
		}
		want := map[string]bool{}
		for _, n := range spec.Nodes {
			want[n.Host] = true
			had, ok := byHost[n.Host]
			switch {
			case !ok:
				added = append(added, n)
			case had.Role != n.Role || (n.Role == config.RoleWorker && had.Pool != n.Pool):
				refused = append(refused, fmt.Sprintf("%s: a machine keeps its role and its pool: remove it from the file, apply, then add it back where you want it", n.Host))
			}
		}
		for _, n := range current {
			if !want[n.Host] {
				removed = append(removed, n)
			}
		}
	}
	return added, removed, refused
}

// planMachineChanges plans the machines the file adds and removes, and those of the cluster's
// that are gone (deleted outside tow: OpenTofu makes them again). Refused first: the
// first control plane leaving or gone (the others joined through it), and control planes
// added where the API's address would change.
func (o *Orchestrator) planMachineChanges(ctx context.Context, cfg, old *config.Config, prev *state.ClusterState, spec providers.Spec, gone []providers.Node) (*machinePlan, error) {
	if cfg.Mode == config.ModeLocal || prev.Outputs == nil || len(prev.Outputs.Nodes) == 0 {
		return nil, nil
	}
	added, removed, refused := diffMachines(cfg, spec, prev)
	added = append(slices.Clone(gone), slices.DeleteFunc(added, func(a providers.Node) bool {
		return slices.ContainsFunc(gone, func(g providers.Node) bool { return g.Name == a.Name })
	})...)
	mp := &machinePlan{added: added, removed: removed, gone: gone, before: slices.Clone(prev.Outputs.Nodes)}
	if !mp.changes() && len(refused) == 0 {
		return nil, nil
	}
	if old == nil {
		old = cfg
	}
	count := func(nodes []providers.Node) int {
		n := 0
		for _, x := range nodes {
			if x.Role == config.RoleControlPlane {
				n++
			}
		}
		return n
	}
	addsCP, removesCP := count(added) > 0, count(removed) > 0
	var first providers.Node
	for _, n := range prev.Outputs.Nodes {
		if n.Role == config.RoleControlPlane {
			first = n
			break
		}
	}
	if slices.ContainsFunc(removed, func(r providers.Node) bool { return r.Name == first.Name && r.Host == first.Host }) {
		refused = append(refused, fmt.Sprintf("%s: the first control plane stays (the others joined through it): remove another one", first.Label()))
	}
	if slices.ContainsFunc(gone, func(g providers.Node) bool { return g.Name == first.Name }) {
		refused = append(refused, fmt.Sprintf("%s, the first control plane, is gone (deleted outside tow?): an empty one would start another cluster, so it isn't made again. Build a new cluster, or bring the machine back", first.Label()))
	}
	if addsCP {
		// On a public cloud, the load balancer in front of the API comes with the second
		// control plane (the module's).
		cloudLB := old.PublicCloud() && old.ControlPlaneCount() > 1
		switch {
		case cfg.Distribution() == config.DistributionKubeadm && kubeadmOptions(old).ControlPlaneEndpoint == "" && !cloudLB:
			refused = append(refused, "spec.controlPlane: kubeadm's control planes join through an address in front of them, set when the cluster was made (apiEndpoint, kube-vip, or several control planes on a public cloud): build a new cluster with it and move your workloads")
		case old.PublicCloud() && !cloudLB:
			refused = append(refused, fmt.Sprintf("spec.controlPlane: on %s a second control plane brings the load balancer in front of the API, whose address every node and certificate uses: build a new cluster with several control planes and move your workloads", config.CloudName(old.Provider())))
		}
	}
	if cfg.Mode == config.ModeBYOH && len(added) > 0 {
		if err := checkNewHosts(cfg); err != nil {
			refused = append(refused, err.Error())
		}
	}
	if len(refused) > 0 {
		return nil, refusal{refused}
	}
	if (addsCP || removesCP) && cfg.ControlPlaneCount()%2 == 0 {
		mp.notes = append(mp.notes, fmt.Sprintf("%d control planes: etcd needs a majority of them, so the API stops when any one is down; 1 or 3 is the usual", cfg.ControlPlaneCount()))
	}

	kubeconfig, known := prev.Outputs.Kubeconfig, filepath.Join(state.Dir(cfg.Name()), "known_hosts")
	toolOpts := tools.Options{Install: o.installTools, DryRun: o.dryRun}
	var kubectl string
	if len(removed)+len(gone) > 0 {
		var err error
		if kubectl, err = findKubectl(ctx, tools.Kubectl(prev.K8sVersion), toolOpts); err != nil {
			return nil, err
		}
	}

	// OpenTofu's two runs: the new machines while the leaving ones stay, then without them.
	if spec.Machines != nil {
		mp.first, mp.last = spec, spec
		mp.first.Guard, mp.last.Guard = providers.GuardRunning, providers.GuardRunning
		m := *spec.Machines
		m.Pools = unionPools(spec.Machines.Pools, prev.Spec.Machines.Pools)
		mp.first.Machines = &m
		for _, n := range added {
			mp.first.Adding = append(mp.first.Adding, n.Name)
		}
		for _, n := range removed {
			mp.last.Removing = append(mp.last.Removing, n.Name)
		}
		// What's left of a machine that's gone: its Node (k3s then drops its node password,
		// and its etcd member for a control plane); on kubeadm, a control plane's etcd member.
		for _, n := range gone {
			lines := []string{fmt.Sprintf("%s is gone (deleted outside tow?): its Node is deleted, then OpenTofu makes it again under its name and address, and it joins", describeNode(n))}
			kubeadmCP := cfg.Distribution() == config.DistributionKubeadm && n.Role == config.RoleControlPlane
			if kubeadmCP {
				lines[0] = strings.Replace(lines[0], "its Node is deleted", "its etcd member and its Node are deleted", 1)
			}
			mp.stale = append(mp.stale, planStep{lines: lines, run: func(ctx context.Context) error {
				done := ui.Step("Clearing what's left of " + n.Name)
				if kubeadmCP {
					if err := forgetEtcdMember(ctx, kubectl, kubeconfig, first.Name, n.Name); err != nil {
						fmt.Println("failed")
						return err
					}
				}
				if err := deleteNode(ctx, kubeconfig, n.Name); err != nil {
					fmt.Println("failed")
					return fmt.Errorf("delete node %s: %w", n.Name, err)
				}
				done()
				return nil
			}})
		}
	} else if len(added) > 0 {
		// The user's own: checked as when the cluster was made, only them (the others run
		// Kubernetes, so their ports are taken).
		checkSpec := spec
		checkSpec.Nodes = added
		var hosts []string
		for _, n := range added {
			hosts = append(hosts, n.Host)
		}
		mp.checkNew = &planStep{lines: []string{"Check the new machines as when the cluster was made: " + strings.Join(hosts, ", ")},
			run: func(ctx context.Context) error {
				p, err := newProvider(providerForMode[cfg.Mode])
				if err != nil {
					return err
				}
				done := ui.Step("Checking the new machines")
				res, err := p.Provision(ctx, checkSpec)
				if err != nil {
					fmt.Println("failed")
					return err
				}
				done()
				return keepNodes(prev, append(slices.Clone(prev.Outputs.Nodes), res.Nodes...))
			}}
	}

	// Joining: k3s or kubeadm as at creation, their secrets read now.
	if len(added) > 0 {
		var inst *k3s.Options
		var kad *kubeadm.Options
		var err error
		switch cfg.Distribution() {
		case config.DistributionK3s:
			inst = k3sOptions(cfg)
			if !o.dryRun {
				if inst.Secrets, err = readK3sSecrets(inst); err != nil {
					return nil, err
				}
			}
		case config.DistributionKubeadm:
			kad = kubeadmOptions(cfg)
			kad.Version = prev.K8sVersion
			if !o.dryRun {
				if kad.Secrets, err = readKubeadmSecrets(cfg, kad); err != nil {
					return nil, err
				}
			}
		}
		var names []string
		for _, n := range added {
			names = append(names, describeNode(n))
		}
		joins := "k3s joins them"
		if kad != nil {
			joins = fmt.Sprintf("kubeadm joins them (Kubernetes %s)", prev.K8sVersion)
		}
		mp.join = append(mp.join, planStep{lines: []string{"Join the new machines: " + strings.Join(names, ", "),
			"prepared like the others (whose firewalls let them in), then " + joins},
			run: func(ctx context.Context) error { return o.joinMachines(ctx, cfg, prev, added, inst, kad) }})
	}

	// Leaving, one at a time.
	byoh := cfg.Mode == config.ModeBYOH
	for _, n := range removed {
		lines := []string{fmt.Sprintf("%s leaves: drained (PodDisruptionBudgets respected, at most %s, nothing forced), Kubernetes removed from it, its Node deleted", describeNode(n), o.drainLimit())}
		if n.Role == config.RoleControlPlane {
			lines[0] = strings.Replace(lines[0], "Kubernetes removed", "it leaves etcd while it still runs, Kubernetes removed", 1)
		}
		if byoh {
			if o.reset {
				lines = append(lines, "its preparation is undone too (--reset)")
			} else {
				lines = append(lines, "the cluster's firewall rules go with it; the rest of its preparation stays ('apply --reset' undoes it too)")
			}
		}
		if vols, err := localVolumes(ctx, kubeconfig, n.Name); err == nil && len(vols) > 0 {
			lines = append(lines, fmt.Sprintf("its local volumes lose their data (%d): %s", len(vols), describeNames(vols)))
		}
		mp.leave = append(mp.leave, planStep{lines: lines, run: func(ctx context.Context) error {
			return o.removeMachine(ctx, cfg, prev, n, kubectl, known)
		}})
	}
	if len(removed) > 0 && spec.Machines != nil {
		var names []string
		for _, n := range removed {
			names = append(names, n.Name)
		}
		mp.leave = append(mp.leave, planStep{lines: []string{fmt.Sprintf("OpenTofu (%s): destroy %s", where(prev), strings.Join(names, ", "))},
			run: func(ctx context.Context) error {
				p, err := newProvider(providerForMode[cfg.Mode])
				if err != nil {
					return err
				}
				done := ui.Step("Destroying the machines that left")
				res, err := p.Provision(ctx, mp.last)
				if err != nil {
					fmt.Println("failed")
					return err
				}
				done()
				return keepNodes(prev, res.Nodes)
			}})
	}
	if len(removed) > 0 {
		mp.leave = append(mp.leave, planStep{lines: []string{"The machines that stay close the firewall rules for the ones that left"},
			run: func(ctx context.Context) error {
				opts := o.prepareOptions(cfg)
				opts.Nodes, opts.KnownHosts, opts.Confirm, opts.Confirmed = prev.Outputs.Nodes, known, nil, true
				done := ui.Step("Closing the firewall rules for the machines that left")
				if err := prepareMachines(ctx, *opts); err != nil {
					fmt.Println("failed")
					return err
				}
				done()
				return nil
			}})
	}
	// The built-in add-ons that name the nodes, for the new set of them.
	which := nodeAddons(cfg)
	if len(which) == 0 {
		return mp, nil
	}
	reapply := planStep{lines: []string{"The add-ons that depend on the nodes follow them where they need it: " + strings.Join(which, ", ")},
		run: func(ctx context.Context) error {
			bp, err := o.planBuiltins(ctx, cfg, cfg, prev, mp.before, nil)
			if err != nil {
				return err
			}
			for _, s := range slices.Concat(bp.early, bp.installs, bp.late) {
				if err := s.run(ctx); err != nil {
					return err
				}
			}
			return nil
		}}
	if len(removed) > 0 {
		mp.leave = append(mp.leave, reapply)
	} else {
		mp.join = append(mp.join, reapply)
	}
	return mp, nil
}

// nodeAddons are the built-in add-ons whose settings name the nodes or count them.
func nodeAddons(cfg *config.Config) []string {
	k := cfg.Spec.Kubernetes
	var out []string
	if m := k.MetricsServer.Enabled; m != nil && *m && k.Distribution == config.DistributionKubeadm {
		out = append(out, "kubelet-csr-approver (the nodes it approves)")
	}
	if slices.Contains(k.Storage.CSI, config.CSIProxmox) {
		out = append(out, "the Proxmox CSI plugin (its labels on each node)")
	}
	if k.Networking.CNI == config.CNICilium && addons.InstallsCNI(k.Networking.CNI, k.Distribution) {
		out = append(out, "Cilium (its operator's replicas)")
	}
	return out
}

// unionPools is the file's pools, each as large as it was or is, with the pools that leave:
// OpenTofu's first run, which keeps the leaving machines while the new ones join.
func unionPools(now, before []providers.Pool) []providers.Pool {
	out := slices.Clone(now)
	for _, b := range before {
		i := slices.IndexFunc(out, func(p providers.Pool) bool { return p.Name == b.Name && p.Role == b.Role })
		switch {
		case i < 0:
			out = append(out, b)
		case out[i].Count < b.Count:
			out[i].Count = b.Count
		}
	}
	return out
}

// joinMachines prepares every machine (the new ones fully, the others for their new peers),
// then lets k3s or kubeadm join the new ones (those already in the cluster are left alone),
// and waits until they're Ready.
func (o *Orchestrator) joinMachines(ctx context.Context, cfg *config.Config, prev *state.ClusterState, added []providers.Node, inst *k3s.Options, kad *kubeadm.Options) error {
	nodes := prev.Outputs.Nodes
	opts := o.prepareOptions(cfg)
	opts.Nodes, opts.KnownHosts, opts.Confirm, opts.Confirmed = nodes, filepath.Join(state.Dir(cfg.Name()), "known_hosts"), nil, true
	done := ui.Step("Preparing the machines for the new ones")
	if err := prepareMachines(ctx, *opts); err != nil {
		fmt.Println("failed")
		return err
	}
	done()

	spec := specFromConfig(cfg, prev.K8sVersion)
	st, res := *prev, *prev.Outputs
	switch {
	case inst != nil:
		if err := o.installKubernetes(ctx, spec, &st, &res, inst); err != nil {
			return err
		}
	case kad != nil:
		if err := installKubeadm(ctx, spec, &st, &res, kad); err != nil {
			return err
		}
	}
	// The new machines' names come from the provider: OpenTofu's, or the user's own once checked.
	var names []string
	for _, n := range nodes {
		if slices.ContainsFunc(added, func(a providers.Node) bool {
			return (a.Name != "" && a.Name == n.Name) || (a.Name == "" && a.Host == n.Host)
		}) {
			names = append(names, n.Name)
		}
	}
	done = ui.Step(fmt.Sprintf("Waiting from this machine for %s to be Ready", ui.Plural(len(names), "new node")))
	if err := waitNewReady(ctx, prev.Outputs.Kubeconfig, names); err != nil {
		fmt.Println("failed")
		return err
	}
	done()
	return nil
}

// removeMachine takes one machine out of the cluster: drained, out of etcd (a kubeadm
// control plane), Kubernetes removed from it (on the user's own, the cluster's firewall rules
// too, or all its preparation with --reset), its Node deleted. A machine tow created that
// can't be reached is destroyed next anyway: that's only a warning.
func (o *Orchestrator) removeMachine(ctx context.Context, cfg *config.Config, prev *state.ClusterState, n providers.Node, kubectl, known string) error {
	byoh, kubeconfig := cfg.Mode == config.ModeBYOH, prev.Outputs.Kubeconfig
	done := ui.Step(fmt.Sprintf("Draining %s", n.Name))
	if err := drainNode(ctx, kubectl, kubeconfig, n.Name, o.drainLimit()); err != nil {
		fmt.Println("failed")
		return err
	}
	done()

	// A control plane leaves etcd while it still runs: the others keep their majority. Never
	// skipped, even for a machine destroyed next: a member left behind can stop the API.
	if n.Role == config.RoleControlPlane {
		done = ui.Step(fmt.Sprintf("Taking %s out of etcd", n.Name))
		var err error
		switch cfg.Distribution() {
		case config.DistributionKubeadm:
			err = leaveEtcd(ctx, n, known, nil)
		case config.DistributionK3s:
			err = leaveK3sEtcd(ctx, kubectl, kubeconfig, n.Name)
		}
		if err != nil {
			fmt.Println("failed")
			return fmt.Errorf("%w\n%s is drained (it stays cordoned); nothing else changed on it", err, n.Name)
		}
		done()
	}

	done = ui.Step(fmt.Sprintf("Removing Kubernetes from %s", n.Label()))
	if err := undoMachines(ctx, prepare.UndoOptions{Cluster: cfg.Name(), Nodes: []providers.Node{n}, KnownHosts: known, Firewall: byoh}, byoh && o.reset); err != nil {
		fmt.Println("failed")
		if byoh {
			return fmt.Errorf("%w\n%s is drained and out of the cluster's etcd; running apply again finishes taking it out", err, n.Name)
		}
		ui.Warn(fmt.Sprintf("%v: its machine is destroyed next all the same", err))
	} else {
		done()
	}

	// The API's address may move as the machine goes (kube-vip's leader): tried for a minute.
	var err error
	for try := 1; try <= nodeTries; try++ {
		if err = deleteNode(ctx, kubeconfig, n.Name); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(nodeWait):
		}
	}
	if err != nil {
		return fmt.Errorf("delete node %s: %w\nKubernetes is off the machine; running apply again finishes taking it out", n.Name, err)
	}
	return keepNodes(prev, slices.DeleteFunc(slices.Clone(prev.Outputs.Nodes), func(x providers.Node) bool { return x.Name == n.Name && x.Host == n.Host }))
}

// Deleting a Node is tried this many times, this far apart. Vars so tests don't wait.
var (
	nodeTries = 12
	nodeWait  = 5 * time.Second
)

// unreachable are the machines the checks before OpenTofu can't reach: those that are gone
// (made again, then prepared and joined).
func (mp *machinePlan) unreachable() []string {
	if mp == nil {
		return nil
	}
	return namesOf(mp.gone)
}

// namesOf are the machines' names.
func namesOf(nodes []providers.Node) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.Name)
	}
	return out
}

// keepNodes records the cluster's machines as they now are: a run that stops later picks up
// from them.
func keepNodes(prev *state.ClusterState, nodes []providers.Node) error {
	out := *prev.Outputs
	out.Nodes = nodes
	prev.Outputs = &out
	if err := state.Save(prev.Name, *prev); err != nil {
		return fmt.Errorf("save state: %w", err)
	}
	return nil
}

// describeNode is a machine as the plan names it: "lab-workers-1 (worker, pool general)".
func describeNode(n providers.Node) string {
	name := n.Name
	if n.Host != "" {
		name = n.Label()
	}
	if n.Role == config.RoleControlPlane {
		return name + " (control plane)"
	}
	return fmt.Sprintf("%s (worker, pool %s)", name, n.Pool)
}

func (o *Orchestrator) drainLimit() time.Duration {
	if o.drainTimeout > 0 {
		return o.drainTimeout
	}
	return DefaultDrainTimeout
}

// errMachinesAndAddress stops a run that would add or remove machines this machine can't
// reach yet: the address change goes first.
var errMachinesAndAddress = errors.New("this machine can't reach the machines until OpenTofu follows its new address, and the file also adds or removes machines: apply once without the machine changes, then again with them")
