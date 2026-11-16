package byoh

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/prepare"
)

// requirements are what one node needs before Phase 2 can install its engine and role on it.
type requirements struct {
	what     string // "k3s server", for messages
	cpus     int
	memMB    int      // nominal size, as sold: "2 GB"
	diskGB   int      // free space under /var, where images and etcd live
	ports    []int    // TCP ports that must not be taken by something else already
	archs    []string // uname -m values the engine ships binaries for
	internet []string // reached over HTTPS: where Phase 2 downloads the engine from
}

// requirementsFor combines the distribution's minimum machine (config.MinimumFor) with the
// ports it listens on and where Phase 2 downloads it from.
// Ports come from docs.k3s.io and kubernetes.io/docs/reference/networking/ports-and-protocols.
func requirementsFor(engine, role, cni string) (requirements, error) {
	min, ok := config.MinimumFor(engine, role)
	if !ok {
		return requirements{}, fmt.Errorf("no machine requirements for distribution %q", engine)
	}
	r := requirements{what: min.What, cpus: min.CPUs, memMB: min.MemoryMB, diskGB: min.DiskGB, archs: min.Archs}
	cp := role == config.RoleControlPlane
	switch engine {
	case config.DistributionK3s:
		r.ports = []int{10250}
		if cp {
			r.ports = []int{6443, 10250}
		}
		r.internet = []string{"https://get.k3s.io", "https://github.com"}
	case config.DistributionKubeadm:
		r.ports = []int{10250, 10256}
		if cp {
			r.ports = []int{6443, 2379, 2380, 10250, 10257, 10259}
		}
		r.internet = []string{"https://pkgs.k8s.io", "https://registry.k8s.io"}
	}
	r.ports = append(r.ports, cniTCPPorts[cni]...)
	return r, nil
}

// minMemMB is the RAM the kernel must report for a machine of nominal size: firmware and the
// kernel keep some, so a "2 GB" machine shows about 1.9 GB. 83% matches kubeadm's own
// preflight, which accepts 1700 MB as "2 GB".
func minMemMB(nominal int) int { return nominal * 83 / 100 }

// cniTCPPorts are the TCP ports each CNI uses between all nodes: Calico's BGP, Cilium's health checks.
var cniTCPPorts = map[string][]int{
	"calico": {179},
	"cilium": {4240},
}

// cniUDPNote names the overlay traffic that can't be tested without starting a listener on
// the node, which a read-only check won't do.
var cniUDPNote = map[string]string{
	"flannel": "UDP 8472 (flannel VXLAN)",
	"cilium":  "UDP 8472 (Cilium VXLAN)",
	"calico":  "IP-in-IP (IP protocol 4) or UDP 4789 (Calico VXLAN)",
}

