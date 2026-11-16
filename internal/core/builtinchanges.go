package core

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
	"github.com/towproj/tow/internal/ui"
)

// builtinClient is what planning the built-in add-ons uses of helm, kubectl and the API;
// swapped in tests.
type builtinClient interface {
	ReleaseVersions(ctx context.Context) (map[string]string, error)
	DeploymentImage(ctx context.Context, namespace, name string) (string, error)
	Volumes(ctx context.Context, drivers []string) ([]string, error)
	LoadBalancerServices(ctx context.Context) ([]string, error)
	Ingresses(ctx context.Context, class string, isDefault bool) ([]string, error)
	Install(ctx context.Context, b addons.Builtin) error
	Remove(ctx context.Context, b addons.Builtin) error
	DeleteObjects(ctx context.Context, objs []addons.Object) error
	UpdateCNI(ctx context.Context, n addons.Network) error
}

// liveBuiltins is builtinClient against a real cluster.
type liveBuiltins struct {
	ad  addons.Options
	api *kubeapi.Client
}

var newBuiltinClient = func(ad addons.Options) (builtinClient, error) {
	api, err := kubeapiFromFile(ad.Kubeconfig)
	if err != nil {
		return nil, err
	}
	return liveBuiltins{ad, api}, nil
}

func (l liveBuiltins) ReleaseVersions(ctx context.Context) (map[string]string, error) {
	return l.ad.ReleaseVersions(ctx)
}
func (l liveBuiltins) DeploymentImage(ctx context.Context, namespace, name string) (string, error) {
	return l.api.DeploymentImage(ctx, namespace, name)
}
func (l liveBuiltins) Volumes(ctx context.Context, drivers []string) ([]string, error) {
	vols, err := l.api.VolumesOf(ctx, drivers)
	var out []string
	for _, v := range vols {
		out = append(out, cmp.Or(v.Claim, v.Name))
	}
	return out, err
}
func (l liveBuiltins) LoadBalancerServices(ctx context.Context) ([]string, error) {
	return l.api.LoadBalancerServices(ctx)
}
func (l liveBuiltins) Ingresses(ctx context.Context, class string, isDefault bool) ([]string, error) {
	return l.api.Ingresses(ctx, class, isDefault)
}
func (l liveBuiltins) Install(ctx context.Context, b addons.Builtin) error {
	return l.ad.InstallBuiltin(ctx, b)
}
func (l liveBuiltins) Remove(ctx context.Context, b addons.Builtin) error {
	return l.ad.RemoveBuiltin(ctx, b)
}
func (l liveBuiltins) DeleteObjects(ctx context.Context, objs []addons.Object) error {
	return l.ad.DeleteObjects(ctx, objs)
}
func (l liveBuiltins) UpdateCNI(ctx context.Context, n addons.Network) error {
	return addons.UpdateCNI(ctx, n)
}

// refusal is a change planning found it can't make, with what was already known: nothing
// has changed yet. Its lines are shown like the file's refused changes.
type refusal struct{ lines []string }

func (r refusal) Error() string { return strings.Join(r.lines, "; ") }

// builtinPlan is what the built-in add-ons need, in three parts: removals that must come
// first (a load balancer the file replaces, the default StorageClass moving), then what's
// turned on or changed (the CNI's settings first), and, after the file's own charts and
// manifests, what's turned off.
type builtinPlan struct {
	early, installs, late []planStep
	notes                 []string
}

