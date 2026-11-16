package core

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/towproj/tow/internal/config"
)

// changeClass says what happens when a field of the file changes on a running cluster.
type changeClass int

const (
	// inPlace: apply plans the change and makes it.
	inPlace changeClass = iota + 1
	// notYet: possible later, refused for now.
	notYet
	// never: refused, with the reason and the way to get there instead.
	never
)

type changeRule struct {
	class changeClass
	why   string // notYet, never: why it's refused, and what to do
}

// Reasons shared by several fields.
const (
	whyRuntime    = "the container runtime's release changes with a Kubernetes upgrade (change it in the same apply as spec.kubernetes.version)"
	whyFlannel    = "nodes on different flannel backends can't reach each other, so the backend can't change one node at a time: build a new cluster with it and move your workloads"
	whyVersion    = "an add-on keeps its version on a running cluster: versions change with a Kubernetes upgrade (change it in the same apply as spec.kubernetes.version)"
	whyCalicoPool = "Calico's operator doesn't change an IP pool once it's made (Tigera's docs): change the IPPool with calicoctl yourself, or build a new cluster with it"
	whyMetrics    = "turning metrics-server on or off isn't possible on a running cluster yet (k3s bundles it; on kubeadm every kubelet needs new settings)"
	whySize       = "a running machine keeps its size: add a worker pool with the new size, apply, then remove the old pool"
	whyNetwork    = "the cluster's network can't change once it runs: build a new cluster with it and move your workloads"
	whyProvider   = "a running cluster stays where it was built: build a new cluster there and move your workloads"
	whySSH        = "how tow reaches the machines can't change once they're set up"
	whyKubeProxy  = "it adds or removes kube-proxy on every node: build a new cluster with it and move your workloads"
	whyEndpoint   = "every node and certificate uses the API's address: build a new cluster with it and move your workloads"
	whyLocal      = "a local cluster can't change: delete it ('tow delete --name %s') and apply again"
	whyUnknownFld = "tow doesn't know whether this can change on a running cluster"
)

// changeRules classify fields by their generic path (config.Generic). A field takes the rule
// of its longest matching path: the rule's path itself, or one of its fields, map keys or
// list items. The test walks the config's Go types and fails on any field with no rule.
var changeRules = map[string]changeRule{
	"apiVersion":    {class: inPlace},
	"kind":          {class: inPlace},
	"metadata.name": {never, "the name is the cluster: a new name builds a new cluster"},
	"spec.flavor":   {class: inPlace}, // a preset: what it changes is classified field by field

	"spec.kubernetes.distribution":     {never, "the distribution can't change under a running cluster: build a new cluster with it and move your workloads"},
	"spec.kubernetes.version":          {class: inPlace}, // an upgrade: what can't be done is refused by upgradeOf
	"spec.kubernetes.containerRuntime": {never, "every node runs its pods with this runtime: build a new cluster with it and move your workloads"},
	"spec.kubernetes.containerd":       {notYet, whyRuntime},
	"spec.kubernetes.cri-o":            {notYet, whyRuntime},
	"spec.kubernetes.apiEndpoint":      {never, whyEndpoint},
	"spec.kubernetes.apiLoadBalancer":  {never, whyEndpoint},
	// Component settings roll over the nodes; what a distribution can't change yet, or k3s
	// can't change at all, is refused by refusedSettings.
	"spec.kubernetes.apiServer":         {class: inPlace},
	"spec.kubernetes.controllerManager": {class: inPlace},
	"spec.kubernetes.scheduler":         {class: inPlace},
	"spec.kubernetes.etcd":              {class: inPlace},
	"spec.kubernetes.kubelet":           {class: inPlace},
	"spec.kubernetes.k3s":               {class: inPlace},

	"spec.kubernetes.networking":                             {never, whyNetwork},
	"spec.kubernetes.networking.flannel":                     {never, whyFlannel},
	"spec.kubernetes.networking.flannel.version":             {notYet, whyVersion},
	"spec.kubernetes.networking.calico":                      {notYet, whyVersion},
	"spec.kubernetes.networking.calico.encapsulation":        {never, whyCalicoPool},
	"spec.kubernetes.networking.calico.dataplane":            {never, whyKubeProxy},
	"spec.kubernetes.networking.cilium":                      {class: inPlace}, // its version: checked against the one that runs (builtinchanges.go)
	"spec.kubernetes.networking.cilium.kubeProxyReplacement": {never, whyKubeProxy},
	// The built-in add-ons: what can't change depends on the distribution and on what runs
	// (planBuiltins; k3s's bundled ones are its settings: refusedSettings, planSettings).
	"spec.kubernetes.storage":                       {class: inPlace},
	"spec.kubernetes.serviceLoadBalancer":           {class: inPlace},
	"spec.kubernetes.ingress":                       {class: inPlace},
	"spec.kubernetes.metricsServer":                 {class: inPlace}, // k3s's bundled one; kubeadm's: refusedSettings
	"spec.kubernetes.addons.helmCharts":             {class: inPlace},
	"spec.kubernetes.addons.helmCharts[].namespace": {never, "a release can't move to another namespace: remove the chart from the file, apply, then add it back"},
	"spec.kubernetes.addons.manifests":              {class: inPlace}, // a move to another namespace: movedManifests
	"spec.kubernetes.kubeconfig":                    {class: inPlace},
	"spec.kubernetes.backups":                       {class: inPlace}, // kubeadm's timer (planBackups); k3s's settings (planSettings)

	"spec.infrastructure.provider":          {never, whyProvider},
	"spec.infrastructure.os":                {never, "the machines' operating system is chosen when the cluster is built: build a new cluster with it and move your workloads"},
	"spec.infrastructure.proxmox.cpuType":   {never, "running VMs keep their CPU model: build a new cluster with it and move your workloads"},
	"spec.infrastructure.aws":               {never, whyProvider},
	"spec.infrastructure.gcp":               {never, whyProvider},
	"spec.infrastructure.azure":             {never, whyProvider},
	"spec.infrastructure.proxmox":           {never, whyProvider},
	"spec.infrastructure.proxmox.url":       {class: inPlace}, // the same server under another address
	"spec.infrastructure.proxmox.template":  {class: inPlace}, // new VMs only: running ones keep theirs
	"spec.infrastructure.network":           {never, whyNetwork},
	"spec.infrastructure.access.ssh":        {never, whySSH},
	"spec.infrastructure.access.adminCIDRs": {class: inPlace},
	"spec.infrastructure.access.ports":      {class: inPlace},

	// Machines added and removed: what can't be done depends on what runs (machinechanges.go).
	"spec.controlPlane.count":          {class: inPlace},
	"spec.controlPlane.machines":       {class: inPlace},
	"spec.controlPlane.machines[].ssh": {never, whySSH},
	"spec.controlPlane.instanceType":   {never, whySize},
	"spec.controlPlane.resources":      {never, whySize},
	"spec.controlPlane.disk":           {never, whySize},
	"spec.controlPlane.labels":         {class: inPlace},
	"spec.controlPlane.taints":         {class: inPlace},
	"spec.controlPlane.schedulable":    {class: inPlace},
	"spec.controlPlane.kubelet":        {class: inPlace},
	"spec.workers":                     {class: inPlace}, // a renamed pool is a new pool, and the old one leaving
	"spec.workers[].name":              {class: inPlace},
	"spec.workers[].count":             {class: inPlace},
	"spec.workers[].machines":          {class: inPlace},
	"spec.workers[].machines[].ssh":    {never, whySSH},
	"spec.workers[].instanceType":      {never, whySize},
	"spec.workers[].resources":         {never, whySize},
	"spec.workers[].disk":              {never, whySize},
	"spec.workers[].labels":            {class: inPlace},
	"spec.workers[].taints":            {class: inPlace},
	"spec.workers[].kubelet":           {class: inPlace},
}

