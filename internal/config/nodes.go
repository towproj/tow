package config

import (
	"fmt"
	"regexp"
	"strings"
)

// NodeSettings apply to every node of a machine group.
type NodeSettings struct {
	Labels  map[string]string `yaml:"labels,omitempty"`
	Taints  []Taint           `yaml:"taints,omitempty"`
	Kubelet Component         `yaml:"kubelet,omitempty"` // merged over spec.kubernetes.kubelet; the pool wins
}

type Taint struct {
	Key    string `yaml:"key,omitempty"`
	Value  string `yaml:"value,omitempty"`
	Effect string `yaml:"effect,omitempty"` // NoSchedule | PreferNoSchedule | NoExecute
}

// ControlPlane is the control-plane group, which alone decides whether workloads may run on it.
type ControlPlane struct {
	Group       `yaml:",inline"`
	Schedulable string `yaml:"schedulable,omitempty"` // auto | true | false
}

const (
	SchedulableAuto  = "auto" // workloads run on control planes only when there are no workers
	SchedulableTrue  = "true"
	SchedulableFalse = "false"

	// controlPlaneTaint is the taint that keeps workloads off control planes.
	controlPlaneTaint = "node-role.kubernetes.io/control-plane"
)

var taintEffects = []string{"NoSchedule", "PreferNoSchedule", "NoExecute"}

// ControlPlaneSchedulable reports whether workloads may run on the control planes.
func (c *Config) ControlPlaneSchedulable() bool {
	switch c.Spec.ControlPlane.Schedulable {
	case SchedulableTrue:
		return true
	case SchedulableFalse:
		return false
	}
	return c.WorkerCount() == 0
}

// Label rules, from Kubernetes: an optional DNS-subdomain prefix and a name of at most 63
// characters; values follow the name's rule and may be empty.
var labelName = regexp.MustCompile(`^([A-Za-z0-9][-A-Za-z0-9_.]*)?[A-Za-z0-9]$`)

// Labels in the reserved kubernetes.io and k8s.io namespaces that a kubelet may still set on
// its own node (the NodeRestriction rules); the kubelet refuses any other.
var (
	kubeletSettableLabels = []string{
		"topology.kubernetes.io/zone", "topology.kubernetes.io/region",
		"failure-domain.beta.kubernetes.io/zone", "failure-domain.beta.kubernetes.io/region",
		"node.kubernetes.io/instance-type", "beta.kubernetes.io/instance-type",
	}
	kubeletSettableNamespaces = []string{"node.kubernetes.io", "kubelet.kubernetes.io"}
	kubeletOwnLabels          = []string{
		"kubernetes.io/hostname", "kubernetes.io/os", "kubernetes.io/arch",
		"beta.kubernetes.io/os", "beta.kubernetes.io/arch",
	}
	// distributionRoles are the role labels kubeadm and k3s put on control planes themselves.
	distributionRoles = []string{"control-plane", "master", "etcd"}
)

// NodeRoleLabelPrefix starts role labels, which kubectl shows in its ROLES column. A kubelet
// may not set them on its own node, so tow applies them through the API once it joined.
const NodeRoleLabelPrefix = "node-role.kubernetes.io/"

// SplitLabels separates the labels a kubelet registers its node with from the role labels
// tow applies through the API afterwards.
func SplitLabels(labels map[string]string) (kubelet, api map[string]string) {
	kubelet, api = map[string]string{}, map[string]string{}
	for k, v := range labels {
		if strings.HasPrefix(k, NodeRoleLabelPrefix) {
			api[k] = v
		} else {
			kubelet[k] = v
		}
	}
	return kubelet, api
}

