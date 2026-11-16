package kubeadm

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/kubevip"
	"github.com/towproj/tow/internal/parallel"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/ui"
)

// Sources (read 2026-11-10), followed here:
//   - Creating a cluster with kubeadm: init on the first control plane, then join, the pod
//     network on top; "kubeadm reset" undoes init or join, and is how a failed one is retried
//     https://kubernetes.io/docs/setup/production-environment/tools/kubeadm/create-cluster-kubeadm/
//   - kubeadm's v1beta4 config (above, config.go); 'kubeadm config validate'
//   - kubeadm join: token discovery with the CA's public key hash, so a node checks the
//     cluster before trusting it  https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-join/
//   - bootstrap tokens: 'kubeadm token create' and 'kubeadm token delete'
//     https://kubernetes.io/docs/reference/setup-tools/kubeadm/kubeadm-token/

// What the machine's record calls kubeadm's run there. It's written before kubeadm runs, so
// a machine where it failed is known to be tow's to reset and retry.
const (
	whatControlPlane = "kubeadm (control plane)"
	whatWorker       = "kubeadm (worker)"

	adminConf = "/etc/kubernetes/admin.conf"
	caCert    = "/etc/kubernetes/pki/ca.crt"
	logLines  = 40
	// stoppedLogs containers that stopped get their last stoppedLines lines shown.
	stoppedLogs, stoppedLines = 3, 15

	// kubeadmDone names the cluster once kubeadm init or join finished on the machine. Its
	// files alone don't say so: kubeadm writes them, and starts the kubelet, before it waits
	// for the control plane, so a failed run leaves them too.
	kubeadmDone = "/etc/tow/kubeadm-done"
)

// started reports whether tow ran kubeadm init or join on the machine for this cluster.
func (o Options) started(f facts) bool {
	return o.recorded(f, whatControlPlane) || o.recorded(f, whatWorker)
}

// joined reports whether kubeadm finished on the machine for this cluster and its files are
// still there: the kubelet's credentials, and a control plane's admin kubeconfig.
func (o Options) joined(f facts, controlPlane bool) bool {
	return f.kubeadmDone == o.Cluster && f.kubeletConf && (!controlPlane || f.adminConf)
}

// node is one machine of the cluster, with what the file asks of it.
type node struct {
	*machine
	controlPlane bool
	settings     NodeSettings
	config       []byte
	vipIface     string // where kube-vip announces the virtual IP on a control plane
}

func (n *node) done(o Options) bool { return o.started(n.facts) && o.joined(n.facts, n.controlPlane) }

