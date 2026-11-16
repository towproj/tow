package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/secretenv"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
	"github.com/towproj/tow/internal/ui"
	"github.com/towproj/tow/internal/versions"
)

// Swappable so tests run without network access.
var resolveVersion = versions.Resolve

// Swappable so tests run without machines.
var (
	prepareMachines = prepare.Run
	setupKubeadm    = kubeadm.Setup
)

type Orchestrator struct {
	dryRun       bool
	yes          bool   // prepare the user's own machines without asking
	installTools bool   // install a missing tool on this machine without asking
	stopAfter    string // tests: the last stage to reach (state.StageMachines or StagePrepared)
	volumes      bool   // delete also deletes the cluster's volumes (delete -v)
	reset        bool   // delete also undoes the preparation of the user's own machines (--reset)
	forget       bool   // delete only forgets a cluster on the user's own machines (--forget)
	// askReset, when set, asks whether delete should undo the preparation too (a terminal
	// without --reset or --yes).
	askReset func(question string) bool
	// drainTimeout is how long a machine leaving a running cluster gets to move its pods
	// away (apply --drain-timeout); 0 = DefaultDrainTimeout.
	drainTimeout time.Duration
	// noBackup: restore takes no safety backup first (restore --no-backup).
	noBackup bool
	// renewCerts: apply renews the cluster's certificates now (apply --renew-certificates).
	renewCerts bool
}

// WithCertRenewal makes apply renew the cluster's certificates now, not only within 30 days
// of their expiry.
func (o *Orchestrator) WithCertRenewal() *Orchestrator {
	o.renewCerts = true
	return o
}

// WithoutBackup makes restore skip its safety backup of the state it leaves.
func (o *Orchestrator) WithoutBackup() *Orchestrator {
	o.noBackup = true
	return o
}

// WithDrainTimeout gives a machine leaving the cluster this long to move its pods away.
func (o *Orchestrator) WithDrainTimeout(d time.Duration) *Orchestrator {
	o.drainTimeout = d
	return o
}

// WithVolumes makes delete also delete the cluster's volumes that outlive it: on EBS or
// Proxmox disks, and on the user's own machines' disks (delete -v); without it, a cluster
// that has some isn't deleted.
func (o *Orchestrator) WithVolumes() *Orchestrator {
	o.volumes = true
	return o
}

// WithReset makes delete, on the user's own machines, also undo what tow prepared there
// (delete --reset); and apply, on a machine of the user's leaving the cluster (apply --reset).
func (o *Orchestrator) WithReset() *Orchestrator {
	o.reset = true
	return o
}

// AskReset has delete ask whether to undo the preparation too, when there is some.
func (o *Orchestrator) AskReset(ask func(question string) bool) *Orchestrator {
	o.askReset = ask
	return o
}

// Forget makes delete forget a cluster on the user's own machines without touching them
// (delete --forget): for a machine that's gone for good.
func (o *Orchestrator) Forget() *Orchestrator {
	o.forget = true
	return o
}

// StopAfter makes apply stop once the machines reach a stage: state.StageMachines leaves
// them as created, state.StagePrepared prepared, with no Kubernetes on them. For tests that
// need such machines (the byoh test's fixture); no command offers it.
func (o *Orchestrator) StopAfter(stage string) *Orchestrator {
	o.stopAfter = stage
	return o
}

func (o *Orchestrator) noKubernetes() bool {
	return o.stopAfter == state.StagePrepared || o.stopAfter == state.StageMachines
}

// InstallTools installs a tool tow needs on this machine (helm, kubectl, tofu), when it's missing or
// unsupported, without asking (apply --install-tools). Without it, tow asks.
func (o *Orchestrator) InstallTools() *Orchestrator {
	o.installTools = true
	return o
}

func NewOrchestrator(dryRun bool) *Orchestrator {
	return &Orchestrator{dryRun: dryRun}
}

// AssumeYes makes the changes the user's own machines need without asking (apply --yes).
// They're listed either way.
func (o *Orchestrator) AssumeYes() *Orchestrator {
	o.yes = true
	return o
}