// planBuiltins compares the built-in add-ons of the last applied file (old) with the file's,
// each built at the version that runs: a new one is installed, a changed one applied again
// at its version, one that left the file removed. Version changes are refused, and so is
// removing storage that still has volumes. before are the nodes old's add-ons were built for
// (nil: the same as now): machines added or removed change what names them.
//
// With an upgrade (up), they move to this tow's releases, or the file's pins, instead:
// never backwards, and never further than each one's docs allow in one go.
func (o *Orchestrator) planBuiltins(ctx context.Context, cfg, old *config.Config, prev *state.ClusterState, before []providers.Node, up *upgrade) (builtinPlan, error) {
	var bp builtinPlan
	if old == nil || cfg.Mode == config.ModeLocal || prev.Outputs == nil || prev.Outputs.Kubeconfig == "" {
		return bp, nil
	}
	minor := minorOf(prev.K8sVersion) // the Kubernetes they run on: the new one, with an upgrade
	if up != nil {
		minor = minorOf(up.to)
	}
	oldNodes, newNodes := before, prev.Outputs.Nodes
	if oldNodes == nil {
		oldNodes = newNodes
	}
	was := Pending(old)
	for _, p := range Pending(cfg) {
		if !slices.Contains(was, p) {
			bp.notes = append(bp.notes, p+": this tow doesn't install it yet, so nothing changes on the cluster")
		}
	}

	options := func(c *config.Config, nodes []providers.Node, running map[string]string) addons.Options {
		return addons.Options{Kubeconfig: prev.Outputs.Kubeconfig, Dir: state.Dir(c.Name()), Kubernetes: c.Spec.Kubernetes, Cluster: c.Name(),
			Proxmox: proxmoxDisks(c), Nodes: nodes, Running: running}
	}
	// Cilium is the CNI whose settings change in place; the others' are refused. With an
	// upgrade, any CNI tow installed moves to its new release.
	network := func(c *config.Config, nodes []providers.Node, running string) *addons.Network {
		k := c.Spec.Kubernetes
		if !addons.InstallsCNI(k.Networking.CNI, k.Distribution) || (k.Networking.CNI != config.CNICilium && up == nil) {
			return nil
		}
		host, port := apiAddress(prev.Outputs)
		return &addons.Network{Kubeconfig: prev.Outputs.Kubeconfig, Dir: state.Dir(c.Name()), Networking: k.Networking, ServiceLB: k.ServiceLoadBalancer,
			Ingress: k.Ingress, KubeMinor: minor, APIHost: host, APIPort: port, Nodes: len(nodes), Running: running, Compat: prev.CiliumCompat}
	}
	oldB, newB := addons.Builtins(options(old, oldNodes, nil)), addons.Builtins(options(cfg, newNodes, nil))
	oldNet, newNet := network(old, oldNodes, ""), network(cfg, newNodes, "")
	if len(oldB)+len(newB) == 0 && oldNet == nil && newNet == nil {
		return bp, nil
	}

	// helm and kubectl, as for the file's own charts and manifests.
	uses := func(program string) bool {
		return slices.ContainsFunc(append(slices.Clone(oldB), newB...), func(b addons.Builtin) bool { return b.Uses(program) })
	}
	toolOpts := tools.Options{Install: o.installTools, DryRun: o.dryRun}
	ad := options(cfg, newNodes, nil)
	var err error
	if uses("helm") || newNet != nil {
		if ad.Helm, err = findHelm(ctx, tools.Helm, toolOpts); err != nil {
			return bp, err
		}
	}
	if uses("kubectl") || (newNet != nil && (newNet.NeedsKubectl() || up != nil)) { // an upgrade's Cilium pre-flight check
		if ad.Kubectl, err = findKubectl(ctx, tools.Kubectl(fmt.Sprintf("v1.%d.0", minor)), toolOpts); err != nil {
			return bp, err
		}
	}
	api, err := newBuiltinClient(ad)
	if err != nil {
		return bp, err
	}
	running, cniRunning, err := runningVersions(ctx, api, append(slices.Clone(oldB), newB...), cniOf(cfg))
	if err != nil {
		return bp, err
	}
	// Built again at the versions that run: a running cluster keeps them, until an upgrade.
	keep, keepCNI := running, cniRunning
	if up != nil {
		keep, keepCNI = nil, ""
	}
	oldB, newB = addons.Builtins(options(old, oldNodes, running)), addons.Builtins(options(cfg, newNodes, keep))
	oldNet, newNet = network(old, oldNodes, cniRunning), network(cfg, newNodes, keepCNI)
	oldIdx, newIdx := byName(oldB), byName(newB)

	// Refused: another version of something that runs (with an upgrade: an older one, or
	// one too far), and storage that still has volumes.
	var refused []string
	for _, b := range newB {
		v := running[b.Name()]
		switch {
		case oldIdx[b.Name()] == nil || v == "":
		case up != nil:
			refused = append(refused, versionMove(b.Name(), v, b.Version())...)
		case b.Pin() != "" && b.Pin() != v:
			refused = append(refused, fmt.Sprintf("%s: %s runs, the file asks for %s: %s", b.Name(), v, b.Pin(), whyVersion))
		}
	}
	switch {
	case oldNet == nil || newNet == nil || cniRunning == "":
	case up != nil:
		refused = append(refused, versionMove(newNet.Networking.CNI, cniRunning, newNet.Version())...)
	case newNet.Pin() != "" && newNet.Pin() != cniRunning:
		refused = append(refused, fmt.Sprintf("cilium: %s runs, the file asks for %s: %s", cniRunning, newNet.Pin(), whyVersion))
	}
	for _, b := range oldB {
		if newIdx[b.Name()] != nil || !b.Storage() {
			continue
		}
		vols, err := api.Volumes(ctx, []string{addons.VolumeDrivers[b.CSI()]})
		if err != nil {
			return bp, err
		}
		if len(vols) > 0 {
			refused = append(refused, fmt.Sprintf("spec.kubernetes.storage.csi: %s still has %s (%s): delete them (their claims) first, or keep it in the file",
				b.CSI(), ui.Plural(len(vols), "volume"), describeNames(vols)))
		}
	}
	if len(refused) > 0 {
		return bp, refusal{refused}
	}

	// The CNI's settings: Cilium's encryption, load balancer and ingress.
	if oldNet != nil && newNet != nil && !newNet.Same(*oldNet) {
		lines := []string{fmt.Sprintf("cilium %s: apply the file's new settings (its agents restart one node at a time)", newNet.Version())}
		if newNet.Version() != oldNet.Version() {
			newNet.UpgradeFrom = oldNet.Version()
			lines = []string{fmt.Sprintf("%s %s → %s: upgrade%s", newNet.Networking.CNI, oldNet.Version(), newNet.Version(), map[string]string{
				config.CNICilium: " (its pre-flight check pulls the new images first; its agents restart one node at a time)",
				config.CNICalico: " (its CRDs first, then its operator)",
			}[newNet.Networking.CNI])}
		}
		if addons.CiliumIngress(old.Spec.Kubernetes.Ingress) && !addons.CiliumIngress(cfg.Spec.Kubernetes.Ingress) {
			ings, err := api.Ingresses(ctx, config.IngressCilium, old.Spec.Kubernetes.Ingress.DefaultClass == config.IngressCilium)
			if err != nil {
				return bp, err
			}
			if len(ings) > 0 {
				lines = append(lines, fmt.Sprintf("the Ingresses it serves lose their controller (%d): %s", len(ings), describeNames(ings)))
			}
		}
		n := *newNet
		n.Helm, n.Kubectl = ad.Helm, ad.Kubectl
		bp.installs = append(bp.installs, planStep{lines: lines, run: func(ctx context.Context) error {
			if err := api.UpdateCNI(ctx, n); err != nil {
				return err
			}
			// Cilium keeps the defaults of the release it was installed at (its upgrade
			// docs): known from its first upgrade on.
			if n.Networking.CNI == config.CNICilium && n.UpgradeFrom != "" && prev.CiliumCompat == "" {
				prev.CiliumCompat = n.CompatMinor()
				return state.Save(prev.Name, *prev)
			}
			return nil
		}})
	}
	if older := newerReleases(newB, running, cniOf(cfg), cniRunning); len(older) > 0 && up == nil {
		bp.notes = append(bp.notes, newerNote(older))
	}

	// What's turned on or changed, in install order. Their secrets are read below, once
	// it's known which need them.
	installer := map[string]addons.Builtin{}
	var secretsFor []string // the storage drivers installed or changed
	remark := map[string]bool{}
	for _, b := range newB {
		ob := oldIdx[b.Name()]
		hasVersion := b.Release() != "" || b.Deployment() != ""
		var lines []string
		var replaced, dropped []addons.Object
		switch {
		case ob == nil:
			lines = []string{fmt.Sprintf("%s%s: turn on", b.Name(), at(b.Version()))}
		case hasVersion && running[b.Name()] == "":
			lines = []string{fmt.Sprintf("%s%s: install again (it's gone: removed outside tow?)", b.Name(), at(b.Version()))}
		case !b.Same(*ob):
			lines = []string{fmt.Sprintf("%s%s: apply the file's new settings", b.Name(), at(b.Version()))}
			if v := running[b.Name()]; up != nil && v != "" && v != b.Version() {
				lines = []string{fmt.Sprintf("%s %s → %s: upgrade", b.Name(), v, b.Version())}
			}
			replaced, dropped = b.Replaced(*ob), addons.Missing(ob.Objects(), b.Objects())
			for _, r := range replaced {
				lines = append(lines, fmt.Sprintf("StorageClass %s is deleted and made again (its settings can't change in place); volumes made before keep the old ones", r.Name))
				remark[r.Name] = true
			}
			if len(dropped) > 0 {
				lines = append(lines, "then delete what it no longer has: "+describeObjects(dropped))
			}
		case b.DefaultClass() && remark[b.Class()]:
			lines = []string{fmt.Sprintf("StorageClass %s: marked the default again (it was made again)", b.Class())}
		default:
			continue
		}
		if b.CSI() != "" {
			secretsFor = append(secretsFor, b.CSI())
		}
		name := b.Name()
		bp.installs = append(bp.installs, planStep{lines: lines, run: func(ctx context.Context) error {
			done := ui.Step("Applying " + strings.TrimSpace(name+" "+installer[name].Version()))
			if err := api.DeleteObjects(ctx, replaced); err != nil {
				fmt.Println("failed")
				return err
			}
			if err := api.Install(ctx, installer[name]); err != nil {
				fmt.Println("failed")
				return err
			}
			if err := api.DeleteObjects(ctx, dropped); err != nil {
				fmt.Println("failed")
				return err
			}
			done()
			return nil
		}})
	}
	withSecrets := options(cfg, newNodes, keep)
	withSecrets.Helm, withSecrets.Kubectl = ad.Helm, ad.Kubectl
	if len(secretsFor) > 0 && !o.dryRun {
		k := cfg.Spec.Kubernetes
		k.Addons, k.Storage.CSI = config.Addons{}, secretsFor
		if withSecrets.Secrets, err = readSecrets(addons.Secrets(k)); err != nil {
			return bp, err
		}
	}
	for _, b := range addons.Builtins(withSecrets) {
		installer[b.Name()] = b
	}

	// What's turned off, in reverse install order: the load balancer and the default
	// StorageClass early, the rest last.
	newLB := ""
	for _, b := range newB {
		if b.ServiceLB() {
			newLB = cfg.Spec.Kubernetes.ServiceLoadBalancer.Provider
		}
	}
	for i := len(oldB) - 1; i >= 0; i-- {
		b := oldB[i]
		if newIdx[b.Name()] != nil {
			continue
		}
		lines := []string{fmt.Sprintf("%s: turn off", b.Name())}
		early := false
		switch {
		case b.Storage():
			lines[0] += fmt.Sprintf(", with its StorageClass %s", b.Class())
		case b.DefaultClass():
			if slices.ContainsFunc(oldB, func(s addons.Builtin) bool { return s.Storage() && s.Class() == b.Class() && newIdx[s.Name()] == nil }) {
				continue // its class goes with its driver
			}
			lines, early = []string{fmt.Sprintf("StorageClass %s: no longer the default", b.Class())}, true
		case b.ServiceLB():
			early = true
			svcs, err := api.LoadBalancerServices(ctx)
			if err != nil {
				return bp, err
			}
			svcs = outside(svcs, removedNamespaces(oldB, newIdx))
			switch {
			case len(svcs) > 0 && newLB != "":
				lines = append(lines, fmt.Sprintf("the LoadBalancer Services get a new address from %s (%d): %s", newLB, len(svcs), describeNames(svcs)))
			case len(svcs) > 0:
				lines = append(lines, fmt.Sprintf("the LoadBalancer Services lose their address (%d): %s", len(svcs), describeNames(svcs)))
			}
		case b.Ingress():
			ings, err := api.Ingresses(ctx, b.Class(), old.Spec.Kubernetes.Ingress.DefaultClass == b.Class())
			if err != nil {
				return bp, err
			}
			if len(ings) > 0 {
				lines = append(lines, fmt.Sprintf("the Ingresses it serves lose their controller (%d): %s", len(ings), describeNames(ings)))
			}
		}
		step := planStep{lines: lines, run: func(ctx context.Context) error {
			done := ui.Step("Removing " + b.Name())
			if err := api.Remove(ctx, b); err != nil {
				fmt.Println("failed")
				return err
			}
			done()
			return nil
		}}
		if early {
			bp.early = append(bp.early, step)
		} else {
			bp.late = append(bp.late, step)
		}
	}
	return bp, nil
}