// Install sets up the cluster on machines Setup got ready: kubeadm init on the control plane,
// then the workers join, all at once. Every machine's kubeadm config is checked by kubeadm
// first, so nothing starts when one is wrong. The join token is tow's own, lives an hour at
// most, and is deleted once the workers joined. It returns the admin kubeconfig, pointing at
// the API address, once the API answers from this machine and every node registered (each
// with its role labels). Machines kubeadm already finished on for this cluster are left
// alone; one where it failed is reset (kubeadm reset) and tried again.
func Install(ctx context.Context, o Options) ([]byte, error) {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
	if o.API == nil {
		o.API = func(kubeconfig []byte) (API, error) { return kubeapi.New(kubeconfig) }
	}

	var cps, workers []*node
	for _, n := range o.Nodes {
		if n.Name == "" {
			return nil, fmt.Errorf("%s has no node name", n.Host)
		}
		s, ok := o.Node(n)
		if !ok {
			return nil, fmt.Errorf("worker %s belongs to pool %q, which the file doesn't have", n.Label(), n.Pool)
		}
		x := &node{machine: &machine{n: n, label: n.Label()}, controlPlane: n.Role == config.RoleControlPlane, settings: s}
		if x.controlPlane {
			cps = append(cps, x)
		} else {
			workers = append(workers, x)
		}
	}
	if len(cps) == 0 {
		return nil, errors.New("no control plane to run kubeadm init on")
	}
	first, others := cps[0], cps[1:]
	all := slices.Concat(cps, workers)
	joiners := slices.Concat(others, workers)
	ms := make([]*machine, len(all))
	for i, x := range all {
		ms[i] = x.machine
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
		return nil, err
	}
	if err := o.refuse(ms); err != nil {
		return nil, err
	}
	if o.KubeVIP != "" {
		parallel.Each(ms, func(m *machine) {
			for _, x := range cps {
				if x.machine == m {
					x.vipIface, m.err = kubevip.Interface(ctx, m.r, m.label, cmp.Or(m.n.PrivateIP, m.n.Host), o.KubeVIP)
				}
			}
		})
		if err := parallel.Errors(ms, "can't hold the API address"); err != nil {
			return nil, err
		}
	}

	// Every config, checked by kubeadm itself before anything starts. The joining nodes' carry
	// placeholders for what only exists once the cluster does: the token's format and the
	// CA's hash are all kubeadm checks. Several control planes share their certificates
	// through kubeadm, encrypted with a key tow makes; like the token, it's never saved.
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	ui.RegisterSecret(token)
	certKey := ""
	if len(others) > 0 {
		if certKey, err = newCertificateKey(); err != nil {
			return nil, err
		}
		ui.RegisterSecret(certKey)
	}
	server := endpoint(cmp.Or(first.n.PrivateIP, first.n.Host))
	if first.config, err = o.initConfig(first.n, first.settings, token, certKey); err != nil {
		return nil, err
	}
	for _, x := range joiners {
		if x.config, err = o.joinConfig(x.n, x.settings, server, token, "sha256:"+strings.Repeat("0", 64), certKey); err != nil {
			return nil, err
		}
	}
	if err := ui.StepRun("Checking each machine's kubeadm config with 'kubeadm config validate'", func() error {
		parallel.Each(ms, func(m *machine) {
			for _, x := range all {
				if x.machine == m {
					m.err = o.validate(ctx, x)
				}
			}
		})
		return parallel.Errors(ms, "kubeadm refused the config for")
	}); err != nil {
		return nil, err
	}

	// The first control plane. One kubeadm (re)initializes is a new cluster, with a new CA:
	// every other node joins it, even one that had joined the cluster before. Your own load
	// balancer is tested first, as kubeadm's HA page does.
	reinit := !first.done(o)
	var cpTodo, todo []*node
	for _, x := range others {
		if reinit || !x.done(o) {
			cpTodo = append(cpTodo, x)
		}
	}
	for _, w := range workers {
		if reinit || !w.done(o) {
			todo = append(todo, w)
		}
	}
	if reinit && o.CheckEndpoint {
		if err := o.checkEndpoint(ctx, first); err != nil {
			return nil, err
		}
	}
	// The token tow made is deleted once the nodes joined, and the certificates kubeadm
	// uploaded once the control planes did, or on any way out before.
	tokenLive, certsLive := false, false
	endToken := func() {
		if tokenLive {
			o.deleteToken(ctx, first, token)
			tokenLive = false
		}
	}
	endCerts := func() {
		if certsLive {
			o.deleteCerts(ctx, first)
			certsLive = false
		}
	}
	defer endToken()
	defer endCerts()
	if reinit {
		command := "init --skip-token-print"
		if certKey != "" {
			command += " --upload-certs"
		}
		// kube-vip goes in first: kubeadm init waits for the API on the virtual IP. It reads
		// super-admin.conf meanwhile, then admin.conf once init made that usable.
		pre := ""
		if o.KubeVIP != "" {
			if pre, err = o.kubeVIPScript(first, kubevip.SuperAdminConf); err != nil {
				return nil, err
			}
		}
		if err := ui.StepRun(fmt.Sprintf("Starting the cluster: kubeadm init %s on the first control plane, %s", o.Version, first.label), func() error {
			return o.kubeadm(ctx, first, command, pre)
		}); err != nil {
			return nil, err
		}
		tokenLive, certsLive = true, certKey != ""
		if err := o.ensureKubeVIP(ctx, first); err != nil {
			return nil, err
		}
	} else {
		// A resumed install: init's token may have expired, and kubeadm deletes the uploaded
		// certificates after two hours.
		if len(cpTodo)+len(todo) > 0 {
			if out, err := first.r.RunInput(ctx, asRoot, fmt.Sprintf("kubeadm token create '%s' --ttl 1h --description '%s' >/dev/null", token, tokenDescription)); err != nil {
				return nil, fmt.Errorf("%s: kubeadm token create: %w\n%s", first.label, err, ui.LastLines(out, 10))
			}
			tokenLive = true
		}
		if len(cpTodo) > 0 {
			if out, err := first.r.RunInput(ctx, asRoot, fmt.Sprintf("kubeadm init phase upload-certs --upload-certs --certificate-key '%s' >/dev/null", certKey)); err != nil {
				return nil, fmt.Errorf("%s: kubeadm init phase upload-certs: %w\n%s", first.label, err, ui.LastLines(out, 10))
			}
			certsLive = true
		}
	}

	// Everything that joins checks the cluster's CA before it trusts it.
	caHash := ""
	if len(cpTodo)+len(todo) > 0 {
		if caHash, err = o.caHash(ctx, first); err != nil {
			return nil, err
		}
	}

	// The other control planes, one at a time: each adds a member to etcd.
	for i, x := range cpTodo {
		if x.config, err = o.joinConfig(x.n, x.settings, server, token, caHash, certKey); err != nil {
			return nil, err
		}
		if err := ui.StepRun(fmt.Sprintf("Joining control plane %d of %d, %s", i+1, len(cpTodo), x.label), func() error {
			return o.kubeadm(ctx, x, "join", o.waitVIP())
		}); err != nil {
			return nil, err
		}
	}
	endCerts()
	// kube-vip on every control plane, once each joined: any of them can then take the
	// virtual IP over. A resumed install puts back what's missing.
	for _, x := range cps {
		if err := o.ensureKubeVIP(ctx, x); err != nil {
			return nil, err
		}
	}

	// The workers, all at once.
	if len(todo) > 0 {
		for _, w := range todo {
			if w.config, err = o.joinConfig(w.n, w.settings, server, token, caHash, ""); err != nil {
				return nil, err
			}
		}
		err = ui.StepRun(fmt.Sprintf("Joining %s", ui.Plural(len(todo), "worker")), func() error {
			wm := make([]*machine, len(todo))
			for i, w := range todo {
				wm[i] = w.machine
			}
			parallel.Each(wm, func(m *machine) {
				for _, w := range todo {
					if w.machine == m {
						m.err = o.kubeadm(ctx, w, "join", o.waitVIP())
					}
				}
			})
			return parallel.Errors(wm, "could not join")
		})
		endToken()
		if err != nil {
			return nil, err
		}
	}
	endToken()

	kubeconfig, err := o.kubeconfig(ctx, first)
	if err != nil {
		return nil, err
	}
	if err := ui.StepRun("Waiting from this machine for the API and the nodes to register", func() error {
		return o.wait(ctx, kubeconfig, all)
	}); err != nil {
		return nil, err
	}
	return kubeconfig, nil
}