// Apply builds the cluster a validated config describes, using the adapter its mode calls for.
// Re-applying an unchanged config is a no-op; a changed one is refused with a diff (drift).
func (o *Orchestrator) Apply(ctx context.Context, cfg *config.Config) error {
	// The variables holding the file's secrets never reach the programs tow starts.
	secretenv.Hide(cfg.SecretEnv()...)

	remote := cfg.Mode != config.ModeLocal
	p, err := newProvider(providerForMode[cfg.Mode])
	if err != nil {
		return err
	}

	// One run changes a cluster at a time; a dry run changes nothing.
	if !o.dryRun {
		unlock, err := state.Lock(cfg.Name())
		if err != nil {
			return err
		}
		defer unlock()
	}

	prev, err := state.Load(cfg.Name())
	var unfinished *state.ClusterState
	switch {
	case err == nil && prev.ConfigHash != "" && prev.Mode != cfg.Mode:
		// Checked before drift and before retries: a raw diff wouldn't say why this is refused.
		return modeSwitchError(prev, cfg)
	case err == nil && prev.Status == state.StatusFailed:
		// A half-finished cluster isn't drift: nothing runs on it yet, so the (possibly fixed)
		// config is applied again and the provider reuses whatever it already created.
		ui.Info(fmt.Sprintf("Last apply of '%s' did not finish; retrying", cfg.Name()))
		unfinished = prev
	case err == nil:
		return o.update(ctx, p, cfg, prev)
	case !errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("read state for '%s': %w", cfg.Name(), err)
	}

	if cfg.Mode == config.ModeBYOH {
		if err := checkMachinesFree(cfg); err != nil {
			return err
		}
	}

	// k3s or kubeadm goes on the machines. The kubeconfig context must be free before anything
	// is created, unless an unfinished apply of this cluster made it.
	var inst *k3s.Options
	var kad *kubeadm.Options
	switch {
	case remote && cfg.Distribution() == config.DistributionK3s:
		inst = k3sOptions(cfg)
	case remote && cfg.Distribution() == config.DistributionKubeadm:
		kad = kubeadmOptions(cfg)
	}
	if remote {
		if kc := cfg.Spec.Kubernetes.Kubeconfig; *kc.Merge && !ownContext(unfinished, kc.Context) {
			if err := kubecfg.CheckFree(kc.Context); err != nil {
				return err
			}
		}
	}

	// The add-ons (storage, metrics-server, Helm charts, manifests) and the CNIs tow
	// installs need helm on this machine: found, or installed after a yes, before anything is
	// created. A run that stops once the machines are prepared (tests only) installs neither.
	var ad *addons.Options
	var net *addons.Network
	k := cfg.Spec.Kubernetes
	if extras := (addons.Options{Kubernetes: k, Charts: k.Addons.HelmCharts, ManifestPath: cfg.ManifestPath, Cluster: cfg.Name(), Proxmox: proxmoxDisks(cfg)}); !extras.Empty() {
		ad = &extras
	}
	cni := remote && addons.InstallsCNI(k.Networking.CNI, cfg.Distribution())
	if o.noKubernetes() {
		ad, cni = nil, false
	}
	toolOpts := tools.Options{Install: o.installTools, DryRun: o.dryRun}
	if (ad != nil && ad.NeedsHelm()) || cni {
		helm, err := findHelm(ctx, tools.Helm, toolOpts)
		if err != nil {
			return err
		}
		if ad != nil {
			ad.Helm = helm
		}
		if cni {
			net = &addons.Network{Helm: helm, Networking: k.Networking, ServiceLB: k.ServiceLoadBalancer, Ingress: k.Ingress}
		}
	}

	// Preflight before resolving the version: no network calls for a setup that can't run.
	spec := specFromConfig(cfg, "")
	// A retried apply whose machines may already run Kubernetes keeps them as they are:
	// OpenTofu replaces or deletes none of them. Machines nothing was installed on yet may
	// still change, after a fixed instance type say.
	if unfinished != nil && slices.Contains([]string{state.StagePrepared, state.StageKubernetes, state.StageAddons}, unfinished.Stage) {
		spec.Guard = providers.GuardKeep
	}
	if err := o.findProviderTool(ctx, p); err != nil {
		return err
	}
	if err := p.PreFlight(ctx, spec); err != nil {
		return err
	}
	// Secrets are read before anything is created; a dry run reads none.
	if inst != nil && !o.dryRun {
		if inst.Secrets, err = readK3sSecrets(inst); err != nil {
			return err
		}
	}
	if kad != nil && !o.dryRun {
		if kad.Secrets, err = readKubeadmSecrets(cfg, kad); err != nil {
			return err
		}
	}
	if ad != nil && !o.dryRun {
		if ad.Secrets, err = readSecrets(addons.Secrets(k)); err != nil {
			return err
		}
		if token, ok := ad.Secrets[config.ProxmoxCSITokenEnv]; ok {
			if _, _, err := addons.ProxmoxCSIToken(token); err != nil {
				return err
			}
		}
	}

	// Resume: when the last apply got the machines (or the cluster) and they're still what
	// the file asks for, they're reused as they are instead of being created or checked again.
	var resume *state.ClusterState
	if unfinished != nil && slices.Contains([]string{state.StageMachines, state.StagePrepared, state.StageKubernetes}, unfinished.Stage) &&
		unfinished.Outputs != nil && sameMachines(unfinished.Spec, spec) {
		resume = unfinished
	}

	// A resumed install keeps the release it started with: "stable" may have moved since, and
	// so may tow's own default; the machines must all run the same one.
	version := cfg.Spec.Kubernetes.Release()
	if pinned := cfg.Spec.Kubernetes.Version; resume != nil && (pinned == "" || pinned == config.VersionStable) && resume.K8sVersion != "" {
		version = resume.K8sVersion
	} else {
		if version, err = resolveVersion(ctx, cfg.Distribution(), version); err != nil {
			return err
		}
		ui.Debug("version %s resolved to %s", cfg.Spec.Kubernetes.Release(), version)
	}
	spec.Version = version

	// kubeadm's machines get their container runtime and packages after preparation. The
	// runtime's release is checked against the Kubernetes release now that it's known.
	if kad != nil {
		kad.Version = version
		warning, err := kubeadm.CheckVersions(*kad)
		if err != nil {
			return err
		}
		if warning != "" && cfg.Spec.Kubernetes.Version == config.VersionStable {
			ui.Warn(warning) // with a pinned release, the file's checks already said it
		}
	}

	// Calico's CRDs, flannel's namespace and some add-ons need kubectl of the cluster's minor
	// release: checked now that the release is known, still before anything is created.
	if (net != nil && net.NeedsKubectl()) || (ad != nil && ad.NeedsKubectl()) {
		kubectl, err := findKubectl(ctx, tools.Kubectl(version), toolOpts)
		if err != nil {
			return err
		}
		if net != nil {
			net.Kubectl = kubectl
		}
		if ad != nil {
			ad.Kubectl = kubectl
		}
	}
	if net != nil {
		net.KubeMinor = minorOf(version)
	}

	// Machines that exist get prepared for Kubernetes; k3d brings its own.
	var prep *prepare.Options
	if cfg.Mode != config.ModeLocal {
		prep = o.prepareOptions(cfg)
		if kad != nil && !o.noKubernetes() {
			// A runtime already on a machine that can't run this Kubernetes stops everything
			// before preparation changes anything.
			prep.Preflight = kad.Preflight
		}
	}

	fields := cfg.Fields()
	st := state.ClusterState{
		Mode:       cfg.Mode,
		ConfigHash: config.HashFields(fields),
		Config:     fields,
		File:       cfg.Effective(),
	}
	if spec.Guard == providers.GuardKeep {
		// Saved again if OpenTofu refuses or fails: the next run still keeps the machines.
		st.Stage = unfinished.Stage
	}
	return o.provision(ctx, p, spec, applyLabel(cfg.Mode, cfg.Name(), cfg), st, prep, kad, inst, net, ad, resume)
}

