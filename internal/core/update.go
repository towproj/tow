package core

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// nodeClient reads and changes nodes through the cluster's API; swapped in tests.
type nodeClient interface {
	NodeMetas(ctx context.Context) ([]kubeapi.NodeMeta, error)
	Relabel(ctx context.Context, node string, set map[string]string, remove []string) error
	Retaint(ctx context.Context, node string, add []kubeapi.Taint, remove []string) error
}

var nodeAPI = func(kubeconfig string) (nodeClient, error) { return kubeapiFromFile(kubeconfig) }

// checkMachines is preparation's read-only check; swapped in tests.
var checkMachines = prepare.Check

// After OpenTofu opened the firewall to this machine, reaching the machines and the nodes
// is tried this many times, this far apart. Vars so tests don't wait.
var (
	reachTries = 4
	reachWait  = 10 * time.Second
)

// planStep is one change apply makes on a running cluster.
type planStep struct {
	lines []string // what it does, as the plan shows it
	run   func(ctx context.Context) error
}

// updatePlan is what apply would change on a running cluster, in the order it runs:
// infrastructure first, then the cluster, then the machines leaving it.
type updatePlan struct {
	fileChanges []string   // the file's changes since the last apply
	infra       []planStep // OpenTofu, firewalls
	cluster     []planStep // Kubernetes and add-ons
	notes       []string   // shown in the cluster section, nothing to do: nodes tow didn't make
	leave       []planStep // machines leaving, last
}

func (u updatePlan) steps() []planStep { return slices.Concat(u.infra, u.cluster, u.leave) }

// print shows the plan through out, one line per call.
func (u updatePlan) print(out func(string), name string) {
	out(fmt.Sprintf("Plan for cluster '%s':", name))
	if len(u.fileChanges) > 0 {
		out("  The file changed since the last apply:")
		for _, l := range u.fileChanges {
			out("    " + l)
		}
	}
	section := func(title string, steps []planStep, notes []string) {
		out("  " + title + ":")
		if len(steps)+len(notes) == 0 {
			out("    none")
		}
		for _, s := range steps {
			for i, l := range s.lines {
				if i == 0 {
					out("    - " + l)
				} else {
					out("      " + l)
				}
			}
		}
		for _, n := range notes {
			out("    - " + n)
		}
	}
	section("Infrastructure (OpenTofu, firewalls)", u.infra, nil)
	section("Cluster (Kubernetes and add-ons)", u.cluster, u.notes)
	if len(u.leave) > 0 {
		section("Machines leaving, one at a time, last", u.leave, nil)
	}
}

