// Package proxmox is the Proxmox VE provision target: its OpenTofu module (bpg/proxmox) plus
// the few Proxmox API calls OpenTofu doesn't make. It talks to the API over plain HTTPS, so
// it needs no SDK.
package proxmox

import (
	"context"
	"crypto/tls"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
	"github.com/towproj/tow/internal/ui"
)

// The same variables the OpenTofu provider reads, so one setting serves both.
const (
	EnvToken    = "PROXMOX_VE_API_TOKEN"
	EnvInsecure = "PROXMOX_VE_INSECURE"
)

// No "NAME=value" example here: output redaction would hide it as a secret.
const tokenHelp = `Create an API token in the Proxmox web UI (Datacenter → Permissions → API Tokens), then export ` + EnvToken + ` before running tow.
Its value is the token ID (like tow@pve!tow), an equals sign, then the secret Proxmox showed once.
Proxmox has no login command, so this variable is the standard way; never put the token in tow.yaml.`

// Hidden files (.terraform from local runs) are excluded by go:embed, except the lock file,
// named: it pins the provider release tow is tested with, and lets 'tofu init' reuse
// the shared provider cache without writing it again.
//
//go:embed module module/.terraform.lock.hcl
var module embed.FS

// Cloud reaches the Proxmox API at the request's URL with the token from the environment.
type Cloud struct {
	// HTTP is used instead of a client built from the environment when set (tests).
	HTTP *http.Client
}

func (Cloud) Module() fs.FS {
	sub, err := fs.Sub(module, "module")
	if err != nil {
		panic(err) // the directory is embedded at build time
	}
	return sub
}

func (Cloud) MachineType() string { return "proxmox_virtual_environment_vm" }

// AdminRules: none; each VM's firewall rules are one resource, changed in place.
func (Cloud) AdminRules() []string { return nil }

// Vars gives each VM its address, and returns where the VMs go and their network; their
// sizes come in the shared machines.
func (Cloud) Vars(_ context.Context, spec providers.Spec, machines []cloud.Machine, prev *cloud.Inputs) (map[string]any, error) {
	m := spec.Machines
	if m.Network == nil {
		return nil, errors.New("proxmox needs the VM network in the request")
	}
	prefix, err := netip.ParsePrefix(m.Network.Subnet)
	if err != nil {
		return nil, fmt.Errorf("subnet %q: %w", m.Network.Subnet, err)
	}
	if err := placeAddresses(machines, prev.Placed(), m.Network.Addresses); err != nil {
		return nil, err
	}
	vars := map[string]any{
		"proxmox_url":   m.URL,
		"template_id":   m.Template,
		"storage":       m.Storage,
		"bridge":        m.Bridge,
		"prefix_length": prefix.Bits(),
		"gateway":       m.Network.Gateway,
		"dns_servers":   m.Network.DNS,
		"api_endpoint":  spec.Endpoint,
		"pod_cidr":      spec.PodCIDR,
		// Never null: OpenTofu iterates over it.
		"service_lb_cidrs": append([]string{}, spec.ServiceLBCIDRs...),
	}
	if m.CPUType != "" {
		vars["cpu_type"] = m.CPUType // else the module's default
	}
	return vars, nil
}