// proxmoxDisks is where the Proxmox CSI plugin puts its disks; nil off Proxmox. Its
// certificate check follows PROXMOX_VE_INSECURE, the setting tow itself uses there.
func proxmoxDisks(cfg *config.Config) *addons.Proxmox {
	p := cfg.Spec.Infrastructure.Proxmox
	if cfg.Provider() != config.ProviderProxmox || p == nil {
		return nil
	}
	insecure, _ := strconv.ParseBool(os.Getenv("PROXMOX_VE_INSECURE"))
	return &addons.Proxmox{URL: p.URL, Node: p.Node, Storage: p.Storage, Insecure: insecure}
}

// prepareOptions says how to prepare the cluster's machines. Machines tow created are
// prepared without asking; the user's own are, after a yes (or apply --yes).
func (o *Orchestrator) prepareOptions(cfg *config.Config) *prepare.Options {
	k := cfg.Spec.Kubernetes
	opts := &prepare.Options{
		Cluster:     cfg.Name(),
		Distro:      k.Distribution,
		PodCIDR:     k.Networking.PodCIDR,
		ServiceCIDR: k.Networking.ServiceCIDR,
		Needs:       func(role string) prepare.Needs { return prepare.NeedsFor(cfg, role) },
		AdminCIDRs:  cfg.Spec.Infrastructure.Access.AdminCIDRs,
	}
	if cfg.Mode == config.ModeBYOH && !o.yes {
		opts.Confirm = func(summary string) bool {
			fmt.Println(summary)
			if !ui.Interactive() {
				ui.Hint("Nobody to ask here: run 'tow apply --yes' to make these changes")
				return false
			}
			return ui.Confirm("Make these changes on your machines?")
		}
	}
	return opts
}

