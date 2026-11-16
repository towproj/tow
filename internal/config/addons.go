package config

import (
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

const MetricsServerName = "metrics-server"

type MetricsServer struct {
	Enabled *bool  `yaml:"enabled,omitempty"` // empty = the flavor's choice, else off
	Version string `yaml:"version,omitempty"` // not on k3s, which bundles it
}

// Addons are what tow has no field for: installed last, in the file's order. Nothing goes
// into the "default" namespace: a chart or manifest that names none gets one of its own.
type Addons struct {
	HelmCharts []HelmChart `yaml:"helmCharts,omitempty"`
	Manifests  []Manifest  `yaml:"manifests,omitempty"`
}

// HelmChart is installed as written; Helm validates the chart and its values.
type HelmChart struct {
	Name      string         `yaml:"name,omitempty"` // the release name
	Repo      string         `yaml:"repo,omitempty"` // https:// or oci://
	Chart     string         `yaml:"chart,omitempty"`
	Version   string         `yaml:"version,omitempty"`   // exact: a range would make drift meaningless
	Namespace string         `yaml:"namespace,omitempty"` // default: one named after the release, created
	Values    map[string]any `yaml:"values,omitempty"`
	// ValuesFromEnv sets secret values at install time: a value's path (auth.password) and the
	// environment variable holding it. The value is never in the file or the state.
	ValuesFromEnv map[string]string `yaml:"valuesFromEnv,omitempty"`
}

// Manifest is an https URL, or a file or folder relative to the cluster file, applied as it
// is. Written as a plain string, or {path, namespace}.
type Manifest struct {
	Path string `yaml:"path,omitempty"`
	// Namespace is where its objects that name none go; default: one named after the file
	// (app.yaml → app), created. Objects that name their own keep it.
	Namespace string `yaml:"namespace,omitempty"`
}

// UnmarshalYAML takes a plain string (the path) or {path, namespace}.
func (m *Manifest) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		return n.Decode(&m.Path)
	}
	type plain Manifest
	return n.Decode((*plain)(m))
}

// MarshalYAML writes a manifest without a namespace as the plain string it was given.
func (m Manifest) MarshalYAML() (any, error) {
	if m.Namespace == "" {
		return m.Path, nil
	}
	type plain Manifest
	return plain(m), nil
}