// update applies the file to a cluster that runs. What changed since the last apply is
// classified first: a change that can't be made stops everything, with its reason. The plan
// then comes from what's really there (OpenTofu's plan, the nodes through the API), so a run
// that stopped partway is simply planned again. It's shown in two sections, confirmed, and
// run in order; the state's copy of the file is updated once all of it succeeded.
func (o *Orchestrator) update(ctx context.Context, p providers.Provider, cfg *config.Config, prev *state.ClusterState) error {
	fields := cfg.Fields()
	// The file as last applied, for the built-in add-ons' settings.
	var old *config.Config
	if prev.File != "" {
		var err error
		if old, err = config.Restore(prev.File); err != nil {
			return err
		}
	}
	diff := config.DiffFields(prev.Config, fields)
	up, refusedUpgrade := upgradeOf(cfg, prev, diff)
	refused := refusedChanges(prev.Config, fields, cfg.Mode, cfg.Name())
	if up != nil {
		// The versions of what tow installs move with an upgrade (refused otherwise).
		refused = slices.DeleteFunc(refused, func(line string) bool {
			path, _, _ := strings.Cut(line, ": ")
			return movesWithUpgrade(config.Generic(path))
		})
	}
	refused = append(refused, movedManifests(prev.Config, cfg)...)
	refused = append(refused, refusedSettings(old, cfg, diff)...)
	if refused = append(refused, refusedUpgrade...); len(refused) > 0 {
		return cantFollow(cfg.Name(), refused)
	}
	// What each manifest made, as steps change it; saved even when a run stops partway.
	records := map[string][]addons.Object{}
	maps.Copy(records, prev.Manifests)
	keepRecords := func() {
		if !sameRecords(records, prev.Manifests) {
			if err := saveRecords(prev, records); err != nil {
				ui.Warn("what the manifests made couldn't be saved: " + err.Error())
			}
		}
	}

	// The running release stays: upgrading is a change of its own.
	spec := specFromConfig(cfg, prev.K8sVersion)
	r, reconciles := p.(providers.Reconciler)
	reconciles = reconciles && spec.Machines != nil
	if reconciles {
		// OpenTofu runs below: it's found (or installed after a yes), then credentials, with
		// the chance to log in again.
		if err := o.findProviderTool(ctx, p); err != nil {
			return err
		}
		if err := p.PreFlight(ctx, spec); err != nil {
			return err
		}
	}
	// Make sure the cluster wasn't removed behind tow's back.
	res, err := p.Status(ctx, specFromState(prev))
	if err != nil {
		return err
	}
	if res.State == "not_found" {
		return fmt.Errorf("state says cluster '%s' exists, but %s can't find it (deleted outside tow?); run 'tow delete --name %s' to clean up, then apply again", prev.Name, p.Name(), prev.Name)
	}
	// A machine tow made that's missing is created again below, when OpenTofu plans it;
	// otherwise an unhealthy cluster is fixed first.
	unhealthy := fmt.Errorf("cluster '%s' isn't healthy: %s (run 'tow status --name %s'); fix that first", prev.Name, res.State, prev.Name)
	if !res.Healthy && !reconciles {
		return unhealthy
	}

	// Machines added and removed: OpenTofu's first run keeps the leaving ones while the new
	// ones join; they leave last.
	mp, err := o.planMachineChanges(ctx, cfg, old, prev, spec, nil)
	if r, ok := errors.AsType[refusal](err); ok {
		return cantFollow(cfg.Name(), r.lines)
	}
	if err != nil {
		return err
	}
	tfSpec := spec
	if mp.changes() && spec.Machines != nil {
		tfSpec = mp.first
	}

	plan := updatePlan{fileChanges: config.DiffFields(prev.Config, fields)}
	if reconciles {
		spec.Guard, tfSpec.Guard = providers.GuardRunning, providers.GuardRunning
		tfSpec.Known = namesOf(prev.Outputs.Nodes)
		planned, err := r.Changes(ctx, tfSpec)
		if err != nil {
			return err
		}
		// Machines of the cluster's that OpenTofu creates are missing: made again, under their
		// name and address, and joined.
		var gone []providers.Node
		for _, n := range prev.Outputs.Nodes {
			if slices.Contains(planned.Creating, n.Name) {
				gone = append(gone, n)
			}
		}
		switch {
		case len(gone) > 0:
			mp, err = o.planMachineChanges(ctx, cfg, old, prev, spec, gone)
			if r, ok := errors.AsType[refusal](err); ok {
				return cantFollow(cfg.Name(), r.lines)
			}
			if err != nil {
				return err
			}
			tfSpec = mp.first
			tfSpec.Known = namesOf(prev.Outputs.Nodes)
			plan.infra = append(plan.infra, mp.stale...)
		case !res.Healthy:
			return unhealthy
		}
		lines := planned.Lines
		// When the file's adminCIDRs changed, that's what moved, not this machine: the
		// file's changes already say so.
		if slices.ContainsFunc(plan.fileChanges, func(c string) bool { return strings.HasPrefix(c, "spec.infrastructure.access.adminCIDRs:") }) {
			lines = slices.DeleteFunc(lines, func(l string) bool { return strings.HasPrefix(l, "This machine's address changed") })
		}
		if len(lines) > 0 {
			plan.infra = append(plan.infra, planStep{lines: append([]string{"OpenTofu (" + where(prev) + "):"}, lines...), run: func(ctx context.Context) error {
				done := ui.Step("Updating the infrastructure")
				res, err := p.Provision(ctx, tfSpec)
				if err != nil {
					fmt.Println("failed")
					return err
				}
				done()
				if mp.changes() {
					return keepNodes(prev, res.Nodes) // the new machines exist: a run that stops later picks up from them
				}
				return nil
			}})
		}
	}
	steps, err := kubeconfigSteps(prev, cfg)
	if err != nil {
		return err
	}
	plan.cluster = append(plan.cluster, steps...)
	if mp.changes() {
		if mp.checkNew != nil {
			plan.infra = append(plan.infra, *mp.checkNew)
		}
		plan.cluster = append(plan.cluster, mp.join...)
		plan.leave, plan.notes = mp.leave, append(plan.notes, mp.notes...)
	}

	// The machines and the nodes are checked now, unless this machine can't reach them while
	// OpenTofu has changes to make: its address moved, say, and the firewall follows it once
	// OpenTofu runs. They're then planned and changed right after OpenTofu, in this run.
	tofuFirst := len(plan.infra) > 0
	rest, err := o.planRest(ctx, plan.fileChanges, cfg, old, prev, tofuFirst, records, mp.unreachable(), up)
	if r, ok := errors.AsType[refusal](err); ok {
		return cantFollow(cfg.Name(), r.lines) // before OpenTofu changes anything
	}
	deferred := err != nil && tofuFirst
	switch {
	case deferred && mp.changes():
		return errMachinesAndAddress
	case deferred:
		why, _, _ := strings.Cut(err.Error(), "\n")
		plan.notes = append(plan.notes, fmt.Sprintf("the machines and the nodes are checked once OpenTofu's changes are made: this machine can't reach them yet (%s)", why))
	case err != nil:
		return err
	default:
		plan.infra = append(plan.infra, rest.infra...)
		plan.cluster = append(plan.cluster, rest.cluster...)
		plan.notes = append(plan.notes, rest.notes...)
	}
	// Certificates renewed first when they expire soon (the API's checks rely on them); an
	// upgrade (alone in its apply), its snapshot first; or a newer release noted.
	var upPlan updatePlan
	if err := o.planCerts(ctx, &upPlan, cfg, prev, up); err != nil {
		return err
	}
	if err := o.planUpgrade(ctx, &upPlan, cfg, prev, up); err != nil {
		if r, ok := errors.AsType[refusal](err); ok {
			return cantFollow(cfg.Name(), r.lines)
		}
		return err
	}
	plan.cluster = append(upPlan.cluster, plan.cluster...)
	plan.notes = append(plan.notes, upPlan.notes...)

	out := func(l string) { fmt.Println(l) }
	if o.dryRun {
		out = ui.DryRun
	}
	if len(plan.steps()) == 0 {
		for _, n := range plan.notes {
			ui.Warn(n)
		}
		if len(plan.fileChanges) > 0 {
			if o.dryRun {
				ui.DryRun(fmt.Sprintf("The file's changes need nothing on cluster '%s': apply would only record them", cfg.Name()))
				return nil
			}
			if err := saveUpdate(prev, cfg, spec, records); err != nil {
				return err
			}
			ui.Info(fmt.Sprintf("The file's changes need nothing on cluster '%s'; recorded them", cfg.Name()))
		} else if !o.dryRun {
			keepRecords()
		}
		ui.Info(fmt.Sprintf("Cluster '%s' is up to date (applied %s, Kubernetes %s); nothing to do", prev.Name, prev.UpdatedAt.Local().Format("2006-01-02 15:04"), prev.K8sVersion))
		return nil
	}

	plan.print(out, cfg.Name())
	if o.dryRun {
		return nil
	}
	if !o.yes {
		if !ui.Interactive() {
			ui.Hint("Nobody to ask here: run it with --yes to make these changes")
			return errors.New("nothing was changed")
		}
		if !ui.Confirm("Make these changes?") {
			return errors.New("nothing was changed")
		}
	}
	var done []string
	stopped := func(err error) error {
		keepRecords()
		return fmt.Errorf("%w%s", err, stoppedAt(done))
	}
	for _, s := range plan.steps() {
		if err := s.run(ctx); err != nil {
			return stopped(err)
		}
		done = append(done, s.lines[0])
	}
	if deferred {
		// The same yes covers it: the plan said these come after OpenTofu. A firewall can
		// take a few seconds to apply new rules (Proxmox reloads them every few seconds).
		var rest updatePlan
		for try := 1; ; try++ {
			rest, err = o.planRest(ctx, plan.fileChanges, cfg, old, prev, false, records, nil, up)
			if r, ok := errors.AsType[refusal](err); ok {
				return stopped(cantFollow(cfg.Name(), r.lines))
			}
			if err == nil || try == reachTries {
				break
			}
			ui.Debug("the machines don't answer yet: %v", err)
			select {
			case <-ctx.Done():
				return stopped(ctx.Err())
			case <-time.After(reachWait):
			}
		}
		if err != nil {
			return stopped(err)
		}
		if len(rest.steps()) > 0 {
			fmt.Println("Now that OpenTofu's changes are made, the machines and the nodes need:")
			rest.print(func(l string) { fmt.Println(l) }, cfg.Name())
		}
		for _, s := range rest.steps() {
			if err := s.run(ctx); err != nil {
				return stopped(err)
			}
			done = append(done, s.lines[0])
		}
		for _, n := range rest.notes {
			ui.Warn(n)
		}
	}
	// Putting back what changed outside tow applies nothing new: the file's record
	// stays, and only what the manifests made is kept up to date.
	if up != nil {
		spec.Version = up.to
	}
	if len(plan.fileChanges) > 0 {
		if err := saveUpdate(prev, cfg, spec, records); err != nil {
			return err
		}
	} else {
		keepRecords()
	}
	ui.Info(fmt.Sprintf("Cluster '%s' now matches the file", cfg.Name()))
	return nil
}