// sameMachines reports whether the machine part of two specs is the same: the version
// ("stable" resolves anew) and the secret variables don't count, nor do the Kubernetes
// settings on machines (installing them is idempotent). On k3d they do: they are the cluster.
func sameMachines(saved *providers.Spec, spec providers.Spec) bool {
	if saved == nil {
		return false
	}
	a, b := *saved, spec
	a.Version, b.Version = "", ""
	if b.Local == nil {
		a.Kubernetes, b.Kubernetes = nil, nil
	}
	a.SecretEnv, b.SecretEnv = nil, nil
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}

// provision runs plan-or-provision → prepare the machines → install Kubernetes → persist
// state. st carries caller-specific fields (mode, config fingerprint); provision fills in the
// rest. label names the slow step. prep is nil when there are no machines to prepare (k3d).
// kad, when set, installs Kubernetes on them with kubeadm (container runtime, packages, init,
// join); inst, with k3s (k3d does it itself). net, when set, is the CNI tow installs
// right after Kubernetes (Calico, Cilium; flannel on kubeadm). ad, when
// set, are the Helm charts to install once Kubernetes runs. resume, when set, is an
// unfinished apply whose machines (or k3d cluster) are reused as they are.
//
// A failed run leaves no state, except once machines (or a k3d cluster) may exist: state is
// then saved as failed, with the last stage finished, for 'apply' to resume and 'delete' to
// clean up.
func (o *Orchestrator) provision(ctx context.Context, p providers.Provider, spec providers.Spec, label string, st state.ClusterState, prep *prepare.Options, kad *kubeadm.Options, inst *k3s.Options, net *addons.Network, ad *addons.Options, resume *state.ClusterState) error {
	if o.noKubernetes() {
		kad, inst, net, ad = nil, nil, nil, nil
	}
	if o.stopAfter == state.StageMachines {
		prep = nil
	}
	if o.dryRun {
		steps, err := p.Plan(ctx, spec)
		if err != nil {
			return err
		}
		for _, s := range steps {
			ui.DryRun(s)
		}
		if prep != nil {
			ui.DryRun("Then each machine would be checked against the official docs and get only what it's missing:")
			for _, line := range prepare.Describe(prep.Needs(config.RoleControlPlane)) {
				ui.DryRun("  - " + line)
			}
			if prep.Confirm != nil {
				ui.DryRun("You'd be asked before anything changes on your machines ('apply --yes' skips the question)")
			}
		}
		if kad != nil {
			for _, line := range append(kubeadm.Describe(*kad), kubeadm.DescribeInstall(*kad)...) {
				ui.DryRun(line)
			}
		}
		if inst != nil {
			inst.Version = spec.Version
			for _, line := range k3s.Describe(*inst) {
				ui.DryRun(line)
			}
		}
		if kad != nil || inst != nil {
			k := spec.Kubernetes
			if k.Merge {
				ui.DryRun(fmt.Sprintf("Would add context '%s' to your kubeconfig and switch to it", k.Context))
			}
			ui.DryRun(fmt.Sprintf("Would save the cluster's kubeconfig to %s", filepath.Join(state.Dir(spec.Name), "kubeconfig")))
		}
		if net != nil {
			net.Kubeconfig, net.APIHost, net.APIPort = filepath.Join(state.Dir(spec.Name), "kubeconfig"), "<the API's address>", "6443"
			ui.DryRun(fmt.Sprintf("Then the network: %s, then a wait until every node is Ready:", net.Networking.CNI))
			for _, c := range addons.CNICommands(*net) {
				ui.DryRun("  " + c)
			}
		}
		if ad != nil {
			ad.Kubeconfig = filepath.Join(state.Dir(spec.Name), "kubeconfig")
			ui.DryRun("Then the add-ons, in order, each waited for:")
			for _, c := range addons.Commands(*ad) {
				ui.DryRun("  " + c)
			}
		}
		return nil
	}

	now := time.Now()
	st.Name = spec.Name
	st.Provider = p.Name()
	st.K8sVersion = spec.Version
	st.CreatedAt, st.UpdatedAt = now, now
	st.Spec = &spec

	var res *providers.Result
	if resume != nil {
		res, st.CreatedAt = resume.Outputs, resume.CreatedAt
		ui.Info("The machines from the last apply are still as the file asks; picking up from there")
	} else {
		if spec.Machines != nil {
			// Saved as failed before OpenTofu starts, not once it returns: an apply killed
			// while it runs leaves state that 'delete' cleans up from and 'apply' retries.
			st.Status = state.StatusFailed
			if err := state.Save(spec.Name, st); err != nil {
				return fmt.Errorf("save state: %w", err)
			}
		}
		done := ui.Step(label)
		var err error
		res, err = p.Provision(ctx, spec)
		if err != nil {
			fmt.Println("failed")
			return err
		}
		done()
	}
	st.Outputs = res

	if prep != nil && res != nil && len(res.Nodes) > 0 {
		// The machines exist: remember them before changing anything, so a failure can be
		// resumed ('apply') or cleaned up ('delete').
		st.Status, st.Stage = state.StatusFailed, state.StageMachines
		if err := state.Save(spec.Name, st); err != nil {
			return fmt.Errorf("save state: %w", err)
		}
		prep.Nodes = res.Nodes
		prep.KnownHosts = filepath.Join(state.Dir(spec.Name), "known_hosts")
		if err := prepareMachines(ctx, *prep); err != nil {
			return fmt.Errorf("%w%s", err, pickUp(state.StageMachines))
		}
		st.Stage = state.StagePrepared

		if kad != nil {
			if err := installKubeadm(ctx, spec, &st, res, kad); err != nil {
				return err
			}
		}
		if inst != nil {
			if err := o.installKubernetes(ctx, spec, &st, res, inst); err != nil {
				return err
			}
		}
		if net != nil {
			if err := installNetwork(ctx, spec, &st, res, net); err != nil {
				return err
			}
		}
	}

	if ad != nil && res != nil && res.Kubeconfig != "" {
		if err := installAddons(ctx, spec, &st, res, ad); err != nil {
			return err
		}
	}

	st.Status, st.UpdatedAt = state.StatusReady, time.Now()
	if err := state.Save(spec.Name, st); err != nil {
		return fmt.Errorf("save state: %w", err)
	}

	ui.Info(fmt.Sprintf("State saved to ~/.tow/clusters/%s", spec.Name))
	switch {
	case res == nil || res.KubeContext == "":
	case spec.Kubernetes != nil && !spec.Kubernetes.Merge:
		ui.Hint(fmt.Sprintf("Run: kubectl --kubeconfig %s cluster-info", res.Kubeconfig))
	default:
		ui.Hint(fmt.Sprintf("Run: kubectl cluster-info --context %s", res.KubeContext))
	}
	return nil
}

