package core

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
)

// fieldPaths walks the config's Go types as the file is written: yaml names, inlined structs
// at their parent's path, pointers and list items followed (a list item is "[]"), and maps
// stopped at, since their keys are free (labels, extraArgs, chart values). Unexported fields
// and those yaml leaves out ("-") aren't in the file. Every field's generic path goes in out.
func fieldPaths(t reflect.Type, path string, out map[string]bool, depth int) {
	if depth > 20 {
		panic("config types nest too deep at " + path + ": a type that contains itself?")
	}
	switch t.Kind() {
	case reflect.Pointer:
		fieldPaths(t.Elem(), path, out, depth+1)
	case reflect.Slice, reflect.Array:
		out[path] = true // the list as one value (a set), or its items:
		fieldPaths(t.Elem(), path+"[]", out, depth+1)
	case reflect.Map:
		out[path] = true
	case reflect.Struct:
		for i := range t.NumField() {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			name, opts, _ := strings.Cut(f.Tag.Get("yaml"), ",")
			if name == "-" {
				continue
			}
			if strings.Contains(opts, "inline") {
				fieldPaths(f.Type, path, out, depth+1)
				continue
			}
			if name == "" {
				name = strings.ToLower(f.Name) // yaml's default
			}
			if path != "" {
				name = path + "." + name
			}
			fieldPaths(f.Type, name, out, depth+1)
		}
	default:
		out[path] = true
	}
}

// Every field of the file has a change class, so a field added later can't slip through
// unclassified; and every rule names a field that exists.
func TestEveryFieldHasAChangeClass(t *testing.T) {
	paths := map[string]bool{}
	fieldPaths(reflect.TypeOf(config.Config{}), "", paths, 0)
	for _, want := range []string{"spec.workers[].labels", "spec.controlPlane.schedulable", "spec.kubernetes.addons.helmCharts[].values", "metadata.name"} {
		if !paths[want] {
			t.Fatalf("the walker missed %s: it's out of date with the config types", want)
		}
	}
	for p := range paths {
		if ruleFor(p) == nil {
			t.Errorf("%s has no change class: add it to changeRules", p)
		}
	}
	for rule := range changeRules {
		used := false
		for p := range paths {
			used = used || p == rule || strings.HasPrefix(p, rule+".") || strings.HasPrefix(p, rule+"[")
		}
		if !used {
			t.Errorf("rule %s matches no field of the config", rule)
		}
	}
}

func fieldsOf(t *testing.T, workers string, extra string) map[string]string {
	t.Helper()
	cfg, err := config.Parse([]byte(`apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata: {name: lab}
spec:
  infrastructure: {provider: byoh, access: {ssh: {user: pi}}}
  kubernetes: {distribution: k3s` + extra + `}
  controlPlane: {machines: [{host: 10.0.0.1}]}
  workers:
` + workers))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Fields()
}

func TestRefusedChanges(t *testing.T) {
	const general = "    - {name: general, machines: [{host: 10.0.0.2}], labels: {tier: web}}\n"
	base := fieldsOf(t, general, "")
	for _, tc := range []struct {
		name          string
		workers, kube string
		mode          string
		want          []string
	}{
		{name: "labels and taints change in place", workers: "    - {name: general, machines: [{host: 10.0.0.2}], labels: {tier: batch, gpu: \"yes\"}, taints: [{key: a, effect: NoSchedule}]}\n"},
		// Machines added and removed are planned (machinechanges.go), never refused here.
		{name: "pools listed in another order", workers: general + "    - {name: gpu, machines: [{host: 10.0.0.3}]}\n"},
		{name: "a renamed pool", workers: "    - {name: web, machines: [{host: 10.0.0.2}], labels: {tier: web}}\n"},
		{name: "a machine added to a pool", workers: "    - {name: general, machines: [{host: 10.0.0.2}, {host: 10.0.0.4}], labels: {tier: web}}\n"},
		{name: "a machine's SSH port", workers: "    - {name: general, machines: [{host: 10.0.0.2, ssh: {port: 2222}}], labels: {tier: web}}\n", want: []string{
			"spec.workers[general].machines[10.0.0.2].ssh.port: 22 → 2222: " + whySSH}},
		{name: "the pod range", workers: general, kube: ", networking: {podCIDR: 10.50.0.0/16}", want: []string{
			"spec.kubernetes.networking.podCIDR: 10.42.0.0/16 → 10.50.0.0/16: " + whyNetwork}},
		// An upgrade: what it can't do is refused by upgradeOf.
		{name: "a release", workers: general, kube: ", version: v1.36.4"},
		{name: "the kubeconfig", workers: general, kube: ", kubeconfig: {context: lab-2, merge: false}"},
		{name: "a local cluster", workers: "    - {name: general, machines: [{host: 10.0.0.2}], labels: {tier: batch}}\n", mode: config.ModeLocal, want: []string{
			"spec.workers[general].labels.tier: web → batch: " + strings.ReplaceAll(whyLocal, "%s", "lab")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode := tc.mode
			if mode == "" {
				mode = config.ModeBYOH
			}
			if tc.name == "pools listed in another order" {
				// The new pool aside, general moving to second place is no change.
				tc.workers = "    - {name: gpu, machines: [{host: 10.0.0.3}]}\n" + general
			}
			got := refusedChanges(base, fieldsOf(t, tc.workers, tc.kube), mode, "lab")
			if !slices.Equal(got, tc.want) {
				t.Errorf("got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(tc.want, "\n  "))
			}
		})
	}
}

func TestRuleForPicksTheLongestPath(t *testing.T) {
	for path, want := range map[string]changeClass{
		"spec.kubernetes.networking.podCIDR":                     never,
		"spec.kubernetes.networking.calico.encapsulation":        never,
		"spec.kubernetes.networking.calico.version":              notYet,
		"spec.kubernetes.networking.cilium.encryption":           inPlace,
		"spec.kubernetes.networking.calico.dataplane":            never,
		"spec.kubernetes.storage.longhorn.replicas":              inPlace,
		"spec.kubernetes.metricsServer.enabled":                  inPlace,
		"spec.workers[gpu].labels.example.com/tier":              inPlace,
		"spec.workers[gpu].taints[a:NoSchedule].value":           inPlace,
		"spec.kubernetes.addons.helmCharts[web].values.replicas": inPlace,
		"spec.kubernetes.addons.helmCharts[web].namespace":       never,
		"spec.infrastructure.proxmox.template":                   inPlace,
		"spec.infrastructure.proxmox.node":                       never,
	} {
		if r := ruleFor(path); r == nil || r.class != want {
			t.Errorf("%s: %+v, want class %d", path, r, want)
		}
	}
	if ruleFor("spec.kubernetes.networkingX") != nil {
		t.Error("a rule matches whole names only")
	}
}