// LeaveEtcd takes a control plane that leaves a running cluster out of etcd, from the machine
// itself, before kubeadm reset (which tow's records run without this phase, for delete):
// "kubeadm reset phase remove-etcd-member". A machine where kubeadm never ran has nothing to
// leave.
func LeaveEtcd(ctx context.Context, n providers.Node, knownHosts string, connect func(ctx context.Context, n providers.Node, knownHosts string) (prepare.Runner, error)) error {
	if connect == nil {
		connect = prepare.Connect
	}
	r, err := connect(ctx, n, knownHosts)
	if err != nil {
		return err
	}
	defer r.Close()
	if out, err := r.RunInput(ctx, asRoot, "if [ -f /etc/kubernetes/admin.conf ]; then kubeadm reset phase remove-etcd-member --kubeconfig /etc/kubernetes/admin.conf; fi\n"); err != nil {
		return fmt.Errorf("%s: leave etcd: %w\n%s", n.Label(), err, strings.TrimSpace(out))
	}
	return nil
}

// heredocwrites a kubeadm config into a root-only temporary file, removed when the script
// ends: it may hold secrets (the token, {env} flags).
func heredoc(cfg []byte) (string, error) {
	if bytes.Contains(cfg, []byte(heredocEnd)) {
		return "", fmt.Errorf("the kubeadm config contains the heredoc marker %s", heredocEnd)
	}
	return `cfg=$(mktemp)
trap 'rm -f "$cfg"' EXIT
cat > "$cfg" <<'` + heredocEnd + `'
` + string(cfg) + heredocEnd + `
`, nil
}

