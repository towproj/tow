package core

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
	"github.com/towproj/tow/internal/ui"
	"github.com/towproj/tow/internal/versions"
)

// Sources (read 2026-11-13), followed here:
//   - k3s's manual upgrades: server nodes first, one at a time, then agents; never skip a
//     minor release; the installer again with the same settings and INSTALL_K3S_VERSION. It
//     doesn't drain or cordon; the docs advise doing so for workloads that matter
//     https://docs.k3s.io/upgrades/manual
//   - the version skew policy: the control plane moves one minor release at a time
//     https://kubernetes.io/releases/version-skew-policy/
//   - kubectl drain, then uncordon once the node is back
//     https://kubernetes.io/docs/tasks/administer-cluster/safely-drain-node/
//   - /readyz on an API server includes its etcd check
//     https://kubernetes.io/docs/reference/using-api/health-checks/

// upgradeWait is how long an upgraded node gets to be Ready at its new release, and its API
// to answer.
const upgradeWait = 5 * time.Minute

// upgrade is the release the file asks for, newer than the one that runs.
type upgrade struct {
	from, to string // as the state and the file write them: v1.37.2
	release  string // what the nodes report once upgraded: v1.38.1+k3s1 on k3s
}

// Swappable so tests run without a cluster, machines or the network.
var (
	newestPatch   = versions.NewestPatch
	releaseExists = versions.Exists
	// waitNodeAt waits until a node is Ready and its kubelet runs the release.
	waitNodeAt = func(ctx context.Context, kubeconfig, node, release string) error {
		ctx, cancel := context.WithTimeout(ctx, upgradeWait)
		defer cancel()
		seen := "not seen yet"
		for {
			if api, err := nodeAPI(kubeconfig); err == nil {
				if metas, err := api.NodeMetas(ctx); err == nil {
					for _, m := range metas {
						if m.Name != node {
							continue
						}
						if m.Ready && m.Version == release {
							return nil
						}
						seen = fmt.Sprintf("%s, Ready %v", m.Version, m.Ready)
					}
				}
			}
			select {
			case <-ctx.Done():
				return fmt.Errorf("%s isn't Ready at %s after %s (last seen: %s); its k3s or kubelet log says why", node, release, upgradeWait, seen)
			case <-time.After(5 * time.Second):
			}
		}
	}
	uncordonNode = func(ctx context.Context, kubectl, kubeconfig, node string) error {
		if out, err := exec.CommandContext(ctx, kubectl, "uncordon", node, "--kubeconfig", kubeconfig).CombinedOutput(); err != nil {
			return fmt.Errorf("uncordon %s: %w\n%s", node, err, ui.Redact(string(out)))
		}
		return nil
	}
	// controlPlanesHealthy checks, on each control plane, that its own API server answers
	// /readyz (etcd included), for up to upgradeWait; on kubeadm, that its control-plane
	// containers run and every etcd member is healthy too.
	controlPlanesHealthy = func(ctx context.Context, cfg *config.Config, cps []providers.Node, known string) error {
		for _, n := range cps {
			script := readyzScript()
			if cfg.Distribution() == config.DistributionKubeadm {
				script = kubeadm.HealthScript(cfg.Spec.Kubernetes.ContainerRuntime, n)
			}
			deadline := time.Now().Add(upgradeWait)
			for {
				out, err := runOn(ctx, n, known, script)
				if err == nil {
					break
				}
				if time.Now().After(deadline) || ctx.Err() != nil {
					return fmt.Errorf("the API on %s isn't ready (/readyz): %w\n%s", n.Label(), err, ui.LastLines(out, 15))
				}
				select {
				case <-ctx.Done():
				case <-time.After(5 * time.Second):
				}
			}
		}
		return nil
	}
)

// readyzScript asks a k3s server's own API server for /readyz; when it isn't ready, the
// verbose answer says which check fails.
func readyzScript() string {
	return "out=$(k3s kubectl get --raw=/readyz 2>&1) && [ \"$out\" = ok ] && exit 0\nk3s kubectl get --raw='/readyz?verbose' 2>&1 | tail -n 20\nexit 1\n"
}

