package core

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/providers"
)

// controlPlaneTaint keeps workloads off control planes (kubeadm and k3s use the same one);
// spec.controlPlane.schedulable decides whether they have it.
var controlPlaneTaint = kubeapi.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: "NoSchedule"}

// nodeChange is what one node needs for its labels and taints to match the file.
type nodeChange struct {
	node      string
	setLabels map[string]string
	rmLabels  []string
	addTaints []kubeapi.Taint
	rmTaints  []string // by key:effect
}

func (c nodeChange) empty() bool {
	return len(c.setLabels)+len(c.rmLabels)+len(c.addTaints)+len(c.rmTaints) == 0
}

// describe says what changes, the same way for every node that needs the same.
func (c nodeChange) describe() string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(c.setLabels)) {
		parts = append(parts, "label "+k+"="+c.setLabels[k])
	}
	for _, k := range c.rmLabels {
		parts = append(parts, "remove label "+k)
	}
	for _, t := range c.addTaints {
		parts = append(parts, "taint "+t.String())
	}
	for _, id := range c.rmTaints {
		parts = append(parts, "remove taint "+id)
	}
	return strings.Join(parts, "; ")
}

// nodeChanges compares each node tow made with the file. tow owns the labels and
// taints the file has, or had at the last apply (old, the fields applied then), and the
// control-plane taint on control planes: it sets or removes those, never any other (a
// cloud's, an add-on's, one set with kubectl). It also returns the nodes the API has that
// tow didn't make, and those it made that the API doesn't have.
func nodeChanges(cfg *config.Config, old map[string]string, nodes []providers.Node, live []kubeapi.NodeMeta) (changes []nodeChange, unknown, missing []string) {
	byName := map[string]kubeapi.NodeMeta{}
	for _, m := range live {
		byName[m.Name] = m
	}
	ours := map[string]bool{}
	for _, n := range nodes {
		ours[n.Name] = true
		m, ok := byName[n.Name]
		if !ok {
			missing = append(missing, n.Name)
			continue
		}
		g, path := groupOf(cfg, n)
		if g == nil {
			continue // its pool left the file: refused before a plan is made
		}
		c := nodeChange{node: n.Name, setLabels: map[string]string{}}

		// Labels: those the file has, then those it had and no longer has.
		for k, v := range g.Labels {
			if have, ok := m.Labels[k]; !ok || have != v {
				c.setLabels[k] = v
			}
		}
		for _, k := range keysUnder(old, path+".labels.") {
			if _, wanted := g.Labels[k]; !wanted {
				if _, has := m.Labels[k]; has {
					c.rmLabels = append(c.rmLabels, k)
				}
			}
		}

		// Taints, by key and effect; the control-plane taint's value doesn't matter.
		want := map[string]kubeapi.Taint{}
		for _, t := range g.Taints {
			kt := kubeapi.Taint{Key: t.Key, Value: t.Value, Effect: t.Effect}
			want[kt.ID()] = kt
		}
		owned := map[string]bool{}
		for id := range want {
			owned[id] = true
		}
		for _, id := range itemsUnder(old, path+".taints[") {
			owned[id] = true
		}
		if n.Role == config.RoleControlPlane {
			owned[controlPlaneTaint.ID()] = true
			if !cfg.ControlPlaneSchedulable() {
				want[controlPlaneTaint.ID()] = controlPlaneTaint
			}
		}
		have := map[string]kubeapi.Taint{}
		for _, t := range m.Taints {
			have[t.ID()] = t
		}
		for _, id := range slices.Sorted(maps.Keys(want)) {
			t, ok := have[id]
			if !ok || (id != controlPlaneTaint.ID() && t.Value != want[id].Value) {
				c.addTaints = append(c.addTaints, want[id])
			}
		}
		for _, id := range slices.Sorted(maps.Keys(owned)) {
			if _, wanted := want[id]; !wanted {
				if _, has := have[id]; has {
					c.rmTaints = append(c.rmTaints, id)
				}
			}
		}
		if !c.empty() {
			changes = append(changes, c)
		}
	}
	for _, m := range live {
		if !ours[m.Name] {
			unknown = append(unknown, m.Name)
		}
	}
	slices.Sort(unknown)
	return changes, unknown, missing
}

// groupOf is the part of the file a node comes from, and that part's field path.
func groupOf(cfg *config.Config, n providers.Node) (*config.Group, string) {
	if n.Role == config.RoleControlPlane {
		return &cfg.Spec.ControlPlane.Group, "spec.controlPlane"
	}
	p := workerPool(cfg, n.Pool)
	if p == nil {
		return nil, ""
	}
	return &p.Group, fmt.Sprintf("spec.workers[%s]", p.Name)
}

// keysUnder lists the map keys of fields under prefix: "spec.workers[a].labels." gives the
// pool's label keys (which may contain dots themselves).
func keysUnder(fields map[string]string, prefix string) []string {
	var out []string
	for k := range fields {
		if rest, ok := strings.CutPrefix(k, prefix); ok {
			out = append(out, rest)
		}
	}
	slices.Sort(out)
	return out
}

// itemsUnder lists the keys of a keyed list's items: "spec.controlPlane.taints[" gives each
// taint's key:effect.
func itemsUnder(fields map[string]string, prefix string) []string {
	seen := map[string]bool{}
	for k := range fields {
		if rest, ok := strings.CutPrefix(k, prefix); ok {
			if item, _, ok := strings.Cut(rest, "]"); ok {
				seen[item] = true
			}
		}
	}
	return slices.Sorted(maps.Keys(seen))
}