// AdminCIDR is the address this machine uses to reach the VMs' network. On a LAN nothing
// translates it, so it is what the VMs see. Dialing UDP sends no packet: it only picks the route.
func (Cloud) AdminCIDR(ctx context.Context, m *providers.MachineRequest) (string, error) {
	if m.Network == nil {
		return "", errors.New("proxmox needs the VM network in the request")
	}
	first, _, err := providers.ParseRange(m.Network.Addresses)
	if err != nil {
		return "", err
	}
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp4", net.JoinHostPort(first.String(), "22"))
	if err != nil {
		return "", fmt.Errorf("find this machine's address on the VM network %s (SSH is opened to it only): %w", m.Network.Subnet, err)
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP.String() + "/32", nil
}

// Identity checks the token, the node and the template, and warns when the firewall rules
// tow creates would not be enforced.
func (c Cloud) Identity(ctx context.Context, m *providers.MachineRequest) (string, error) {
	api, err := c.client(m)
	if err != nil {
		return "", err
	}

	var version struct {
		Version string `json:"version"`
	}
	if err := api.get(ctx, "/version", &version); err != nil {
		return "", err
	}

	var nodes []struct {
		Node   string `json:"node"`
		Status string `json:"status"`
	}
	if err := api.get(ctx, "/nodes", &nodes); err != nil {
		return "", err
	}
	var names []string
	found := false
	for _, n := range nodes {
		names = append(names, n.Node)
		if n.Node == m.Region {
			found = true
			if n.Status != "online" {
				return "", fmt.Errorf("proxmox node %s is %s; start it, or pick another provision.node", n.Node, n.Status)
			}
		}
	}
	if !found {
		return "", cloud.Permanent(fmt.Errorf("proxmox has no node %q (nodes: %s); fix provision.node", m.Region, strings.Join(names, ", ")))
	}

	var tmpl struct {
		Template int `json:"template"`
	}
	path := fmt.Sprintf("/nodes/%s/qemu/%d/config", url.PathEscape(m.Region), m.Template)
	if err := api.get(ctx, path, &tmpl); err != nil {
		return "", fmt.Errorf("template VM %d on node %s: %w", m.Template, m.Region, err)
	}
	if tmpl.Template != 1 {
		return "", fmt.Errorf("VM %d on node %s is not a template; convert it with 'qm template %d' or set provision.template to a template's ID", m.Template, m.Region, m.Template)
	}

	api.warnIfFirewallOff(ctx)
	return fmt.Sprintf("Proxmox VE %s at %s (node %s) as %s", version.Version, api.base.Host, m.Region, api.tokenID), nil
}

// warnIfFirewallOff: the VM rules tow writes only apply while the Datacenter firewall is on.
func (a *apiClient) warnIfFirewallOff(ctx context.Context) {
	var opts struct {
		Enable int `json:"enable"`
	}
	if err := a.get(ctx, "/cluster/firewall/options", &opts); err != nil {
		ui.Debug("could not read the Datacenter firewall setting: %v", err)
		return
	}
	if opts.Enable != 1 {
		ui.Warn("The Proxmox Datacenter firewall is off, so the VM firewall rules tow creates (SSH, the API and NodePorts open to this machine only) are not enforced. " +
			"Turn it on under Datacenter → Firewall → Options; Proxmox keeps its web UI and SSH reachable from its local network.")
	}
}

type vm struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Node     string `json:"node"`
	Status   string `json:"status"`
	Tags     string `json:"tags"`
	Template int    `json:"template"`
}

// tagged lists the VMs carrying the cluster's tag, on every node.
func (c Cloud) tagged(ctx context.Context, m *providers.MachineRequest, cluster string) ([]vm, error) {
	api, err := c.client(m)
	if err != nil {
		return nil, err
	}
	var all []vm
	if err := api.get(ctx, "/cluster/resources?type=vm", &all); err != nil {
		return nil, err
	}
	want := cloud.TagCluster + "." + cluster
	var out []vm
	for _, v := range all {
		if v.Template != 1 && hasTag(v.Tags, want) {
			out = append(out, v)
		}
	}
	return out, nil
}

func (c Cloud) Instances(ctx context.Context, m *providers.MachineRequest, cluster string) ([]cloud.Instance, error) {
	vms, err := c.tagged(ctx, m, cluster)
	if err != nil {
		return nil, err
	}
	out := make([]cloud.Instance, 0, len(vms))
	for _, v := range vms {
		inst := cloud.Instance{ID: strconv.Itoa(v.VMID), State: v.Status}
		for _, t := range splitTags(v.Tags) {
			if role, ok := strings.CutPrefix(t, cloud.TagRole+"."); ok {
				inst.Role = role
			}
		}
		out = append(out, inst)
	}
	return out, nil
}

// Leftovers: a VM's disks, cloud-init drive and firewall rules go with it, so VMs are all
// there is to check.
func (c Cloud) Leftovers(ctx context.Context, m *providers.MachineRequest, cluster string) ([]string, error) {
	vms, err := c.tagged(ctx, m, cluster)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, v := range vms {
		out = append(out, fmt.Sprintf("VM %d (%s) on node %s, tagged %s.%s", v.VMID, v.Name, v.Node, cloud.TagCluster, cluster))
	}
	return out, nil
}

func splitTags(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
}

func hasTag(tags, want string) bool {
	for _, t := range splitTags(tags) {
		if t == want {
			return true
		}
	}
	return false
}

// apiClient is one authenticated connection to the Proxmox API.
type apiClient struct {
	http    *http.Client
	base    *url.URL
	auth    string
	tokenID string // user@realm!name: safe to show, unlike the secret after '='
}

var warnInsecure sync.Once