// runOn runs a script as root on a machine.
func runOn(ctx context.Context, n providers.Node, known, script string) (string, error) {
	r, err := prepare.Connect(ctx, n, known)
	if err != nil {
		return "", err
	}
	defer r.Close()
	return r.RunInput(ctx, remote.AsRoot, script)
}

// releaseNum is a release as numbers: v1.37.2+k3s1 is {37, 2, 1}; kubeadm's have no build.
type releaseNum struct{ minor, patch, build int }

func parseRelease(v string) (releaseNum, bool) {
	rest, ok := strings.CutPrefix(v, "v1.")
	if !ok {
		return releaseNum{}, false
	}
	rest, build, hasBuild := strings.Cut(rest, "+k3s")
	minor, patch, ok := strings.Cut(rest, ".")
	var r releaseNum
	var err1, err2, err3 error
	r.minor, err1 = strconv.Atoi(minor)
	r.patch, err2 = strconv.Atoi(patch)
	if hasBuild {
		r.build, err3 = strconv.Atoi(build)
	}
	return r, ok && err1 == nil && err2 == nil && err3 == nil
}

func (a releaseNum) compare(b releaseNum) int {
	if a.minor != b.minor {
		return a.minor - b.minor
	}
	if a.patch != b.patch {
		return a.patch - b.patch
	}
	return a.build - b.build
}

// nodeRelease is a version as the distribution's nodes report it: k3s's first build of a
// release when the version names none.
func nodeRelease(distribution, version string) string {
	if distribution == config.DistributionK3s {
		return k3s.Release(version)
	}
	return version
}

// upgradeOf reads the upgrade the file asks for: a version it names, newer than the one that
// runs. An empty or "stable" version never upgrades a running cluster. What can't be done is
// refused: a downgrade, a skipped minor release, other changes in the same apply.
func upgradeOf(cfg *config.Config, prev *state.ClusterState, fileChanges []string) (*upgrade, []string) {
	want := cfg.Spec.Kubernetes.Version
	if cfg.Mode == config.ModeLocal || want == "" || want == config.VersionStable || prev.K8sVersion == "" {
		return nil, nil
	}
	d := cfg.Distribution()
	from, to := nodeRelease(d, prev.K8sVersion), nodeRelease(d, want)
	a, okA := parseRelease(from)
	b, okB := parseRelease(to)
	if !okA || !okB {
		return nil, []string{fmt.Sprintf("spec.kubernetes.version: %s can't be compared with %s, which runs", want, prev.K8sVersion)}
	}
	switch c := b.compare(a); {
	case c == 0:
		return nil, nil
	case c < 0:
		return nil, []string{fmt.Sprintf("spec.kubernetes.version: %s is older than %s, which runs: Kubernetes has no supported downgrade, so the release stays", to, from)}
	case b.minor > a.minor+1:
		var between []string
		for m := a.minor + 1; m < b.minor; m++ {
			between = append(between, fmt.Sprintf("v1.%d", m))
		}
		return nil, []string{fmt.Sprintf("spec.kubernetes.version: %s to %s skips %s: Kubernetes upgrades one minor release at a time, so upgrade to %s first", from, to, strings.Join(between, ", "), between[0])}
	}
	var others []string
	for _, c := range fileChanges {
		path, _, _ := strings.Cut(c, ": ")
		if g := config.Generic(path); g != "spec.kubernetes.version" && !movesWithUpgrade(g) {
			others = append(others, path)
		}
	}
	if len(others) > 0 {
		return nil, []string{fmt.Sprintf("spec.kubernetes.version: an upgrade goes alone; apply the file's other changes first, with version %s, then the new version (changed too: %s)", prev.K8sVersion, strings.Join(others, ", "))}
	}
	return &upgrade{from: prev.K8sVersion, to: want, release: to}, nil
}

// movesWithUpgrade: the versions of what tow installs (the CNI, the built-in add-ons,
// the container runtime) may change with Kubernetes; the file's own charts may not.
func movesWithUpgrade(generic string) bool {
	return strings.HasPrefix(generic, "spec.kubernetes.") && strings.HasSuffix(generic, ".version") &&
		!strings.HasPrefix(generic, "spec.kubernetes.addons.")
}