// Delete removes a cluster: the provider tears down what it created, then state goes.
// confirm sees what is about to happen, after preflight has shown which account is used.
// State is kept when teardown fails, so 'delete' can simply be run again.
func (o *Orchestrator) Delete(ctx context.Context, name string, confirm func(summary string) bool) error {
	st, err := state.Load(name)
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no cluster named '%s' (nothing in ~/.tow/clusters/%s)", name, name)
	}
	if err != nil {
		return fmt.Errorf("read state for '%s': %w", name, err)
	}
	unlock, err := state.Lock(name)
	if err != nil {
		return err
	}
	defer unlock()

	byoh := st.Mode == config.ModeBYOH
	if (o.forget || o.reset) && !byoh {
		return fmt.Errorf("--forget and --reset are for clusters on your own machines; delete removes the machines tow created for '%s'", name)
	}
	if o.forget {
		return o.forgetCluster(st, confirm)
	}

	p, err := newProvider(st.Provider)
	if err != nil {
		return err
	}
	spec := specFromState(st)
	secretenv.Hide(spec.SecretEnv...)
	if err := o.findProviderTool(ctx, p); err != nil {
		return err
	}
	if err := p.PreFlight(ctx, spec); err != nil {
		return err
	}
	// What tow left on the user's own machines, read before anything changes.
	var left []prepare.Left
	if byoh {
		if left, err = readLeft(ctx, undoOptions(st)); err != nil {
			return fmt.Errorf("%w\nNothing was changed. For a machine that's gone for good, 'tow delete --name %s --forget' forgets the cluster without touching any machine", err, name)
		}
	}
	vols, err := o.volumesToDelete(ctx, st)
	if err != nil {
		return err
	}
	summary := describeDelete(st)
	if byoh {
		summary = describeMachines(st, left, o.reset)
	}
	if len(vols) > 0 {
		goes := "goes"
		if len(vols) > 1 {
			goes = "go"
		}
		summary += fmt.Sprintf(".\nIts %s %s too, with their data (delete -v):", ui.Plural(len(vols), "volume"), goes)
		for _, v := range vols {
			summary += "\n  " + describeVolume(v)
		}
	}
	if !confirm(summary) {
		return errors.New("delete cancelled")
	}
	reset := o.reset
	if byoh && !reset && o.askReset != nil && preparationLeft(left) {
		reset = o.askReset("Also undo what tow prepared on these machines (listed above)?")
	}
	if len(vols) > 0 {
		done := ui.Step(fmt.Sprintf("Deleting %s through Kubernetes, so their drivers delete the data", ui.Plural(len(vols), "volume")))
		if err := deleteVolumes(ctx, st.Outputs.Kubeconfig, vols); err != nil {
			fmt.Println("failed")
			return fmt.Errorf("%w\nNothing else was deleted; run 'tow delete --name %s -v' again", err, name)
		}
		done()
	}
	if byoh && toUndo(left, reset) {
		done := ui.Step(undoLabel(len(left), reset))
		if err := undoMachines(ctx, undoOptions(st), reset); err != nil {
			fmt.Println("failed")
			return fmt.Errorf("%w\nState for '%s' was kept: run 'tow delete --name %s' again to finish (what's undone isn't done again), or add --forget for a machine that's gone for good", err, name, name)
		}
		done()
	}

	done := ui.Step(deleteLabel(st))
	if err := p.Teardown(ctx, spec); err != nil {
		fmt.Println("failed")
		return fmt.Errorf("%w\nState for '%s' was kept; fix the problem and run 'tow delete --name %s' again", err, name, name)
	}
	done()
	removeContext(st)

	if err := state.Remove(name); err != nil {
		return fmt.Errorf("cluster deleted, but its state could not be removed: %w", err)
	}
	ui.Info(fmt.Sprintf("Cluster '%s' deleted and its state removed", name))
	if byoh {
		afterUndo(st, left, reset)
	}
	backupsLeft(name, left)
	return nil
}