// factsScript gathers everything in one round trip, as the login user, and changes nothing:
// it only reads files and runs read-only commands. Output is key=value lines.
func factsScript(urls []string) string {
	return `# tow: read-only machine check
[ -r /etc/os-release ] && . /etc/os-release
echo "kernel=$(uname -s)"
echo "arch=$(uname -m)"
echo "os_id=${ID:-}"
echo "os_like=${ID_LIKE:-}"
echo "os_version=${VERSION_ID:-}"
echo "os_name=${PRETTY_NAME:-}"
echo "cpus=$(nproc 2>/dev/null)"
echo "mem_kb=$(awk '/^MemTotal:/ {print $2}' /proc/meminfo 2>/dev/null)"
echo "disk_kb=$(df -Pk /var 2>/dev/null | awk 'NR==2 {print $4}')"
echo "systemd=$([ -d /run/systemd/system ] && echo yes || echo no)"
if [ "$(id -u)" = 0 ]; then echo "sudo=root"
elif sudo -n true 2>/dev/null; then echo "sudo=yes"
else echo "sudo=no"; fi
echo "hostname=$(hostname 2>/dev/null || cat /proc/sys/kernel/hostname)"
echo "machine_id=$(cat /etc/machine-id 2>/dev/null)"
echo "ip=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "src") {print $(i+1); exit}}')"
if command -v ss >/dev/null 2>&1; then
  echo "listening=$(ss -Htln 2>/dev/null | awk '{n = split($4, a, ":"); print a[n]}' | sort -un | tr '\n' ' ')"
else echo "listening=unknown"; fi
echo "probe_tools=$(command -v bash >/dev/null 2>&1 && command -v timeout >/dev/null 2>&1 && echo yes || echo no)"
if command -v ufw >/dev/null 2>&1; then
  if [ "$(id -u)" = 0 ]; then u=$(ufw status 2>/dev/null | head -n1); else u=$(sudo -n ufw status 2>/dev/null | head -n1); fi
  case "$u" in *inactive*) echo "ufw=inactive" ;; *active*) echo "ufw=active" ;; *) echo "ufw=unknown" ;; esac
else echo "ufw=none"; fi
if pgrep -x firewalld >/dev/null 2>&1; then echo "firewalld=active"; else echo "firewalld=inactive"; fi
net=ok
for u in ` + strings.Join(urls, " ") + `; do
  if command -v curl >/dev/null 2>&1; then
    curl -sS -o /dev/null --max-time 10 "$u" 2>/dev/null || { net="fail $u"; break; }
  elif command -v wget >/dev/null 2>&1; then
    wget -q -O /dev/null -T 10 "$u" 2>/dev/null; rc=$?
    [ "$rc" -eq 0 ] || [ "$rc" -eq 8 ] || { net="fail $u"; break; }
  else net=notool; break; fi
done
echo "internet=$net"
`
}

type facts map[string]string

func parseFacts(out string) facts {
	f := facts{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			f[k] = strings.TrimSpace(v)
		}
	}
	return f
}

func (f facts) int(key string) (int, bool) {
	n, err := strconv.Atoi(f[key])
	return n, err == nil
}

// hostFirewall is the firewall that runs on the machine and that preparation opens for the
// cluster: ufw or firewalld; "" for none.
func (f facts) hostFirewall() string {
	switch {
	case f["ufw"] == "active":
		return "ufw"
	case f["firewalld"] == "active":
		return "firewalld"
	}
	return ""
}

// finding is one problem on one node. Warnings are reported but don't stop the apply.
type finding struct {
	check, problem, hint string
	warn                 bool
}

func fail(check, problem, hint string) finding {
	return finding{check: check, problem: problem, hint: hint}
}

// Kubernetes names nodes after their lowercased hostname, which must be a DNS subdomain.
var nodeName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