// saveRecords keeps what the manifests made, and nothing else of this run.
func saveRecords(prev *state.ClusterState, records map[string][]addons.Object) error {
	st := *prev
	st.Manifests = records
	return state.Save(st.Name, st)
}

func sameRecords(a, b map[string][]addons.Object) bool {
	return maps.EqualFunc(a, b, func(x, y []addons.Object) bool { return slices.Equal(x, y) })
}

// saveUpdate records the file as applied: its fields and its effective config, what the
// provider was asked for, and what the manifests made.
func saveUpdate(prev *state.ClusterState, cfg *config.Config, spec providers.Spec, records map[string][]addons.Object) error {
	st := *prev
	spec.Guard = providers.GuardOff
	fields := cfg.Fields()
	st.ConfigHash, st.Config, st.File, st.Spec, st.Manifests = config.HashFields(fields), fields, cfg.Effective(), &spec, records
	if st.Outputs != nil && spec.Kubernetes != nil {
		out := *st.Outputs
		out.KubeContext = spec.Kubernetes.Context
		st.Outputs = &out
	}
	st.UpdatedAt = time.Now()
	return state.Save(st.Name, st)
}

// kubeconfigSteps follows the file's kubeconfig settings: a new context name, or the
// cluster's context added to or removed from the user's kubeconfig. The copy in the state
// folder is renamed too; the user's current context only moves with its own cluster.
func kubeconfigSteps(prev *state.ClusterState, cfg *config.Config) ([]planStep, error) {
	if cfg.Mode == config.ModeLocal || prev.Spec == nil || prev.Spec.Kubernetes == nil || prev.Outputs == nil || prev.Outputs.Kubeconfig == "" {
		return nil, nil
	}
	was, now := *prev.Spec.Kubernetes, cfg.Spec.Kubernetes.Kubeconfig
	merge, path := *now.Merge, prev.Outputs.Kubeconfig
	if was.Context == now.Context && was.Merge == merge {
		return nil, nil
	}
	if merge && (was.Context != now.Context || !was.Merge) {
		if err := kubecfg.CheckFree(now.Context); err != nil {
			return nil, err
		}
	}
	var line string
	switch {
	case was.Context != now.Context && merge:
		line = fmt.Sprintf("kubeconfig: context '%s' becomes '%s', in your kubeconfig and in %s", was.Context, now.Context, path)
	case was.Context != now.Context:
		line = fmt.Sprintf("kubeconfig: context '%s' becomes '%s', only in %s", was.Context, now.Context, path)
	case merge:
		line = fmt.Sprintf("kubeconfig: add context '%s' to your kubeconfig", now.Context)
	default:
		line = fmt.Sprintf("kubeconfig: remove context '%s' from your kubeconfig (it stays in %s)", was.Context, path)
	}
	return []planStep{{lines: []string{line}, run: func(context.Context) error {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if data, err = kubecfg.Rename(data, now.Context); err != nil {
			return err
		}
		if err := kubecfg.Save(path, data); err != nil {
			return err
		}
		if merge {
			if err := kubecfg.Add(data, was.Context); err != nil {
				return err
			}
		}
		if was.Merge && (!merge || was.Context != now.Context) {
			if err := kubecfg.Remove(was.Context); err != nil {
				return err
			}
		}
		ui.Info(line)
		return nil
	}}}, nil
}

