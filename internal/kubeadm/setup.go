// Package kubeadm installs upstream Kubernetes with kubeadm on prepared machines, following
// the official documentation. Setup gets each machine what kubeadm needs: the container
// runtime the file asks for, and kubelet, kubeadm and kubectl from the Kubernetes package
// repository. A runtime already on a machine is used as it is when it goes with the
// Kubernetes release; otherwise nothing changes and the user is told why.
//
// Sources (read 2026-11-10):
//   - Installing kubeadm, Debian-based: pkgs.k8s.io per minor release, its keyring, apt-mark
//     hold; Red Hat-based (read 2026-11-14): /etc/yum.repos.d/kubernetes.repo with its exclude
//     line, dnf with disable_excludes (and install_weak_deps=False on RHEL 10), SELinux
//     permissive (prepare); "the kubelet is now restarting every few seconds, as it waits in a
//     crashloop for kubeadm to tell it what to do"
//     https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/install-kubeadm/
//   - Container runtimes: containerd's SystemdCgroup and sandbox image, the CRI plugin not
//     disabled; CRI-O's systemd cgroups by default and pause_image in a crio.conf.d drop-in
//     https://kubernetes.io/docs/setup/production-environment/container-runtimes/
//   - containerd: release tarball under /usr/local, runc, the systemd unit
//     https://github.com/containerd/containerd/blob/main/docs/getting-started.md
//   - CRI-O packages per minor on openSUSE's build service  https://github.com/cri-o/packaging
package kubeadm

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/towproj/tow/internal/parallel"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/runtimes"
	"github.com/towproj/tow/internal/ui"
)

const nodeTimeout = 15 * time.Minute // per machine: apt and two downloads on a small machine

// Options say what kubeadm's machines get.
type Options struct {
	Cluster    string
	Version    string // the Kubernetes release, resolved: v1.37.2
	Runtime    string // runtimes.Containerd or runtimes.CRIO
	Containerd string // the containerd release the file asks for; "" = runtimes.DefaultContainerd
	CRIO       string // the CRI-O release the file asks for; "" = the newest of the Kubernetes minor
	Nodes      []providers.Node
	KnownHosts string

	// The cluster kubeadm sets up (Install).
	// Endpoint is the API address the kubeconfig uses; "" = the first control plane's.
	// ControlPlaneEndpoint goes into kubeadm's config: the address in front of the control
	// planes the nodes use (the user's own load balancer, AWS's internal one, kube-vip's).
	Endpoint             string
	ControlPlaneEndpoint string
	CheckEndpoint        bool   // ControlPlaneEndpoint is the user's own load balancer: tested from the first control plane before init
	KubeVIP              string // the virtual IP kube-vip holds for ControlPlaneEndpoint; "" = no kube-vip
	KubeVIPServices      bool   // that kube-vip announces LoadBalancer Services' addresses too
	// APIName is ControlPlaneEndpoint's host when it's a name each machine resolves in its
	// /etc/hosts (GCP: a machine behind the internal load balancer can't reach it): a worker
	// to APIAddress, the load balancer; a control plane to itself once kubeadm set it up, to
	// the first control plane while it joins. "" = none.
	APIName, APIAddress  string
	ControlPlanes        int // how many the file asks for, for a dry run before the machines exist
	PodCIDR, ServiceCIDR string
	DNSDomain            string
	CertSANs             []string // the file's; the endpoint and the control plane's addresses are added
	APIServer            []providers.Flag
	ControllerManager    []providers.Flag
	Scheduler            []providers.Flag
	Etcd                 []providers.Flag
	SkipKubeProxy        bool // the CNI replaces kube-proxy
	// ServerTLSBootstrap: kubelets get serving certificates the cluster signs (metrics-server
	// checks them); kubelet-csr-approver, installed with the add-ons, approves them.
	ServerTLSBootstrap bool
	Schedulable        bool // workloads may run on the control plane: no control-plane taint
	// Node says what the file asks of a node's group; false = a pool the file doesn't have.
	Node    func(n providers.Node) (NodeSettings, bool)
	Secrets map[string]string // values of the {env} flags, read before anything started
	// Snapshots, when set, are etcd snapshots each control plane takes on a schedule.
	Snapshots *Snapshots

	Connect func(ctx context.Context, n providers.Node, knownHosts string) (prepare.Runner, error)
	API     func(kubeconfig []byte) (API, error)
}