// validate has kubeadm check the machine's config, changing nothing.
func (o Options) validate(ctx context.Context, x *node) error {
	doc, err := heredoc(x.config)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if out, err := x.r.RunInput(ctx, asRoot, "set -e\n"+doc+`kubeadm config validate --config "$cfg"`+"\n"); err != nil {
		return fmt.Errorf("%w\n%s", err, ui.LastLines(out, 15))
	}
	return nil
}

// kubeadm runs kubeadm init or join on a machine. The run is recorded first; a machine where
// an earlier run for this cluster failed is reset before it's tried again. pre runs right
// before kubeadm, after the reset (which empties /etc/kubernetes/manifests).
func (o Options) kubeadm(ctx context.Context, x *node, command, pre string) error {
	var b strings.Builder
	b.WriteString("set -e\nrm -f " + kubeadmDone + "\n")
	if o.started(x.facts) {
		fmt.Fprintf(&b, "kubeadm reset -f --cri-socket '%s'\n", criSocket(o.Runtime))
	}
	b.WriteString(pre)
	what, role := whatWorker, "worker"
	if x.controlPlane {
		what, role = whatControlPlane, "control plane"
	}
	hostsAfter := ""
	if o.APIName != "" {
		b.WriteString(prepare.HostsScript(o.APIName, o.apiNameTarget(x, strings.HasPrefix(command, "init"))))
		if x.controlPlane {
			hostsAfter = prepare.HostsScript(o.APIName, ownAddress(x.n))
		}
	}
	revertHosts := ""
	if o.APIName != "" {
		revertHosts = prepare.HostsRevert(o.APIName)
	}
	if !o.recorded(x.facts, what) {
		rec, err := prepare.RecordScript(x.facts.record, []prepare.Record{{
			Cluster: o.Cluster, What: what, At: time.Now().UTC(),
			Detail: fmt.Sprintf("Kubernetes %s %s, node %s: kubeadm %s; its files in /etc/kubernetes and /var/lib/kubelet", o.Version, role, x.n.Name, strings.Fields(command)[0]),
			Undo:   "sudo kubeadm reset -f (then remove /etc/cni/net.d, as kubeadm reset says) and sudo rm -f " + kubeadmDone,
			// The whole cluster goes, so no etcd member is taken out of it first.
			Revert: "if command -v kubeadm >/dev/null 2>&1; then kubeadm reset -f --skip-phases remove-etcd-member --cri-socket '" + criSocket(o.Runtime) + "'; fi\n" +
				"rm -rf /etc/cni/net.d\nrm -f " + kubeadmDone + "\n" + revertHosts,
			Kubernetes: true,
		}})
		if err != nil {
			return err
		}
		b.WriteString(rec)
	}
	doc, err := heredoc(x.config)
	if err != nil {
		return err
	}
	b.WriteString(doc)
	fmt.Fprintf(&b, "kubeadm %s --config \"$cfg\"\n", command)
	b.WriteString(hostsAfter)
	fmt.Fprintf(&b, "echo '%s' > %s\n", o.Cluster, kubeadmDone)

	ctx2, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	if out, err := x.r.RunInput(ctx2, asRoot, b.String()); err != nil {
		return fmt.Errorf("%s: kubeadm %s failed: %w\n%s%s\nThe next 'tow apply' resets this machine (kubeadm reset) and tries again", x.label, strings.Fields(command)[0], err, ui.LastLines(out, 25), o.logs(ctx, x.machine))
	}
	return nil
}