// evaluate compares one node's facts with what its role needs.
func evaluate(f facts, r requirements) []finding {
	var out []finding

	if k := f["kernel"]; k != "Linux" {
		return []finding{fail("os", fmt.Sprintf("kernel is %q, Kubernetes nodes must run Linux", k), "")}
	}
	// tow prepares each machine with its own system's tools: other systems stop here,
	// before anything changes.
	if _, err := prepare.Family(f["os_id"], f["os_like"], f["os_version"]); err != nil {
		out = append(out, fail("os", err.Error(), "use an Ubuntu, Debian, RHEL, Rocky or AlmaLinux machine (RHEL family: 9 or 10)"))
	}
	if a := f["arch"]; !contains(r.archs, a) {
		out = append(out, fail("arch", fmt.Sprintf("%s has no %s builds (supported: %s)", a, r.what, strings.Join(r.archs, ", ")), ""))
	}
	if f["systemd"] != "yes" {
		out = append(out, fail("systemd", "systemd is not running; the engine's services need it", ""))
	}

	if n, ok := f.int("cpus"); !ok {
		out = append(out, fail("cpu", "could not count CPUs (nproc failed)", ""))
	} else if n < r.cpus {
		out = append(out, fail("cpu", fmt.Sprintf("%d CPU(s), a %s needs at least %d", n, r.what, r.cpus), "give the machine more CPUs or use it as a worker"))
	}
	if kb, ok := f.int("mem_kb"); !ok {
		out = append(out, fail("memory", "could not read /proc/meminfo", ""))
	} else if mb := kb / 1024; mb < minMemMB(r.memMB) {
		out = append(out, fail("memory", fmt.Sprintf("%d MB of RAM, a %s needs at least %s", mb, r.what, sizeMB(r.memMB)), ""))
	}
	if kb, ok := f.int("disk_kb"); !ok {
		out = append(out, fail("disk", "could not read free space under /var", ""))
	} else if gb := kb / (1024 * 1024); gb < r.diskGB {
		out = append(out, fail("disk", fmt.Sprintf("%d GB free under /var, tow needs at least %d GB on a %s", gb, r.diskGB, r.what), "free up space or grow the disk"))
	}

	if f["sudo"] == "no" {
		out = append(out, fail("sudo", "the SSH user can't run sudo without a password; tow installs unattended",
			"allow it with a sudoers rule such as '<user> ALL=(ALL) NOPASSWD:ALL' in /etc/sudoers.d/, or connect as root"))
	}

	switch net := f["internet"]; {
	case net == "notool":
		out = append(out, fail("internet", "neither curl nor wget is installed, so downloads can't be tested (or made)", "install curl"))
	case strings.HasPrefix(net, "fail "):
		out = append(out, fail("internet", "can't reach "+strings.TrimPrefix(net, "fail ")+" over HTTPS", "check DNS, the default route and any proxy or firewall on the way out"))
	case net != "ok":
		out = append(out, fail("internet", "internet check did not run", ""))
	}

	if l := f["listening"]; l == "unknown" {
		out = append(out, finding{check: "ports", problem: "'ss' is missing, so ports in use could not be checked", warn: true})
	} else {
		busy := map[string]bool{}
		for _, p := range strings.Fields(l) {
			busy[p] = true
		}
		var taken []string
		for _, p := range r.ports {
			if busy[strconv.Itoa(p)] {
				taken = append(taken, strconv.Itoa(p))
			}
		}
		if len(taken) > 0 {
			out = append(out, fail("ports", fmt.Sprintf("TCP port(s) %s already in use; a %s needs them", strings.Join(taken, ", "), r.what),
				"is Kubernetes already installed here? find the owner with 'sudo ss -tlnp'"))
		}
	}

	if h := strings.ToLower(f["hostname"]); !nodeName.MatchString(h) {
		out = append(out, fail("hostname", fmt.Sprintf("hostname %q is not a valid Kubernetes node name", f["hostname"]), "use lowercase letters, digits, '-' and '.'"))
	}
	if f["ip"] == "" {
		out = append(out, fail("network", "no default IPv4 route, so the node has no address for other nodes to reach", ""))
	}
	return out
}

// peer is one address and port a node must reach on another node.
type peer struct {
	node string // display name of the other node
	ip   string
	port int
}

// peersFor lists the TCP ports node i must reach on the other nodes, per the engines' port
// tables: every control plane's API server; the kubelet (k3s: from every node, kubeadm: from
// control planes); etcd between HA control planes; and the CNI's own ports between all nodes.
func peersFor(i int, nodes []nodeView, engine, cni string) []peer {
	var out []peer
	cps := 0
	for _, n := range nodes {
		if n.role == config.RoleControlPlane {
			cps++
		}
	}
	from := nodes[i]
	fromCP := from.role == config.RoleControlPlane
	for j, o := range nodes {
		if j == i || o.ip == "" {
			continue
		}
		toCP := o.role == config.RoleControlPlane
		var ports []int
		if toCP {
			ports = append(ports, 6443)
		}
		if engine == config.DistributionK3s || fromCP {
			ports = append(ports, 10250)
		}
		if fromCP && toCP && cps > 1 {
			ports = append(ports, 2379, 2380)
		}
		ports = append(ports, cniTCPPorts[cni]...)
		for _, p := range ports {
			out = append(out, peer{node: o.label, ip: o.ip, port: p})
		}
	}
	return out
}