// API is what the install needs from the cluster's API, reached from this machine.
type API interface {
	WaitReachable(ctx context.Context) error
	WaitNodes(ctx context.Context, names []string, ready bool) error
	Label(ctx context.Context, node string, labels map[string]string) error
	SpreadOverNodes(ctx context.Context, namespace, name string) error
}

func (o Options) want() string       { return strings.TrimPrefix(o.Version, "v") }
func (o Options) minor() int         { return runtimes.KubeMinor(o.Version) }
func (o Options) containerd() string { return cmp.Or(o.Containerd, runtimes.DefaultContainerd) }

// runtimeName names the runtime the machines get, for messages: "containerd 2.3.7".
func (o Options) runtimeName() string {
	if o.Runtime == runtimes.CRIO {
		if o.CRIO != "" {
			return "CRI-O " + strings.TrimPrefix(o.CRIO, "v")
		}
		return fmt.Sprintf("CRI-O (the newest 1.%d release)", o.minor())
	}
	return "containerd " + o.containerd()
}

// CheckVersions checks the runtime's release against the Kubernetes release once that's
// known: the file's checks can't, when it says "stable". The warning is for a Kubernetes
// containerd's table doesn't list yet.
func CheckVersions(o Options) (warning string, err error) {
	switch o.Runtime {
	case runtimes.Containerd:
		warning, err = runtimes.CheckContainerd(o.containerd(), o.minor())
	case runtimes.CRIO:
		if o.CRIO != "" {
			err = runtimes.CheckCRIO(o.CRIO, o.minor())
		}
	}
	if err != nil {
		return "", fmt.Errorf("Kubernetes %s: %w", o.Version, err)
	}
	return warning, nil
}

// machine is one node's progress.
type machine struct {
	n     providers.Node
	label string
	r     prepare.Runner
	facts facts
	err   error
}

// Setup gets every machine what kubeadm needs, in parallel: kubelet, kubeadm and kubectl,
// then the container runtime. Each machine is checked first, and nothing changes anywhere
// when one of them can't take it. What a machine already has is left alone; every change is
// recorded on it, then checked again.
func Setup(ctx context.Context, o Options) error {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
	ms := make([]*machine, len(o.Nodes))
	for i, n := range o.Nodes {
		ms[i] = &machine{n: n, label: n.Label()}
	}
	defer func() {
		for _, m := range ms {
			if m.r != nil {
				m.r.Close()
			}
		}
	}()

	parallel.Each(ms, func(m *machine) {
		r, err := o.Connect(ctx, m.n, o.KnownHosts)
		if err != nil {
			m.err = err
			return
		}
		m.r = r
		m.facts, m.err = o.read(ctx, m)
	})
	if err := parallel.Errors(ms, "could not check"); err != nil {
		return err
	}
	if err := o.refuse(ms); err != nil {
		return err
	}

	var todo []*machine
	for _, m := range ms {
		if r, theirs := o.theirs(m.facts); theirs {
			ui.Info(fmt.Sprintf("%s: uses the %s %s already there (%s)", m.label, o.Runtime, r.version, r.source()))
		}
		if len(o.missing(m.facts)) > 0 {
			todo = append(todo, m)
		}
	}
	if len(todo) == 0 {
		ui.Info(fmt.Sprintf("The machines already have %s and kubelet, kubeadm, kubectl %s", o.runtimeName(), o.want()))
		return nil
	}
	if err := ui.StepRun(fmt.Sprintf("Installing kubelet, kubeadm, kubectl %s and %s on %s", o.want(), o.runtimeName(), ui.Plural(len(todo), "machine")), func() error {
		parallel.Each(todo, func(m *machine) { m.err = o.ensure(ctx, m) })
		return parallel.Errors(todo, "could not set up")
	}); err != nil {
		return err
	}
	ui.Info(fmt.Sprintf("kubelet, kubeadm and kubectl %s are held at that release; each kubelet restarts every few seconds until kubeadm sets it up, which is expected", o.want()))
	return nil
}