func (c *Config) validateNodeSettings() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	for _, g := range c.namedGroups() {
		for _, key := range sortedKeys(g.Labels) {
			if err := labelKeyError(key); err != "" {
				add("%s.labels: %s %s", g.path, key, err)
			} else if v := g.Labels[key]; v != "" && (len(v) > 63 || !labelName.MatchString(v)) {
				add("%s.labels: the value of %s, %q, must be at most 63 letters, digits, '-', '_' or '.', starting and ending with a letter or digit", g.path, key, v)
			}
		}

		seen := map[string]bool{}
		for i, t := range g.Taints {
			at := fmt.Sprintf("%s.taints[%d]", g.path, i)
			if err := qualifiedNameError(t.Key); err != "" {
				add("%s.key %q %s", at, t.Key, err)
			}
			if t.Value != "" && (len(t.Value) > 63 || !labelName.MatchString(t.Value)) {
				add("%s.value %q must be at most 63 letters, digits, '-', '_' or '.', starting and ending with a letter or digit", at, t.Value)
			}
			if !contains(taintEffects, t.Effect) {
				add("%s.effect %q must be %s", at, t.Effect, joinOr(taintEffects))
			}
			if seen[t.Key+":"+t.Effect] {
				add("%s: %s:%s is already set", at, t.Key, t.Effect)
			}
			seen[t.Key+":"+t.Effect] = true
			if t.Key == controlPlaneTaint && g.role == RoleControlPlane {
				add("%s: %s conflicts with spec.controlPlane.schedulable", at, t.Key)
			}
		}

		errs = append(errs, validateArgs(g.path+".kubelet.extraArgs", g.Kubelet.ExtraArgs, kubeletFlags, false)...)
	}

	cp := c.Spec.ControlPlane
	if err := oneOf("spec.controlPlane.schedulable", cp.Schedulable, SchedulableAuto, SchedulableTrue, SchedulableFalse); err != nil {
		errs = append(errs, err)
	} else if cp.Schedulable == SchedulableFalse && c.WorkerCount() == 0 && c.ControlPlaneCount() > 0 {
		add("spec.controlPlane.schedulable is false but there are no workers: your workloads would have nowhere to run")
	}
	return errs
}

// labelKeyError explains why a node label key is refused, or returns "".
func labelKeyError(key string) string {
	if err := qualifiedNameError(key); err != "" {
		return err
	}
	if contains(kubeletOwnLabels, key) {
		return "is set by the kubelet itself"
	}
	if role, ok := strings.CutPrefix(key, NodeRoleLabelPrefix); ok {
		if contains(distributionRoles, role) {
			return "conflicts with a label the distribution sets itself"
		}
		return ""
	}
	prefix, _, ok := strings.Cut(key, "/")
	if !ok || !reserved(prefix) || contains(kubeletSettableLabels, key) {
		return ""
	}
	for _, ns := range kubeletSettableNamespaces {
		if prefix == ns || strings.HasSuffix(prefix, "."+ns) {
			return ""
		}
	}
	return "uses a reserved prefix (kubernetes.io, k8s.io): nodes may only carry topology.kubernetes.io/zone and /region, node.kubernetes.io/instance-type, node-role.kubernetes.io/<role>, and labels under node.kubernetes.io/ or kubelet.kubernetes.io/"
}

func reserved(prefix string) bool {
	for _, r := range []string{"kubernetes.io", "k8s.io"} {
		if prefix == r || strings.HasSuffix(prefix, "."+r) {
			return true
		}
	}
	return false
}

// qualifiedNameError checks a Kubernetes key like example.com/name: an optional DNS-subdomain
// prefix, then a name of at most 63 characters.
func qualifiedNameError(key string) string {
	prefix, name, hasPrefix := strings.Cut(key, "/")
	if !hasPrefix {
		prefix, name = "", key
	}
	if hasPrefix && (len(prefix) > 253 || !dnsSubdomain.MatchString(prefix)) {
		return "must have a lowercase DNS prefix, like example.com/name"
	}
	if len(name) > 63 || !labelName.MatchString(name) {
		return "must be at most 63 letters, digits, '-', '_' or '.', starting and ending with a letter or digit (optionally after a prefix like example.com/)"
	}
	return ""
}
