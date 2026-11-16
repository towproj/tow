package core

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/tools"
	"github.com/towproj/tow/internal/ui"
)

const (
	chartsPath    = "spec.kubernetes.addons.helmCharts"
	manifestsPath = "spec.kubernetes.addons.manifests"
)

// addonClient is what planning the add-ons uses of addons.Options; swapped in tests.
type addonClient interface {
	Releases(ctx context.Context) ([]string, error)
	InstallChart(ctx context.Context, h config.HelmChart) error
	UninstallChart(ctx context.Context, name, namespace string) error
	CheckManifest(ctx context.Context, m config.Manifest) (bool, []addons.Object, error)
	ApplyManifest(ctx context.Context, m config.Manifest) ([]addons.Object, error)
	DeleteObjects(ctx context.Context, objs []addons.Object) error
}

var newAddonClient = func(o addons.Options) addonClient { return o }

// movedManifests refuses a manifest whose objects would go into another namespace: like a
// chart, remove it, apply, then add it back. Compared by where they land, so writing the
// default namespace out is no move.
func movedManifests(old map[string]string, cfg *config.Config) []string {
	var out []string
	for _, m := range cfg.Spec.Kubernetes.Addons.Manifests {
		item := manifestsPath + "[" + m.Path + "]"
		if !hasItem(old, item) {
			continue
		}
		was := config.Manifest{Path: m.Path, Namespace: old[item+".namespace"]}
		if was.Target() != m.Target() {
			out = append(out, fmt.Sprintf("%s: its objects would move from namespace %s to %s: remove it from the file, apply, then add it back", item, was.Target(), m.Target()))
		}
	}
	return out
}