// planUpgrade reads each node's release through the API. With an upgrade, it plans an etcd
// snapshot (unless a node already runs the new release: a resumed upgrade), then the nodes
// one at a time. Without one, a node already on a newer release means an upgrade stopped
// partway and the file went back: refused.
func (o *Orchestrator) planUpgrade(ctx context.Context, plan *updatePlan, cfg *config.Config, prev *state.ClusterState, up *upgrade) error {
	if cfg.Mode == config.ModeLocal || prev.Outputs == nil || prev.Outputs.Kubeconfig == "" || len(prev.Outputs.Nodes) == 0 {
		return nil
	}
	d := cfg.Distribution()
	live, err := liveNodes(ctx, prev.Outputs.Kubeconfig)
	if err != nil {
		if up == nil {
			return nil // planNodes notes that the API doesn't answer
		}
		return fmt.Errorf("upgrading needs the cluster's API, which doesn't answer from this machine: %w", err)
	}
	want := nodeRelease(d, prev.K8sVersion)
	if up != nil {
		want = up.release
	}
	w, _ := parseRelease(want)
	var ahead []string
	for _, n := range live {
		if r, ok := parseRelease(n.Version); ok && r.compare(w) > 0 {
			ahead = append(ahead, fmt.Sprintf("%s (%s)", n.Name, n.Version))
		}
	}
	if len(ahead) > 0 {
		return refusal{[]string{fmt.Sprintf("spec.kubernetes.version: an upgrade stopped partway, and %s already run a newer release than %s: they can't go back, so set the version they run and apply again to finish it", strings.Join(ahead, ", "), want)}}
	}
	if up == nil {
		if note := newerKubernetes(ctx, d, prev.K8sVersion); note != "" {
			plan.notes = append(plan.notes, note)
		}
		return nil
	}

	if err := releaseExists(ctx, d, up.release); err != nil {
		return fmt.Errorf("spec.kubernetes.version: %w", err)
	}
	kubectl, err := findKubectl(ctx, tools.Kubectl(up.to), tools.Options{Install: o.installTools, DryRun: o.dryRun})
	if err != nil {
		return err
	}
	// How each distribution upgrades a node, and what it needs first.
	var how []string
	var roll func(ctx context.Context, done []string, each rollFunc) error
	switch d {
	case config.DistributionKubeadm:
		kad := kubeadmOptions(cfg)
		kad.Version, kad.Nodes, kad.KnownHosts = up.to, prev.Outputs.Nodes, filepath.Join(state.Dir(cfg.Name()), "known_hosts")
		if _, err := kubeadm.CheckVersions(*kad); err != nil {
			return refusal{[]string{fmt.Sprintf("spec.kubernetes.version: %v: change its version in the file with this upgrade", err)}}
		}
		runtimes, err := runtimeChanges(ctx, *kad)
		if err != nil {
			return err
		}
		how = []string{fmt.Sprintf("on each: the kubeadm package (its repository moved to v1.%d), 'kubeadm upgrade apply' on the first control plane ('kubeadm upgrade node' on the others), then kubelet and kubectl, the kubelet restarted", minorOf(up.to))}
		if len(runtimes) > 0 {
			how = append(how, "the container runtime too, before the kubelet, where it doesn't go with the new release: "+describeChanges(runtimes))
		}
		how = append(how, "kubeadm renews the certificates it manages, and moves CoreDNS and kube-proxy with the last control plane; CoreDNS is spread over the nodes again after")
		roll = func(ctx context.Context, done []string, each rollFunc) error {
			return o.rollKubeadm(ctx, prev, kad, done, each)
		}
	default:
		inst := k3sOptions(cfg)
		if !o.dryRun {
			if inst.Secrets, err = readK3sSecrets(inst); err != nil {
				return err
			}
		}
		how = []string{"on each: k3s's installer at the new release, with the settings it has"}
		roll = func(ctx context.Context, _ []string, each rollFunc) error {
			out := prev.Outputs
			inst.Version, inst.Nodes, inst.Endpoint, inst.NodeEndpoint, inst.APIName = up.to, out.Nodes, out.Endpoint, out.NodeEndpoint, out.NodeAPIName
			inst.KnownHosts, inst.Rolling = filepath.Join(state.Dir(cfg.Name()), "known_hosts"), each
			_, err := installK3s(ctx, *inst)
			return err
		}
	}

	// The nodes in the order they go: control planes first, as the state lists them.
	version := map[string]string{}
	for _, n := range live {
		version[n.Name] = n.Version
	}
	var done, cps, workers []string
	for _, n := range prev.Outputs.Nodes {
		switch {
		case version[n.Name] == up.release:
			done = append(done, n.Name)
		case n.Role == config.RoleControlPlane:
			cps = append(cps, n.Name)
		default:
			workers = append(workers, n.Name)
		}
	}
	if len(done) == 0 {
		plan.cluster = append(plan.cluster, planStep{lines: []string{fmt.Sprintf("An etcd snapshot first, copied to %s (it can be restored on %s too)", state.BackupsDir(cfg.Name()), up.release)},
			run: func(ctx context.Context) error {
				_, err := o.backup(ctx, cfg.Name(), up.to)
				return err
			}})
	}
	order := ""
	switch {
	case len(cps) > 0 && len(workers) > 0:
		order = fmt.Sprintf("control planes %s, then workers %s", strings.Join(cps, ", "), strings.Join(workers, ", "))
	case len(cps) > 0:
		order = "control planes " + strings.Join(cps, ", ")
	case len(workers) > 0:
		order = "workers " + strings.Join(workers, ", ")
	default:
		order = "none left"
	}
	lines := append([]string{
		fmt.Sprintf("Upgrade %s %s → %s, one node at a time: %s", distributionName(d), nodeRelease(d, up.from), up.release, order),
		fmt.Sprintf("each drained (PodDisruptionBudgets respected, at most %s, nothing forced), upgraded, Ready at %s, then uncordoned; after a control plane, the control planes are checked (the API ready on each%s)", o.drainLimit(), up.release, map[bool]string{true: ", their components running, etcd healthy"}[d == config.DistributionKubeadm]),
	}, how...)
	if len(done) > 0 {
		lines = append(lines, fmt.Sprintf("already at %s, skipped: %s", up.release, strings.Join(done, ", ")))
	}
	if left := slices.DeleteFunc(slices.Clone(prev.Cordoned), func(n string) bool { return !slices.Contains(done, n) }); len(left) > 0 {
		lines = append(lines, fmt.Sprintf("left cordoned by the last run: %s; uncordoned once Ready and checked", strings.Join(left, ", ")))
	}
	if len(prev.Outputs.Nodes) == 1 {
		plan.notes = append(plan.notes, "a one-machine cluster: its workloads stop while it's drained and upgraded")
	} else if pods := podNodes(cfg, prev.Outputs.Nodes); len(pods) == 1 {
		plan.notes = append(plan.notes, fmt.Sprintf("%s is the only node your pods can run on (the control planes take none): they wait while it's drained, and a PodDisruptionBudget that needs one of them running stops its drain", pods[0]))
	}
	if b, ok := parseRelease(nodeRelease(d, config.DefaultKubernetesVersion)); ok && w.compare(b) > 0 {
		plan.notes = append(plan.notes, fmt.Sprintf("%s is newer than %s, the release this tow is tested with", up.release, nodeRelease(d, config.DefaultKubernetesVersion)))
	}
	plan.cluster = append(plan.cluster, planStep{lines: lines, run: func(ctx context.Context) error {
		return o.upgradeNodes(ctx, cfg, prev, up, kubectl, done, roll)
	}})
	return nil
}