// Preflight is preparation's hook: it finds what would stop Setup on a machine, before
// anything changes there.
func (o Options) Preflight(ctx context.Context, r prepare.Runner, n providers.Node) ([]prepare.Problem, error) {
	f, err := o.read(ctx, &machine{n: n, r: r})
	if err != nil {
		return nil, err
	}
	return o.problems(f), nil
}

// read runs the read-only check.
func (o Options) read(ctx context.Context, m *machine) (facts, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, err := m.r.RunInput(ctx, asRoot, factsScript(o.Version))
	f := parseFacts(out)
	if !f.complete {
		if strings.Contains(out, "password is required") || strings.Contains(out, "sudo:") {
			return f, fmt.Errorf("sudo needs a password: tow installs unattended (allow '%s ALL=(ALL) NOPASSWD:ALL' in /etc/sudoers.d/)", m.n.User)
		}
		if err == nil {
			err = errors.New("the check did not finish")
		}
		return f, fmt.Errorf("%w\n%s", err, withoutRecord(out))
	}
	return f, nil
}

// problems are what only the user can solve on a machine: another Kubernetes on it, packages
// at another release, or a runtime that doesn't go with the Kubernetes the file asks for.
// tow never replaces what it didn't install.
func (o Options) problems(f facts) []prepare.Problem {
	var ps []prepare.Problem
	add := func(what, hint string) { ps = append(ps, prepare.Problem{What: what, Hint: hint}) }
	want := o.want()

	if f.k3s {
		add("k3s is installed here: kubeadm and k3s can't share a machine",
			"use another machine, or remove k3s first (sudo /usr/local/bin/k3s-uninstall.sh; on a worker, k3s-agent-uninstall.sh)")
	}
	if f.kubeletConf && !o.started(f) {
		add("this machine is already a Kubernetes node (/etc/kubernetes/kubelet.conf exists), and not one tow set up for this cluster",
			"use another machine, or reset it first: sudo kubeadm reset -f")
	}
	if !o.recorded(f, whatPackages) {
		var other []string
		for _, p := range []string{"kubelet", "kubeadm", "kubectl"} {
			if v, ok := f.pkgs[p]; ok && !strings.HasPrefix(v, want+"-") {
				other = append(other, p+" "+v)
			}
		}
		if len(other) > 0 {
			remove := "sudo apt-mark unhold kubelet kubeadm kubectl && sudo apt-get remove kubelet kubeadm kubectl"
			if f.rhel() {
				remove = "sudo dnf remove kubelet kubeadm kubectl"
			}
			add(fmt.Sprintf("%s already installed, and the file asks for Kubernetes %s", strings.Join(other, ", "), want),
				"use another machine, remove them ("+remove+"), or set spec.kubernetes.version to the release they have")
		}
	}

	const keep = "tow doesn't change a container runtime it didn't install"
	switch o.Runtime {
	case runtimes.Containerd:
		c := f.containerd
		switch {
		case c.owner != "" && c.owner != o.Cluster:
			add(fmt.Sprintf("containerd here was set up by tow for cluster '%s'", c.owner), "use another machine, or remove that containerd first (its undo is in "+prepare.RecordFile+")")
		case c.path == "" || c.owner == o.Cluster:
			if !slices.Contains(runtimes.Archs, f.arch) {
				add(fmt.Sprintf("this machine is %s: containerd and runc publish %s builds", f.arch, strings.Join(runtimes.Archs, " and ")), "use another machine, or set spec.kubernetes.containerRuntime: cri-o")
			}
		default:
			var why []string
			if o.Containerd != "" && !sameRelease(c.version, o.Containerd) {
				why = append(why, "the file asks for containerd "+strings.TrimPrefix(o.Containerd, "v"))
			}
			if _, err := runtimes.CheckContainerd(cmp.Or(c.version, "unknown"), o.minor()); err != nil {
				why = append(why, err.Error())
			}
			switch {
			case !c.active:
				why = append(why, "it isn't running")
			case !c.cri:
				why = append(why, "its Kubernetes plugin (CRI) is off: Docker's containerd.io turns it off in "+containerdConfig)
			}
			if len(why) > 0 {
				add(fmt.Sprintf("containerd %s (%s) is already here and can't run Kubernetes %s as it is: %s", cmp.Or(c.version, "of unknown release"), c.source(), want, strings.Join(why, "; ")),
					keep+": upgrade or reconfigure it yourself, use another machine, or set spec.kubernetes.containerRuntime: cri-o")
			}
		}
	case runtimes.CRIO:
		c := f.crio
		switch {
		case c.owner != "" && c.owner != o.Cluster:
			add(fmt.Sprintf("CRI-O here was set up by tow for cluster '%s'", c.owner), "use another machine, or remove that CRI-O first (its undo is in "+prepare.RecordFile+")")
		case c.path == "" || c.owner == o.Cluster:
		default:
			var why []string
			if o.CRIO != "" && !sameRelease(c.version, o.CRIO) {
				why = append(why, "the file asks for CRI-O "+strings.TrimPrefix(o.CRIO, "v"))
			}
			if err := runtimes.CheckCRIO(cmp.Or(c.version, "unknown"), o.minor()); err != nil {
				why = append(why, err.Error())
			}
			if !c.active {
				why = append(why, "it isn't running")
			}
			if len(why) > 0 {
				add(fmt.Sprintf("CRI-O %s (%s) is already here and can't run Kubernetes %s as it is: %s", cmp.Or(c.version, "of unknown release"), c.source(), want, strings.Join(why, "; ")),
					keep+": upgrade it yourself, use another machine, or set spec.kubernetes.containerRuntime: containerd")
			}
		}
	}
	return ps
}

