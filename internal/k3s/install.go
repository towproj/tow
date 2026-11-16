package k3s

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net"
	"reflect"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/kubevip"
	"github.com/towproj/tow/internal/parallel"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/ui"
)

// Sources (read 2026-11-09), followed here:
//   - configuration: one config.yaml, repeatable flags as YAML lists, booleans as true/false;
//     the install script never touches it  https://docs.k3s.io/installation/configuration
//   - tokens: the first server takes only a plain password; joining nodes use the secure
//     K10<CA hash> form, so they check the cluster before sending credentials; the agent
//     token joins workers only  https://docs.k3s.io/cli/token
//   - HA with embedded etcd: the first server with cluster-init, the others join it; a few
//     flags (agent-token, CIDRs, disable, flannel-backend, ...) must be the same on every
//     server  https://docs.k3s.io/datastore/ha-embedded, https://docs.k3s.io/cli/server
//   - node-external-ip is never set: behind NAT (public clouds) it breaks
//     externalTrafficPolicy=Local  https://docs.k3s.io/networking/networking-services

const (
	nodeTimeout = 10 * time.Minute // per machine: k3s is a ~70 MB download
	logLines    = 40
)

// API is what the install needs from the cluster's API, reached from this machine.
type API interface {
	WaitReachable(ctx context.Context) error
	WaitNodes(ctx context.Context, names []string, ready bool) error
	Label(ctx context.Context, node string, labels map[string]string) error
	ScaleUp(ctx context.Context, namespace, name string, replicas int32) error
}

// Options say where k3s goes and how each machine is set up.
type Options struct {
	Cluster    string
	Version    string           // resolved: v1.37.2+k3s1, or v1.37.2 for its first k3s build
	Nodes      []providers.Node // from the provider: names, addresses, roles, pools
	KnownHosts string

	// Endpoint is the API address in front of the control planes (host:port), added to the
	// API's certificate. The kubeconfig uses it; "" = the first control plane.
	Endpoint string
	// KubeVIP is the virtual IP kube-vip holds for Endpoint; "" = no kube-vip.
	// KubeVIPServices has that kube-vip announce LoadBalancer Services' addresses too.
	KubeVIP         string
	KubeVIPServices bool
	// NodeEndpoint is the nodes' own address in front of the control planes, when not
	// Endpoint (AWS: an internal load balancer); added to the API's certificate too, since a
	// CNI that replaces kube-proxy reaches the API through it.
	NodeEndpoint string
	// APIName is the name that CNI reaches the API by instead, when a machine behind the
	// internal load balancer can't reach it (GCP): each machine resolves it in /etc/hosts, a
	// worker to NodeEndpoint's address, a control plane to itself. In the certificate too.
	// (k3s itself joins through the first control plane, and its agents balance over the
	// control planes they learn.) "" = none.
	APIName string

	ServerFlags []providers.Flag                           // every control plane gets the same (k3s docs: critical values)
	AgentFlags  func(pool string) ([]providers.Flag, bool) // a worker pool's; false = no such pool
	RoleLabels  func(n providers.Node) map[string]string   // node-role.kubernetes.io/..., set through the API
	Secrets     map[string]string                          // values of the {env} flags, read before anything started
	WaitReady   bool                                       // the CNI comes with k3s (flannel): every node must get Ready
	Pools       []string                                   // the worker pools, in file order, for Describe
	// CoreDNSReplicas is how many CoreDNS replicas the cluster gets at least; 0 = one, as k3s
	// ships it. With several control planes one isn't enough: when its machine goes away,
	// Kubernetes waits five minutes before it starts the replica elsewhere, and the cluster
	// has no DNS meanwhile.
	CoreDNSReplicas int32

	// Rolling, when set, changes a running cluster one machine at a time, control planes
	// first (k3s's upgrade docs): each machine that doesn't run the release as the file asks
	// changes inside Rolling, which drains its node before and waits for it after. Machines
	// that already do are left alone.
	Rolling func(ctx context.Context, n providers.Node, change func() error) error

	Connect func(ctx context.Context, n providers.Node, knownHosts string) (prepare.Runner, error)
	API     func(kubeconfig []byte) (API, error)
}

// machine is one node's progress.
type machine struct {
	n        providers.Node
	label    string
	server   bool
	flags    []providers.Flag
	r        prepare.Runner
	facts    facts
	config   []byte // the config.yaml it gets
	vipIface string // a control plane's interface, where kube-vip would announce the API address
	err      error
}