// runningVersions reads which version of each built-in add-on runs, by name (absent: it
// doesn't run), from its Helm release or its Deployment's image, and the CNI's.
func runningVersions(ctx context.Context, api builtinClient, units []addons.Builtin, net *addons.Network) (map[string]string, string, error) {
	running := map[string]string{}
	var releases map[string]string
	needReleases := net != nil || slices.ContainsFunc(units, func(b addons.Builtin) bool { return b.Release() != "" })
	if needReleases {
		var err error
		if releases, err = api.ReleaseVersions(ctx); err != nil {
			return nil, "", err
		}
	}
	for _, b := range units {
		switch {
		case b.Release() != "":
			if v, ok := releases[b.Release()]; ok {
				running[b.Name()] = v
			}
		case b.Deployment() != "":
			ns, name, _ := strings.Cut(b.Deployment(), "/")
			image, err := api.DeploymentImage(ctx, ns, name)
			if err != nil {
				return nil, "", err
			}
			if v := addons.ImageVersion(image); v != "" {
				running[b.Name()] = v
			}
		}
	}
	cni := ""
	if net != nil {
		cni = releases[net.Release()]
	}
	return running, cni, nil
}

// removedNamespaces are the namespaces of the add-ons this run turns off: what's in them
// goes too.
func removedNamespaces(oldB []addons.Builtin, newIdx map[string]*addons.Builtin) []string {
	var out []string
	for _, b := range oldB {
		if newIdx[b.Name()] == nil && b.Namespace() != "" {
			out = append(out, b.Namespace())
		}
	}
	return out
}