// theirs reports the runtime the machine already has that tow will use as it is.
func (o Options) theirs(f facts) (runtime, bool) {
	r := f.containerd
	if o.Runtime == runtimes.CRIO {
		r = f.crio
	}
	return r, r.path != "" && r.owner == ""
}

// recorded reports whether the machine's record lists this part for this cluster.
func (o Options) recorded(f facts, what string) bool {
	return prepare.Recorded(f.record, func(r prepare.Record) bool { return r.Cluster == o.Cluster && r.What == what })
}

// missing lists what the machine still needs; empty = ready for kubeadm.
func (o Options) missing(f facts) []string {
	return append(o.missingPackages(f), o.missingRuntime(f)...)
}

// missingPackages: kubelet, kubeadm and kubectl at the release and held, cri-tools, the
// repository for the minor, the kubelet enabled.
func (o Options) missingPackages(f facts) []string {
	var out []string
	want := o.want()
	for _, p := range []string{"kubelet", "kubeadm", "kubectl"} {
		switch v, ok := f.pkgs[p]; {
		case !ok:
			out = append(out, p+" isn't installed")
		case !strings.HasPrefix(v, want+"-"):
			out = append(out, fmt.Sprintf("%s is %s, not %s", p, v, want))
		case !f.held[p]:
			out = append(out, p+" isn't held")
		}
	}
	if _, ok := f.pkgs["cri-tools"]; !ok {
		out = append(out, "cri-tools isn't installed")
	}
	if repo := kubeRepo(o.minor(), f.rhel()); !strings.Contains(f.kubeRepo, repo) {
		out = append(out, "the Kubernetes repository isn't "+repo)
	}
	if !f.kubeletEnabled {
		out = append(out, "the kubelet isn't enabled")
	}
	return out
}

