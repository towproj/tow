package config

import (
	"fmt"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Args are pass-through command-line flags for one component: the flag's name, without "--",
// and its value. A list repeats the flag, in order. tow checks only their shape; the tool
// that receives them (kubeadm, k3s or the component itself) judges their content.
type Args map[string]ArgValues

// ArgValues is one flag's value: written in the file, several to repeat the flag, or read
// from an environment variable at apply time, for a secret.
type ArgValues struct {
	Values []string
	Env    string // the variable holding the value; the value itself is never in the file or the state
}

// Value is a plain flag value; Several repeats a flag; FromEnv names the variable holding it.
func Value(v string) ArgValues      { return ArgValues{Values: []string{v}} }
func Several(v ...string) ArgValues { return ArgValues{Values: v} }
func FromEnv(name string) ArgValues { return ArgValues{Env: name} }
func (v ArgValues) empty() bool     { return len(v.Values) == 0 && v.Env == "" }
func (v ArgValues) fromEnv() bool   { return v.Env != "" }

const argShapes = "a flag takes a plain value, a list of them to repeat it, or {env: VARIABLE} for a secret"

// UnmarshalYAML takes a plain value (30, true, "x"), a list of plain values, or {env: NAME}.
// A key written with no value never reaches here: it decodes to an empty ArgValues, refused
// by validateArgs.
func (v *ArgValues) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		*v = Value(n.Value)
		return nil
	case yaml.SequenceNode:
		out := ArgValues{Values: []string{}}
		for _, item := range n.Content {
			if item.Kind != yaml.ScalarNode {
				return fmt.Errorf("line %d: a flag's list holds plain values only", item.Line)
			}
			out.Values = append(out.Values, item.Value)
		}
		*v = out
		return nil
	case yaml.MappingNode:
		if name, ok := envRef(n); ok {
			*v = FromEnv(name)
			return nil
		}
	}
	return fmt.Errorf("line %d: %s", n.Line, argShapes)
}

// envRef reads {env: NAME}.
func envRef(n *yaml.Node) (string, bool) {
	if n.Kind != yaml.MappingNode || len(n.Content) != 2 || n.Content[0].Value != "env" || n.Content[1].Kind != yaml.ScalarNode {
		return "", false
	}
	return n.Content[1].Value, true
}

// MarshalYAML writes the value as the file would, so the effective config (and the state's
// fingerprint) holds a variable's name, never its value.
func (v ArgValues) MarshalYAML() (any, error) {
	switch {
	case v.fromEnv():
		return map[string]string{"env": v.Env}, nil
	case len(v.Values) == 1:
		return v.Values[0], nil
	}
	return v.Values, nil
}

// Component holds the pass-through flags of one Kubernetes component.
type Component struct {
	ExtraArgs Args `yaml:"extraArgs,omitempty"`
}

// APIServer is kube-apiserver: extra names for its certificate, and its flags.
type APIServer struct {
	CertSANs  []string `yaml:"certSANs,omitempty"` // apiEndpoint and node addresses are added automatically
	ExtraArgs Args     `yaml:"extraArgs,omitempty"`
}

// K3s holds k3s's own flags, written as keys of /etc/rancher/k3s/config.yaml.
type K3s struct {
	Server Component `yaml:"server,omitempty"`
	Agent  Component `yaml:"agent,omitempty"`
}

var flagName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Owners of flags that a field of the file, or tow itself (""), already sets. Setting one
// through extraArgs too would give one setting two sources, so it is refused.
const (
	ownerPodCIDR       = "spec.kubernetes.networking.podCIDR"
	ownerServiceCIDR   = "spec.kubernetes.networking.serviceCIDR"
	ownerClusterDomain = "spec.kubernetes.networking.clusterDomain"
	ownerLabels        = "the pools' labels"
	ownerTaints        = "the pools' taints"
)