// Swappable so tests run without a cluster or machines.
var (
	readLeft     = prepare.ReadLeft
	undoMachines = prepare.Undo
	cloudVolumes = func(ctx context.Context, kubeconfig string, drivers []string) ([]kubeapi.Volume, error) {
		api, err := kubeapiFromFile(kubeconfig)
		if err != nil {
			return nil, err
		}
		return api.VolumesOf(ctx, drivers)
	}
	deleteVolumes = func(ctx context.Context, kubeconfig string, vols []kubeapi.Volume) error {
		api, err := kubeapiFromFile(kubeconfig)
		if err != nil {
			return err
		}
		return api.DeleteVolumes(ctx, vols)
	}
)

func kubeapiFromFile(path string) (*kubeapi.Client, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return kubeapi.New(data)
}

// volumesToDelete lists the cluster's volumes whose data a delete would leave behind or
// lose: on EBS or Proxmox disks, which outlive the machines, and on the user's own
// machines' disks. Without -v, a cluster that has some (or can't say) isn't deleted.
func (o *Orchestrator) volumesToDelete(ctx context.Context, st *state.ClusterState) ([]kubeapi.Volume, error) {
	classes, drivers, where := addons.CloudVolumes, addons.CloudVolumeDrivers, "that outlive its machines"
	if st.Mode == config.ModeBYOH {
		classes = append(slices.Clone(classes), addons.LocalVolumes...)
		drivers = append(slices.Clone(drivers), addons.LocalVolumeDrivers...)
		where = "with data on your machines' disks or that outlive them"
	}
	if st.Spec == nil || !slices.ContainsFunc(st.Spec.Storage, func(s string) bool { return slices.Contains(classes, s) }) ||
		st.Outputs == nil || st.Outputs.Kubeconfig == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	vols, err := cloudVolumes(ctx, st.Outputs.Kubeconfig, drivers)
	switch {
	case err != nil && o.volumes:
		ui.Warn(fmt.Sprintf("the cluster's volumes couldn't be listed (%v): any left behind stay where they are", err))
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("cluster '%s' may have volumes %s, and they couldn't be listed: %w\nNothing was deleted. 'tow delete --name %s -v' deletes the cluster anyway, and its volumes when it can reach them",
			st.Name, where, err, st.Name)
	case len(vols) > 0 && !o.volumes:
		var b strings.Builder
		for _, v := range vols {
			b.WriteString("\n  " + describeVolume(v))
		}
		return nil, fmt.Errorf("cluster '%s' has %s %s:%s\nNothing was deleted. Back up what you need, then 'tow delete --name %s -v' deletes them with the cluster",
			st.Name, ui.Plural(len(vols), "volume"), where, b.String(), st.Name)
	}
	return vols, nil
}

func describeVolume(v kubeapi.Volume) string {
	s := v.Name
	if v.Claim != "" {
		s += " (claim " + v.Claim + ")"
	}
	return s + ", " + strings.TrimSpace(v.Size+" on "+v.Driver)
}

func describeDelete(st *state.ClusterState) string {
	switch {
	case st.Spec != nil && st.Spec.Machines != nil:
		m := st.Spec.Machines
		return fmt.Sprintf("This destroys cluster '%s' and everything tow created for it in %s %s (%d machine(s))",
			st.Name, m.Cloud, m.Region, m.Total())
	default:
		return fmt.Sprintf("This deletes the %s cluster '%s'", st.Provider, st.Name)
	}
}
