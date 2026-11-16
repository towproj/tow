package config

import (
	"strings"
	"testing"
)

func TestRuntimeDefaults(t *testing.T) {
	k := mustParse(t, header+"spec: {kubernetes: {distribution: kubeadm}, "+onBYOH+"}\n").Spec.Kubernetes
	if k.ContainerRuntime != "containerd" || k.Containerd != nil || k.Version != "" || k.Release() != DefaultKubernetesVersion {
		t.Errorf("kubeadm defaults: runtime %q, containerd %+v, version %q", k.ContainerRuntime, k.Containerd, k.Version)
	}
	// k3s brings its own containerd: nothing to choose.
	if k := mustParse(t, header+"spec: {kubernetes: {distribution: k3s}, "+onBYOH+"}\n").Spec.Kubernetes; k.ContainerRuntime != "" {
		t.Errorf("k3s runtime = %q", k.ContainerRuntime)
	}
}

func TestRuntimeAccepted(t *testing.T) {
	for _, spec := range []string{
		"{distribution: kubeadm, containerRuntime: cri-o}",
		"{distribution: kubeadm, version: v1.37.0, containerRuntime: cri-o, cri-o: {version: 1.37.2}}",
		"{distribution: kubeadm, version: v1.36.4, containerd: {version: 2.2.10}}",
		"{distribution: kubeadm, version: v1.36.4}", // the default containerd goes with 1.36 too
		"{distribution: kubeadm, version: stable, containerd: {version: 2.0.0}}",
	} {
		mustParse(t, header+"spec: {kubernetes: "+spec+", "+onBYOH+"}\n")
	}
	// A Kubernetes containerd's table doesn't list yet: any 2.x, with a warning.
	c := mustParse(t, header+"spec: {kubernetes: {distribution: kubeadm, version: v1.38.0}, "+onBYOH+"}\n")
	if !strings.Contains(strings.Join(c.Warnings(), "\n"), "doesn't list Kubernetes 1.38 yet") {
		t.Errorf("warnings = %q", c.Warnings())
	}
}

func TestRuntimeRefusals(t *testing.T) {
	on := func(k string) string { return "spec: {kubernetes: " + k + ", " + onBYOH + "}" }
	expectErrors(t, on("{distribution: k3s, containerRuntime: containerd, containerd: {version: 2.3.7}}"),
		"spec.kubernetes.containerRuntime is for kubeadm: k3s brings its own containerd",
		"spec.kubernetes.containerd is for kubeadm")
	expectErrors(t, "spec: {kubernetes: {cri-o: {}}, infrastructure: {provider: local}}", "spec.kubernetes.cri-o is for kubeadm: k3d brings its own containerd")
	expectErrors(t, on("{distribution: kubeadm, containerRuntime: docker}"), "containerRuntime docker can't run Kubernetes nodes", "cri-dockerd")
	expectErrors(t, on("{distribution: kubeadm, containerRuntime: podman}"), "Podman has no CRI", "cri-o")
	expectErrors(t, on("{distribution: kubeadm, containerRuntime: rkt}"), "rkt was archived in 2019")
	expectErrors(t, on("{distribution: kubeadm, containerRuntime: gvisor}"), `containerRuntime "gvisor" must be containerd or cri-o`)
	expectErrors(t, on("{distribution: kubeadm, cri-o: {version: 1.37.2}}"), "spec.kubernetes.cri-o is set but containerRuntime is containerd")

	// containerd: 2.x on a line containerd recommends for the Kubernetes minor.
	expectErrors(t, on("{distribution: kubeadm, containerd: {version: 1.7.28}}"), "spec.kubernetes.containerd.version: containerd 1.7.28 is a 1.x release")
	expectErrors(t, on("{distribution: kubeadm, containerd: {version: 2.2.10}}"),
		"spec.kubernetes.containerd.version: containerd 2.2.10 isn't one containerd recommends for Kubernetes 1.37 (2.4.0+ or 2.3.0+")
	expectErrors(t, on("{distribution: kubeadm, containerd: {version: latest}}"), `containerd.version "latest" must be an exact release like 2.3.7`)
	// The default containerd, with a Kubernetes it isn't recommended for: say what to set.
	expectErrors(t, on("{distribution: kubeadm, version: v1.34.2}"),
		"spec.kubernetes.version v1.34.2: tow's containerd (2.3.7) doesn't go with it", "set spec.kubernetes.containerd.version")

	// CRI-O follows Kubernetes' minor.
	expectErrors(t, on("{distribution: kubeadm, containerRuntime: cri-o, cri-o: {version: 1.36.4}}"),
		"spec.kubernetes.cri-o.version: CRI-O 1.36.4 doesn't go with Kubernetes 1.37", "must be a 1.37 release")
	expectErrors(t, on("{distribution: kubeadm, version: banana}"), `spec.kubernetes.version "banana" must be a release like v1.37.2, or "stable"`)
}