var (
	apiServerFlags = map[string]string{
		"service-cluster-ip-range": ownerServiceCIDR,
		"advertise-address":        "",
		"secure-port":              "",
	}
	controllerManagerFlags = map[string]string{
		"cluster-cidr":             ownerPodCIDR,
		"service-cluster-ip-range": ownerServiceCIDR,
	}
	kubeletFlags = map[string]string{
		"cluster-domain":       ownerClusterDomain,
		"node-labels":          ownerLabels,
		"register-with-taints": ownerTaints,
		"node-ip":              "",
		"hostname-override":    "",
	}
	k3sFlags = map[string]string{
		"cluster-cidr":                ownerPodCIDR,
		"service-cidr":                ownerServiceCIDR,
		"cluster-domain":              ownerClusterDomain,
		"tls-san":                     "spec.kubernetes.apiServer.certSANs",
		"node-label":                  ownerLabels,
		"node-taint":                  ownerTaints,
		"kube-apiserver-arg":          "spec.kubernetes.apiServer.extraArgs",
		"kube-controller-manager-arg": "spec.kubernetes.controllerManager.extraArgs",
		"kube-scheduler-arg":          "spec.kubernetes.scheduler.extraArgs",
		"kubelet-arg":                 "spec.kubernetes.kubelet.extraArgs",
		"etcd-arg":                    "spec.kubernetes.etcd.extraArgs",
		"flannel-backend":             "spec.kubernetes.networking.flannel.backend",
		"disable-kube-proxy":          "spec.kubernetes.networking (cilium.kubeProxyReplacement, calico.dataplane: ebpf)",
		"token":                       "", // the server's may come from {env}: see userTokens
		"token-file":                  "",
		"agent-token":                 "",
		"agent-token-file":            "",
		"server":                      "",
		"cluster-init":                "",
		"datastore-endpoint":          "", // tow always uses embedded etcd
		"etcd-snapshot-schedule-cron": "spec.kubernetes.backups.schedule",
		"etcd-snapshot-retention":     "spec.kubernetes.backups.keep",
		"node-name":                   "",
		"node-ip":                     "",
		"https-listen-port":           "",
		"disable":                     "", // follows the components the file selects
		"disable-network-policy":      "",
	}
)

// secretFlags take a secret inline: they accept only {env: VARIABLE} (k3s's join tokens and
// its etcd snapshot keys).
var secretFlags = []string{"token", "agent-token", "etcd-s3-access-key", "etcd-s3-secret-key", "etcd-s3-session-token"}

// userTokens are join tokens a k3s server may take from the user ({env} only): tow then
// uses them instead of generating its own, and gives agents the right one.
var userTokens = []string{"token", "agent-token"}

// validateArgs checks one extraArgs map: flag names, values, and flags already set elsewhere.
// tokens is true for k3s servers, which may take the user's join tokens.
func validateArgs(path string, args Args, owned map[string]string, tokens bool) []error {
	var errs []error
	for _, name := range sortedKeys(args) {
		v := args[name]
		flag := strings.ReplaceAll(strings.ToLower(name), "_", "-")
		if !flagName.MatchString(name) {
			errs = append(errs, fmt.Errorf("%s: %q must be a flag name without leading dashes or \"=\", like audit-log-maxage", path, name))
			continue
		}
		switch {
		case v.empty():
			errs = append(errs, fmt.Errorf("%s: %s has no value (write true for an on/off flag)", path, name))
		case v.fromEnv() && !envName.MatchString(v.Env):
			errs = append(errs, fmt.Errorf("%s: %s: %q must name an environment variable, like S3_SECRET_KEY", path, name, v.Env))
		case !v.fromEnv() && contains(secretFlags, flag):
			errs = append(errs, fmt.Errorf("%s: %s takes a secret: write %s: {env: VARIABLE} and set VARIABLE when you run tow", path, name, name))
		}
		if tokens && v.fromEnv() && contains(userTokens, flag) {
			continue
		}
		owner, taken := owned[flag]
		switch {
		case !taken:
		case owner == "":
			errs = append(errs, fmt.Errorf("%s: %s conflicts with a flag tow sets itself", path, name))
		default:
			errs = append(errs, fmt.Errorf("%s: %s conflicts with %s", path, name, owner))
		}
	}
	return errs
}

