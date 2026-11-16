package proxmox

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
)

func TestIPConfigAddress(t *testing.T) {
	for in, want := range map[string]string{
		"ip=192.168.100.20/24,gw=192.168.100.1": "192.168.100.20",
		"gw=10.0.0.1, ip=10.0.0.5/16":           "10.0.0.5",
		"ip=dhcp":                               "",
		"":                                      "",
	} {
		if got := ipconfigAddress(in); got != want {
			t.Errorf("%q → %q, want %q", in, got, want)
		}
	}
}

// On loopback every address refuses a closed port, which is how a live machine answers.
func TestBeforeCreateFindsTakenAddresses(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs Linux, where all of 127.0.0.0/8 is loopback")
	}
	answers := map[string]string{}
	for k, v := range lab {
		answers[k] = v
	}
	answers["/api2/json/nodes/pve/qemu/101/config"] = `{"ipconfig0":"ip=127.0.0.20/8,gw=127.0.0.1"}`
	answers["/api2/json/nodes/pve/qemu/102/config"] = `{"ipconfig0":"ip=127.0.0.21/8,gw=127.0.0.1"}`
	srv, c := fakeAPI(t, answers)
	probePorts = []string{"1"} // nothing listens there
	t.Cleanup(func() { probePorts = []string{"22", "443"} })

	m := request(srv.URL) // 1 control plane + 2 workers
	m.Network = &providers.StaticNetwork{Subnet: "127.0.0.0/8", Addresses: "127.0.0.20-127.0.0.29", Gateway: "127.0.0.1", DNS: []string{"127.0.0.53"}}

	// A new cluster: every address and the virtual IP are checked.
	err := c.BeforeCreate(context.Background(), providers.Spec{Name: "fresh", Endpoint: "127.0.0.30:6443", Machines: m}, placed(t, m))
	if err == nil {
		t.Fatal("taken addresses must be refused")
	}
	for _, want := range []string{"127.0.0.20 (a VM address", "127.0.0.22 (a VM address", "127.0.0.30 (controlPlaneEndpoint"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}

	// Retrying cluster "demo": its VMs hold .20 and .21, so only .22 is checked, not the virtual IP.
	err = c.BeforeCreate(context.Background(), providers.Spec{Name: "demo", Endpoint: "127.0.0.30:6443", Machines: m}, placed(t, m))
	if err == nil || !strings.Contains(err.Error(), "127.0.0.22") || strings.Contains(err.Error(), "127.0.0.20 ") ||
		strings.Contains(err.Error(), "127.0.0.21") || strings.Contains(err.Error(), "127.0.0.30") {
		t.Errorf("err = %v, want only 127.0.0.22", err)
	}
}

// Silence means free. TEST-NET-1 (RFC 5737) is never assigned to anything.
func TestBeforeCreateAcceptsSilentAddresses(t *testing.T) {
	srv, c := fakeAPI(t, lab)
	probeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { probeTimeout = 1500 * time.Millisecond })

	m := request(srv.URL)
	m.Network = &providers.StaticNetwork{Subnet: "192.0.2.0/24", Addresses: "192.0.2.20-192.0.2.29", Gateway: "192.0.2.1", DNS: []string{"192.0.2.1"}}
	if err := c.BeforeCreate(context.Background(), providers.Spec{Name: "fresh", Machines: m}, placed(t, m)); err != nil {
		t.Errorf("unused addresses refused: %v", err)
	}
}

// placed is a new cluster's machines with their addresses.
func placed(t *testing.T, m *providers.MachineRequest) []cloud.Machine {
	t.Helper()
	machines := cloud.Machines("fresh", m.Pools)
	if err := placeAddresses(machines, nil, m.Network.Addresses); err != nil {
		t.Fatal(err)
	}
	return machines
}

// The Proxmox CSI plugin needs Proxmox in a cluster: refused before anything is created.
func TestBeforeCreateNeedsClusterForCSI(t *testing.T) {
	probeTimeout = 200 * time.Millisecond
	t.Cleanup(func() { probeTimeout = 1500 * time.Millisecond })
	answers := map[string]string{}
	for k, v := range lab {
		answers[k] = v
	}
	answers["/api2/json/cluster/status"] = `[{"type":"node","name":"pve"}]`
	srv, c := fakeAPI(t, answers)
	m := request(srv.URL)
	m.Network = &providers.StaticNetwork{Subnet: "192.0.2.0/24", Addresses: "192.0.2.20-192.0.2.29", Gateway: "192.0.2.1", DNS: []string{"192.0.2.1"}}
	spec := providers.Spec{Name: "fresh", Machines: m, Storage: []string{"proxmox"}}
	if err := c.BeforeCreate(context.Background(), spec, placed(t, m)); err == nil || !strings.Contains(err.Error(), "pvecm create") {
		t.Errorf("err = %v", err)
	}
	answers["/api2/json/cluster/status"] = `[{"type":"cluster","name":"lab"},{"type":"node","name":"pve"}]`
	srv, c = fakeAPI(t, answers)
	m.URL = srv.URL
	if err := c.BeforeCreate(context.Background(), spec, placed(t, m)); err != nil {
		t.Errorf("clustered: %v", err)
	}
}