// planAddons adds what the file's Helm charts and manifests need, compared with what runs:
// a chart added or changed (or whose release is gone) is installed; a manifest that differs
// from what runs is applied again. Removals come last: the objects a manifest dropped, the
// objects of a manifest that left the file, then the charts that left it (only those
// tow installed: the last file had them). records, state's copy of what each manifest
// made, follows every step.
func (o *Orchestrator) planAddons(ctx context.Context, plan *updatePlan, cfg *config.Config, prev *state.ClusterState, records map[string][]addons.Object) error {
	if prev.Outputs == nil || prev.Outputs.Kubeconfig == "" {
		return nil
	}
	k := cfg.Spec.Kubernetes
	oldCharts := itemsUnder(prev.Config, chartsPath+"[")
	oldManifests := itemsUnder(prev.Config, manifestsPath+"[")
	if len(oldCharts)+len(k.Addons.HelmCharts)+len(oldManifests)+len(k.Addons.Manifests) == 0 {
		return nil
	}

	ad := addons.Options{Kubeconfig: prev.Outputs.Kubeconfig, Dir: state.Dir(cfg.Name()), Kubernetes: k, Charts: k.Addons.HelmCharts,
		ManifestPath: cfg.ManifestPath, Cluster: cfg.Name(), CloudStorage: prev.Outputs.StorageConfig}
	toolOpts := tools.Options{Install: o.installTools, DryRun: o.dryRun}
	var err error
	if len(oldCharts)+len(k.Addons.HelmCharts) > 0 {
		if ad.Helm, err = findHelm(ctx, tools.Helm, toolOpts); err != nil {
			return err
		}
	}
	if len(oldManifests)+len(k.Addons.Manifests) > 0 {
		if ad.Kubectl, err = findKubectl(ctx, tools.Kubectl(prev.K8sVersion), toolOpts); err != nil {
			return err
		}
	}
	fields := cfg.Fields()
	var installs, removals []planStep

	// Charts, by release.
	if len(oldCharts)+len(k.Addons.HelmCharts) > 0 {
		api := newAddonClient(ad)
		releases, err := api.Releases(ctx)
		if err != nil {
			return err
		}
		var toInstall []config.HelmChart
		var installer addonClient // with the secret values the charts to install need, read below
		for _, h := range k.Addons.HelmCharts {
			item := chartsPath + "[" + h.Name + "]"
			running := slices.Contains(releases, h.Namespace+"/"+h.Name)
			var line string
			switch {
			case !hasItem(prev.Config, item):
				line = fmt.Sprintf("chart %s: install %s %s into %s", h.Name, h.Chart, h.Version, h.Namespace)
			case !running:
				line = fmt.Sprintf("chart %s: install again (its release is gone: uninstalled outside tow?)", h.Name)
			case !sameUnder(prev.Config, fields, item):
				line = fmt.Sprintf("chart %s: upgrade to what the file says (%s %s)", h.Name, h.Chart, h.Version)
			default:
				continue
			}
			toInstall = append(toInstall, h)
			installs = append(installs, planStep{lines: []string{line}, run: func(ctx context.Context) error {
				done := ui.Step(fmt.Sprintf("Installing Helm chart %s %s into %s", h.Name, h.Version, h.Namespace))
				if err := installer.InstallChart(ctx, h); err != nil {
					fmt.Println("failed")
					return err
				}
				done()
				return nil
			}})
		}
		// Their secret values are read now: a missing variable stops everything first.
		withSecrets := ad
		if len(toInstall) > 0 && !o.dryRun {
			kk := k
			kk.Addons.HelmCharts, kk.Storage = toInstall, config.Storage{}
			if withSecrets.Secrets, err = readSecrets(addons.Secrets(kk)); err != nil {
				return err
			}
		}
		installer = newAddonClient(withSecrets)
		for _, name := range oldCharts {
			if slices.ContainsFunc(k.Addons.HelmCharts, func(h config.HelmChart) bool { return h.Name == name }) {
				continue
			}
			ns := prev.Config[chartsPath+"["+name+"].namespace"]
			if !slices.Contains(releases, ns+"/"+name) {
				continue // already gone
			}
			removals = append(removals, planStep{
				lines: []string{fmt.Sprintf("chart %s left the file: helm uninstall (namespace %s, and any volumes it claimed, stay)", name, ns)},
				run: func(ctx context.Context) error {
					done := ui.Step("Removing Helm chart " + name)
					if err := api.UninstallChart(ctx, name, ns); err != nil {
						fmt.Println("failed")
						return err
					}
					done()
					return nil
				}})
		}
	}

	// Manifests, by path. Their content can change while the file doesn't: each is
	// compared with what runs.
	if len(oldManifests)+len(k.Addons.Manifests) > 0 {
		api := newAddonClient(ad)
		var dropped []planStep
		for _, m := range k.Addons.Manifests {
			differs, objs, err := api.CheckManifest(ctx, m)
			if err != nil {
				return err
			}
			had := records[m.Path]
			// Its namespace is gone (deleted outside tow): what it has is known once
			// it's applied again, and so is what it dropped.
			unknown := differs && objs == nil
			gone := addons.Missing(had, objs) // in the manifest before, not any more
			if unknown {
				gone = nil
			}
			if !differs {
				records[m.Path] = append(slices.Clone(objs), gone...) // what it has, and what it dropped until deleted
			}
			// What it has now: what the apply below made, once it ran.
			var applied []addons.Object
			if differs {
				line := fmt.Sprintf("manifest %s: apply again (it differs from what runs)", m.Path)
				switch {
				case !hasItem(prev.Config, manifestsPath+"["+m.Path+"]"):
					line = fmt.Sprintf("manifest %s: apply (into %s)", m.Path, m.Target())
				case unknown:
					line = fmt.Sprintf("manifest %s: apply again (namespace %s is gone)", m.Path, m.Target())
				}
				installs = append(installs, planStep{lines: []string{line}, run: func(ctx context.Context) error {
					done := ui.Step("Applying manifest " + m.Path)
					var err error
					if applied, err = api.ApplyManifest(ctx, m); err != nil {
						fmt.Println("failed")
						return err
					}
					// What it made, and what it dropped until that's deleted below.
					records[m.Path] = append(slices.Clone(applied), addons.Missing(had, applied)...)
					done()
					return nil
				}})
			}
			var line string
			switch {
			case len(gone) > 0:
				line = fmt.Sprintf("manifest %s no longer has %s: delete them", m.Path, describeObjects(gone))
			case unknown && len(had) > 0:
				line = fmt.Sprintf("manifest %s: then delete whatever it made before and no longer has", m.Path)
			default:
				continue
			}
			dropped = append(dropped, planStep{lines: []string{line}, run: func(ctx context.Context) error {
				del := gone
				if applied != nil {
					del = addons.Missing(had, applied) // never what the apply just made
				}
				if len(del) == 0 {
					return nil
				}
				if err := api.DeleteObjects(ctx, del); err != nil {
					return err
				}
				records[m.Path] = addons.Missing(records[m.Path], del)
				ui.Info(fmt.Sprintf("Deleted what manifest %s no longer has", m.Path))
				return nil
			}})
		}
		for _, path := range oldManifests {
			if slices.ContainsFunc(k.Addons.Manifests, func(m config.Manifest) bool { return m.Path == path }) {
				continue
			}
			objs := records[path]
			if len(objs) == 0 {
				delete(records, path)
				continue
			}
			dropped = append(dropped, planStep{
				lines: []string{fmt.Sprintf("manifest %s left the file: delete %s (its namespace stays)", path, describeObjects(objs))},
				run: func(ctx context.Context) error {
					if err := api.DeleteObjects(ctx, objs); err != nil {
						return err
					}
					delete(records, path)
					ui.Info(fmt.Sprintf("Deleted what manifest %s made", path))
					return nil
				}})
		}
		removals = append(dropped, removals...) // the manifests' objects before the charts
	}
	plan.cluster = append(plan.cluster, installs...)
	plan.cluster = append(plan.cluster, removals...)
	return nil
}

// sameUnder reports whether two field sets agree on everything under item.
func sameUnder(a, b map[string]string, item string) bool {
	under := func(f map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range f {
			if k == item || strings.HasPrefix(k, item+".") || strings.HasPrefix(k, item+"[") {
				out[k] = v
			}
		}
		return out
	}
	return maps.Equal(under(a), under(b))
}

// describeObjects names a few objects, then counts the rest.
func describeObjects(objs []addons.Object) string {
	const shown = 4
	var names []string
	for i, o := range objs {
		if i == shown {
			names = append(names, fmt.Sprintf("and %d more", len(objs)-shown))
			break
		}
		names = append(names, o.String())
	}
	return strings.Join(names, ", ")
}
