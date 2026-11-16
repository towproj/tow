package config

import (
	"cmp"
	"fmt"

	"github.com/towproj/tow/internal/runtimes"
)

// Containerd is the containerd a kubeadm cluster gets on machines that have none.
type Containerd struct {
	Version string `yaml:"version,omitempty"` // empty = runtimes.DefaultContainerd
}

// CRIO is the CRI-O a kubeadm cluster gets on machines that have none.
type CRIO struct {
	Version string `yaml:"version,omitempty"` // empty = the newest of the Kubernetes minor
}

// refusedRuntimes are runtimes people ask for that the kubelet can't use, and why.
var refusedRuntimes = map[string]string{
	"docker":  "Kubernetes dropped Docker Engine (dockershim) in 1.24: it now needs the cri-dockerd adapter. Docker itself runs on containerd, which tow installs",
	"podman":  "Podman has no CRI, so the kubelet can't use it; its Kubernetes counterpart is cri-o",
	"rkt":     "rkt was archived in 2019",
	"dockerd": "Kubernetes dropped Docker Engine (dockershim) in 1.24: it now needs the cri-dockerd adapter. Docker itself runs on containerd, which tow installs",
}

// validateRuntime checks the container runtime: kubeadm only (k3s and k3d bring their own
// containerd), a release that goes with the Kubernetes the file asks for. With "stable", the
// release is checked again at apply time, once it's known.
func (c *Config) validateRuntime() []error {
	var errs []error
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	const at = "spec.kubernetes"
	k := c.Spec.Kubernetes

	if k.Distribution != DistributionKubeadm {
		for _, f := range []struct {
			name string
			set  bool
		}{{"containerRuntime", k.ContainerRuntime != ""}, {runtimes.Containerd, k.Containerd != nil}, {runtimes.CRIO, k.CRIO != nil}} {
			if f.set {
				add("%s.%s is for kubeadm: %s brings its own containerd", at, f.name, k.Distribution)
			}
		}
		return errs
	}

	switch r := k.ContainerRuntime; {
	case r == runtimes.Containerd || r == runtimes.CRIO:
	case refusedRuntimes[r] != "":
		add("%s.containerRuntime %s can't run Kubernetes nodes: %s", at, r, refusedRuntimes[r])
	default:
		add("%s.containerRuntime %q must be %s or %s", at, r, runtimes.Containerd, runtimes.CRIO)
	}
	errs = append(errs, checkBlocks(at, []optionBlock{{runtimes.Containerd, k.Containerd != nil}, {runtimes.CRIO, k.CRIO != nil}},
		func(name string) bool { return name == k.ContainerRuntime }, "containerRuntime is "+k.ContainerRuntime)...)

	minor := runtimes.KubeMinor(k.Release()) // 0 with "stable"
	switch k.ContainerRuntime {
	case runtimes.Containerd:
		var asked string
		if k.Containerd != nil {
			asked = k.Containerd.Version
		}
		if asked != "" && !exactVersion.MatchString(asked) {
			add("%s.containerd.version %q must be an exact release like %s", at, asked, runtimes.DefaultContainerd)
			break
		}
		warn, err := runtimes.CheckContainerd(cmp.Or(asked, runtimes.DefaultContainerd), minor)
		switch {
		case err != nil && asked != "":
			add("%s.containerd.version: %v", at, err)
		case err != nil:
			add("%s.version %s: tow's containerd (%s) doesn't go with it: %v\n  → set %s.containerd.version to a release containerd recommends for it", at, k.Release(), runtimes.DefaultContainerd, err, at)
		case warn != "":
			c.warnings = append(c.warnings, warn)
		}
	case runtimes.CRIO:
		if k.CRIO == nil || k.CRIO.Version == "" {
			break
		}
		if !exactVersion.MatchString(k.CRIO.Version) {
			add("%s.cri-o.version %q must be an exact release like 1.37.2", at, k.CRIO.Version)
		} else if err := runtimes.CheckCRIO(k.CRIO.Version, minor); err != nil {
			add("%s.cri-o.version: %v", at, err)
		}
	}
	return errs
}
