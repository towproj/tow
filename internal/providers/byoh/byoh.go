package byoh

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/parallel"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

const (
	checkTimeout = 2 * time.Minute // per node: the internet check alone may take 10s per site
	apiPort      = 6443
)

// Provider uses machines the user already has ("bring your own host"), reached over SSH.
// In Phase 1 it only checks them: it reads, never changes anything. Preparing the machines
// (packages, sysctl, swap) is the start of Phase 2.
type Provider struct {
	connect func(ctx context.Context, n providers.Node, knownHosts string) (runner, error)
	// reach reports whether a TCP port on a node answers from this machine, open or refused.
	reach func(ctx context.Context, addr string) error
}

// runner is the slice of an SSH client the checks use, so tests can fake machines.
type runner interface {
	Run(ctx context.Context, cmd string) (string, error)
	Close() error
}

func New() providers.Provider { return &Provider{connect: dial, reach: reachTCP} }

// reachTCP succeeds when the port accepts or actively refuses a connection: either way the
// packets got through. Before Phase 2 nothing listens, so refused is the normal answer.
func reachTCP(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: 5 * time.Second}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err == nil {
		c.Close()
		return nil
	}
	// Windows reports WSAECONNREFUSED, which errors.Is doesn't map; its message still says "refused".
	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "refused") {
		return nil
	}
	return err
}

func dial(ctx context.Context, n providers.Node, knownHosts string) (runner, error) {
	return remote.Dial(ctx, target(n), knownHosts)
}

func target(n providers.Node) remote.Target {
	return remote.Target{Host: n.Host, Port: n.Port, User: n.User, KeyPath: n.KeyPath}
}

func knownHostsPath(name string) string { return filepath.Join(state.Dir(name), "known_hosts") }

func (p *Provider) Name() string { return "byoh" }

// PreFlight fails before any machine is contacted when there are no nodes or no SSH key.
func (p *Provider) PreFlight(ctx context.Context, spec providers.Spec) error {
	if len(spec.Nodes) == 0 {
		return fmt.Errorf("cluster '%s' lists no nodes to use", spec.Name)
	}
	if _, err := requirementsFor(spec.Engine, config.RoleWorker, spec.CNI); err != nil {
		return err
	}
	checked := map[string]bool{}
	for _, n := range spec.Nodes {
		if checked[n.KeyPath] {
			continue
		}
		checked[n.KeyPath] = true
		if err := remote.CheckKeys(n.KeyPath); err != nil {
			return err
		}
	}
	return nil
}

// Plan runs the real checks: they only read, so a dry run can safely tell the user whether
// their machines are ready. Only saving the cluster is left out.
// Host keys seen during a dry run go to a throwaway copy of the pinned ones: a dry run writes
// nothing under ~/.tow.
func (p *Provider) Plan(ctx context.Context, spec providers.Spec) ([]string, error) {
	tmp, err := os.CreateTemp("", "tow-known-hosts-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if pinned, err := os.ReadFile(knownHostsPath(spec.Name)); err == nil {
		tmp.Write(pinned)
	}
	tmp.Close()

	res, err := p.check(ctx, spec, tmp.Name())
	if err != nil {
		return nil, err
	}
	lines := []string{fmt.Sprintf("All checks passed; would adopt %d machine(s), changing nothing on them:", len(res.Nodes))}
	for _, n := range res.Nodes {
		lines = append(lines, fmt.Sprintf("  %s → node %s (%s), %s", n.Host, n.Name, n.PrivateIP, n.Role))
	}
	return append(lines, "API endpoint: "+res.Endpoint), nil
}

// nodeCheck is one node's progress through the checks.
type nodeCheck struct {
	node     providers.Node
	label    string
	client   runner
	facts    facts
	findings []finding
}

// Provision checks every node and reports all problems at once, so the user fixes their
// machines in one round. It changes nothing on them. On success the nodes come back with
// the hostname and IP Kubernetes will use, for Phase 2.
func (p *Provider) Provision(ctx context.Context, spec providers.Spec) (*providers.Result, error) {
	return p.check(ctx, spec, knownHostsPath(spec.Name))
}

