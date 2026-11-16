package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Fields flattens the effective config (after defaults) into "path: value" pairs, like
// "spec.workers[general].count: 3". Comments, whitespace and key order in the file never show
// up here; only meaning does. Empty values are left out, so a diff shows "(none) → x" for an
// added field. Every field counts, including ones added later, with no list to maintain.
//
// Lists of things with a name are keyed by it, so reordering them changes nothing (keyedLists);
// lists whose order means nothing are one sorted value (unorderedLists). Other lists keep their
// positions: their order matters, like DNS servers or a flag given twice.
func (c *Config) Fields() map[string]string {
	var tree any
	if err := yaml.Unmarshal([]byte(c.Effective()), &tree); err != nil {
		panic(err)
	}
	f := map[string]string{}
	flatten("", tree, f)
	return f
}

// Effective is the config after defaults, as YAML: what state keeps for Restore.
func (c *Config) Effective() string {
	data, err := yaml.Marshal(c)
	if err != nil {
		panic(err) // plain structs always marshal
	}
	return string(data)
}

// Restore reads back what Effective wrote: the config a running cluster was last applied
// with. It isn't checked again (it was when applied, and the files its manifests name may be
// gone since).
func Restore(effective string) (*Config, error) {
	var c Config
	if err := yaml.Unmarshal([]byte(effective), &c); err != nil {
		return nil, fmt.Errorf("read the last applied config: %w", err)
	}
	if err := c.resolveProvider(); err != nil {
		return nil, err
	}
	return &c, nil
}

func flatten(path string, v any, out map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			p := k
			if path != "" {
				p = path + "." + k
			}
			flatten(p, child, out)
		}
	case []any:
		if slices.Contains(unorderedLists, Generic(path)) {
			var values []string
			for _, child := range t {
				values = append(values, fmt.Sprint(child))
			}
			slices.Sort(values)
			if len(values) > 0 {
				out[path] = strings.Join(values, ", ")
			}
			return
		}
		keyOf := keyedLists[Generic(path)]
		if keyOf == nil && strings.HasSuffix(path, ".bgp.peers") {
			keyOf = field("address")
		}
		seen := map[string]bool{}
		for i, child := range t {
			index := strconv.Itoa(i)
			if keyOf != nil {
				// An item written as a plain value (a manifest's path) is its own key. A
				// missing or repeated key (refused by validation anyway) keeps the position.
				k := fmt.Sprint(child)
				if m, ok := child.(map[string]any); ok {
					k = keyOf(m)
				}
				if k != "" && !seen[k] {
					index, seen[k] = k, true
				}
			}
			flatten(path+"["+index+"]", child, out)
		}
	case nil:
	default:
		// false stays: a setting turned off reads "true → false", not "true → (none)".
		if s := fmt.Sprint(t); s != "" && s != "0" {
			out[path] = s
		}
	}
}

// keyedLists key each item of these lists (by their generic path) by one of its fields.
var keyedLists = map[string]func(map[string]any) string{
	"spec.workers":                      field("name"),
	"spec.controlPlane.machines":        field("host"),
	"spec.workers[].machines":           field("host"),
	"spec.controlPlane.taints":          taintKey,
	"spec.workers[].taints":             taintKey,
	"spec.kubernetes.addons.helmCharts": field("name"),
	"spec.kubernetes.addons.manifests":  field("path"),
}

// unorderedLists are sets: one sorted value each.
var unorderedLists = []string{
	"spec.infrastructure.access.adminCIDRs",
	"spec.infrastructure.access.ports",
	"spec.kubernetes.storage.csi",
	"spec.kubernetes.ingress.controllers",
	"spec.kubernetes.apiServer.certSANs",
}

func field(name string) func(map[string]any) string {
	return func(m map[string]any) string { return fmt.Sprint(valueOr(m[name])) }
}

// taintKey: a node has one taint per key and effect.
func taintKey(m map[string]any) string {
	return fmt.Sprintf("%v:%v", valueOr(m["key"]), valueOr(m["effect"]))
}

func valueOr(v any) any {
	if v == nil {
		return ""
	}
	return v
}

// Generic turns a field's path into the path of its kind, with every list key or position
// left out: spec.workers[general].labels.tier becomes spec.workers[].labels.tier.
func Generic(path string) string {
	var b strings.Builder
	depth := 0
	for _, r := range path {
		switch {
		case r == '[':
			if depth == 0 {
				b.WriteString("[]")
			}
			depth++
		case r == ']':
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// HashFields is a stable SHA-256 over fields, independent of map order.
func HashFields(fields map[string]string) string {
	h := sha256.New()
	for _, k := range sortedKeys(fields) {
		fmt.Fprintf(h, "%s=%s\n", k, fields[k])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *Config) Hash() string { return HashFields(c.Fields()) }

// DiffFields lists what changed from old to new, one "path: before → after" line per field.
func DiffFields(old, new map[string]string) []string {
	keys := map[string]bool{}
	for k := range old {
		keys[k] = true
	}
	for k := range new {
		keys[k] = true
	}

	var lines []string
	for _, k := range sortedKeys(keys) {
		before, after := old[k], new[k]
		if before == after {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s → %s", k, orNone(before), orNone(after)))
	}
	return lines
}

func orNone(v string) string {
	if v == "" {
		return "(none)"
	}
	return v
}