// facts is what a machine already has.
type facts struct {
	installed bool   // a k3s binary or service
	version   string // the k3s binary's release
	active    map[string]bool
	config    []byte
	record    []byte
	selinux   string // getenforce: Enforcing, Permissive, Disabled; "" without SELinux
}

// enforcing: SELinux enforces its policy on the machine.
func (f facts) enforcing() bool { return f.selinux == "Enforcing" }

func (m *machine) role() string    { return map[bool]string{true: roleServer, false: roleAgent}[m.server] }
func (m *machine) service() string { return map[bool]string{true: "k3s", false: "k3s-agent"}[m.server] }
func (m *machine) exec() string    { return map[bool]string{true: "server", false: "agent"}[m.server] }

func uninstaller(role string) string {
	if role == roleServer {
		return "/usr/local/bin/k3s-uninstall.sh"
	}
	return "/usr/local/bin/k3s-agent-uninstall.sh"
}

// Install puts k3s on every machine: the first control plane starts the cluster, the other
// control planes join it one at a time, then the workers all at once. It returns the
// cluster's admin kubeconfig, pointing at the API address, once the API answers from this
// machine and every node has joined (and is Ready, when k3s brings the CNI). Machines that
// already run k3s as the file asks are left alone, so a failed install resumes.
func Install(ctx context.Context, o Options) ([]byte, error) {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
	if o.API == nil {
		o.API = func(kubeconfig []byte) (API, error) { return kubeapi.New(kubeconfig) }
	}
	release := Release(o.Version)

	ms, err := o.machines()
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, m := range ms {
			if m.r != nil {
				m.r.Close()
			}
		}
	}()

	// Check every machine first: nothing changes when one of them can't take k3s.
	parallel.Each(ms, func(m *machine) {
		r, err := o.Connect(ctx, m.n, o.KnownHosts)
		if err != nil {
			m.err = err
			return
		}
		m.r = r
		m.err = m.check(ctx)
	})
	if err := parallel.Errors(ms, "could not check"); err != nil {
		return nil, err
	}
	if err := o.refuse(ms); err != nil {
		return nil, err
	}
	vip, err := o.kubeVIP(ctx, ms)
	if err != nil {
		return nil, err
	}

	token, agentToken, err := o.tokens(ms[0])
	if err != nil {
		return nil, err
	}
	sans := o.sans(ms)
	first := ms[0]
	first.config, err = renderConfig(setup{cluster: o.Cluster, role: roleServer, name: first.n.Name, ip: first.n.PrivateIP,
		first: true, token: token, agentToken: agentToken, sans: sans, flags: first.flags, selinux: first.facts.enforcing()}, o.value)
	if err != nil {
		return nil, err
	}
	if o.Rolling != nil {
		// A running cluster: every machine's settings are known before any of them changes.
		if err := o.joinConfigs(ctx, ms, agentToken, sans); err != nil {
			return nil, err
		}
		return o.roll(ctx, ms, vip, release)
	}
	if err := ui.StepRun(fmt.Sprintf("Installing k3s %s on the first control plane, %s", release, first.label), func() error {
		if err := putKubeVIP(ctx, first, vip); err != nil {
			return err
		}
		return o.ensure(ctx, first, release)
	}); err != nil {
		return nil, err
	}

	if err := o.joinConfigs(ctx, ms, agentToken, sans); err != nil {
		return nil, err
	}
	var servers, agents []*machine
	for _, m := range ms[1:] {
		if m.server {
			servers = append(servers, m)
		} else {
			agents = append(agents, m)
		}
	}

	if len(servers) > 0 {
		// etcd grows one member at a time.
		if err := ui.StepRun(fmt.Sprintf("Joining %s, one at a time", ui.Plural(len(servers), "more control plane")), func() error {
			for _, m := range servers {
				if err := putKubeVIP(ctx, m, vip); err != nil {
					return err
				}
				if err := o.ensure(ctx, m, release); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	if len(agents) > 0 {
		if err := ui.StepRun(fmt.Sprintf("Joining %s", ui.Plural(len(agents), "worker")), func() error {
			parallel.Each(agents, func(m *machine) { m.err = o.ensure(ctx, m, release) })
			return parallel.Errors(agents, "could not install k3s on")
		}); err != nil {
			return nil, err
		}
	}

	kubeconfig, err := o.kubeconfig(ctx, first)
	if err != nil {
		return nil, err
	}
	what := "the nodes to join"
	if o.WaitReady {
		what = "every node to be Ready"
	}
	if err := ui.StepRun(fmt.Sprintf("Waiting from this machine for the API and %s", what), func() error {
		return o.wait(ctx, kubeconfig, ms)
	}); err != nil {
		return nil, err
	}
	return kubeconfig, nil
}

// joinConfigs renders the config.yaml of every machine but the first, which runs k3s: joining
// nodes get the tokens with the cluster CA's hash, so they check they found the right
// cluster before sending the password.
func (o *Options) joinConfigs(ctx context.Context, ms []*machine, agentToken string, sans []string) error {
	first := ms[0]
	joinToken, joinAgentToken, err := readTokens(ctx, first)
	if err != nil {
		return err
	}
	url := "https://" + net.JoinHostPort(cmp.Or(first.n.PrivateIP, first.n.Host), apiPort)
	for _, m := range ms[1:] {
		s := setup{cluster: o.Cluster, role: m.role(), name: m.n.Name, ip: m.n.PrivateIP, server: url, token: joinAgentToken, flags: m.flags, selinux: m.facts.enforcing()}
		if m.server {
			// The workers' password must be the same on every control plane (k3s docs).
			s.token, s.agentToken, s.sans = joinToken, agentToken, sans
		}
		if m.config, err = renderConfig(s, o.value); err != nil {
			return err
		}
	}
	return nil
}

// roll changes a running cluster one machine at a time, control planes first; a machine
// that runs the release as the file asks is left alone. It returns the cluster's admin
// kubeconfig once every node is back.
func (o *Options) roll(ctx context.Context, ms []*machine, vip, release string) ([]byte, error) {
	for _, m := range ms {
		// kube-vip's manifest follows the file on every control plane; k3s applies it as
		// it changes, no restart needed.
		if m.server {
			if err := putKubeVIP(ctx, m, vip); err != nil {
				return nil, err
			}
		}
		if m.upToDate(release) {
			ui.Debug("%s: k3s %s already runs as the file asks", m.label, release)
			continue
		}
		if err := o.Rolling(ctx, m.n, func() error {
			return o.ensure(ctx, m, release)
		}); err != nil {
			return nil, err
		}
	}
	kubeconfig, err := o.kubeconfig(ctx, ms[0])
	if err != nil {
		return nil, err
	}
	return kubeconfig, o.wait(ctx, kubeconfig, ms)
}

// upToDate: the machine runs the release, with the settings it should have.
func (m *machine) upToDate(release string) bool {
	return m.facts.version == release && sameSettings(m.facts.config, m.config) && m.facts.active[m.service()]
}

// machines lists the nodes control planes first: the first one starts the cluster.
func (o *Options) machines() ([]*machine, error) {
	var servers, agents []*machine
	for _, n := range o.Nodes {
		m := &machine{n: n, label: n.Label(), server: n.Role == config.RoleControlPlane}
		if n.Name == "" {
			return nil, fmt.Errorf("%s has no node name", n.Host)
		}
		if m.server {
			m.flags = withoutTokens(o.ServerFlags)
			servers = append(servers, m)
			continue
		}
		flags, ok := o.AgentFlags(n.Pool)
		if !ok {
			return nil, fmt.Errorf("worker %s belongs to pool %q, which the file doesn't have", m.label, n.Pool)
		}
		m.flags = flags
		agents = append(agents, m)
	}
	if len(servers) == 0 {
		return nil, errors.New("no control plane to install k3s on")
	}
	ms := append(servers, agents...)
	for _, m := range ms {
		for _, f := range m.flags {
			if _, ok := o.Secrets[f.Env]; f.Env != "" && !ok {
				return nil, fmt.Errorf("environment variable %s wasn't read before the install", f.Env)
			}
		}
	}
	return ms, nil
}

// kubeVIP is the script that puts kube-vip's DaemonSet in a control plane's manifests folder,
// where k3s deploys it; "" without kube-vip. One DaemonSet runs on every control plane, so
// their interfaces must share a name: tow won't guess between them.
func (o *Options) kubeVIP(ctx context.Context, ms []*machine) (string, error) {
	if o.KubeVIP == "" {
		return "", nil
	}
	var servers []*machine
	for _, m := range ms {
		if m.server {
			servers = append(servers, m)
		}
	}
	parallel.Each(servers, func(m *machine) {
		m.vipIface, m.err = kubevip.Interface(ctx, m.r, m.label, cmp.Or(m.n.PrivateIP, m.n.Host), o.KubeVIP)
	})
	if err := parallel.Errors(servers, "can't hold the API address"); err != nil {
		return "", err
	}
	iface := servers[0].vipIface
	var named []string
	for _, m := range servers {
		named = append(named, fmt.Sprintf("%s: %s", m.label, m.vipIface))
	}
	for _, m := range servers {
		if m.vipIface != iface {
			return "", fmt.Errorf("kube-vip runs as one DaemonSet on k3s, so the control planes' network interfaces must have the same name; nothing was changed:\n  %s\nRename them (netplan's set-name), or use kubeadm, where each control plane gets its own", strings.Join(named, "\n  "))
		}
	}
	ds, err := kubevip.DaemonSet(o.KubeVIP, iface, o.KubeVIPServices)
	if err != nil {
		return "", err
	}
	return kubevip.WriteScript(kubevip.K3sManifest, ds)
}

// putKubeVIP runs kubeVIP's script on a control plane, before k3s starts there.
func putKubeVIP(ctx context.Context, m *machine, script string) error {
	if script == "" {
		return nil
	}
	if out, err := m.r.RunInput(ctx, asRoot, "set -e\n"+script); err != nil {
		return fmt.Errorf("%s: could not put kube-vip in %s: %w\n%s", m.label, kubevip.K3sManifest, err, ui.LastLines(out, 10))
	}
	return nil
}

// withoutTokens leaves out the user's join tokens: tow places them itself.
func withoutTokens(flags []providers.Flag) []providers.Flag {
	return slices.DeleteFunc(slices.Clone(flags), func(f providers.Flag) bool { return f.Name == "token" || f.Name == "agent-token" })
}

// value is a flag's value, with its secret read from the environment beforehand.
func (o *Options) value(f providers.Flag) string {
	if f.Env == "" {
		return f.Value
	}
	return f.Value + o.Secrets[f.Env]
}

// tokens are the join passwords: the user's ({env}), else the ones the first control plane
// already runs with (a resumed install), else new ones. They never go into tow's state.
func (o *Options) tokens(first *machine) (server, agent string, err error) {
	for _, f := range o.ServerFlags {
		switch f.Name {
		case "token":
			server = o.Secrets[f.Env]
		case "agent-token":
			agent = o.Secrets[f.Env]
		}
	}
	prevServer, prevAgent := tokensIn(first.facts.config)
	server, agent = cmp.Or(server, prevServer), cmp.Or(agent, prevAgent)
	if server == "" {
		if server, err = newToken(); err != nil {
			return "", "", err
		}
	}
	if agent == "" {
		if agent, err = newToken(); err != nil {
			return "", "", err
		}
	}
	ui.RegisterSecret(server)
	ui.RegisterSecret(agent)
	return server, agent, nil
}

// sans are the names the API's certificate gets besides the file's certSANs: the address in
// front of the control planes, and each control plane's own addresses.
func (o *Options) sans(ms []*machine) []string {
	var out []string
	add := func(h string) {
		if h != "" && !slices.Contains(out, h) {
			out = append(out, h)
		}
	}
	for _, e := range []string{o.Endpoint, o.NodeEndpoint} {
		if e != "" {
			add(hostOf(e))
		}
	}
	add(o.APIName)
	for _, m := range ms {
		if m.server {
			add(m.n.Host)
			add(m.n.PrivateIP)
		}
	}
	return out
}

// check reads what the machine already has, changing nothing.
func (m *machine) check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, err := m.r.RunInput(ctx, asRoot, checkScript)
	f, complete := parseFacts(out)
	if !complete {
		if strings.Contains(out, "password is required") || strings.Contains(out, "sudo:") {
			return fmt.Errorf("sudo needs a password: tow installs unattended (allow '%s ALL=(ALL) NOPASSWD:ALL' in /etc/sudoers.d/)", m.n.User)
		}
		if err == nil {
			err = errors.New("the check did not finish")
		}
		return fmt.Errorf("%w\n%s", err, withoutFiles(out))
	}
	m.facts = f
	return nil
}

func parseFacts(out string) (facts, bool) {
	f := facts{active: map[string]bool{}}
	complete := false
	for _, line := range strings.Split(out, "\n") {
		key, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "installed":
			f.installed = v == "yes"
		case "version":
			f.version = v
		case "k3s", "k3s-agent":
			f.active[key] = v == "active"
		case "config":
			f.config, _ = base64.StdEncoding.DecodeString(v)
		case "record":
			f.record, _ = base64.StdEncoding.DecodeString(v)
		case "selinux":
			f.selinux = v
		case "complete":
			complete = true
		}
	}
	return f, complete
}

// withoutFiles drops the files the check read from its output: config.yaml holds passwords.
func withoutFiles(out string) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "config=") && !strings.HasPrefix(line, "record=") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}

