package prepare

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/parallel"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/ui"
)

const nodeTimeout = 15 * time.Minute // per node: apt or dnf can be slow on a small machine

// Runner is the slice of an SSH client preparation uses, so tests can fake machines.
type Runner interface {
	RunInput(ctx context.Context, cmd, stdin string) (string, error)
	Close() error
}

// Options say which machines to prepare and how.
type Options struct {
	Cluster     string
	Nodes       []providers.Node // with PrivateIP, as the provider returned them
	KnownHosts  string           // the cluster's pinned SSH host keys
	Distro      string           // kubeadm checks product_uuid too
	PodCIDR     string
	ServiceCIDR string
	// AdminCIDRs are who the "from admin" firewall rules let in: the file's adminCIDRs, or,
	// when it gives none, this machine's address as each node sees it.
	AdminCIDRs []string
	Needs      func(role string) Needs

	// Confirm is asked before anything changes, with the list of changes. nil = don't ask
	// (machines tow created itself).
	Confirm func(summary string) bool
	// Confirmed says the changes were shown and agreed to already (apply's plan, on a running
	// cluster, after Check): Run makes them without listing them again.
	Confirmed bool

	// Preflight, when set, checks each machine for what comes after preparation (kubeadm: a
	// container runtime already there). Its problems stop everything before any change, with
	// preparation's own.
	Preflight func(ctx context.Context, r Runner, n providers.Node) ([]Problem, error)

	Connect func(ctx context.Context, n providers.Node, knownHosts string) (Runner, error)
}

// node is one machine's progress.
type node struct {
	n        providers.Node
	label    string
	r        Runner
	needs    Needs
	rules    []ufwRule
	fw       firewalldWant
	info     clusterInfo
	facts    Facts
	changes  []Change
	problems []Problem
	warnings []string
	done     []Record
	err      error
}

// Connect is the real SSH connection.
func Connect(ctx context.Context, n providers.Node, knownHosts string) (Runner, error) {
	return remote.Dial(ctx, remote.Target{Host: n.Host, Port: n.Port, User: n.User, KeyPath: n.KeyPath}, knownHosts)
}

// Check reads what each machine still needs and changes nothing: one line per change,
// "<machine>: <change>". A problem only the user can solve is an error.
func Check(ctx context.Context, o Options) ([]string, error) {
	nodes, closeAll, err := checkAll(ctx, o)
	defer closeAll()
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, x := range nodes {
		for _, c := range x.changes {
			lines = append(lines, x.label+": "+c.Summary)
		}
	}
	return lines, nil
}

// Run prepares every node: check, ask (when Confirm is set), change what's missing, record
// it, then check again. A problem only the user can solve stops it before anything changes.
func Run(ctx context.Context, o Options) error {
	nodes, closeAll, err := checkAll(ctx, o)
	defer closeAll()
	if err != nil {
		return err
	}

	summary := describe(nodes)
	if summary == "" {
		if !o.Confirmed {
			ui.Info("Machines already prepared; nothing to change")
		}
		return nil
	}
	if o.Confirm != nil && !o.Confirm(summary) {
		return errors.New("nothing was changed on your machines")
	}
	if o.Confirm == nil && !o.Confirmed {
		fmt.Println(summary)
	}

	// Change what's missing, then record it, in parallel.
	parallel.Each(nodes, func(x *node) {
		ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
		defer cancel()
		for _, c := range x.changes {
			if out, err := x.r.RunInput(ctx, asRoot, "set -e\n"+c.Script); err != nil {
				x.err = fmt.Errorf("%s: %w\n%s", c.Summary, err, strings.TrimSpace(out))
				break
			}
			rec := c.Record
			rec.Cluster, rec.At = o.Cluster, time.Now().UTC()
			x.done = append(x.done, rec)
		}
		if len(x.done) > 0 {
			script, err := RecordScript(x.facts.Prepared, x.done)
			if err == nil {
				_, err = x.r.RunInput(ctx, asRoot, "set -e\n"+script)
			}
			if err != nil && x.err == nil {
				x.err = fmt.Errorf("changes made but not recorded in %s: %w", RecordFile, err)
			}
		}
		// Check again: whatever is still missing means a change didn't hold.
		if x.err == nil {
			if x.err = x.check(ctx); x.err == nil && len(x.changes) > 0 {
				var left []string
				for _, c := range x.changes {
					left = append(left, c.Summary)
				}
				x.err = fmt.Errorf("still not ready after preparing: %s", strings.Join(left, "; "))
			}
		}
	})
	if err := parallel.Errors(nodes, "could not prepare"); err != nil {
		return err
	}
	if !o.Confirmed {
		ui.Info(fmt.Sprintf("Machines prepared; every change is listed in %s on each of them", RecordFile))
	}
	return nil
}

