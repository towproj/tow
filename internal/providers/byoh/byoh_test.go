package byoh

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
)

// fakeMachine answers the facts script with its facts and the peer script with a fixed verdict.
type fakeMachine struct {
	facts    facts
	peerWord string // what every peer probe reports: refused, open, timeout
	ran      []string
}

func (m *fakeMachine) Run(ctx context.Context, cmd string) (string, error) {
	m.ran = append(m.ran, cmd)
	var b strings.Builder
	if strings.Contains(cmd, "node-to-node check") {
		for _, line := range strings.Split(cmd, "\n") {
			var ip string
			var port int
			if n, _ := fmt.Sscanf(line, "probe %s %d", &ip, &port); n == 2 {
				fmt.Fprintf(&b, "%s:%d=%s\n", ip, port, m.peerWord)
			}
		}
		return b.String(), nil
	}
	for k, v := range m.facts {
		fmt.Fprintf(&b, "%s=%s\n", k, v)
	}
	return b.String(), nil
}

func (m *fakeMachine) Close() error { return nil }

func machine(host, ip string) *fakeMachine {
	f := goodFacts()
	f["hostname"], f["machine_id"], f["ip"] = "Node-"+host, "id-"+host, ip
	return &fakeMachine{facts: f, peerWord: "refused"}
}

// fakeProvider connects to machines by host; a missing host can't be reached.
func fakeProvider(machines map[string]*fakeMachine, apiReach error) *Provider {
	return &Provider{
		connect: func(ctx context.Context, n providers.Node, knownHosts string) (runner, error) {
			if m, ok := machines[n.Host]; ok {
				return m, nil
			}
			return nil, errors.New("dial tcp " + n.Host + ":22: connect: no route to host")
		},
		reach: func(ctx context.Context, addr string) error { return apiReach },
	}
}

func spec(t *testing.T, nodes ...providers.Node) providers.Spec {
	t.Setenv("HOME", t.TempDir()) // known_hosts lives under ~/.tow
	return providers.Spec{Name: "lab", Engine: config.DistributionKubeadm, CNI: "calico", Nodes: nodes}
}

func node(host, role string) providers.Node {
	return providers.Node{Host: host, Role: role, User: "ubuntu", Port: 22}
}

func TestProvisionHandsOverHealthyNodes(t *testing.T) {
	cp, w := machine("a", "10.0.0.1"), machine("b", "10.0.0.2")
	p := fakeProvider(map[string]*fakeMachine{"203.0.113.1": cp, "203.0.113.2": w}, nil)

	res, err := p.Provision(context.Background(), spec(t,
		node("203.0.113.1", config.RoleControlPlane), node("203.0.113.2", config.RoleWorker)))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Nodes) != 2 || res.Nodes[0].Name != "node-a" || res.Nodes[0].PrivateIP != "10.0.0.1" || res.Nodes[1].PrivateIP != "10.0.0.2" {
		t.Errorf("nodes should carry the lowercased hostname and node IP: %+v", res.Nodes)
	}
	if res.Endpoint != "203.0.113.1:6443" {
		t.Errorf("endpoint = %q, want the first control plane's API address", res.Endpoint)
	}
	// Node-to-node goes over the IPs Kubernetes will use, not the SSH addresses.
	if peers := w.ran[len(w.ran)-1]; !strings.Contains(peers, "probe 10.0.0.1 6443") || strings.Contains(peers, "203.0.113") {
		t.Errorf("worker probes:\n%s", peers)
	}
}

func TestProvisionReportsEveryProblemAtOnce(t *testing.T) {
	cp := machine("a", "10.0.0.1")
	clone := machine("b", "10.0.0.2")
	clone.facts["hostname"], clone.facts["machine_id"] = cp.facts["hostname"], cp.facts["machine_id"]
	clone.facts["mem_kb"] = "500000"
	clone.peerWord = "timeout"
	p := fakeProvider(map[string]*fakeMachine{"203.0.113.1": cp, "203.0.113.2": clone}, errors.New("i/o timeout"))

	_, err := p.Provision(context.Background(), spec(t,
		node("203.0.113.1", config.RoleControlPlane),
		node("203.0.113.2", config.RoleWorker),
		node("203.0.113.3", config.RoleWorker)))
	if err == nil {
		t.Fatal("expected problems")
	}
	for _, want := range []string{
		"changed nothing",
		"api port: this machine can't reach 203.0.113.1:6443",
		"memory: 488 MB",
		"same hostname as",
		"same machine ID as",
		"node-to-node: can't reach",
		"203.0.113.3, worker:\n  ✗ ssh: dial tcp 203.0.113.3:22",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q:\n%v", want, err)
		}
	}
}

func TestPlanReportsWhatWouldBeAdopted(t *testing.T) {
	p := fakeProvider(map[string]*fakeMachine{"203.0.113.1": machine("a", "10.0.0.1")}, nil)
	lines, err := p.Plan(context.Background(), spec(t, node("203.0.113.1", config.RoleControlPlane)))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"All checks passed; would adopt 1 machine(s), changing nothing on them:",
		"  203.0.113.1 → node node-a (10.0.0.1), control-plane",
		"API endpoint: 203.0.113.1:6443",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("got:\n%s", strings.Join(lines, "\n"))
	}
}

func TestStatusCountsReachableMachines(t *testing.T) {
	p := fakeProvider(map[string]*fakeMachine{"203.0.113.1": machine("a", "10.0.0.1")}, nil)
	res, err := p.Status(context.Background(), spec(t,
		node("203.0.113.1", config.RoleControlPlane), node("203.0.113.2", config.RoleWorker)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Healthy || res.ControlPlanes != "1/1" || res.Workers != "0/1" || !strings.Contains(res.Message, "203.0.113.2") {
		t.Errorf("status = %+v", res)
	}
}

func TestPreFlight(t *testing.T) {
	p := New()
	if err := p.PreFlight(context.Background(), providers.Spec{Name: "x", Engine: config.DistributionK3s}); err == nil {
		t.Error("no nodes should fail")
	}
	s := providers.Spec{Name: "x", Engine: config.DistributionK3s, Nodes: []providers.Node{{Host: "h", KeyPath: "/nonexistent/key"}}}
	if err := p.PreFlight(context.Background(), s); err == nil || !strings.Contains(err.Error(), "/nonexistent/key") {
		t.Errorf("missing key: %v", err)
	}
}