// rollFunc upgrades one node: change is what the distribution does on it, inside the drain
// and the checks (rollNode).
type rollFunc func(ctx context.Context, n providers.Node, change func() error) error

// Swappable so tests run without machines.
var (
	runtimeChanges = kubeadm.RuntimeChanges
	upgradeKubeadm = kubeadm.UpgradeNode
	spreadCoreDNS  = func(ctx context.Context, kubeconfig string) error {
		c, err := kubeapiFromFile(kubeconfig)
		if err != nil {
			return err
		}
		return c.SpreadOverNodes(ctx, "kube-system", "coredns")
	}
)

func distributionName(d string) string {
	if d == config.DistributionKubeadm {
		return "Kubernetes"
	}
	return d
}

// describeChanges groups machines that change the same way: "containerd 2.2.1 → 2.3.7 (cp-0, w-0)".
func describeChanges(byNode map[string]string) string {
	byWhat := map[string][]string{}
	for n, what := range byNode {
		byWhat[what] = append(byWhat[what], n)
	}
	var out []string
	for what, ns := range byWhat {
		slices.Sort(ns)
		out = append(out, fmt.Sprintf("%s (%s)", what, strings.Join(ns, ", ")))
	}
	slices.Sort(out)
	return strings.Join(out, "; ")
}