// planRest plans what reaching the machines and the nodes tells: the machines' own changes
// (infrastructure, after OpenTofu) and the nodes' labels and taints (cluster). strict makes
// a machine or an API that doesn't answer an error, for the caller to try again after
// OpenTofu; otherwise an API that doesn't answer is only noted, unless the file changed the
// nodes' settings.
func (o *Orchestrator) planRest(ctx context.Context, fileChanges []string, cfg, old *config.Config, prev *state.ClusterState, strict bool, records map[string][]addons.Object, skip []string, up *upgrade) (updatePlan, error) {
	rest := updatePlan{fileChanges: fileChanges}
	if err := o.planMachines(ctx, &rest, cfg, prev, skip); err != nil {
		return rest, err
	}
	if err := o.planNodes(ctx, &rest, cfg, prev, strict); err != nil {
		return rest, err
	}
	if err := o.planSettings(ctx, &rest, cfg, old, prev, up); err != nil {
		return rest, err
	}
	// The built-in add-ons around the file's own: a replaced load balancer goes before
	// anything is installed, what's turned off goes last.
	bp, err := o.planBuiltins(ctx, cfg, old, prev, nil, up)
	if err != nil {
		return rest, err
	}
	rest.cluster = append(append(rest.cluster, bp.early...), bp.installs...)
	rest.notes = append(rest.notes, bp.notes...)
	if err := o.planAddons(ctx, &rest, cfg, prev, records); err != nil {
		return rest, err
	}
	rest.cluster = append(rest.cluster, bp.late...)
	planBackups(&rest, cfg, old, prev)
	return rest, nil
}