// apiNameTarget is the address the API name stands for on a machine while kubeadm runs there:
// the first control plane's own, for init and for every control plane that joins (its own API
// server doesn't run yet, and the load balancer would hand it back to itself), the load
// balancer's on a worker.
func (o Options) apiNameTarget(x *node, init bool) string {
	switch {
	case init:
		return ownAddress(x.n)
	case x.controlPlane:
		for _, n := range o.Nodes {
			if n.Role == config.RoleControlPlane {
				return ownAddress(n)
			}
		}
	}
	return o.APIAddress
}

// ownAddress is the address a node's API server answers on, for the other machines.
func ownAddress(n providers.Node) string { return cmp.Or(n.PrivateIP, n.Host) }

// kubeVIPScript puts a control plane's kube-vip static pod in place, reading kubeconfig.
func (o Options) kubeVIPScript(x *node, kubeconfig string) (string, error) {
	pod, err := kubevip.StaticPod(o.KubeVIP, x.vipIface, kubeconfig, o.KubeVIPServices)
	if err != nil {
		return "", err
	}
	return kubevip.WriteScript(kubevip.StaticPodFile, pod)
}

// waitVIP has a joining machine wait, for up to 2 minutes, until the API answers on kube-vip's
// virtual IP: right after init, kube-vip restarts on the first control plane to read
// admin.conf, and the address goes unannounced for a few seconds ("no route to host").
func (o Options) waitVIP() string {
	if o.KubeVIP == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(o.ControlPlaneEndpoint)
	if err != nil {
		host, port = o.ControlPlaneEndpoint, "6443"
	}
	return fmt.Sprintf(`i=0
until timeout 3 bash -c 'exec 3<>/dev/tcp/%s/%s' 2>/dev/null; do
  i=$((i + 1))
  [ "$i" -lt 40 ] || { echo "the API's virtual IP %s doesn't answer on port %s"; exit 1; }
  sleep 3
done
`, host, port, host, port)
}

// ensureKubeVIP gives a control plane that kubeadm finished on its kube-vip, reading
// admin.conf; nothing changes when it already runs so.
func (o Options) ensureKubeVIP(ctx context.Context, x *node) error {
	if o.KubeVIP == "" {
		return nil
	}
	script, err := o.kubeVIPScript(x, kubevip.AdminConf)
	if err != nil {
		return err
	}
	if out, err := x.r.RunInput(ctx, asRoot, "set -e\n"+script); err != nil {
		return fmt.Errorf("%s: could not put kube-vip in %s: %w\n%s", x.label, kubevip.StaticPodFile, err, ui.LastLines(out, 10))
	}
	return nil
}

// logs are the kubelet's last lines, the containers the runtime knows, and the last lines of
// the ones that stopped (a control-plane component that crashed, usually), to explain a
// failure.
func (o Options) logs(ctx context.Context, m *machine) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	var b strings.Builder
	if out, err := m.r.RunInput(ctx, asRoot, fmt.Sprintf("journalctl -u kubelet --no-pager -n %d -o cat 2>/dev/null", logLines)); err == nil && strings.TrimSpace(out) != "" {
		b.WriteString("\nLast lines of the kubelet's log:\n" + ui.Indent(out))
	}
	crictl := fmt.Sprintf("crictl --runtime-endpoint '%s'", criSocket(o.Runtime))
	if out, err := m.r.RunInput(ctx, asRoot, crictl+" ps -a 2>/dev/null"); err == nil && strings.Count(out, "\n") > 1 {
		b.WriteString("\nContainers on the machine (crictl ps -a; 'sudo crictl logs <id>' shows one's log):\n" + ui.Indent(out))
	}
	stopped := fmt.Sprintf(`for id in $(%[1]s ps -a --state exited -q 2>/dev/null | head -n %[2]d); do
  echo "== $id"
  %[1]s logs --tail %[3]d "$id" 2>&1
done
`, crictl, stoppedLogs, stoppedLines)
	if out, err := m.r.RunInput(ctx, asRoot, stopped); err == nil && strings.TrimSpace(out) != "" {
		b.WriteString(fmt.Sprintf("\nLast lines of the containers that stopped (at most %d):\n", stoppedLogs) + ui.Indent(out))
	}
	return b.String()
}

