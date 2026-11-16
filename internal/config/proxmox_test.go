package config

import (
	"strings"
	"testing"
)

const proxmoxBase = header + `spec:
  kubernetes: {distribution: kubeadm}
  infrastructure:
    provider: proxmox
    proxmox: {url: "https://192.168.100.10:8006", node: pve, template: 9000}
    network: {subnet: 192.168.100.0/24, addresses: 192.168.100.20-192.168.100.29, gateway: 192.168.100.1, dns: [1.1.1.1]}
`

func TestProxmoxDefaults(t *testing.T) {
	c := mustParse(t, proxmoxBase+"  workers: [{count: 1}]\n")
	p := c.Spec.Infrastructure.Proxmox
	if p.Storage != DefaultProxmoxStorage || p.Bridge != DefaultProxmoxBridge {
		t.Errorf("defaults not applied: storage=%s bridge=%s", p.Storage, p.Bridge)
	}
	for _, g := range c.groups() {
		if *g.Resources != DefaultProxmoxResources || g.Disk != DefaultProxmoxDisk {
			t.Errorf("group size defaults: %+v %s", g.Resources, g.Disk)
		}
	}
	if c.Location() != "pve" || c.Spec.Infrastructure.Access.SSH.User != DefaultSSHUser {
		t.Errorf("location=%s user=%s", c.Location(), c.Spec.Infrastructure.Access.SSH.User)
	}
}

func TestProxmoxErrors(t *testing.T) {
	tests := []struct {
		name    string
		replace [2]string // applied to proxmoxBase
		extra   string    // appended under spec
		want    []string
	}{
		{name: "missing basics", replace: [2]string{`proxmox: {url: "https://192.168.100.10:8006", node: pve, template: 9000}`, "proxmox: {}"},
			want: []string{"proxmox.url is required", "proxmox.node is required", "proxmox.template is required"}},
		{name: "no proxmox block", replace: [2]string{`    proxmox: {url: "https://192.168.100.10:8006", node: pve, template: 9000}` + "\n", ""},
			want: []string{"spec.infrastructure.proxmox is required"}},
		{name: "plain http", replace: [2]string{"https://192.168.100.10:8006", "http://192.168.100.10:8006"},
			want: []string{"must be the Proxmox API address"}},
		{name: "no network", replace: [2]string{"    network: {subnet: 192.168.100.0/24, addresses: 192.168.100.20-192.168.100.29, gateway: 192.168.100.1, dns: [1.1.1.1]}\n", ""},
			want: []string{"spec.infrastructure.network is required on proxmox"}},
		{name: "range outside subnet", replace: [2]string{"192.168.100.20-192.168.100.29", "192.168.101.20-192.168.101.29"},
			want: []string{"must be inside subnet 192.168.100.0/24"}},
		{name: "range too small", replace: [2]string{"192.168.100.20-192.168.100.29", "192.168.100.20-192.168.100.20"}, extra: "  workers: [{count: 1}]\n",
			want: []string{"holds 1 addresses, but 2 VMs need one each"}},
		{name: "not a range", replace: [2]string{"192.168.100.20-192.168.100.29", "192.168.100.20"},
			want: []string{"is not a range"}},
		{name: "gateway in range", replace: [2]string{"gateway: 192.168.100.1", "gateway: 192.168.100.21"},
			want: []string{"gateway 192.168.100.21 is inside the VM address range"}},
		{name: "no dns", replace: [2]string{"dns: [1.1.1.1]", "dns: []"},
			want: []string{"at least one DNS server"}},
		{name: "too small for kubeadm", extra: "  controlPlane: {resources: {cpu: 1, memory: 1Gi}, disk: 5Gi}\n  workers: [{count: 1, resources: {memory: 512Mi}}]\n",
			want: []string{"spec.controlPlane.resources.cpu is 1; a kubeadm control plane needs at least 2",
				"spec.controlPlane.resources.memory is 1024Mi; a kubeadm control plane needs at least 2048Mi",
				"spec.controlPlane.disk is 5Gi", "spec.workers[0].resources.memory is 512Mi; a kubeadm worker needs at least 2048Mi"}},
		{name: "bad memory", extra: "  controlPlane: {resources: {memory: 2GB}}\n", want: []string{`"2GB" is not a size`}},
		{name: "HA needs an endpoint", extra: "  controlPlane: {count: 3}\n",
			want: []string{"spec.kubernetes.apiEndpoint is required with 3 control planes"}},
		{name: "endpoint inside the range", replace: [2]string{"kubernetes: {distribution: kubeadm}", `kubernetes: {distribution: kubeadm, apiEndpoint: "192.168.100.25:6443"}`},
			want: []string{"apiEndpoint 192.168.100.25 is inside spec.infrastructure.network.addresses"}},
		{name: "endpoint off the VMs' network", replace: [2]string{"kubernetes: {distribution: kubeadm}", `kubernetes: {distribution: kubeadm, apiEndpoint: "192.168.1.100:6443"}`},
			want: []string{"apiEndpoint 192.168.1.100 isn't in spec.infrastructure.network.subnet 192.168.100.0/24: kube-vip announces it on the VMs' network"}},
		{name: "ranges overlap the subnet", replace: [2]string{"kubernetes: {distribution: kubeadm}", "kubernetes: {distribution: kubeadm, networking: {podCIDR: 192.168.0.0/16}}"},
			want: []string{"podCIDR 192.168.0.0/16 overlaps spec.infrastructure.network.subnet"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			y := proxmoxBase
			if tt.replace[0] != "" {
				if !strings.Contains(y, tt.replace[0]) {
					t.Fatalf("test bug: %q not in base", tt.replace[0])
				}
				y = strings.Replace(y, tt.replace[0], tt.replace[1], 1)
			}
			_, err := Parse([]byte(y + tt.extra))
			if err == nil {
				t.Fatalf("expected an error for:\n%s", y+tt.extra)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error missing %q:\n%v", w, err)
				}
			}
		})
	}
}

// k3s accepts smaller machines than kubeadm: a 1 CPU / 1Gi worker is fine.
func TestProxmoxMinimumsFollowDistribution(t *testing.T) {
	y := strings.Replace(proxmoxBase, "distribution: kubeadm", "distribution: k3s", 1)
	mustParse(t, y+"  workers: [{count: 1, resources: {cpu: 1, memory: 1Gi}, disk: 10Gi}]\n")
}
