package tofu

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/towproj/tow/internal/providers"
)

// tfPlan is the part of 'tofu show -json <plan file>' tow reads.
type tfPlan struct {
	ResourceChanges []resourceChange `json:"resource_changes"`
}

type resourceChange struct {
	Address      string `json:"address"`
	Type         string `json:"type"`
	Index        any    `json:"index"`         // the for_each key: a machine's name
	ActionReason string `json:"action_reason"` // why it is replaced or deleted
	Change       struct {
		Actions      []string `json:"actions"`
		ReplacePaths [][]any  `json:"replace_paths"` // the attributes that force a replacement
	} `json:"change"`
}

func parsePlan(raw []byte) (*tfPlan, error) {
	var plan tfPlan
	if err := json.Unmarshal(raw, &plan); err != nil {
		return nil, fmt.Errorf("read OpenTofu plan: %w", err)
	}
	return &plan, nil
}

// verb is what the plan does to the resource: create, update, replace or delete; "" for
// nothing (no-op, a data source read).
func (rc resourceChange) verb() string {
	switch a := strings.Join(rc.Change.Actions, ","); a {
	case "create", "update", "delete":
		return a
	case "delete,create", "create,delete":
		return "replace"
	}
	return ""
}

// creating names the machines the plan creates.
func (p *tfPlan) creating(machineType string) []string {
	var out []string
	for _, rc := range p.ResourceChanges {
		if name, ok := rc.Index.(string); ok && rc.Type == machineType && rc.verb() == "create" {
			out = append(out, name)
		}
	}
	return out
}

// changeLines says what the plan changes, by verb and resource type; nil when nothing.
func (p *tfPlan) changeLines(where string) []string {
	byVerb := map[string]map[string]int{}
	totals := map[string]int{}
	for _, rc := range p.ResourceChanges {
		verb := rc.verb()
		if verb == "" {
			continue
		}
		if byVerb[verb] == nil {
			byVerb[verb] = map[string]int{}
		}
		byVerb[verb][rc.Type]++
		totals[verb]++
	}
	var lines []string
	for _, verb := range []string{"create", "replace", "update", "delete"} {
		if totals[verb] == 0 {
			continue
		}
		types := make([]string, 0, len(byVerb[verb]))
		for t, n := range byVerb[verb] {
			types = append(types, fmt.Sprintf("%s ×%d", t, n))
		}
		sort.Strings(types)
		lines = append(lines, fmt.Sprintf("Would %s %s in %s: %s", verb, pluralize(totals[verb], "resource"), where, strings.Join(types, ", ")))
	}
	return lines
}

// guard refuses a plan that changes what the cluster runs on in a way tow didn't plan.
// Once Kubernetes may run on the machines, nothing is replaced or deleted: a replaced load
// balancer or network breaks a cluster as surely as a replaced machine. On a running cluster
// no machine is added either: one missing was deleted outside tow, and a new blank one
// wouldn't join the cluster. allowed, when set, names what this run plans to replace or
// delete itself (firewall rules for an old address or a dropped port).
func guard(plan *tfPlan, level providers.Guard, machineType, cluster string, allowed func(resourceChange) bool) error {
	if level == providers.GuardOff {
		return nil
	}
	// A machine made again (allowed) brings new ids: what's keyed by its name (its firewall,
	// its load balancer attachments) is replaced with it.
	var remade []string
	for _, rc := range plan.ResourceChanges {
		if name, ok := rc.Index.(string); ok && rc.Type == machineType && rc.verb() == "create" && allowed != nil && allowed(rc) {
			remade = append(remade, name)
		}
	}
	keyedByRemade := func(rc resourceChange) bool {
		key, _ := rc.Index.(string)
		return rc.Type != machineType && slices.ContainsFunc(remade, func(name string) bool { return key == name || strings.HasSuffix(key, "-"+name) })
	}
	var refused []string
	for _, rc := range plan.ResourceChanges {
		what := rc.Address
		name, isMachine := rc.Index.(string)
		isMachine = isMachine && rc.Type == machineType
		if isMachine {
			what = "machine " + name
		}
		switch verb := rc.verb(); {
		case verb == "replace" && keyedByRemade(rc):
		case (verb == "replace" || verb == "delete") && allowed != nil && allowed(rc):
		case verb == "replace" || verb == "delete":
			refused = append(refused, fmt.Sprintf("%s would be %sd: %s", what, verb, reason(rc, cluster)))
		case verb == "create" && isMachine && level == providers.GuardRunning && !(allowed != nil && allowed(rc)):
			refused = append(refused, fmt.Sprintf("%s would be created: it is missing (deleted outside tow?)", what))
		}
	}
	if len(refused) == 0 {
		return nil
	}
	return fmt.Errorf("OpenTofu would change what cluster '%s' runs on in ways tow didn't plan, so nothing was changed:\n  - %s\n"+
		"If this was changed outside tow, put it back; if your file asks for it, tow can't do that yet on machines Kubernetes may run on. Then run 'tow apply' again.",
		cluster, strings.Join(refused, "\n  - "))
}

// reason says why OpenTofu replaces or deletes a resource, from its plan.
func reason(rc resourceChange, cluster string) string {
	switch rc.ActionReason {
	case "replace_because_tainted":
		return fmt.Sprintf("it is marked for replacement ('tofu untaint' in %s undoes it)", workdir(cluster))
	case "replace_by_request":
		return "a replacement was requested"
	case "replace_because_cannot_update":
		var paths []string
		for _, path := range rc.Change.ReplacePaths {
			var steps []string
			for _, s := range path {
				steps = append(steps, fmt.Sprint(s))
			}
			paths = append(paths, strings.Join(steps, "."))
		}
		if len(paths) > 0 {
			return strings.Join(paths, ", ") + " changed, which can't change in place"
		}
		return "a setting changed that can't change in place"
	case "delete_because_no_resource_config", "delete_because_each_key", "delete_because_count_index",
		"delete_because_no_module", "delete_because_wrong_repetition", "delete_because_no_move_target":
		return "tow no longer asks for it"
	}
	return "OpenTofu's plan says so"
}