// rollKubeadm upgrades the nodes kubeadm's way, control planes first, one at a time; the
// first control plane runs 'kubeadm upgrade apply'. Then CoreDNS, which kubeadm wrote again,
// is spread over the nodes.
func (o *Orchestrator) rollKubeadm(ctx context.Context, prev *state.ClusterState, kad *kubeadm.Options, done []string, each rollFunc) error {
	nodes := slices.Clone(prev.Outputs.Nodes)
	slices.SortStableFunc(nodes, func(a, b providers.Node) int {
		return map[bool]int{true: 0, false: 1}[a.Role == config.RoleControlPlane] - map[bool]int{true: 0, false: 1}[b.Role == config.RoleControlPlane]
	})
	first := ""
	if len(nodes) > 0 && nodes[0].Role == config.RoleControlPlane {
		first = nodes[0].Name
	}
	for _, n := range nodes {
		if slices.Contains(done, n.Name) {
			continue
		}
		if err := each(ctx, n, func() error { return upgradeKubeadm(ctx, *kad, n, n.Name == first) }); err != nil {
			return err
		}
	}
	if len(nodes) > 1 {
		return ui.StepRun("Spreading CoreDNS over the nodes again", func() error { return spreadCoreDNS(ctx, prev.Outputs.Kubeconfig) })
	}
	return nil
}

// podNodes are the nodes the cluster's own pods can run on: the workers, and the control
// planes when they're schedulable.
func podNodes(cfg *config.Config, nodes []providers.Node) []string {
	var out []string
	for _, n := range nodes {
		if n.Role != config.RoleControlPlane || cfg.ControlPlaneSchedulable() {
			out = append(out, n.Name)
		}
	}
	return out
}

// upgradeNodes rolls the new release over the nodes the distribution's way (roll), each one
// inside rollNode, then records it. done are the nodes that already ran it when the plan was
// made.
func (o *Orchestrator) upgradeNodes(ctx context.Context, cfg *config.Config, prev *state.ClusterState, up *upgrade, kubectl string, done []string,
	roll func(ctx context.Context, done []string, each rollFunc) error) error {
	out := prev.Outputs
	var upgraded []string
	stopped := func(err error) error {
		if len(upgraded) > 0 {
			err = fmt.Errorf("%w\nAlready on %s, so skipped when you run it again: %s", err, up.release, strings.Join(upgraded, ", "))
		}
		return err
	}
	// Nodes a stopped run upgraded but didn't uncordon: they run the release, so the roll
	// below leaves them alone; they're finished first. One it cordoned but didn't upgrade (its
	// drain stopped) goes through the roll as any other.
	for _, name := range slices.Clone(prev.Cordoned) {
		i := slices.IndexFunc(out.Nodes, func(n providers.Node) bool { return n.Name == name })
		if i < 0 || !slices.Contains(done, name) {
			continue
		}
		if err := o.nodeBack(ctx, cfg, prev, up, kubectl, out.Nodes[i]); err != nil {
			return stopped(err)
		}
	}
	each := func(ctx context.Context, n providers.Node, change func() error) error {
		if err := o.rollNode(ctx, cfg, prev, up, kubectl, n, change); err != nil {
			return err
		}
		upgraded = append(upgraded, n.Name)
		return nil
	}
	if err := roll(ctx, done, each); err != nil {
		return stopped(err)
	}
	prev.K8sVersion = up.to
	if prev.Spec != nil {
		spec := *prev.Spec
		spec.Version = up.to
		prev.Spec = &spec
	}
	if err := state.Save(prev.Name, *prev); err != nil {
		return fmt.Errorf("every node runs %s, but the state couldn't be saved: %w", up.release, err)
	}
	ui.Info(fmt.Sprintf("Every node runs %s %s", distributionName(cfg.Distribution()), up.release))
	return nil
}