// cantFollow is the error for changes a running cluster can't follow, one line each.
func cantFollow(name string, lines []string) error {
	return fmt.Errorf("cluster '%s' runs, and the file changed in ways it can't follow; nothing was changed:\n  - %s", name, strings.Join(lines, "\n  - "))
}

// planMachines checks every machine as preparation does, read-only: what it would change
// (a ufw rule to add or close, something undone by hand) is planned, after OpenTofu. The
// plan's yes is the question preparation would ask on the user's own machines.
func (o *Orchestrator) planMachines(ctx context.Context, plan *updatePlan, cfg *config.Config, prev *state.ClusterState, skip []string) error {
	if cfg.Mode == config.ModeLocal || prev.Outputs == nil || len(prev.Outputs.Nodes) == 0 {
		return nil
	}
	// Machines that are gone are prepared once they're made again, with the new ones.
	nodes := slices.DeleteFunc(slices.Clone(prev.Outputs.Nodes), func(n providers.Node) bool { return slices.Contains(skip, n.Name) })
	opts := o.prepareOptions(cfg)
	opts.Nodes, opts.KnownHosts, opts.Confirm = nodes, filepath.Join(state.Dir(cfg.Name()), "known_hosts"), nil
	lines, err := checkMachines(ctx, *opts)
	if err != nil {
		return fmt.Errorf("%w\n  → is SSH open to this machine's address on them (their firewall, ufw)?", err)
	}
	if len(lines) == 0 {
		return nil
	}
	opts.Confirmed = true
	plan.infra = append(plan.infra, planStep{lines: append([]string{"The machines (each change is recorded in " + prepare.RecordFile + " on it):"}, lines...),
		run: func(ctx context.Context) error {
			done := ui.Step("Changing the machines")
			if err := prepareMachines(ctx, *opts); err != nil {
				fmt.Println("failed")
				return err
			}
			done()
			return nil
		}})
	return nil
}