// ruleFor finds a field's rule: the longest rule path the field's generic path starts with,
// at a boundary (the end, a '.' or a '['). nil when none.
func ruleFor(path string) *changeRule {
	generic := config.Generic(path)
	best := ""
	for p := range changeRules {
		if len(p) <= len(best) || !strings.HasPrefix(generic, p) {
			continue
		}
		if rest := generic[len(p):]; rest == "" || rest[0] == '.' || rest[0] == '[' {
			best = p
		}
	}
	if best == "" {
		return nil
	}
	r := changeRules[best]
	return &r
}

// refusedChanges lists the changes from old to new fields that can't be made on a running
// cluster of this mode, one line each with the reason; empty when all can. A list item
// that's only on one side (a new pool, a removed machine) counts as one change, classified as
// the item, not field by field.
func refusedChanges(old, new map[string]string, mode, name string) []string {
	byItem := map[string]bool{}
	var lines []string
	for _, line := range config.DiffFields(old, new) {
		path, change, _ := strings.Cut(line, ": ")
		if item := itemOnOneSide(path, old, new); item != "" {
			if byItem[item] {
				continue
			}
			byItem[item] = true
			path, change = item, "removed"
			if hasItem(new, item) {
				change = "added"
			}
		}
		r := ruleFor(path)
		switch {
		case mode == config.ModeLocal && !slices.Contains([]string{"apiVersion", "kind", "spec.flavor"}, path) &&
			!strings.HasPrefix(path, chartsPath) && !strings.HasPrefix(path, manifestsPath):
			lines = append(lines, fmt.Sprintf("%s: %s: "+whyLocal, path, change, name))
		case r == nil:
			lines = append(lines, fmt.Sprintf("%s: %s: %s", path, change, whyUnknownFld))
		case r.class != inPlace:
			lines = append(lines, fmt.Sprintf("%s: %s: %s", path, change, r.why))
		}
	}
	sort.Strings(lines)
	return lines
}

// itemOnOneSide returns the outermost list item in path (up to a ']') that only one of the
// field sets has: a pool, a machine, a chart added or removed. "" when there's none.
func itemOnOneSide(path string, old, new map[string]string) string {
	for i, r := range path {
		if r != ']' {
			continue
		}
		item := path[:i+1]
		if hasItem(old, item) != hasItem(new, item) {
			return item
		}
	}
	return ""
}

func hasItem(fields map[string]string, item string) bool {
	for k := range fields {
		if k == item || strings.HasPrefix(k, item+".") || strings.HasPrefix(k, item+"[") {
			return true
		}
	}
	return false
}
