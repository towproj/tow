package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
)

const testToken = "tow@pve!terraform=0b7e2a54-1111-2222-3333-444455556666" // gitleaks:allow

// fakeAPI answers like Proxmox VE: JSON wrapped in {"data": ...}, token auth required.
func fakeAPI(t *testing.T, answers map[string]string) (*httptest.Server, Cloud) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "PVEAPIToken="+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		body, ok := answers[r.URL.RequestURI()]
		if !ok {
			http.Error(w, "Configuration file does not exist", http.StatusInternalServerError)
			return
		}
		w.Write([]byte(`{"data":` + body + `}`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv(EnvToken, testToken)
	return srv, Cloud{HTTP: srv.Client()}
}

var lab = map[string]string{
	"/api2/json/version":                    `{"version":"9.2","release":"9.2"}`,
	"/api2/json/nodes":                      `[{"node":"pve","status":"online"}]`,
	"/api2/json/nodes/pve/qemu/9000/config": `{"name":"ubuntu-2404-template","template":1}`,
	"/api2/json/cluster/firewall/options":   `{"enable":1}`,
	"/api2/json/cluster/resources?type=vm": `[
		{"vmid":9000,"name":"ubuntu-2404-template","node":"pve","status":"stopped","template":1,"tags":"tow-cluster.demo"},
		{"vmid":101,"name":"demo-cp-0","node":"pve","status":"running","tags":"tow;tow-cluster.demo;tow-role.control-plane"},
		{"vmid":102,"name":"demo-worker-0","node":"pve","status":"stopped","tags":"tow;tow-cluster.demo;tow-role.worker"},
		{"vmid":103,"name":"demo-2-cp-0","node":"pve","status":"running","tags":"tow;tow-cluster.demo-2;tow-role.control-plane"},
		{"vmid":104,"name":"unrelated","node":"pve","status":"running"}
	]`,
}

func request(url string) *providers.MachineRequest {
	return &providers.MachineRequest{
		Cloud: "proxmox", Region: "pve", URL: url, Template: 9000, Pools: []providers.Pool{
			{Name: "control-plane", Role: "control-plane", Count: 1, CPUs: 2, MemoryMB: 2048, DiskGB: 20},
			{Name: "workers", Role: "worker", Count: 2, CPUs: 2, MemoryMB: 2048, DiskGB: 20},
		},
		Storage: "local-lvm", Bridge: "vmbr0",
		Network: &providers.StaticNetwork{Subnet: "192.168.100.0/24", Addresses: "192.168.100.20-192.168.100.29", Gateway: "192.168.100.1", DNS: []string{"1.1.1.1"}},
	}
}

func TestIdentity(t *testing.T) {
	srv, c := fakeAPI(t, lab)
	id, err := c.Identity(context.Background(), request(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(id, "Proxmox VE 9.2") || !strings.Contains(id, "tow@pve!terraform") || !strings.Contains(id, "node pve") {
		t.Errorf("identity = %q", id)
	}
	if strings.Contains(id, "0b7e2a54") {
		t.Error("the token secret must never be shown")
	}
}

func TestIdentityProblems(t *testing.T) {
	answers := func(override map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range lab {
			out[k] = v
		}
		for k, v := range override {
			if v == "" {
				delete(out, k)
			} else {
				out[k] = v
			}
		}
		return out
	}
	cases := []struct {
		name      string
		token     string // "" keeps the valid one
		answers   map[string]string
		want      string
		permanent bool // retrying in the same process can't help
	}{
		{"no token", "-", lab, EnvToken + " is not set", true},
		{"malformed token", "just-a-secret", lab, "doesn't look like an API token", true},
		{"wrong token", "tow@pve!terraform=wrong", lab, "refused the API token tow@pve!terraform (401)", true},
		{"unknown node", "", answers(map[string]string{"/api2/json/nodes": `[{"node":"pve2","status":"online"}]`}), `no node "pve" (nodes: pve2)`, true},
		{"node offline", "", answers(map[string]string{"/api2/json/nodes": `[{"node":"pve","status":"offline"}]`}), "node pve is offline", false},
		{"no template", "", answers(map[string]string{"/api2/json/nodes/pve/qemu/9000/config": ""}), "template VM 9000 on node pve", false},
		{"not a template", "", answers(map[string]string{"/api2/json/nodes/pve/qemu/9000/config": `{"template":0}`}), "is not a template", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, c := fakeAPI(t, tc.answers)
			switch tc.token {
			case "":
			case "-":
				t.Setenv(EnvToken, "")
			default:
				t.Setenv(EnvToken, tc.token)
			}
			_, err := c.Identity(context.Background(), request(srv.URL))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
			if cloud.IsPermanent(err) != tc.permanent {
				t.Errorf("permanent = %v, want %v", cloud.IsPermanent(err), tc.permanent)
			}
			if strings.Contains(err.Error(), "0b7e2a54") || strings.Contains(err.Error(), "=wrong") {
				t.Error("the token secret must never be shown")
			}
		})
	}
}

func TestUntrustedCertificate(t *testing.T) {
	srv, _ := fakeAPI(t, lab)
	t.Setenv(EnvInsecure, "")
	_, err := Cloud{}.Identity(context.Background(), request(srv.URL)) // default client: verifies TLS
	if err == nil || !strings.Contains(err.Error(), EnvInsecure+"=true") {
		t.Errorf("err = %v, want the self-signed certificate hint", err)
	}

	t.Setenv(EnvInsecure, "true")
	if _, err := (Cloud{}).Identity(context.Background(), request(srv.URL)); err != nil {
		t.Errorf("with %s=true the lab certificate is accepted: %v", EnvInsecure, err)
	}
}

// Only the cluster's own VMs count: not a cluster whose name starts the same, not templates.
func TestInstancesAndLeftovers(t *testing.T) {
	srv, c := fakeAPI(t, lab)
	m := request(srv.URL)

	insts, err := c.Instances(context.Background(), m, "demo")
	if err != nil {
		t.Fatal(err)
	}
	want := []cloud.Instance{{ID: "101", Role: "control-plane", State: "running"}, {ID: "102", Role: "worker", State: "stopped"}}
	if !slices.Equal(insts, want) {
		t.Errorf("instances = %+v, want %+v", insts, want)
	}

	left, err := c.Leftovers(context.Background(), m, "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 2 || !strings.Contains(left[0], "VM 101 (demo-cp-0) on node pve") {
		t.Errorf("leftovers = %v", left)
	}

	if left, _ := c.Leftovers(context.Background(), m, "gone"); len(left) != 0 {
		t.Errorf("a deleted cluster has no leftovers, got %v", left)
	}
}

// PowerOff and PowerOn find the cluster's VM by name, ask Proxmox to stop or start it, and
// wait for the state they asked for.
func TestPower(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	state, polls := "running", 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Header.Get("Authorization") != "PVEAPIToken="+testToken {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch uri := r.URL.RequestURI(); {
		case uri == "/api2/json/cluster/resources?type=vm":
			w.Write([]byte(`{"data":` + lab[uri] + `}`))
		case r.Method == http.MethodPost && strings.HasPrefix(uri, "/api2/json/nodes/pve/qemu/101/status/"):
			calls = append(calls, strings.TrimPrefix(uri, "/api2/json/nodes/pve/qemu/101/status/"))
			polls = 0
			w.Write([]byte(`{"data":"UPID:pve:0001:qmstop:101:root@pam:"}`))
		case r.Method == http.MethodGet && uri == "/api2/json/nodes/pve/qemu/101/status/current":
			// The state changes on the second poll, as a real VM takes a moment.
			if polls++; polls == 2 {
				state = map[string]string{"stop": "stopped", "start": "running"}[calls[len(calls)-1]]
			}
			w.Write([]byte(`{"data":{"status":"` + state + `"}}`))
		default:
			http.Error(w, "unexpected "+r.Method+" "+uri, http.StatusNotImplemented)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv(EnvToken, testToken)
	c := Cloud{HTTP: srv.Client()}
	m := request(srv.URL)

	if err := c.PowerOff(context.Background(), m, "demo", "demo-cp-0"); err != nil {
		t.Fatal(err)
	}
	if state != "stopped" {
		t.Errorf("state after PowerOff = %s", state)
	}
	if err := c.PowerOn(context.Background(), m, "demo", "demo-cp-0"); err != nil {
		t.Fatal(err)
	}
	if state != "running" || !slices.Equal(calls, []string{"stop", "start"}) {
		t.Errorf("state %s, calls %v", state, calls)
	}
	// Another cluster's VM, or no such VM: nothing is touched.
	if err := c.PowerOff(context.Background(), m, "demo", "demo-2-cp-0"); err == nil || !strings.Contains(err.Error(), "cluster demo has 0 VMs called demo-2-cp-0") {
		t.Errorf("err = %v", err)
	}
	if len(calls) != 2 {
		t.Errorf("calls %v", calls)
	}
}

func TestVars(t *testing.T) {
	m := request("https://192.168.100.10:8006")
	machines := cloud.Machines("demo", m.Pools)
	vars, err := Cloud{}.Vars(context.Background(), providers.Spec{Name: "demo", Endpoint: "192.168.100.30:6443", Machines: m}, machines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cidrs, ok := vars["service_lb_cidrs"].([]string); !ok || cidrs == nil || len(cidrs) != 0 {
		t.Errorf("no pool: an empty list, never null: %#v", vars["service_lb_cidrs"])
	}
	lb, _ := Cloud{}.Vars(context.Background(), providers.Spec{Name: "demo", Machines: m, ServiceLBCIDRs: []string{"192.168.100.200/29"}}, cloud.Machines("demo", m.Pools), nil)
	if !slices.Equal(lb["service_lb_cidrs"].([]string), []string{"192.168.100.200/29"}) {
		t.Errorf("pool: %v", lb["service_lb_cidrs"])
	}
	if got := addressesOf(machines); !slices.Equal(got, []string{"192.168.100.20", "192.168.100.21", "192.168.100.22"}) {
		t.Errorf("addresses = %v", got)
	}
	if vars["prefix_length"] != 24 || vars["api_endpoint"] != "192.168.100.30:6443" || vars["template_id"] != 9000 || vars["storage"] != "local-lvm" {
		t.Errorf("vars = %v", vars)
	}
	// The CPU model: the module's default unless the request names one.
	if _, set := vars["cpu_type"]; set {
		t.Errorf("cpu_type = %v without a request", vars["cpu_type"])
	}
	m.CPUType = "x86-64-v3"
	if v3, _ := (Cloud{}).Vars(context.Background(), providers.Spec{Name: "demo", Machines: m}, cloud.Machines("demo", m.Pools), nil); v3["cpu_type"] != "x86-64-v3" {
		t.Errorf("cpu_type = %v", v3["cpu_type"])
	}
	for k := range vars {
		if strings.Contains(k, "token") {
			t.Errorf("credentials never become module inputs: %s", k)
		}
	}
}

func addressesOf(machines []cloud.Machine) []string {
	var out []string
	for _, m := range machines {
		out = append(out, m.Address)
	}
	return out
}

func pools(cps, workers int) []providers.Pool {
	return []providers.Pool{
		{Name: "control-plane", Role: "control-plane", Count: cps},
		{Name: "workers", Role: "worker", Count: workers},
	}
}

// A new cluster gets the range in order, control planes first. After that a VM keeps its
// address whatever comes or goes, and a new VM gets the lowest one free.
func TestPlaceAddresses(t *testing.T) {
	const rng = "10.0.0.254-10.0.1.3" // six addresses, over a byte boundary
	place := func(machines []cloud.Machine, last []cloud.Machine) ([]string, error) {
		err := placeAddresses(machines, (&cloud.Inputs{Machines: last}).Placed(), rng)
		return addressesOf(machines), err
	}

	first := cloud.Machines("demo", pools(1, 3))
	got, err := place(first, nil)
	if want := []string{"10.0.0.254", "10.0.0.255", "10.0.1.0", "10.0.1.1"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("new cluster: %v, %v; want %v", got, err, want)
	}

	// One more control plane: the workers keep their addresses, which numbering them all in
	// one list didn't do; the new control plane gets the next free one.
	grown := cloud.Machines("demo", pools(2, 3))
	got, _ = place(grown, first)
	if want := []string{"10.0.0.254", "10.0.1.2", "10.0.0.255", "10.0.1.0", "10.0.1.1"}; !slices.Equal(got, want) {
		t.Errorf("one more control plane: %v, want %v", got, want)
	}

	// A worker removed frees its address for the next new VM.
	shrunk := cloud.Machines("demo", pools(1, 2))
	place(shrunk, first)
	regrown := cloud.Machines("demo", pools(2, 2))
	if got, _ := place(regrown, shrunk); !slices.Equal(got, []string{"10.0.0.254", "10.0.1.1", "10.0.0.255", "10.0.1.0"}) {
		t.Errorf("after a removal: %v", got)
	}

	// An address outside the range (a failed first apply whose range was changed) is given anew.
	moved := cloud.Machines("demo", pools(1, 1))
	if got, _ := place(moved, []cloud.Machine{{Name: "demo-cp-0", Address: "192.168.1.5"}, {Name: "demo-workers-0", Address: "10.0.0.254"}}); !slices.Equal(got, []string{"10.0.0.255", "10.0.0.254"}) {
		t.Errorf("out of range: %v", got)
	}

	if _, err := place(cloud.Machines("demo", pools(3, 4)), nil); err == nil || !strings.Contains(err.Error(), "holds 6 addresses, 7 machines need one each") {
		t.Errorf("err = %v, want the range to be too small", err)
	}
}

func TestAdminCIDR(t *testing.T) {
	m := request("https://127.0.0.1:8006")
	m.Network = &providers.StaticNetwork{Subnet: "127.0.0.0/8", Addresses: "127.0.0.20-127.0.0.29", Gateway: "127.0.0.1"}
	cidr, err := Cloud{}.AdminCIDR(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cidr, "127.") || !strings.HasSuffix(cidr, "/32") {
		t.Errorf("admin CIDR = %s, want this machine's address towards the VMs", cidr)
	}
}

func TestModuleEmbedded(t *testing.T) {
	for _, f := range []string{"main.tf", "variables.tf", "outputs.tf", "versions.tf"} {
		if _, err := (Cloud{}).Module().Open(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