// caHash is the hash of the cluster CA's public key, as kubeadm join's discovery takes it.
func (o Options) caHash(ctx context.Context, cp *node) (string, error) {
	out, err := cp.r.RunInput(ctx, asRoot, "cat "+caCert)
	if err != nil {
		return "", fmt.Errorf("%s: could not read %s: %w", cp.label, caCert, err)
	}
	block, _ := pem.Decode([]byte(out))
	if block == nil {
		return "", fmt.Errorf("%s: %s isn't a PEM certificate", cp.label, caCert)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("%s: %s: %w", cp.label, caCert, err)
	}
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// deleteToken ends the join token as soon as the workers joined (or failed to): the next run
// makes a new one.
func (o Options) deleteToken(ctx context.Context, cp *node, token string) {
	id, _, _ := strings.Cut(token, ".")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if out, err := cp.r.RunInput(ctx, asRoot, "kubeadm token delete '"+id+"' >/dev/null"); err != nil {
		ui.Warn(fmt.Sprintf("%s: the join token %s couldn't be deleted; it expires within the hour (%v: %s)", cp.label, id, err, strings.TrimSpace(out)))
	}
}

// deleteCerts ends the certificates kubeadm uploaded for the other control planes as soon as
// they joined (or failed to), rather than leave them the two hours kubeadm would.
func (o Options) deleteCerts(ctx context.Context, cp *node) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if out, err := cp.r.RunInput(ctx, asRoot, deleteCertsScript); err != nil {
		ui.Warn(fmt.Sprintf("%s: the certificates kubeadm uploaded (Secret kube-system/kubeadm-certs) couldn't be deleted; kubeadm deletes them within two hours (%v: %s)", cp.label, err, strings.TrimSpace(out)))
	}
}

const deleteCertsScript = "kubectl --kubeconfig " + adminConf + " -n kube-system delete secret kubeadm-certs --ignore-not-found >/dev/null"

