package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
)

// Ports knocked on to tell whether an address is in use, and how long to wait. Vars so tests
// can point them at a local listener.
var (
	probePorts   = []string{"22", "443"}
	probeTimeout = 1500 * time.Millisecond
)

// placeAddresses gives each VM its address from the range (first-last): the one it has, or
// else the lowest one no other VM of the cluster holds. A VM never moves when others come or
// go; a new cluster gets the range in order, control planes first.
func placeAddresses(machines []cloud.Machine, placed map[string]cloud.Machine, addresses string) error {
	first, last, err := providers.ParseRange(addresses)
	if err != nil {
		return err
	}
	held := map[netip.Addr]bool{}
	for i, m := range machines {
		a, err := netip.ParseAddr(placed[m.Name].Address)
		if err != nil || a.Less(first) || last.Less(a) || held[a] {
			continue // a new VM, or the range changed under it
		}
		machines[i].Address, held[a] = a.String(), true
	}
	next := first
	for i := range machines {
		if machines[i].Address != "" {
			continue
		}
		for held[next] {
			next = next.Next()
		}
		if !next.IsValid() || last.Less(next) {
			return fmt.Errorf("range %s holds %d addresses, %d machines need one each", addresses, providers.RangeSize(first, last), len(machines))
		}
		machines[i].Address, held[next] = next.String(), true
	}
	return nil
}

// clustered checks what the Proxmox CSI plugin requires: "Proxmox nodes must be clustered";
// one node can be clustered with itself (its README).
func (a *apiClient) clustered(ctx context.Context) error {
	var status []struct {
		Type string `json:"type"`
	}
	if err := a.get(ctx, "/cluster/status", &status); err != nil {
		return err
	}
	clustered := false
	for _, s := range status {
		clustered = clustered || s.Type == "cluster"
	}
	if !clustered {
		return fmt.Errorf("the Proxmox CSI plugin (storage.csi proxmox) needs Proxmox in a cluster, and this server isn't in one; nothing was created.\n  → on the Proxmox server, as root: pvecm create <a cluster name> (one server can be a cluster of its own), then apply again")
	}
	return nil
}

// BeforeCreate refuses addresses something on the network already answers on: a VM given an
// address that is taken causes an IP conflict that is hard to trace. Addresses this cluster's
// own VMs hold (a retried apply, a running cluster) are left out. The virtual IP is checked
// only for a new cluster, since kube-vip holds it once Kubernetes runs.
func (c Cloud) BeforeCreate(ctx context.Context, spec providers.Spec, machines []cloud.Machine) error {
	m := spec.Machines
	var ips []string
	for _, mc := range machines {
		ips = append(ips, mc.Address)
	}

	api, err := c.client(m)
	if err != nil {
		return err
	}
	if slices.Contains(spec.Storage, "proxmox") {
		if err := api.clustered(ctx); err != nil {
			return err
		}
	}
	own, err := c.tagged(ctx, m, spec.Name)
	if err != nil {
		return err
	}
	held := map[string]bool{}
	for _, v := range own {
		var cfg struct {
			IPConfig0 string `json:"ipconfig0"`
		}
		path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(v.Node), v.VMID)
		if err := api.get(ctx, path, &cfg); err != nil {
			return fmt.Errorf("read the address of VM %d (%s): %w", v.VMID, v.Name, err)
		}
		if ip := ipconfigAddress(cfg.IPConfig0); ip != "" {
			held[ip] = true
		}
	}

	type target struct{ addr, what string }
	var targets []target
	for _, ip := range ips {
		if !held[ip] {
			targets = append(targets, target{ip, "a VM address from provision.network.addresses"})
		}
	}
	if len(own) == 0 && spec.Endpoint != "" {
		if host, _, err := net.SplitHostPort(spec.Endpoint); err == nil {
			if _, err := netip.ParseAddr(host); err == nil {
				targets = append(targets, target{host, "controlPlaneEndpoint, the API's virtual IP"})
			}
		}
	}

	taken := make([]string, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if port, ok := answers(ctx, t.addr); ok {
				taken[i] = fmt.Sprintf("%s (%s) already answers on port %s", t.addr, t.what, port)
			}
		}()
	}
	wg.Wait()

	var lines []string
	for _, l := range taken {
		if l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > 0 {
		return fmt.Errorf("these addresses are already in use on the network:\n  - %s\nPick free addresses: change provision.network.addresses (or controlPlaneEndpoint), or turn off what holds them",
			strings.Join(lines, "\n  - "))
	}
	return nil
}

// answers reports whether some machine holds addr: it accepts a connection, or refuses one,
// which only a live machine does. Silence or "no route" means nothing is there. A machine
// that drops everything goes unnoticed; that is the limit of checking from outside.
func answers(ctx context.Context, addr string) (string, bool) {
	for _, port := range probePorts {
		d := net.Dialer{Timeout: probeTimeout}
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(addr, port))
		if err == nil {
			conn.Close()
			return port, true
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return port, true
		}
	}
	return "", false
}

// ipconfigAddress reads the IPv4 address out of a Proxmox ipconfig value,
// e.g. "ip=192.168.100.20/24,gw=192.168.100.1".
func ipconfigAddress(v string) string {
	for _, part := range strings.Split(v, ",") {
		if ip, ok := strings.CutPrefix(strings.TrimSpace(part), "ip="); ok {
			if p, err := netip.ParsePrefix(ip); err == nil {
				return p.Addr().String()
			}
		}
	}
	return ""
}