// outside keeps the namespace/name objects that aren't in one of namespaces.
func outside(objs, namespaces []string) []string {
	return slices.DeleteFunc(objs, func(o string) bool {
		ns, _, _ := strings.Cut(o, "/")
		return slices.Contains(namespaces, ns)
	})
}

func byName(bs []addons.Builtin) map[string]*addons.Builtin {
	out := map[string]*addons.Builtin{}
	for i := range bs {
		out[bs[i].Name()] = &bs[i]
	}
	return out
}

// at is " <version>", or nothing for an add-on without one of its own.
func at(version string) string {
	if version == "" {
		return ""
	}
	return " " + version
}

// minorSteps is how many minor releases an add-on may move in one go, where its upgrade docs
// say: Cilium and Longhorn one at a time, Calico from its two previous releases.
var minorSteps = map[string]int{config.CNICilium: 1, config.CNICalico: 2, config.CSILonghorn: 1}

// versionMove refuses what an upgrade can't do to an add-on's release: go back, or move
// further than its docs allow (naming the release to pin first).
func versionMove(name, from, to string) []string {
	a, okA := semverParts(from)
	b, okB := semverParts(to)
	switch {
	case !okA || !okB || from == to:
		return nil
	case addons.Older(to, from):
		return []string{fmt.Sprintf("%s: %s runs, and %s is older: an add-on isn't downgraded; pin %s, or leave its version out", name, from, to, from)}
	case a[0] == b[0] && minorSteps[name] > 0 && b[1]-a[1] > minorSteps[name]:
		return []string{fmt.Sprintf("%s: %s runs, and %s is %d minor releases on, more than its docs allow in one go (%d): pin %d.%d (its newest patch) with this upgrade, and the next upgrade moves it further",
			name, from, to, b[1]-a[1], minorSteps[name], a[0], a[1]+minorSteps[name])}
	}
	return nil
}

