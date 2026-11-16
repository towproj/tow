package config

import (
	"strings"
	"testing"
)

// spec.infrastructure.os: Ubuntu by default on aws, AlmaLinux on request with its own login
// user; refused where tow doesn't pick the system.
func TestOS(t *testing.T) {
	parse := func(spec string) (*Config, error) {
		return Parse([]byte("apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: demo}\nspec: " + spec + "\n"))
	}
	c, err := parse("{kubernetes: {distribution: k3s}, infrastructure: {provider: aws, aws: {region: eu-west-3}}}")
	if err != nil {
		t.Fatal(err)
	}
	if in := c.Spec.Infrastructure; in.OS != OSUbuntu2404 || in.Access.SSH.User != "ubuntu" {
		t.Errorf("default: os %q, user %q", in.OS, in.Access.SSH.User)
	}
	c, err = parse("{kubernetes: {distribution: k3s}, infrastructure: {provider: aws, os: almalinux-10, aws: {region: eu-west-3}}}")
	if err != nil {
		t.Fatal(err)
	}
	if in := c.Spec.Infrastructure; in.Access.SSH.User != "ec2-user" || !RHELFamily(in.OS) {
		t.Errorf("almalinux: user %q", in.Access.SSH.User)
	}
	if _, err := parse("{kubernetes: {distribution: kubeadm}, infrastructure: {provider: aws, os: almalinux-9, aws: {region: eu-west-3}}}"); err != nil {
		t.Errorf("kubeadm on AlmaLinux: %v", err)
	}
	c, err = parse("{kubernetes: {distribution: k3s}, infrastructure: {provider: aws, os: almalinux-9, aws: {region: eu-west-3}, access: {ssh: {user: admin}}}}")
	if err != nil || c.Spec.Infrastructure.Access.SSH.User != "admin" {
		t.Errorf("the file's user wins: %v, %+v", err, c)
	}

	c, err = parse("{kubernetes: {distribution: kubeadm}, infrastructure: {provider: azure, os: almalinux-9, azure: {subscription: 00000000-0000-0000-0000-000000000000, location: westeurope}}}")
	if err != nil || c.Spec.Infrastructure.Access.SSH.User != "almalinux" {
		t.Errorf("azure almalinux: %v, %+v", err, c)
	}

	// GCP: the same systems; the platform creates the user the key names, after the system.
	const gcp = "gcp: {project: my-project, region: europe-west1}"
	for os, user := range map[string]string{"": "ubuntu", "os: almalinux-9, ": "almalinux", "os: almalinux-10, ": "almalinux"} {
		c, err = parse("{kubernetes: {distribution: kubeadm}, infrastructure: {provider: gcp, " + os + gcp + "}}")
		if err != nil {
			t.Fatalf("gcp %q: %v", os, err)
		}
		if in := c.Spec.Infrastructure; in.Access.SSH.User != user || in.OS == "" {
			t.Errorf("gcp %q: os %q, user %q", os, in.OS, in.Access.SSH.User)
		}
	}

	for _, tt := range []struct{ spec, want string }{
		{"{kubernetes: {distribution: k3s}, infrastructure: {provider: aws, os: rocky-9, aws: {region: eu-west-3}}}", `spec.infrastructure.os "rocky-9" is not offered (almalinux-10, almalinux-9 or ubuntu-24.04)`},
		{"{kubernetes: {distribution: k3s}, infrastructure: {provider: byoh, os: ubuntu-24.04, access: {ssh: {user: u}}}, controlPlane: {machines: [{host: 10.0.0.1}]}}", "spec.infrastructure.os isn't used with byoh"},
		{"{flavor: local, infrastructure: {provider: local, os: ubuntu-24.04}}", "spec.infrastructure.os isn't used by local clusters"},
		{`{kubernetes: {distribution: k3s}, infrastructure: {provider: proxmox, os: almalinux-9, proxmox: {url: "https://192.168.100.10:8006", node: pve, template: 9000},
		  network: {subnet: 192.168.100.0/24, addresses: 192.168.100.20-192.168.100.29, gateway: 192.168.100.1, dns: [1.1.1.1]}}}`, "spec.infrastructure.os isn't used on proxmox: the VMs run their template's system"},
	} {
		if _, err := parse(tt.spec); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("%s: %v, want %q", tt.spec, err, tt.want)
		}
	}
}

// spec.infrastructure.proxmox.cpuType: Proxmox's own default for new VMs, or the file's.
func TestProxmoxCPUType(t *testing.T) {
	file := func(cpu string) []byte {
		return []byte(`apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata: {name: demo}
spec:
  kubernetes: {distribution: k3s}
  infrastructure:
    provider: proxmox
    proxmox: {url: "https://192.168.100.10:8006", node: pve, template: 9001` + cpu + `}
    network: {subnet: 192.168.100.0/24, addresses: 192.168.100.20-192.168.100.29, gateway: 192.168.100.1, dns: [1.1.1.1]}
`)
	}
	c, err := Parse(file(""))
	if err != nil || c.Spec.Infrastructure.Proxmox.CPUType != DefaultProxmoxCPUType {
		t.Errorf("default: %v %+v", err, c)
	}
	if c, err := Parse(file(", cpuType: x86-64-v3")); err != nil || c.Spec.Infrastructure.Proxmox.CPUType != "x86-64-v3" {
		t.Errorf("x86-64-v3: %v", err)
	}
	if _, err := Parse(file(`, cpuType: "x86 64; reboot"`)); err == nil || !strings.Contains(err.Error(), "must be a Proxmox CPU model") {
		t.Errorf("bad cpuType: %v", err)
	}
}