// Target is the namespace a manifest's objects go to when they name none: its own, or one
// named after the file or folder (https://x/deploy/app.yaml → app); "" when the name has
// nothing a namespace can be made of.
func (m Manifest) Target() string {
	if m.Namespace != "" {
		return m.Namespace
	}
	p := m.Path
	if u, err := url.Parse(p); err == nil && u.Scheme != "" {
		p = u.Path
	}
	base := path.Base(strings.TrimRight(filepath.ToSlash(p), "/"))
	for _, ext := range []string{".yaml", ".yml", ".json"} {
		base = strings.TrimSuffix(base, ext)
	}
	name := strings.Trim(notLabel.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if len(name) > 63 {
		name = strings.TrimRight(name[:63], "-")
	}
	if name == "." || !dnsLabel.MatchString(name) {
		return ""
	}
	return name
}

// notLabel are the runs of characters a namespace name can't have.
var notLabel = regexp.MustCompile(`[^a-z0-9-]+`)

// releaseName is Helm's limit: a DNS label of at most 53 characters.
var releaseName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,51}[a-z0-9])?$`)

func (c *Config) validateMetricsServer() []error {
	m := c.Spec.Kubernetes.MetricsServer
	if m.Version == "" {
		return nil
	}
	if m.Enabled == nil || !*m.Enabled {
		return []error{fmt.Errorf("spec.kubernetes.metricsServer.version is set but metricsServer.enabled isn't true")}
	}
	if err := c.checkVersion("spec.kubernetes.metricsServer.version", MetricsServerName, m.Version); err != nil {
		return []error{err}
	}
	return nil
}

func (c *Config) validateAddons() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	const at = "spec.kubernetes.addons"
	a := c.Spec.Kubernetes.Addons

	seen := map[string]string{}
	for i, h := range a.HelmCharts {
		path := fmt.Sprintf("%s.helmCharts[%d]", at, i)
		if !releaseName.MatchString(h.Name) {
			add("%s.name %q must be a lowercase DNS label of at most 53 characters (the Helm release name)", path, h.Name)
		}
		if !dnsLabel.MatchString(h.Namespace) {
			add("%s.namespace %q must be a lowercase DNS label", path, h.Namespace)
		}
		if h.Namespace == "default" {
			add("%s.namespace: tow doesn't install into \"default\"; leave it out for a namespace named after the release, or name another", path)
		}
		id := h.Namespace + "/" + h.Name
		if prev, dup := seen[id]; dup {
			add("%s: release %s in namespace %s is already installed by %s", path, h.Name, h.Namespace, prev)
		}
		seen[id] = path
		if u, err := url.Parse(h.Repo); err != nil || (u.Scheme != "https" && u.Scheme != "oci") || u.Host == "" {
			add("%s.repo %q must be a chart repository, https:// or oci://", path, h.Repo)
		}
		if h.Chart == "" {
			add("%s.chart is required", path)
		}
		if !exactVersion.MatchString(h.Version) {
			add("%s.version %q must be an exact chart version like 1.19.1", path, h.Version)
		}
		for _, key := range sortedKeys(h.ValuesFromEnv) {
			switch {
			case !valuePath.MatchString(key):
				add("%s.valuesFromEnv: %q must be a value's path, like auth.password", path, key)
			case !envName.MatchString(h.ValuesFromEnv[key]):
				add("%s.valuesFromEnv: %s: %q must name an environment variable, like DB_PASSWORD", path, key, h.ValuesFromEnv[key])
			case hasValue(h.Values, key):
				add("%s.valuesFromEnv: %s is also set in values", path, key)
			}
		}
	}

	for i, m := range a.Manifests {
		at := fmt.Sprintf("%s.manifests[%d]", at, i)
		switch {
		case m.Path == "":
			add("%s is empty", at)
			continue
		case strings.Contains(m.Path, "://"):
			if u, err := url.Parse(m.Path); err != nil || u.Scheme != "https" || u.Host == "" {
				add("%s %q must be an https:// URL or a file next to the cluster file", at, m.Path)
			}
		default:
			if _, err := os.Stat(c.ManifestPath(m.Path)); err != nil {
				add("%s %s doesn't exist (local paths are relative to the cluster file)", at, m.Path)
			}
		}
		switch {
		case m.Namespace != "" && !dnsLabel.MatchString(m.Namespace):
			add("%s.namespace %q must be a lowercase DNS label", at, m.Namespace)
		case m.Namespace == "default":
			add("%s.namespace: tow doesn't install into \"default\"; name a namespace of its own", at)
		case m.Target() == "":
			add("%s: no namespace can be named after %q; write it as {path: %s, namespace: <name>}", at, m.Path, m.Path)
		}
	}
	return errs
}

// valuePath is a chart value's path as Helm's --set writes it: auth.password, ingress.hosts[0].
var valuePath = regexp.MustCompile(`^[A-Za-z0-9_-]+(\[\d+\])*(\.[A-Za-z0-9_-]+(\[\d+\])*)*$`)

// hasValue reports whether a plain dotted path is already set in a chart's values.
func hasValue(values map[string]any, path string) bool {
	var cur any = values
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		if cur, ok = m[part]; !ok {
			return false
		}
	}
	return true
}

// ManifestPath resolves a local manifest against the cluster file's folder.
func (c *Config) ManifestPath(m string) string {
	if filepath.IsAbs(m) || strings.Contains(m, "://") {
		return m
	}
	return filepath.Join(c.dir, m)
}
