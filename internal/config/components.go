package config

import (
	"fmt"
	"regexp"
)

// choice is one component the file can select, with the real limits on where it works.
type choice struct {
	distributions []string // empty = every distribution
	providers     []string // empty = every provider
	cni           string   // the CNI it is part of, if any
	why           string   // why distributions or providers are limited
}

// remote are the distributions that install onto machines.
var remote = []string{DistributionK3s, DistributionKubeadm}

// checkChoice refuses a selection the distribution, provider or CNI can't run.
func (c *Config) checkChoice(path, name string, catalog map[string]choice) error {
	ch, ok := catalog[name]
	if !ok {
		return fmt.Errorf("%s: %q is unknown (%s)", path, name, joinOr(sortedKeys(catalog)))
	}
	k := c.Spec.Kubernetes
	if len(ch.distributions) > 0 && !contains(ch.distributions, k.Distribution) {
		if k.Distribution == DistributionK3d {
			return fmt.Errorf("%s: %s isn't available on local clusters (they take %s)", path, name, joinOr(choicesFor(catalog, DistributionK3d)))
		}
		return fmt.Errorf("%s: %s needs %s (%s)", path, name, joinOr(ch.distributions), ch.why)
	}
	if len(ch.providers) > 0 && !contains(ch.providers, c.Provider()) {
		return fmt.Errorf("%s: %s works only on provider %s (%s)", path, name, joinOr(ch.providers), ch.why)
	}
	if ch.cni != "" && k.Networking.CNI != ch.cni {
		return fmt.Errorf("%s: %s needs cni: %s (it is part of %s)", path, name, ch.cni, ch.cni)
	}
	return nil
}

// choicesFor lists what a distribution can select from a catalog.
func choicesFor(catalog map[string]choice, distribution string) []string {
	var out []string
	for _, name := range sortedKeys(catalog) {
		if d := catalog[name].distributions; len(d) == 0 || contains(d, distribution) {
			out = append(out, name)
		}
	}
	return out
}

// optionBlock is a block of options named after a choice, like networking.cilium.
type optionBlock struct {
	name string
	set  bool
}

// checkBlocks refuses options for a component the file didn't select: they would do nothing.
func checkBlocks(path string, blocks []optionBlock, selected func(string) bool, what string) []error {
	var errs []error
	for _, b := range blocks {
		if b.set && !selected(b.name) {
			errs = append(errs, fmt.Errorf("%s.%s is set but %s", path, b.name, what))
		}
	}
	return errs
}

// bundledWithK3s are the components k3s ships; their version follows k3s's own.
var bundledWithK3s = []string{CNIFlannel, CSILocalPath, IngressTraefik, ServiceLBServiceLB, MetricsServerName}

// checkVersion checks a pinned add-on version: exact, and not for a component k3s bundles.
func (c *Config) checkVersion(path, component, version string) error {
	if version == "" {
		return nil
	}
	if contains(bundledWithK3s, component) && c.bundles() {
		return fmt.Errorf("%s: %s comes bundled with k3s, so its version follows k3s's (to pick your own, install it with spec.kubernetes.addons.helmCharts)", path, component)
	}
	if !exactVersion.MatchString(version) {
		return fmt.Errorf("%s %q must be an exact release like 1.18.4", path, version)
	}
	return nil
}

// bundles reports whether the distribution is k3s, which ships some components itself.
func (c *Config) bundles() bool {
	d := c.Spec.Kubernetes.Distribution
	return d == DistributionK3s || d == DistributionK3d
}

// exactVersion is a single release; ranges and "latest" would make drift meaningless.
var exactVersion = regexp.MustCompile(`^v?\d+\.\d+\.\d+([-+][0-9A-Za-z.+-]+)?$`)