// check runs every check, pinning host keys in knownHosts.
func (p *Provider) check(ctx context.Context, spec providers.Spec, knownHosts string) (*providers.Result, error) {
	checks := make([]*nodeCheck, len(spec.Nodes))
	for i, n := range spec.Nodes {
		checks[i] = &nodeCheck{node: n, label: n.Host}
	}
	defer func() {
		for _, c := range checks {
			if c.client != nil {
				c.client.Close()
			}
		}
	}()

	// Round 1, in parallel: connect and check each machine on its own.
	parallel.Each(checks, func(c *nodeCheck) {
		ctx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		c.findings = p.checkNode(ctx, c, spec, knownHosts)
		// kubectl on this machine will talk to the API server, so its port must get through.
		if c.node.Role == config.RoleControlPlane && c.client != nil {
			addr := net.JoinHostPort(c.node.Host, fmt.Sprint(apiPort))
			switch err, fw := p.reach(ctx, addr), c.facts.hostFirewall(); {
			case err == nil:
			case fw != "":
				// Preparation opens it in the machine's firewall, to this machine.
				c.findings = append(c.findings, finding{check: "api port", warn: true,
					problem: fmt.Sprintf("%s blocks %s from this machine for now; preparing the machines opens it in %s", fw, addr, fw), hint: firewallHint(fw)})
			default:
				c.findings = append(c.findings, fail("api port", fmt.Sprintf("this machine can't reach %s: %v", addr, err),
					fmt.Sprintf("allow TCP %d from this machine in the node's firewall; kubectl will need it", apiPort)))
			}
		}
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	views := make([]nodeView, len(checks))
	for i, c := range checks {
		views[i] = nodeView{label: c.label, role: c.node.Role}
		if c.facts != nil {
			views[i].ip, views[i].hostname, views[i].machineID = c.facts["ip"], c.facts["hostname"], c.facts["machine_id"]
			views[i].firewall = c.facts.hostFirewall()
		}
	}
	firewallOn := map[string]string{}
	for _, v := range views {
		firewallOn[v.ip] = v.firewall
	}

	// Round 2, now that every node's IP is known: can they reach each other?
	parallel.Each(checks, func(c *nodeCheck) {
		if c.client == nil || c.facts == nil || c.facts["probe_tools"] != "yes" {
			if c.client != nil && c.facts != nil {
				c.findings = append(c.findings, finding{check: "node-to-node", problem: "bash or timeout is missing, so node-to-node traffic could not be tested", warn: true})
			}
			return
		}
		i := indexOf(checks, c)
		peers := peersFor(i, views, spec.Engine, spec.CNI)
		if len(peers) == 0 {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, checkTimeout)
		defer cancel()
		out, err := c.client.Run(ctx, peerScript(peers))
		if err != nil && out == "" {
			c.findings = append(c.findings, fail("node-to-node", "check did not run: "+err.Error(), ""))
			return
		}
		c.findings = append(c.findings, evaluatePeers(out, peers, firewallOn)...)
	})
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for i, f := range evaluateCluster(views) {
		checks[i].findings = append(checks[i].findings, f...)
	}

	if err := report(checks); err != nil {
		return nil, err
	}
	if note := cniUDPNote[spec.CNI]; note != "" && len(checks) > 1 {
		ui.Hint(fmt.Sprintf("Not testable without installing anything: make sure %s is allowed between the nodes", note))
	}
	return result(spec, checks), nil
}

// checkNode connects to one node and runs the single-machine checks.
func (p *Provider) checkNode(ctx context.Context, c *nodeCheck, spec providers.Spec, knownHosts string) []finding {
	req, err := requirementsFor(spec.Engine, c.node.Role, spec.CNI)
	if err != nil {
		return []finding{fail("role", err.Error(), "")}
	}
	client, err := p.connect(ctx, c.node, knownHosts)
	if err != nil {
		return []finding{sshFinding(c.node, err)}
	}
	c.client = client

	out, err := client.Run(ctx, factsScript(req.internet))
	f := parseFacts(out)
	if f["kernel"] == "" {
		msg := "checks did not run"
		if err != nil {
			msg += ": " + err.Error()
		}
		return []finding{fail("ssh", msg, strings.TrimSpace(out))}
	}
	c.facts = f
	if h := f["hostname"]; h != "" {
		c.label = fmt.Sprintf("%s (%s)", c.node.Host, h)
	}
	ui.Debug("%s: %s %s, %s CPU(s), %s kB RAM, ip %s", c.label, f["os_name"], f["arch"], f["cpus"], f["mem_kb"], f["ip"])
	findings := evaluate(f, req)
	for i := range findings {
		findings[i].hint = strings.ReplaceAll(findings[i].hint, "<user>", c.node.User)
	}
	return findings
}

// sshFinding turns a connection error into something the user can act on.
func sshFinding(n providers.Node, err error) finding {
	msg := err.Error()
	hint := ""
	switch {
	case errors.Is(err, remote.ErrHostKeyChanged):
		hint = "" // the error already says what to do
	case strings.Contains(msg, "unable to authenticate"):
		hint = fmt.Sprintf("is your public key in ~%s/.ssh/authorized_keys on the node, and is ssh.user right? try: ssh -p %d %s@%s", n.User, n.Port, n.User, n.Host)
	case strings.Contains(msg, "refused"), strings.Contains(msg, "timeout"), strings.Contains(msg, "no route"):
		hint = fmt.Sprintf("is the machine on, and is SSH listening on port %d and allowed through its firewall?", n.Port)
	}
	return fail("ssh", msg, hint)
}

func indexOf(checks []*nodeCheck, c *nodeCheck) int {
	for i, x := range checks {
		if x == c {
			return i
		}
	}
	return -1
}

// report prints warnings and returns every failure, grouped by node, as one error.
func report(checks []*nodeCheck) error {
	var b strings.Builder
	problems, warned := 0, false
	for _, c := range checks {
		var fails, warns []finding
		for _, f := range c.findings {
			if f.warn {
				warns = append(warns, f)
			} else {
				fails = append(fails, f)
			}
		}
		for _, w := range warns {
			if !warned {
				fmt.Println()
				warned = true
			}
			fmt.Printf("! %s: %s: %s\n", c.label, w.check, w.problem)
		}
		if len(fails) == 0 {
			continue
		}
		problems += len(fails)
		fmt.Fprintf(&b, "\n%s, %s:", c.label, c.node.Role)
		for _, f := range fails {
			fmt.Fprintf(&b, "\n  ✗ %s: %s", f.check, f.problem)
			if f.hint != "" {
				fmt.Fprintf(&b, "\n    → %s", f.hint)
			}
		}
	}
	if problems == 0 {
		return nil
	}
	return fmt.Errorf("%d problem(s) on your machines (tow changed nothing on them):%s\nFix them and run 'tow apply' again.", problems, b.String())
}

// result hands the nodes to Phase 2 with the names and addresses Kubernetes will use.
func result(spec providers.Spec, checks []*nodeCheck) *providers.Result {
	res := &providers.Result{Endpoint: spec.Endpoint}
	for _, c := range checks {
		n := c.node
		n.Name = strings.ToLower(c.facts["hostname"])
		n.PrivateIP = c.facts["ip"]
		res.Nodes = append(res.Nodes, n)
		if res.Endpoint == "" && n.Role == config.RoleControlPlane {
			res.Endpoint = net.JoinHostPort(n.Host, fmt.Sprint(apiPort))
		}
	}
	return res
}

// Status reports how many of the user's machines still answer over SSH.
func (p *Provider) Status(ctx context.Context, spec providers.Spec) (providers.StatusResult, error) {
	up := make([]bool, len(spec.Nodes))
	var wg sync.WaitGroup
	for i, n := range spec.Nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.connect(ctx, n, knownHostsPath(spec.Name))
			if err != nil {
				ui.Debug("%s: %v", n.Host, err)
				return
			}
			c.Close()
			up[i] = true
		}()
	}
	wg.Wait()

	var cps, cpsUp, workers, workersUp int
	var down []string
	for i, n := range spec.Nodes {
		isCP := n.Role == config.RoleControlPlane
		if isCP {
			cps++
		} else {
			workers++
		}
		switch {
		case !up[i]:
			down = append(down, n.Host)
		case isCP:
			cpsUp++
		default:
			workersUp++
		}
	}
	res := providers.StatusResult{
		ControlPlanes: fmt.Sprintf("%d/%d", cpsUp, cps),
		Workers:       fmt.Sprintf("%d/%d", workersUp, workers),
		State:         "ready",
		Healthy:       len(down) == 0,
	}
	if !res.Healthy {
		res.State, res.Message = "degraded", "not reachable over SSH: "+strings.Join(down, ", ")
	}
	return res, nil
}

// Teardown never touches the user's machines: deleting a byoh cluster only makes tow forget it.
func (p *Provider) Teardown(ctx context.Context, spec providers.Spec) error { return nil }