// planNodes adds the label and taint changes the nodes need, and notes the nodes tow
// didn't make. When the API doesn't answer: an error when strict, or when the file changed
// the nodes' settings (those can't be made then); otherwise only a note.
func (o *Orchestrator) planNodes(ctx context.Context, plan *updatePlan, cfg *config.Config, prev *state.ClusterState, strict bool) error {
	if cfg.Mode == config.ModeLocal || prev.Outputs == nil || prev.Outputs.Kubeconfig == "" || len(prev.Outputs.Nodes) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	api, err := nodeAPI(prev.Outputs.Kubeconfig)
	var live []kubeapi.NodeMeta
	if err == nil {
		live, err = api.NodeMetas(ctx)
	}
	if err != nil {
		if strict {
			return fmt.Errorf("the cluster's API doesn't answer from this machine: %w", err)
		}
		for _, c := range plan.fileChanges {
			if g := config.Generic(c); strings.Contains(g, ".labels") || strings.Contains(g, ".taints") || strings.HasPrefix(g, "spec.controlPlane.schedulable") {
				return fmt.Errorf("the file changes the nodes' labels or taints, and the cluster's API doesn't answer from this machine: %w", err)
			}
		}
		plan.notes = append(plan.notes, fmt.Sprintf("the API doesn't answer from this machine (%v): the nodes' labels and taints weren't checked", err))
		return nil
	}

	changes, unknown, missing := nodeChanges(cfg, prev.Config, prev.Outputs.Nodes, live)
	// Nodes that need the same get one line, in the machines' order: control planes first.
	byWhat := map[string][]nodeChange{}
	var order []string
	for _, c := range changes {
		what := c.describe()
		if byWhat[what] == nil {
			order = append(order, what)
		}
		byWhat[what] = append(byWhat[what], c)
	}
	for _, what := range order {
		group := byWhat[what]
		var names []string
		for _, c := range group {
			names = append(names, c.node)
		}
		line := strings.Join(names, ", ") + ": " + what
		plan.cluster = append(plan.cluster, planStep{lines: []string{line}, run: func(ctx context.Context) error {
			for _, c := range group {
				if err := api.Relabel(ctx, c.node, c.setLabels, c.rmLabels); err != nil {
					return err
				}
				if err := api.Retaint(ctx, c.node, c.addTaints, c.rmTaints); err != nil {
					return err
				}
			}
			ui.Info(line)
			return nil
		}})
	}
	for _, n := range unknown {
		plan.notes = append(plan.notes, fmt.Sprintf("node %s isn't one tow made (joined outside tow?): left alone", n))
	}
	for _, n := range missing {
		plan.notes = append(plan.notes, fmt.Sprintf("node %s, which tow made, isn't in the cluster: its labels and taints weren't checked", n))
	}
	return nil
}