// semverParts reads "1.20.4" or "v3.33.0" as numbers.
func semverParts(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.SplitN(strings.TrimPrefix(v, "v"), ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		p, _, _ = strings.Cut(p, "-")
		n, err := strconv.Atoi(p)
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// cniOf is the CNI tow installs on cfg's cluster, for its version; nil when the
// distribution brings its own.
func cniOf(cfg *config.Config) *addons.Network {
	if k := cfg.Spec.Kubernetes; addons.InstallsCNI(k.Networking.CNI, k.Distribution) {
		return &addons.Network{Networking: k.Networking}
	}
	return nil
}

// newerReleases names the built-in add-ons, and the CNI, that run an older release than
// this tow installs, where the file pins none: "longhorn 1.13.0 (1.12.1 runs)".
func newerReleases(units []addons.Builtin, running map[string]string, cni *addons.Network, cniRunning string) []string {
	var out []string
	for _, b := range units {
		if v := running[b.Name()]; v != "" && b.Pin() == "" && addons.Older(v, b.Tested()) {
			out = append(out, fmt.Sprintf("%s %s (%s runs)", b.Name(), b.Tested(), v))
		}
	}
	if cni != nil && cniRunning != "" && cni.Pin() == "" && addons.Older(cniRunning, cni.Tested()) {
		out = append(out, fmt.Sprintf("%s %s (%s runs)", cni.Networking.CNI, cni.Tested(), cniRunning))
	}
	return out
}

func newerNote(older []string) string {
	return "newer releases this tow installs, which come with the next Kubernetes upgrade: " + strings.Join(older, ", ")
}

// installedTool finds a tool without asking or installing; swapped in tests.
var installedTool = tools.Installed

// olderBuiltins is newerReleases for status, read without changing anything: nothing when
// helm isn't on this machine or the cluster can't be read.
func olderBuiltins(ctx context.Context, st *state.ClusterState) []string {
	if st.File == "" || st.Outputs == nil || st.Outputs.Kubeconfig == "" {
		return nil
	}
	cfg, err := config.Restore(st.File)
	if err != nil || cfg.Mode == config.ModeLocal {
		return nil
	}
	helm := installedTool(ctx, tools.Helm)
	if helm == "" {
		return nil
	}
	ad := addons.Options{Helm: helm, Kubeconfig: st.Outputs.Kubeconfig, Kubernetes: cfg.Spec.Kubernetes, Cluster: st.Name,
		Proxmox: proxmoxDisks(cfg), Nodes: st.Outputs.Nodes}
	api, err := newBuiltinClient(ad)
	if err != nil {
		return nil
	}
	running, cniRunning, err := runningVersions(ctx, api, addons.Builtins(ad), cniOf(cfg))
	if err != nil {
		return nil
	}
	ad.Running = running
	return newerReleases(addons.Builtins(ad), running, cniOf(cfg), cniRunning)
}

// describeNames names a few, then counts the rest.
func describeNames(names []string) string {
	const shown = 4
	if len(names) <= shown {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(names[:shown], ", "), len(names)-shown)
}