// missingRuntime: the runtime tow installs, at the release, set up for Kubernetes and
// running. One already there that goes with this Kubernetes needs nothing.
func (o Options) missingRuntime(f facts) []string {
	if _, theirs := o.theirs(f); theirs {
		return nil // checked before: it goes with this Kubernetes as it is
	}
	var out []string
	switch o.Runtime {
	case runtimes.Containerd:
		c := f.containerd
		switch {
		case c.path == "":
			out = append(out, "containerd isn't installed")
		case !sameRelease(c.version, o.containerd()):
			out = append(out, fmt.Sprintf("containerd is %s, not %s", c.version, o.containerd()))
		case !c.systemd || f.pause == "" || c.pause != f.pause:
			out = append(out, fmt.Sprintf("%s isn't set up for Kubernetes (systemd cgroups, pause image %s)", containerdConfig, f.pause))
		case !c.active || !c.cri:
			out = append(out, "containerd's Kubernetes plugin (CRI) doesn't answer")
		}
	case runtimes.CRIO:
		c := f.crio
		switch {
		case c.path == "":
			out = append(out, "CRI-O isn't installed")
		case o.CRIO != "" && !sameRelease(c.version, o.CRIO):
			out = append(out, fmt.Sprintf("CRI-O is %s, not %s", c.version, strings.TrimPrefix(o.CRIO, "v")))
		case runtimes.CheckCRIO(c.version, o.minor()) != nil || !strings.Contains(f.crioRepo, crioRepo(o.minor(), f.rhel())):
			out = append(out, fmt.Sprintf("CRI-O %s isn't from its v1.%d repository", c.version, o.minor()))
		case f.pause == "" || c.pause != f.pause:
			out = append(out, fmt.Sprintf("%s doesn't name the pause image %s", crioDropIn, f.pause))
		case !c.active:
			out = append(out, "CRI-O isn't running")
		}
	}
	return out
}

// ensure installs what the machine is missing: the packages first (kubeadm names the pause
// image the runtime needs), then the runtime. Each part is recorded, then the machine is
// checked again.
func (o Options) ensure(ctx context.Context, m *machine) error {
	ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	var done []prepare.Record
	err := o.install(ctx, m, &done)
	// What finished is recorded, with how to undo it, even when a later part failed.
	if rerr := o.record(ctx, m, done); rerr != nil {
		return errors.Join(err, rerr)
	}
	if err != nil {
		return err
	}
	if left := o.missing(m.facts); len(left) > 0 {
		return fmt.Errorf("%s: still not ready after the install: %s", m.label, strings.Join(left, "; "))
	}
	return nil
}

// install puts on the machine what it's missing: kubelet, kubeadm and kubectl, then the
// container runtime. done gets the record of each part that finished.
func (o Options) install(ctx context.Context, m *machine, done *[]prepare.Record) error {
	run := func(what, script string) error {
		if out, err := m.r.RunInput(ctx, asRoot, script); err != nil {
			return fmt.Errorf("%s: installing %s failed: %w\n%s", m.label, what, err, ui.LastLines(out, 15))
		}
		var err error
		m.facts, err = o.read(ctx, m)
		return err
	}

	if len(o.missingPackages(m.facts)) > 0 {
		if err := run("kubelet, kubeadm and kubectl", packagesScript(o.Version, m.facts)); err != nil {
			return err
		}
		*done = append(*done, packagesRecord(o.Version, m.facts))
	}
	if len(o.missingRuntime(m.facts)) > 0 {
		script, rec, err := o.runtimeScript(m)
		if err != nil {
			return err
		}
		if err := run(o.runtimeName(), script); err != nil {
			return err
		}
		*done = append(*done, rec)
	}
	return nil
}