// checkAll connects to every node and reads what each one needs, in parallel. Problems only
// the user can solve stop it; warnings are shown, once (not again after a Check). closeAll
// ends the connections.
func checkAll(ctx context.Context, o Options) (nodes []*node, closeAll func(), err error) {
	if o.Connect == nil {
		o.Connect = Connect
	}
	nodes = make([]*node, len(o.Nodes))
	for i, n := range o.Nodes {
		var peers []string
		for j, p := range o.Nodes {
			if j != i && p.PrivateIP != "" {
				peers = append(peers, p.PrivateIP)
			}
		}
		x := &node{n: n, label: n.Label(), needs: o.Needs(n.Role),
			info: clusterInfo{name: o.Cluster, peers: peers, podCIDR: o.PodCIDR, serviceCIDR: o.ServiceCIDR, admin: o.AdminCIDRs}}
		x.rules = ufwRules(x.needs, x.info)
		x.fw = firewalldWanted(x.needs, x.info)
		nodes[i] = x
	}
	closeAll = func() {
		for _, x := range nodes {
			if x.r != nil {
				x.r.Close()
			}
		}
	}

	// Check every node, in parallel.
	parallel.Each(nodes, func(x *node) {
		r, err := o.Connect(ctx, x.n, o.KnownHosts)
		if err != nil {
			x.err = err
			return
		}
		x.r = r
		if x.err = x.check(ctx); x.err != nil || o.Preflight == nil {
			return
		}
		more, err := o.Preflight(ctx, r, x.n)
		x.problems, x.err = append(x.problems, more...), err
	})
	if err := parallel.Errors(nodes, "could not check"); err != nil {
		return nil, closeAll, err
	}
	identity(nodes, o.Distro == config.DistributionKubeadm)
	if err := problems(nodes); err != nil {
		return nil, closeAll, err
	}
	if !o.Confirmed {
		for _, x := range nodes {
			for _, w := range x.warnings {
				ui.Warn(fmt.Sprintf("%s: %s", x.label, w))
			}
		}
	}
	return nodes, closeAll, nil
}

// check reads the node's facts and plans what's missing.
func (x *node) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := x.r.RunInput(ctx, asRoot, factsScript(x.needs, x.rules, x.fw, x.info.name))
	f := parseFacts(out)
	if !f.Complete {
		if err == nil {
			err = errors.New("the check did not finish")
		}
		if strings.Contains(out, "password is required") || strings.Contains(out, "sudo:") {
			return fmt.Errorf("sudo needs a password: tow prepares unattended (allow '%s ALL=(ALL) NOPASSWD:ALL' in /etc/sudoers.d/)", x.n.User)
		}
		return fmt.Errorf("%w\n%s", err, strings.TrimSpace(out))
	}
	x.facts = f
	x.changes, x.problems, x.warnings = planNode(x.needs, f, x.rules, x.fw, x.info)
	return nil
}