// nodeView is what the cross-node checks need to know about each node.
type nodeView struct {
	label, role, ip, hostname, machineID string
	firewall                             string // the firewall on: ufw, firewalld or ""
}

// peerScript opens a TCP connection to each peer. Nothing listens on these ports before
// Phase 2, so "refused" is the expected answer: the packet got there and back. A timeout
// means something on the path drops the traffic.
func peerScript(peers []peer) string {
	var b strings.Builder
	b.WriteString(`# tow: read-only node-to-node check
probe() {
  out=$(timeout 5 bash -c "exec 3<>/dev/tcp/$1/$2" 2>&1); rc=$?
  if [ "$rc" -eq 0 ]; then r=open
  elif [ "$rc" -eq 124 ]; then r=timeout
  else case "$out" in *refused*) r=refused ;; *) r=error ;; esac; fi
  echo "$1:$2=$r"
}
`)
	for _, p := range peers {
		fmt.Fprintf(&b, "probe %s %d\n", p.ip, p.port)
	}
	return b.String()
}

// evaluatePeers reads the probes. A port that doesn't answer on a machine whose firewall is on
// (firewallOn, by address: ufw or firewalld) is that firewall's doing as far as tow can
// tell: preparing the machines opens it there, so it's only a warning, which says what else
// could block it.
func evaluatePeers(out string, peers []peer, firewallOn map[string]string) []finding {
	res := parseFacts(out)
	var blocked []string
	byFirewall := map[string][]string{}
	for _, p := range peers {
		switch res[fmt.Sprintf("%s:%d", p.ip, p.port)] {
		case "open", "refused":
		default:
			where := fmt.Sprintf("%s (%s) port %d", p.node, p.ip, p.port)
			if fw := firewallOn[p.ip]; fw != "" {
				byFirewall[fw] = append(byFirewall[fw], where)
			} else {
				blocked = append(blocked, where)
			}
		}
	}
	var found []finding
	if len(blocked) > 0 {
		found = append(found, fail("node-to-node", "can't reach "+strings.Join(blocked, ", "),
			"open these TCP ports between the nodes in any firewall (ufw, firewalld, cloud security groups)"))
	}
	for _, fw := range []string{"ufw", "firewalld"} {
		if where := byFirewall[fw]; len(where) > 0 {
			found = append(found, finding{check: "node-to-node", warn: true,
				problem: fw + " blocks " + strings.Join(where, ", ") + " for now; preparing the machines opens them in " + fw,
				hint:    firewallHint(fw)})
		}
	}
	return found
}

// firewallHint is what a port the machine's firewall blocks before preparation can't tell:
// whether something else blocks it too.
func firewallHint(fw string) string {
	return "tow can't test them past " + fw + " before then: if the install later stalls or fails with an unclear error (nodes not joining, the API not answering), something else blocks them too, such as a cloud security group or another firewall: open them there"
}

// evaluateCluster catches clones: Kubernetes needs every node's name, machine ID and IP to differ.
func evaluateCluster(nodes []nodeView) map[int][]finding {
	out := map[int][]finding{}
	dup := func(check string, key func(nodeView) string, hint string) {
		first := map[string]int{}
		for i, n := range nodes {
			k := key(n)
			if k == "" {
				continue
			}
			if j, seen := first[k]; seen {
				out[i] = append(out[i], fail(check, fmt.Sprintf("same %s as %s (%s)", check, nodes[j].label, k), hint))
				continue
			}
			first[k] = i
		}
	}
	dup("hostname", func(n nodeView) string { return strings.ToLower(n.hostname) }, "give each machine its own hostname with 'sudo hostnamectl set-hostname <name>'")
	dup("machine ID", func(n nodeView) string { return n.machineID }, "cloned machine? regenerate /etc/machine-id (see 'man machine-id')")
	dup("node IP", func(n nodeView) string { return n.ip }, "")
	return out
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func sizeMB(mb int) string {
	if mb%1024 == 0 {
		return fmt.Sprintf("%d GB", mb/1024)
	}
	return fmt.Sprintf("%d MB", mb)
}