// runtimeScript installs the container runtime at its release, set up for this Kubernetes
// (the pause image its kubeadm names), and its record.
func (o Options) runtimeScript(m *machine) (string, prepare.Record, error) {
	if m.facts.pause == "" {
		return "", prepare.Record{}, fmt.Errorf("%s: kubeadm %s named no pause image ('kubeadm config images list')", m.label, o.want())
	}
	if o.Runtime == runtimes.CRIO {
		script, err := crioScript(o.Cluster, o.minor(), o.CRIO, m.facts.pause, m.facts)
		return script, crioRecord(o.minor(), m.facts), err
	}
	script, err := containerdScript(o.Cluster, o.containerd(), m.facts.arch, m.facts.pause)
	return script, containerdRecord(o.containerd()), err
}

// record writes what finished into the machine's record, once per cluster.
func (o Options) record(ctx context.Context, m *machine, done []prepare.Record) error {
	var fresh []prepare.Record
	for _, r := range done {
		if !o.recorded(m.facts, r.What) {
			r.Cluster, r.At = o.Cluster, time.Now().UTC()
			fresh = append(fresh, r)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	script, err := prepare.RecordScript(m.facts.record, fresh)
	if err == nil {
		_, err = m.r.RunInput(context.WithoutCancel(ctx), asRoot, "set -e\n"+script)
	}
	if err != nil {
		return fmt.Errorf("%s: installed, but not recorded in %s: %w", m.label, prepare.RecordFile, err)
	}
	return nil
}

// sameRelease compares two releases, ignoring a leading v and packaging suffixes.
func sameRelease(a, b string) bool {
	x, okA := runtimes.Parse(a)
	y, okB := runtimes.Parse(b)
	return okA && okB && x == y
}

// refuse stops before anything changes when a machine can't be set up as the file asks.
func (o Options) refuse(ms []*machine) error {
	var b strings.Builder
	n := 0
	for _, m := range ms {
		ps := o.problems(m.facts)
		if len(ps) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n%s, %s:", m.label, m.n.Role)
		for _, p := range ps {
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
	return fmt.Errorf("%d problem(s) to solve before kubeadm goes on the machines (nothing was changed):%s", n, b.String())
}

// anyFamily names a repository for a dry run, before the machines' systems are known: its
// deb (apt) or rpm (dnf) form, by each machine's.
func anyFamily(debRepo string) string {
	return strings.TrimSuffix(debRepo, "deb/") + "{deb,rpm}/"
}

// Describe sums up, for a dry run, what Setup would install.
func Describe(o Options) []string {
	minor := o.minor()
	lines := []string{
		"Then each machine would get what kubeadm needs, from the official sources:",
		fmt.Sprintf("  kubelet, kubeadm and kubectl %s from %s, held at that release, and cri-tools", o.want(), anyFamily(kubeRepo(minor, false))),
	}
	switch o.Runtime {
	case runtimes.CRIO:
		lines = append(lines,
			fmt.Sprintf("  %s from %s, with kubeadm's pause image in %s", o.runtimeName(), anyFamily(crioRepo(minor, false)), crioDropIn),
			fmt.Sprintf("  a machine that already has CRI-O keeps it when it's a 1.%d release and runs; otherwise nothing changes", minor))
	default:
		lines = append(lines,
			fmt.Sprintf("  %s and runc %s from their GitHub releases (sha256 checked) under /usr/local; %s from 'containerd config default', with systemd cgroups and kubeadm's pause image",
				o.runtimeName(), runtimes.Runc, containerdConfig),
			fmt.Sprintf("  a machine that already has containerd keeps it when it goes with Kubernetes 1.%d (%s, its CRI plugin on); otherwise nothing changes", minor, runtimes.Recommended(minor)))
	}
	return lines
}

// Label and Err make a machine a parallel.Item.
func (m *machine) Label() string { return m.label }
func (m *machine) Err() error    { return m.err }