// identity: kubeadm needs a unique product_uuid on every node, and two nodes sharing a MAC
// address can't both be on the network (kubeadm docs).
func identity(nodes []*node, kubeadm bool) {
	dup := func(what string, key func(Facts) string, hint string) {
		first := map[string]*node{}
		for _, x := range nodes {
			k := key(x.facts)
			if k == "" {
				continue
			}
			if y, seen := first[k]; seen {
				x.problems = append(x.problems, Problem{What: fmt.Sprintf("same %s as %s (%s)", what, y.label, k), Hint: hint})
				continue
			}
			first[k] = x
		}
	}
	dup("MAC address", func(f Facts) string { return f.MAC }, "cloned VM? give it a new MAC address in the hypervisor")
	if kubeadm {
		dup("product_uuid", func(f Facts) string { return f.ProductUUID }, "cloned VM? give it a new SMBIOS UUID in the hypervisor (kubeadm docs)")
	}
}

func problems(nodes []*node) error {
	var b strings.Builder
	n := 0
	for _, x := range nodes {
		if len(x.problems) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s, %s:", x.label, x.n.Role)
		for _, p := range x.problems {
			n++
			fmt.Fprintf(&b, "\n  ✗ %s", p.What)
			if p.Hint != "" {
				fmt.Fprintf(&b, "\n    → %s", p.Hint)
			}
		}
	}
	if n == 0 {
		return nil
	}
	return fmt.Errorf("%d problem(s) to solve before tow prepares the machines (nothing was changed):%s\nFix them and run 'tow apply' again.", n, b.String())
}

// describe lists the changes to make, node by node; "" when there are none.
func describe(nodes []*node) string {
	var b strings.Builder
	for _, x := range nodes {
		if len(x.changes) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s:", x.label)
		for _, c := range x.changes {
			fmt.Fprintf(&b, "\n  - %s", c.Summary)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "Preparing the machines for Kubernetes needs these changes (each is recorded in " + RecordFile + " on the machine):" + b.String()
}

// Describe sums up what preparation checks and may change, for a dry run, which doesn't
// connect to machines that don't exist yet.
func Describe(n Needs) []string {
	var out []string
	if len(n.Packages) > 0 {
		var names []string
		for _, p := range n.Packages {
			switch {
			case p.Only == OnlyDebian:
				names = append(names, p.Name+" (Ubuntu and Debian)")
			case p.Only == OnlyRHEL10:
				names = append(names, p.Name+" (RHEL 10)")
			case p.Only == OnlyRHEL:
				names = append(names, p.Name+" (RHEL)")
			case p.RPM != "":
				names = append(names, p.Name+" ("+p.RPM+" on RHEL)")
			default:
				names = append(names, p.Name)
			}
		}
		out = append(out, "packages: "+strings.Join(names, ", "))
	}
	for _, m := range n.Modules {
		out = append(out, fmt.Sprintf("kernel module %s (%s)", m.Name, m.Why))
	}
	for _, s := range n.Sysctls {
		out = append(out, fmt.Sprintf("%s = %s (%s)", s.Name, s.Value, s.Why))
	}
	if n.SwapOff {
		out = append(out, "swap off (kubeadm)")
	}
	if n.MinKernel != nil {
		out = append(out, fmt.Sprintf("kernel %s or newer (%s)", n.MinKernel.Value, n.MinKernel.Why))
	}
	if n.QuietMultipath != "" {
		out = append(out, fmt.Sprintf("if multipathd runs: sd* disks blacklisted in %s (%s docs); refused where multipath manages devices", multipathConf, n.QuietMultipath))
	}
	out = append(out, "Ubuntu, Debian, or RHEL 9 or 10 (Rocky, AlmaLinux), the memory cgroup on, unique MAC addresses",
		"if ufw is on: the cluster's traffic allowed in, rules commented 'tow:<cluster>'",
		"if firewalld is on: the cluster's traffic allowed in, through its zone "+fwZone+" and policy "+fwPolicy)
	return out
}

// Label and Err make a node a parallel.Item.
func (x *node) Label() string { return x.label }
func (x *node) Err() error    { return x.err }