// checkEndpoint has the first control plane reach the user's own load balancer before init,
// as kubeadm's HA page tests it: "A connection refused error is expected because the API
// server is not yet running. A timeout, however, means the load balancer cannot communicate
// with the control plane node."
func (o Options) checkEndpoint(ctx context.Context, x *node) error {
	host, port, err := net.SplitHostPort(o.ControlPlaneEndpoint)
	if err != nil || !hostName.MatchString(host) {
		return fmt.Errorf("the API address %q isn't host:port", o.ControlPlaneEndpoint)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := x.r.RunInput(ctx, asRoot, fmt.Sprintf(`out=$(timeout 5 bash -c 'exec 3<>/dev/tcp/%s/%s' 2>&1); rc=$?
case "$rc:$out" in
  0:*) echo open ;;
  124:*) echo timeout ;;
  *refused*) echo refused ;;
  *) echo "other: $out" ;;
esac
`, host, port))
	if err != nil {
		return fmt.Errorf("%s: could not test your load balancer %s: %w\n%s", x.label, o.ControlPlaneEndpoint, err, ui.LastLines(out, 5))
	}
	switch got := strings.TrimSpace(out); got {
	case "open", "refused":
		return nil
	case "timeout":
		return fmt.Errorf("%s can't reach your load balancer %s (no answer within 5 s); nothing was changed. It must forward TCP %s to the control planes, and the control planes must reach it: kubeadm's HA page says a timeout means \"the load balancer cannot communicate with the control plane node\"", x.label, o.ControlPlaneEndpoint, port)
	default:
		return fmt.Errorf("%s can't reach your load balancer %s; nothing was changed: %s", x.label, o.ControlPlaneEndpoint, strings.TrimPrefix(got, "other: "))
	}
}

// hostName is what may go into checkEndpoint's script: a DNS name or an IPv4/IPv6 address.
var hostName = regexp.MustCompile(`^[A-Za-z0-9.:-]+$`)

// kubeconfig reads the admin kubeconfig kubeadm wrote and points it at the API address.
func (o Options) kubeconfig(ctx context.Context, cp *node) ([]byte, error) {
	out, err := cp.r.RunInput(ctx, asRoot, "cat "+adminConf)
	if err != nil {
		return nil, fmt.Errorf("%s: could not read %s: %w", cp.label, adminConf, err)
	}
	server := o.Endpoint
	if server == "" {
		server = endpoint(cp.n.Host)
	}
	return kubecfg.Point([]byte(out), "https://"+server)
}

// wait reaches the API the way the user will, then waits for every node and labels it.
func (o Options) wait(ctx context.Context, kubeconfig []byte, all []*node) error {
	api, err := o.API(kubeconfig)
	if err != nil {
		return err
	}
	if err := api.WaitReachable(ctx); err != nil {
		return err
	}
	names := make([]string, len(all))
	for i, x := range all {
		names[i] = x.n.Name
	}
	if err := api.WaitNodes(ctx, names, false); err != nil {
		return err
	}
	for _, x := range all {
		if err := api.Label(ctx, x.n.Name, x.settings.RoleLabels); err != nil {
			return err
		}
	}
	// Before the CNI, so CoreDNS's pods are first placed under this rule.
	if len(all) > 1 {
		return api.SpreadOverNodes(ctx, "kube-system", "coredns")
	}
	return nil
}

// DescribeInstall sums up, for a dry run, how the cluster would be set up.
func DescribeInstall(o Options) []string {
	lines := []string{
		fmt.Sprintf("Then Kubernetes %s with kubeadm, after each machine's config is checked with 'kubeadm config validate':", o.Version),
		"  kubeadm init on the first control plane: cluster " + o.Cluster + ", pods " + o.PodCIDR + ", services " + o.ServiceCIDR + ", domain " + o.DNSDomain,
	}
	if n := o.ControlPlanes - 1; n > 0 {
		lines = append(lines, fmt.Sprintf("  then the %s join, one at a time; kubeadm shares the certificates with them, encrypted with a key tow makes, never saved and deleted once they joined", ui.Plural(n, "other control plane")))
	}
	lines = append(lines, "  the nodes join with a token tow makes, which lives an hour at most and is deleted once they joined; each checks the cluster's CA first")
	switch {
	case o.KubeVIP != "":
		lines = append(lines, fmt.Sprintf("  the nodes and the kubeconfig reach the API through %s, a virtual IP kube-vip %s holds on one control plane at a time (a static pod on each, put in before kubeadm runs; another takes the address over when that one goes away)", o.ControlPlaneEndpoint, kubevip.Version))
	case o.ControlPlaneEndpoint != "" && o.CheckEndpoint:
		lines = append(lines, "  the nodes reach the API through your load balancer "+o.ControlPlaneEndpoint+", tested from the first control plane before init")
	case o.ControlPlaneEndpoint != "":
		lines = append(lines, "  the nodes reach the API through "+o.ControlPlaneEndpoint)
	case o.ControlPlanes > 1:
		lines = append(lines, "  the nodes reach the API through the internal load balancer created in front of the control planes; yours is a public one (on GCP each machine finds the API by a name in its /etc/hosts: a worker the load balancer, a control plane itself)")
	}
	if o.SkipKubeProxy {
		lines = append(lines, "  without kube-proxy: the network replaces it")
	}
	if o.Schedulable {
		lines = append(lines, "  workloads may run on the control plane (no control-plane taint)")
	}
	if o.ServerTLSBootstrap {
		lines = append(lines, "  each kubelet's serving certificate signed by the cluster (serverTLSBootstrap), so metrics-server can check it; kubelet-csr-approver approves them for the cluster's nodes only")
	}
	var secrets []string
	for _, flags := range [][]providers.Flag{o.APIServer, o.ControllerManager, o.Scheduler, o.Etcd} {
		for _, f := range flags {
			if f.Env != "" {
				secrets = append(secrets, fmt.Sprintf("%s %s($%s)", f.Name, f.Value, f.Env))
			}
		}
	}
	if len(secrets) > 0 {
		lines = append(lines, "  from environment variables, never shown: "+strings.Join(secrets, ", "))
	}
	return lines
}
