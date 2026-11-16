package config

import (
	"maps"
	"path/filepath"
	"slices"
	"testing"
)

// What state keeps of a file reads back as the same config, for every example.
func TestRestoreReadsBackTheEffectiveConfig(t *testing.T) {
	files, _ := filepath.Glob("../../examples/*.yaml")
	for _, f := range files {
		c, err := Load(f)
		if err != nil {
			t.Fatal(err)
		}
		back, err := Restore(c.Effective())
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if back.Mode != c.Mode || !maps.Equal(back.Fields(), c.Fields()) {
			t.Errorf("%s reads back as another config", f)
		}
	}
}

const base = header + `spec:
  flavor: homelab
  infrastructure:
    provider: byoh
    access: {ssh: {user: pi}}
  controlPlane:
    machines: [{host: 10.0.0.1}]
`

func TestHashIgnoresFormatting(t *testing.T) {
	// Same meaning, different comments, spacing, key order and flow-vs-block style.
	reformatted := `
# my homelab
kind: Cluster
metadata:
  name: x
apiVersion: towproj.github.io/v1alpha1
spec:
  controlPlane:
    machines:
      - host: 10.0.0.1   # the Pi
  infrastructure:
    access:
      ssh:
        user: pi
    provider: byoh
  flavor: homelab
`
	if a, b := mustParse(t, base).Hash(), mustParse(t, reformatted).Hash(); a != b {
		t.Errorf("formatting changed the hash:\n%s\n%s", a, b)
	}
}

func TestHashSeesDefaults(t *testing.T) {
	// Writing a default explicitly is not a change.
	explicit := base + "  kubernetes: {distribution: k3s, networking: {cni: flannel}, storage: {csi: [local-path]}}\n"
	if mustParse(t, base).Hash() != mustParse(t, explicit).Hash() {
		t.Error("spelling out defaults changed the hash")
	}
	// The version is the exception: left out, it follows the release tow tests, so a newer
	// tow isn't drift; written, it's pinned.
	if mustParse(t, base).Hash() == mustParse(t, base+"  kubernetes: {version: "+DefaultKubernetesVersion+"}\n").Hash() {
		t.Error("a pinned version must count for drift")
	}
}

func TestDiffFields(t *testing.T) {
	old := mustParse(t, base)
	changed := mustParse(t, base+`  kubernetes: {version: v1.35.0}
  workers:
    - machines: [{host: 10.0.0.2}]
`)

	got := DiffFields(old.Fields(), changed.Fields())
	want := []string{
		"spec.kubernetes.version: (none) → v1.35.0",
		"spec.workers[workers].machines[10.0.0.2].host: (none) → 10.0.0.2",
		"spec.workers[workers].machines[10.0.0.2].ssh.port: (none) → 22",
		"spec.workers[workers].machines[10.0.0.2].ssh.user: (none) → pi",
		"spec.workers[workers].name: (none) → workers",
	}
	if !slices.Equal(got, want) {
		t.Errorf("diff =\n%q\nwant\n%q", got, want)
	}
	if old.Hash() == changed.Hash() {
		t.Error("hash did not change")
	}
	if d := DiffFields(old.Fields(), old.Fields()); len(d) != 0 {
		t.Errorf("identical configs produced a diff: %v", d)
	}
}

// Every architecture choice counts for drift, including ones that came from defaults.
func TestFieldsCoverArchitecture(t *testing.T) {
	f := mustParse(t, base).Fields()
	for key, want := range map[string]string{
		"spec.kubernetes.distribution":                  "k3s",
		"spec.kubernetes.networking.cni":                "flannel",
		"spec.kubernetes.networking.podCIDR":            "10.42.0.0/16",
		"spec.kubernetes.storage.csi":                   "local-path",
		"spec.kubernetes.storage.defaultClass":          "local-path",
		"spec.kubernetes.ingress.controllers":           "traefik",
		"spec.kubernetes.serviceLoadBalancer.provider":  "servicelb",
		"spec.kubernetes.metricsServer.enabled":         "true",
		"spec.kubernetes.kubeconfig.context":            "x",
		"spec.controlPlane.schedulable":                 "true",
		"spec.infrastructure.provider":                  "byoh",
		"spec.controlPlane.machines[10.0.0.1].ssh.port": "22",
		"metadata.name":                                 "x",
	} {
		if f[key] != want {
			t.Errorf("%s = %q, want %q", key, f[key], want)
		}
	}
}

// Lists of named things are matched by name, and sets by content: reordering either is no
// change. Lists whose order matters keep their positions.
func TestFieldsKeyLists(t *testing.T) {
	file := func(pools, cidrs, dns string) string {
		return header + `spec:
  infrastructure:
    provider: proxmox
    proxmox: {url: "https://192.168.1.10:8006", node: pve, template: 9000}
    network: {subnet: 192.168.1.0/24, addresses: 192.168.1.20-192.168.1.29, gateway: 192.168.1.1, dns: ` + dns + `}
    access: {adminCIDRs: ` + cidrs + `}
  kubernetes: {distribution: k3s}
  workers:
` + pools
	}
	a := `    - {name: general, count: 2, taints: [{key: a, effect: NoSchedule}, {key: b, value: x, effect: NoExecute}]}
    - {name: gpu, count: 1}
`
	b := `    - {name: gpu, count: 1}
    - {name: general, count: 2, taints: [{key: b, value: x, effect: NoExecute}, {key: a, effect: NoSchedule}]}
`
	first := mustParse(t, file(a, "[10.0.0.0/8, 192.168.1.5/32]", "[1.1.1.1, 9.9.9.9]"))
	if d := DiffFields(first.Fields(), mustParse(t, file(b, "[192.168.1.5/32, 10.0.0.0/8]", "[1.1.1.1, 9.9.9.9]")).Fields()); len(d) != 0 {
		t.Errorf("reordered pools, taints and CIDRs changed: %v", d)
	}
	f := first.Fields()
	for key, want := range map[string]string{
		"spec.workers[gpu].count":                         "1",
		"spec.workers[general].taints[b:NoExecute].value": "x",
		"spec.infrastructure.access.adminCIDRs":           "10.0.0.0/8, 192.168.1.5/32",
		"spec.infrastructure.network.dns[1]":              "9.9.9.9",
	} {
		if f[key] != want {
			t.Errorf("%s = %q, want %q", key, f[key], want)
		}
	}
	if d := DiffFields(f, mustParse(t, file(a, "[10.0.0.0/8, 192.168.1.5/32]", "[9.9.9.9, 1.1.1.1]")).Fields()); len(d) != 2 {
		t.Errorf("DNS servers are tried in order, so that order counts: %v", d)
	}
	if got := Generic("spec.workers[general].taints[b:NoExecute].value"); got != "spec.workers[].taints[].value" {
		t.Errorf("Generic = %s", got)
	}
}