func (c Cloud) client(m *providers.MachineRequest) (*apiClient, error) {
	token := strings.TrimSpace(os.Getenv(EnvToken))
	if token == "" {
		return nil, cloud.Permanent(fmt.Errorf("%s is not set.\n%s", EnvToken, tokenHelp))
	}
	id, secret, ok := strings.Cut(token, "=")
	if !ok || !strings.Contains(id, "!") || secret == "" {
		return nil, cloud.Permanent(fmt.Errorf("%s doesn't look like an API token (token ID with a '!', then '=', then the secret).\n%s", EnvToken, tokenHelp))
	}
	ui.RegisterSecret(secret)

	base, err := url.Parse(strings.TrimRight(m.URL, "/") + "/api2/json")
	if err != nil {
		return nil, cloud.Permanent(fmt.Errorf("provision.url %q: %w", m.URL, err))
	}

	hc := c.HTTP
	if hc == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		if insecure() {
			warnInsecure.Do(func() {
				ui.Warn(EnvInsecure + " is set: the Proxmox server's certificate is not checked. Fine for a lab; for anything else, give Proxmox a trusted certificate and unset it.")
			})
			tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // the user opted in, and is warned
		}
		hc = &http.Client{Transport: tr, Timeout: 30 * time.Second}
	}
	return &apiClient{http: hc, base: base, auth: "PVEAPIToken=" + token, tokenID: id}, nil
}

func insecure() bool {
	v, _ := strconv.ParseBool(os.Getenv(EnvInsecure))
	return v
}

// PowerOff stops the cluster's VM called name at once, as a power cut would, and waits until
// Proxmox reports it stopped; PowerOn starts it and waits until it runs. Tests take a control
// plane away with them. The token needs VM.PowerMgmt (PVEAdmin has it).
func (c Cloud) PowerOff(ctx context.Context, m *providers.MachineRequest, cluster, name string) error {
	return c.power(ctx, m, cluster, name, "stop", "stopped")
}

func (c Cloud) PowerOn(ctx context.Context, m *providers.MachineRequest, cluster, name string) error {
	return c.power(ctx, m, cluster, name, "start", "running")
}

func (c Cloud) power(ctx context.Context, m *providers.MachineRequest, cluster, name, action, want string) error {
	vms, err := c.tagged(ctx, m, cluster)
	if err != nil {
		return err
	}
	var found []vm
	for _, v := range vms {
		if v.Name == name {
			found = append(found, v)
		}
	}
	if len(found) != 1 {
		return fmt.Errorf("cluster %s has %d VMs called %s", cluster, len(found), name)
	}
	api, err := c.client(m)
	if err != nil {
		return err
	}
	base := fmt.Sprintf("/nodes/%s/qemu/%d/status", url.PathEscape(found[0].Node), found[0].VMID)
	var task string
	if err := api.do(ctx, http.MethodPost, base+"/"+action, &task); err != nil {
		return err
	}
	for {
		var status struct {
			Status string `json:"status"`
		}
		if err := api.get(ctx, base+"/current", &status); err != nil {
			return err
		}
		if status.Status == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("VM %s is %s, not %s yet: %w", name, status.Status, want, ctx.Err())
		case <-time.After(2 * time.Second):
		}
	}
}

// get reads one API path into out (the response's "data" field).
func (a *apiClient) get(ctx context.Context, path string, out any) error {
	return a.do(ctx, http.MethodGet, path, out)
}

// do calls one API path with method, and reads the response's "data" field into out.
func (a *apiClient) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, a.base.String()+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", a.auth)

	resp, err := a.http.Do(req)
	if err != nil {
		var cert *tls.CertificateVerificationError
		if errors.As(err, &cert) {
			return fmt.Errorf("the Proxmox certificate at %s is not trusted: %w\nA fresh Proxmox install uses a self-signed certificate. For a lab, export %s=true; otherwise give Proxmox a trusted certificate", a.base.Host, err, EnvInsecure)
		}
		return fmt.Errorf("can't reach the Proxmox API at %s (is the server up?): %w", a.base.Host, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return cloud.Permanent(fmt.Errorf("proxmox refused the API token %s (401): check %s, or create a new token.\n%s", a.tokenID, EnvToken, tokenHelp))
	case resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("token %s may not use %s (403): give it the PVEAdmin role on / (Datacenter → Permissions), or the permissions listed in docs/testing/proxmox.md", a.tokenID, path)
	case resp.StatusCode != http.StatusOK:
		// Proxmox puts the reason in the status line, e.g. "500 Configuration file ... does not exist".
		return fmt.Errorf("proxmox API %s: %s", path, strings.TrimSpace(resp.Status))
	}

	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("proxmox API %s: unexpected answer: %w", path, err)
	}
	return json.Unmarshal(envelope.Data, out)
}