// rollNode upgrades one node: drained, changed, then back (nodeBack). The state remembers it
// as cordoned until it's back, so a run that stops in between finishes it next time.
func (o *Orchestrator) rollNode(ctx context.Context, cfg *config.Config, prev *state.ClusterState, up *upgrade, kubectl string, n providers.Node, change func() error) error {
	if !slices.Contains(prev.Cordoned, n.Name) {
		prev.Cordoned = append(prev.Cordoned, n.Name)
		if err := state.Save(prev.Name, *prev); err != nil {
			return err
		}
	}
	if err := ui.StepRun(fmt.Sprintf("Draining %s", n.Name), func() error {
		return drainNode(ctx, kubectl, prev.Outputs.Kubeconfig, n.Name, o.drainLimit())
	}); err != nil {
		return err
	}
	if err := ui.StepRun(fmt.Sprintf("Upgrading %s to %s", n.Label(), up.release), change); err != nil {
		return fmt.Errorf("%w\n%s stays cordoned; running it again upgrades it, then uncordons it", err, n.Name)
	}
	return o.nodeBack(ctx, cfg, prev, up, kubectl, n)
}

// nodeBack waits for an upgraded node to be Ready at the new release (a control plane: the
// API ready on every control plane too), then uncordons it.
func (o *Orchestrator) nodeBack(ctx context.Context, cfg *config.Config, prev *state.ClusterState, up *upgrade, kubectl string, n providers.Node) error {
	kubeconfig, cordoned := prev.Outputs.Kubeconfig, fmt.Sprintf("\n%s stays cordoned; running it again checks it, then uncordons it", n.Name)
	if err := o.nodeReady(ctx, cfg, prev, up.release, n); err != nil {
		return fmt.Errorf("%w%s", err, cordoned)
	}
	if err := ui.StepRun(fmt.Sprintf("Uncordoning %s", n.Name), func() error {
		return uncordonNode(ctx, kubectl, kubeconfig, n.Name)
	}); err != nil {
		return fmt.Errorf("%w%s", err, cordoned)
	}
	prev.Cordoned = slices.DeleteFunc(prev.Cordoned, func(c string) bool { return c == n.Name })
	return state.Save(prev.Name, *prev)
}

// liveNodes reads the nodes through the API, for at most 30 seconds.
func liveNodes(ctx context.Context, kubeconfig string) ([]kubeapi.NodeMeta, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	api, err := nodeAPI(kubeconfig)
	if err != nil {
		return nil, err
	}
	return api.NodeMetas(ctx)
}

// newerKubernetes says when a newer patch of the running minor release is out; nothing when
// there's none, or the release channel doesn't answer within a few seconds.
func newerKubernetes(ctx context.Context, distribution, running string) string {
	r, ok := parseRelease(nodeRelease(distribution, running))
	if !ok {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	newest, err := newestPatch(ctx, distribution, r.minor)
	if err != nil {
		ui.Debug("newest patch of v1.%d: %v", r.minor, err)
		return ""
	}
	if n, ok := parseRelease(newest); !ok || n.compare(r) <= 0 {
		return ""
	}
	return fmt.Sprintf("Kubernetes %s is out (%s runs): set spec.kubernetes.version: %s and apply to upgrade", newest, nodeRelease(distribution, running), newest)
}