// KubeletArgs are the kubelet flags of one machine group: the pool's own over the cluster's.
func (c *Config) KubeletArgs(g *Group) Args {
	out := Args{}
	for k, v := range c.Spec.Kubernetes.Kubelet.ExtraArgs {
		out[k] = v
	}
	for k, v := range g.Kubelet.ExtraArgs {
		out[k] = v
	}
	return out
}

// secretEnv lists the variables an extraArgs map reads.
func (a Args) secretEnv() []string {
	var out []string
	for _, v := range a {
		if v.fromEnv() {
			out = append(out, v.Env)
		}
	}
	return out
}

// allArgs lists every extraArgs map in the file, with its path.
func (c *Config) allArgs() []struct {
	path string
	args Args
} {
	k := c.Spec.Kubernetes
	out := []struct {
		path string
		args Args
	}{
		{"spec.kubernetes.apiServer.extraArgs", k.APIServer.ExtraArgs},
		{"spec.kubernetes.controllerManager.extraArgs", k.ControllerManager.ExtraArgs},
		{"spec.kubernetes.scheduler.extraArgs", k.Scheduler.ExtraArgs},
		{"spec.kubernetes.etcd.extraArgs", k.Etcd.ExtraArgs},
		{"spec.kubernetes.kubelet.extraArgs", k.Kubelet.ExtraArgs},
		{"spec.kubernetes.k3s.server.extraArgs", k.K3s.Server.ExtraArgs},
		{"spec.kubernetes.k3s.agent.extraArgs", k.K3s.Agent.ExtraArgs},
	}
	for _, g := range c.namedGroups() {
		out = append(out, struct {
			path string
			args Args
		}{g.path + ".kubelet.extraArgs", g.Kubelet.ExtraArgs})
	}
	return out
}

// SecretEnv lists every environment variable the file reads a secret from, sorted and once
// each. Apply reads each one once, hides its value from all output, and keeps it from the
// programs it starts (tofu, k3d, ...).
func (c *Config) SecretEnv() []string {
	seen := map[string]bool{}
	for _, a := range c.allArgs() {
		for _, name := range a.args.secretEnv() {
			seen[name] = true
		}
	}
	if m := c.Spec.Kubernetes.Storage.SMB; m != nil {
		seen[m.UsernameEnv], seen[m.PasswordEnv] = true, true
	}
	if contains(c.Spec.Kubernetes.Storage.CSI, CSIProxmox) {
		seen[ProxmoxCSITokenEnv] = true
	}
	for _, h := range c.Spec.Kubernetes.Addons.HelmCharts {
		for _, name := range h.ValuesFromEnv {
			seen[name] = true
		}
	}
	delete(seen, "")
	return sortedKeys(seen)
}

// exposedEnvWarnings says where a secret flag's value can still be seen, outside tow's
// own output (which never shows it):
//   - kubeadm runs components from flags in static Pod manifests: the node's process list
//     and the kube-system mirror Pod show them;
//   - k3s writes the flags it gives components into its log (checked 2026-11-09); its own
//     flags (etcd-s3-*, token) stay out of it;
//   - k3d passes everything on its command line: 'docker inspect' on this machine shows it.
func (c *Config) exposedEnvWarnings() {
	const fileHint = "\n  → prefer a flag that takes a file, where the component has one"
	d := c.Spec.Kubernetes.Distribution
	for _, a := range c.allArgs() {
		own := strings.HasPrefix(a.path, "spec.kubernetes.k3s.")
		for _, name := range sortedKeys(a.args) {
			if !a.args[name].fromEnv() {
				continue
			}
			var where string
			switch {
			case d == DistributionKubeadm:
				where = "kubeadm passes it on the component's command line: anyone on the node can see it in the process list, and cluster admins in its kube-system Pod" + fileHint
			case d == DistributionK3d:
				where = "local clusters pass it on k3d's command line: 'docker inspect' on this machine shows it"
			case !own:
				where = "k3s writes the flags it gives components into its log on the node ('journalctl -u k3s')" + fileHint
			default:
				continue // k3s reads its own flags from a root-only file and doesn't log them
			}
			c.warnings = append(c.warnings, fmt.Sprintf("%s: %s comes from an environment variable, but %s", a.path, name, where))
		}
	}
}
