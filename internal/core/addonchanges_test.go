package core

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/state"
)

// fakeAddons is helm and kubectl against the cluster: releases and manifests as given,
// every change recorded.
type fakeAddons struct {
	releases []string
	check    map[string]struct {
		differs bool
		objs    []addons.Object
	}
	applied map[string][]addons.Object // what applying makes, when not the check's objects
	calls   []string
}

func (f *fakeAddons) Releases(context.Context) ([]string, error) { return f.releases, nil }
func (f *fakeAddons) InstallChart(_ context.Context, h config.HelmChart) error {
	f.calls = append(f.calls, "install "+h.Name+" "+h.Version)
	return nil
}
func (f *fakeAddons) UninstallChart(_ context.Context, name, ns string) error {
	f.calls = append(f.calls, "uninstall "+ns+"/"+name)
	return nil
}
func (f *fakeAddons) CheckManifest(_ context.Context, m config.Manifest) (bool, []addons.Object, error) {
	c := f.check[m.Path]
	return c.differs, c.objs, nil
}
func (f *fakeAddons) ApplyManifest(_ context.Context, m config.Manifest) ([]addons.Object, error) {
	f.calls = append(f.calls, "apply "+m.Path)
	if objs, ok := f.applied[m.Path]; ok {
		return objs, nil
	}
	return f.check[m.Path].objs, nil
}
func (f *fakeAddons) DeleteObjects(_ context.Context, objs []addons.Object) error {
	var names []string
	for _, o := range objs {
		names = append(names, o.String())
	}
	f.calls = append(f.calls, "delete "+strings.Join(names, ", "))
	return nil
}

func useAddons(t *testing.T, f *fakeAddons) {
	t.Helper()
	useCharts(t, nil, nil) // the tools are found, nothing installed
	orig := newAddonClient
	newAddonClient = func(addons.Options) addonClient { return f }
	t.Cleanup(func() { newAddonClient = orig })
}

func withAddons(addonsYAML string) string {
	return strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: k3s, addons: "+addonsYAML, 1)
}

func chart(name, version string) string {
	return fmt.Sprintf("{name: %s, repo: \"https://charts.example.com\", chart: %s, version: %s}", name, name, version)
}

var (
	deploy = addons.Object{APIVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "web"}
	svc    = addons.Object{APIVersion: "v1", Kind: "Service", Namespace: "shop", Name: "web"}
	cm     = addons.Object{APIVersion: "v1", Kind: "ConfigMap", Namespace: "shop", Name: "old"}
)

// Charts follow the file by release: added and changed ones installed, one whose release is
// gone installed again, and one that left the file uninstalled, last.
func TestUpdateCharts(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", withAddons("{helmCharts: ["+chart("web", "1.0.0")+", "+chart("same", "1.0.0")+", "+chart("gone", "1.0.0")+", "+chart("old", "1.0.0")+"]}")))
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	f := &fakeAddons{releases: []string{"web/web", "same/same", "old/old"}}
	useAddons(t, f)

	changed := parse(t, "lab", withAddons("{helmCharts: ["+chart("web", "2.0.0")+", "+chart("same", "1.0.0")+", "+chart("gone", "1.0.0")+", "+chart("new", "1.0.0")+"]}"))
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	if want := []string{"install web 2.0.0", "install gone 1.0.0", "install new 1.0.0", "uninstall old/old"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}

	// A release can't move namespace.
	moved := parse(t, "lab", strings.Replace(withAddons("{helmCharts: ["+chart("web", "2.0.0")+"]}"), "chart: web,", "chart: web, namespace: apps,", 1))
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), moved); err == nil || !strings.Contains(err.Error(), "can't move to another namespace") {
		t.Errorf("err = %v", err)
	}
}

// Manifests are compared with what runs: one that differs is applied again, the objects it
// dropped are deleted, and so are those of a manifest that left the file. State keeps what
// each one made.
func TestUpdateManifests(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", withAddons("{manifests: [{path: \"https://example.com/shop.yaml\", namespace: shop}, \"https://example.com/old.yaml\"]}")))
	st, _ := state.Load("lab")
	st.Manifests = map[string][]addons.Object{"https://example.com/shop.yaml": {deploy, svc, cm}, "https://example.com/old.yaml": {cm}}
	state.Save("lab", *st)
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	f := &fakeAddons{check: map[string]struct {
		differs bool
		objs    []addons.Object
	}{
		"https://example.com/shop.yaml": {false, []addons.Object{deploy, svc}},
		"https://example.com/new.yaml":  {true, []addons.Object{deploy}},
	}}
	useAddons(t, f)

	changed := parse(t, "lab", withAddons("{manifests: [{path: \"https://example.com/shop.yaml\", namespace: shop}, {path: \"https://example.com/new.yaml\", namespace: shop}]}"))
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), changed); err != nil {
		t.Fatal(err)
	}
	want := []string{"apply https://example.com/new.yaml", "delete configmap/old in shop", "delete configmap/old in shop"}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	st, _ = state.Load("lab")
	if got := fmt.Sprint(st.Manifests); got != "map[https://example.com/new.yaml:[deployment/web in shop] https://example.com/shop.yaml:[deployment/web in shop service/web in shop]]" {
		t.Errorf("records = %s", got)
	}

	// Its objects moving to another namespace is refused; writing the default out isn't a move.
	moved := parse(t, "lab", withAddons("{manifests: [{path: \"https://example.com/shop.yaml\", namespace: other}, {path: \"https://example.com/new.yaml\", namespace: shop}]}"))
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), moved); err == nil || !strings.Contains(err.Error(), "would move from namespace shop to other") {
		t.Errorf("err = %v", err)
	}
}

// A manifest whose namespace was deleted outside tow can't be compared with what runs:
// it's applied again, and only what it no longer has is deleted after, never what the apply
// just made.
func TestUpdateManifestNamespaceGone(t *testing.T) {
	useFakes(t)
	const shop = "https://example.com/shop.yaml"
	running(t, parse(t, "lab", withAddons("{manifests: [{path: \""+shop+"\", namespace: shop}]}")))
	st, _ := state.Load("lab")
	st.Manifests = map[string][]addons.Object{shop: {deploy, svc, cm}}
	state.Save("lab", *st)
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	f := &fakeAddons{
		check: map[string]struct {
			differs bool
			objs    []addons.Object
		}{shop: {true, nil}}, // kubectl can't tell: the namespace is gone
		applied: map[string][]addons.Object{shop: {deploy, svc}},
	}
	useAddons(t, f)

	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), parse(t, "lab", withAddons("{manifests: [{path: \""+shop+"\", namespace: shop}]}"))); err != nil {
		t.Fatal(err)
	}
	if want := []string{"apply " + shop, "delete configmap/old in shop"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	st, _ = state.Load("lab")
	if got := fmt.Sprint(st.Manifests); got != "map["+shop+":[deployment/web in shop service/web in shop]]" {
		t.Errorf("records = %s", got)
	}
}