// refuse stops before anything changes when a machine runs k3s tow didn't install for
// this cluster and this role: replacing it could destroy someone's cluster.
func (o *Options) refuse(ms []*machine) error {
	var problems []string
	for _, m := range ms {
		cluster, role, ours := owner(m.facts.config)
		switch {
		case ours && cluster != o.Cluster:
			problems = append(problems, fmt.Sprintf("%s: k3s here was installed by tow for cluster '%s'\n    → remove it first: sudo %s", m.label, cluster, uninstaller(role)))
		case ours && role != m.role():
			problems = append(problems, fmt.Sprintf("%s: k3s here runs as a %s, but the file makes this machine a %s\n    → remove it first: sudo %s", m.label, role, m.role(), uninstaller(role)))
		case !ours && m.facts.installed:
			problems = append(problems, fmt.Sprintf("%s: k3s is already installed here, and not by tow (%s has no tow line)\n    → use another machine, or remove it first: sudo %s (a worker: %s)", m.label, ConfigFile, uninstaller(roleServer), uninstaller(roleAgent)))
		}
		if m.facts.enforcing() && slices.ContainsFunc(m.flags, func(f providers.Flag) bool { return f.Name == "data-dir" }) {
			problems = append(problems, fmt.Sprintf("%s: SELinux enforces its policy here, and k3s doesn't support a custom data-dir under SELinux (k3s docs, SELinux support)\n    → drop data-dir from the file, or use a machine where SELinux doesn't enforce", m.label))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("k3s can't be installed on %s; nothing was changed:\n  ✗ %s", ui.Plural(len(problems), "machine"), strings.Join(problems, "\n  ✗ "))
}

// registrationOnly are the config.yaml keys k3s reads only when a node registers: changing
// them later changes nothing (k3s docs: node labels and taints), so a node that differs only
// there keeps running. apply changes a running node's labels and taints through the API.
var registrationOnly = []string{"node-label", "node-taint"}

// sameSettings compares a machine's config.yaml with the one it should have, the keys k3s
// reads only at registration aside.
func sameSettings(have, want []byte) bool {
	if bytes.Equal(have, want) {
		return true
	}
	haveMarker, _, _ := bytes.Cut(have, []byte("\n"))
	wantMarker, _, _ := bytes.Cut(want, []byte("\n"))
	var a, b map[string]any
	if !bytes.Equal(haveMarker, wantMarker) || yaml.Unmarshal(have, &a) != nil || yaml.Unmarshal(want, &b) != nil {
		return false
	}
	for _, k := range registrationOnly {
		delete(a, k)
		delete(b, k)
	}
	return reflect.DeepEqual(a, b)
}

// ensure installs k3s on a machine, or updates its settings, unless it already runs the
// release with the same settings.
func (o *Options) ensure(ctx context.Context, m *machine, release string) error {
	if m.upToDate(release) {
		ui.Debug("%s: k3s %s already runs as the file asks", m.label, release)
		return nil
	}
	record := ""
	if !recorded(m.facts.record, o.Cluster) {
		rec := prepare.Record{
			Cluster: o.Cluster,
			What:    "k3s (" + m.role() + ")",
			Detail:  fmt.Sprintf("k3s %s: /usr/local/bin/k3s, the %s service, settings in /etc/rancher/k3s, data in /var/lib/rancher/k3s", release, m.service()),
			Undo:    "sudo " + uninstaller(m.role()),
			// k3s's own uninstaller: it stops k3s and its containers, and removes its files,
			// network interfaces and rules. It keeps the node's password, for a node that
			// rejoins the same cluster: this one is gone, so it goes too. So do the folders
			// above k3s's own, unless something else keeps files there.
			Revert: "if [ -x " + uninstaller(m.role()) + " ]; then " + uninstaller(m.role()) + "; fi\n" +
				"rm -f /etc/rancher/node/password\n" +
				"rmdir /etc/rancher/node /etc/rancher /var/lib/rancher 2>/dev/null || true\n",
			Kubernetes: true,
			At:         time.Now().UTC(),
		}
		if o.APIName != "" {
			rec.Revert += prepare.HostsRevert(o.APIName)
		}
		var err error
		if record, err = prepare.RecordScript(m.facts.record, []prepare.Record{rec}); err != nil {
			return err
		}
	}
	if o.APIName != "" {
		record = prepare.HostsScript(o.APIName, o.apiNameTarget(m)) + record
	}
	script, err := installScript(m.config, release, m.exec(), record)
	if err != nil {
		return err
	}
	ui.Debug("%s: installing k3s %s as a %s", m.label, release, m.role())
	ctx2, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	if out, err := m.r.RunInput(ctx2, asRoot, script); err != nil {
		return fmt.Errorf("%s: installing k3s failed: %w\n%s%s", m.label, err, ui.LastLines(out, 15), m.logs(ctx))
	}
	return nil
}

// apiNameTarget is the address the API name stands for on a machine: a control plane's own
// (its k3s runs by then), the load balancer's on a worker.
func (o *Options) apiNameTarget(m *machine) string {
	if m.server {
		return cmp.Or(m.n.PrivateIP, m.n.Host)
	}
	return hostOf(o.NodeEndpoint)
}

// recorded reports whether the machine's record already lists k3s for this cluster.
func recorded(record []byte, cluster string) bool {
	var l struct {
		Changes []prepare.Record `json:"changes"`
	}
	if json.Unmarshal(record, &l) != nil {
		return false
	}
	return slices.ContainsFunc(l.Changes, func(r prepare.Record) bool {
		return r.Cluster == cluster && strings.HasPrefix(r.What, "k3s ")
	})
}

// logs is the end of the k3s service's log, to explain a failure.
func (m *machine) logs(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	out, err := m.r.RunInput(ctx, asRoot, fmt.Sprintf("journalctl -u %s --no-pager -n %d -o cat 2>/dev/null", m.service(), logLines))
	if err != nil || strings.TrimSpace(out) == "" {
		return ""
	}
	return fmt.Sprintf("\nLast lines of the %s log:\n%s", m.service(), ui.Indent(out))
}

// readTokens reads the join tokens from the first control plane: the secure K10 form, with
// the cluster CA's hash. Never printed, not even on error.
func readTokens(ctx context.Context, m *machine) (server, agent string, err error) {
	out, err := m.r.RunInput(ctx, asRoot, "cat "+tokenFile+"; echo; cat "+agentTokenFile+"; echo")
	if err != nil {
		return "", "", fmt.Errorf("%s: could not read the join tokens from %s: %w", m.label, tokenFile, err)
	}
	fields := strings.Fields(out)
	if len(fields) != 2 || !strings.HasPrefix(fields[0], "K10") || !strings.HasPrefix(fields[1], "K10") {
		return "", "", fmt.Errorf("%s: %s and %s don't hold k3s's secure join tokens", m.label, tokenFile, agentTokenFile)
	}
	ui.RegisterSecret(fields[0])
	ui.RegisterSecret(fields[1])
	return fields[0], fields[1], nil
}

// kubeconfig reads the admin kubeconfig k3s wrote and points it at the API address.
func (o *Options) kubeconfig(ctx context.Context, first *machine) ([]byte, error) {
	out, err := first.r.RunInput(ctx, asRoot, "cat "+kubeconfigFile)
	if err != nil {
		return nil, fmt.Errorf("%s: could not read %s: %w", first.label, kubeconfigFile, err)
	}
	endpoint := o.Endpoint
	if endpoint == "" {
		endpoint = net.JoinHostPort(first.n.Host, apiPort)
	}
	return kubecfg.Point([]byte(out), "https://"+endpoint)
}

// wait reaches the API the way the user will, then waits for every node and labels it.
func (o *Options) wait(ctx context.Context, kubeconfig []byte, ms []*machine) error {
	api, err := o.API(kubeconfig)
	if err != nil {
		return err
	}
	if err := api.WaitReachable(ctx); err != nil {
		return err
	}
	names := make([]string, len(ms))
	for i, m := range ms {
		names[i] = m.n.Name
	}
	if err := api.WaitNodes(ctx, names, false); err != nil {
		return err
	}
	if o.RoleLabels != nil {
		for _, m := range ms {
			if err := api.Label(ctx, m.n.Name, o.RoleLabels(m.n)); err != nil {
				return err
			}
		}
	}
	// k3s's own manifest sets no replica count, so k3s keeps this one when it applies the
	// manifest again (on each start); its spread rule puts the replicas on different machines.
	if o.CoreDNSReplicas > 0 {
		if err := api.ScaleUp(ctx, "kube-system", "coredns", o.CoreDNSReplicas); err != nil {
			return err
		}
	}
	if o.WaitReady {
		return api.WaitNodes(ctx, names, true)
	}
	return nil
}

// Describe sums up, for a dry run, what each machine's config.yaml would say. Secrets are
// listed apart, by where they come from, never in key: value form (the output's redaction
// would blank such a line).
func Describe(o Options) []string {
	source := func(name string) string {
		for _, f := range o.ServerFlags {
			if f.Name == name && f.Env != "" {
				return name + " from $" + f.Env
			}
		}
		return name + " generated by tow"
	}
	lines := []string{
		fmt.Sprintf("Then k3s %s would be installed with its official installer (sha256 checked), set up by %s on each machine:", Release(o.Version), ConfigFile),
		fmt.Sprintf("  join passwords, never shown: %s (control planes), %s (workers)", source("token"), source("agent-token")),
	}
	add := func(title string, s setup) {
		lines = append(lines, "  "+title)
		var secrets []string
		s.flags = slices.DeleteFunc(slices.Clone(s.flags), func(f providers.Flag) bool {
			if f.Env != "" {
				secrets = append(secrets, fmt.Sprintf("%s %s($%s)", f.Name, f.Value, f.Env))
			}
			return f.Env != ""
		})
		data, err := renderConfig(s, func(f providers.Flag) string { return f.Value })
		if err != nil {
			return
		}
		for _, l := range strings.Split(strings.TrimSpace(string(data)), "\n")[1:] { // without the marker
			lines = append(lines, "    "+l)
		}
		if len(secrets) > 0 {
			lines = append(lines, "    from environment variables, never shown: "+strings.Join(secrets, ", "))
		}
	}
	add("control planes: the first starts the cluster with embedded etcd (cluster-init); the others join it one at a time:", setup{
		cluster: o.Cluster, role: roleServer, name: "<the machine's name>", ip: "<its private address>", first: true,
		sans: []string{"<the API address and every control plane's addresses>"}, flags: withoutTokens(o.ServerFlags)})
	for _, p := range o.Pools {
		flags, _ := o.AgentFlags(p)
		add(fmt.Sprintf("workers of pool %s:", p), setup{
			cluster: o.Cluster, role: roleAgent, name: "<the machine's name>", ip: "<its private address>",
			server: "https://<the first control plane>:" + apiPort, flags: flags})
	}
	if o.RoleLabels != nil {
		groups := []struct {
			name string
			node providers.Node
		}{{"control planes", providers.Node{Role: config.RoleControlPlane}}}
		for _, p := range o.Pools {
			groups = append(groups, struct {
				name string
				node providers.Node
			}{"pool " + p, providers.Node{Role: config.RoleWorker, Pool: p}})
		}
		var roles []string
		for _, g := range groups {
			labels := o.RoleLabels(g.node)
			keys := slices.Sorted(maps.Keys(labels))
			if len(keys) > 0 {
				roles = append(roles, g.name+" "+strings.Join(keys, ", "))
			}
		}
		if len(roles) > 0 {
			lines = append(lines, "  role labels, set through the API once the nodes joined: "+strings.Join(roles, "; "))
		}
	}
	if o.CoreDNSReplicas > 0 {
		lines = append(lines, fmt.Sprintf("  CoreDNS scaled to %d replicas on different machines (k3s ships one: losing its machine would leave the cluster without DNS for minutes)", o.CoreDNSReplicas))
	}
	if o.KubeVIP != "" {
		lines = append(lines, fmt.Sprintf("  the kubeconfig reaches the API through %s, a virtual IP kube-vip %s holds on one control plane at a time (a DaemonSet in %s, put there before k3s starts; another control plane takes the address over when that one goes away)", net.JoinHostPort(o.KubeVIP, apiPort), kubevip.Version, kubevip.K3sManifest))
	}
	return lines
}

// Label and Err make a machine a parallel.Item.
func (m *machine) Label() string { return m.label }
func (m *machine) Err() error    { return m.err }
