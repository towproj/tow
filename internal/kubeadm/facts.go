package kubeadm

import (
	"cmp"
	"encoding/base64"
	"strings"

	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/remote"
)

// rhel reports a RHEL-family machine: dnf and rpm instead of apt and dpkg.
func (f facts) rhel() bool { return f.family == prepare.FamilyRHEL }

// facts are what a machine already has, as far as kubeadm's setup goes.
type facts struct {
	family         string            // prepare.FamilyDebian or prepare.FamilyRHEL: apt or dnf
	major          int               // the system's major release (10 for RHEL 10)
	arch           string            // dpkg's name for it: amd64, arm64
	pkgs           map[string]string // installed package → its version (kubelet, kubeadm, kubectl, cri-tools, cri-o)
	held           map[string]bool   // apt-mark hold; on RHEL, the repository's exclude list
	kubeRepo       string            // /etc/apt/sources.list.d/kubernetes.list or /etc/yum.repos.d/kubernetes.repo
	kubeletEnabled bool
	pause          string // the pause image kubeadm names for the release; "" until kubeadm is installed

	containerd runtime
	crio       runtime
	crioRepo   string // /etc/apt/sources.list.d/cri-o.list or /etc/yum.repos.d/cri-o.repo

	k3s         bool   // k3s is installed
	kubeletConf bool   // /etc/kubernetes/kubelet.conf: the machine is, or was, a node
	adminConf   bool   // /etc/kubernetes/admin.conf: a control plane's
	kubeadmDone string // the cluster whose kubeadm init or join finished here (kubeadmDone file)
	record      []byte
	complete    bool
}

// runtime is a container runtime found on the machine.
type runtime struct {
	path    string // its binary; "" = not installed
	version string // 2.3.7
	pkg     string // the package that installed it; "" = not from a package
	active  bool   // its service runs
	cri     bool   // containerd: the Kubernetes plugin (CRI) answers
	owner   string // the cluster tow set it up for, from its marker line; "" = not tow
	systemd bool   // containerd: SystemdCgroup = true
	pause   string // the pause image its config names (tow's drop-in for CRI-O)
}

// source says where a runtime came from, for messages.
func (r runtime) source() string {
	if r.pkg != "" {
		return "package " + r.pkg
	}
	return r.path
}

// Files tow writes, whose first line says which cluster they're for.
const (
	containerdConfig = "/etc/containerd/config.toml"
	crioDropIn       = "/etc/crio/crio.conf.d/20-tow.conf"
	markerPrefix     = "# written by tow for cluster "
	markerSuffix     = "; tow rewrites this file"
)

func marker(cluster string) string { return markerPrefix + cluster + markerSuffix }

// owner reads a marker line: the cluster, or "" when tow didn't write the file.
func owner(line string) string {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), markerPrefix)
	if !ok {
		return ""
	}
	return strings.TrimSuffix(rest, markerSuffix)
}

// asRoot runs the script on stdin as root (remote.AsRoot).
const asRoot = remote.AsRoot

// factsScript reads, changing nothing, what a machine has for a Kubernetes release (v1.37.2).
func factsScript(version string) string {
	return `# tow: read-only check before kubeadm's setup
[ -r /etc/os-release ] && . /etc/os-release
echo "os_id=${ID:-}"
echo "os_like=${ID_LIKE:-}"
echo "os_version=${VERSION_ID:-}"
# What owns a file: its package's name, or nothing.
owner() { dpkg -S "$1" 2>/dev/null | cut -d: -f1 | head -n1; }
if command -v dpkg-query >/dev/null 2>&1; then
  echo "arch=$(dpkg --print-architecture 2>/dev/null)"
  for p in kubelet kubeadm kubectl cri-tools cri-o; do
    echo "pkg:$p=$(dpkg-query -W -f='${db:Status-Status} ${Version}' "$p" 2>/dev/null)"
  done
  echo "held=$(apt-mark showhold 2>/dev/null | tr '\n' ' ')"
  echo "kube_repo=$(cat /etc/apt/sources.list.d/kubernetes.list 2>/dev/null | tr '\n' ' ')"
  echo "crio_repo=$(cat /etc/apt/sources.list.d/cri-o.list 2>/dev/null | tr '\n' ' ')"
elif command -v rpm >/dev/null 2>&1; then
  case "$(uname -m)" in x86_64) echo arch=amd64 ;; aarch64) echo arch=arm64 ;; *) echo "arch=$(uname -m)" ;; esac
  for p in kubelet kubeadm kubectl cri-tools cri-o; do
    echo "pkg:$p=$(rpm -q --qf 'installed %{VERSION}-%{RELEASE}' "$p" 2>/dev/null | grep '^installed ')"
  done
  # dnf holds them by the repository's exclude line (kubeadm's install page).
  echo "held=$(sed -n 's/^exclude=//p' /etc/yum.repos.d/kubernetes.repo 2>/dev/null | head -n1)"
  echo "kube_repo=$(cat /etc/yum.repos.d/kubernetes.repo 2>/dev/null | tr '\n' ' ')"
  echo "crio_repo=$(cat /etc/yum.repos.d/cri-o.repo 2>/dev/null | tr '\n' ' ')"
  owner() { rpm -qf --qf '%{NAME}\n' "$1" 2>/dev/null | head -n1 | grep -v ' '; }
fi
echo "kubelet_enabled=$(systemctl is-enabled kubelet 2>/dev/null)"
if command -v kubeadm >/dev/null 2>&1; then
  echo "pause=$(kubeadm config images list --kubernetes-version '` + version + `' 2>/dev/null | grep '/pause:' | head -n1)"
fi
c=$(command -v containerd 2>/dev/null || true)
[ -z "$c" ] && [ -x /usr/local/bin/containerd ] && c=/usr/local/bin/containerd
echo "containerd=$c"
if [ -n "$c" ]; then
  echo "containerd_version=$("$c" --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n1)"
  echo "containerd_package=$(owner "$c")"
  ctr="$(dirname "$c")/ctr"
  [ -x "$ctr" ] || ctr=$(command -v ctr 2>/dev/null || true)
  [ -n "$ctr" ] && echo "containerd_cri=$("$ctr" --address /run/containerd/containerd.sock plugins ls 2>/dev/null | awk '$1 == "io.containerd.grpc.v1" && $2 == "cri" {print $NF}')"
fi
echo "containerd_active=$(systemctl is-active containerd 2>/dev/null)"
echo "containerd_marker=$(head -n1 ` + containerdConfig + ` 2>/dev/null)"
echo "containerd_systemd=$(grep -cE '^[[:space:]]*SystemdCgroup = true$' ` + containerdConfig + ` 2>/dev/null)"
echo "containerd_pause=$(sed -n "s/^[[:space:]]*sandbox = '\([^']*\)'.*/\1/p" ` + containerdConfig + ` 2>/dev/null | head -n1)"
x=$(command -v crio 2>/dev/null || true)
echo "crio=$x"
if [ -n "$x" ]; then
  echo "crio_version=$("$x" --version 2>/dev/null | grep -oE '[0-9]+\.[0-9]+\.[0-9]+' | head -n1)"
  echo "crio_package=$(owner "$x")"
fi
echo "crio_active=$(systemctl is-active crio 2>/dev/null)"
echo "crio_marker=$(head -n1 ` + crioDropIn + ` 2>/dev/null)"
echo "crio_pause=$(sed -n 's/^[[:space:]]*pause_image[[:space:]]*=[[:space:]]*"\([^"]*\)".*/\1/p' ` + crioDropIn + ` 2>/dev/null | head -n1)"
echo "k3s=$([ -e /usr/local/bin/k3s ] && echo yes)"
echo "kubelet_conf=$([ -e /etc/kubernetes/kubelet.conf ] && echo yes)"
echo "admin_conf=$([ -e /etc/kubernetes/admin.conf ] && echo yes)"
echo "kubeadm_done=$(head -n1 /etc/tow/kubeadm-done 2>/dev/null)"
echo "record=$(base64 -w0 /etc/tow/prepared.json 2>/dev/null)"
echo complete=yes
`
}

func parseFacts(out string) facts {
	f := facts{pkgs: map[string]string{}, held: map[string]bool{}}
	var id, like, version string
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if name, isPkg := strings.CutPrefix(k, "pkg:"); isPkg {
			if status, version, _ := strings.Cut(v, " "); status == "installed" {
				f.pkgs[name] = version
			}
			continue
		}
		switch k {
		case "os_id":
			id = v
		case "os_like":
			like = v
		case "os_version":
			version = v
		case "arch":
			f.arch = v
		case "held":
			for _, p := range strings.Fields(v) {
				f.held[p] = true
			}
		case "kube_repo":
			f.kubeRepo = v
		case "kubelet_enabled":
			f.kubeletEnabled = v == "enabled"
		case "pause":
			f.pause = v
		case "containerd":
			f.containerd.path = v
		case "containerd_version":
			f.containerd.version = v
		case "containerd_package":
			f.containerd.pkg = v
		case "containerd_cri":
			f.containerd.cri = v == "ok"
		case "containerd_active":
			f.containerd.active = v == "active"
		case "containerd_marker":
			f.containerd.owner = owner(v)
		case "containerd_systemd":
			f.containerd.systemd = v != "" && v != "0"
		case "containerd_pause":
			f.containerd.pause = v
		case "crio":
			f.crio.path = v
		case "crio_version":
			f.crio.version = v
		case "crio_package":
			f.crio.pkg = v
		case "crio_active":
			f.crio.active = v == "active"
		case "crio_marker":
			f.crio.owner = owner(v)
		case "crio_pause":
			f.crio.pause = v
		case "crio_repo":
			f.crioRepo = v
		case "k3s":
			f.k3s = v == "yes"
		case "kubelet_conf":
			f.kubeletConf = v == "yes"
		case "admin_conf":
			f.adminConf = v == "yes"
		case "kubeadm_done":
			f.kubeadmDone = v
		case "record":
			f.record, _ = base64.StdEncoding.DecodeString(v)
		case "complete":
			f.complete = v == "yes"
		}
	}
	// A system kubeadm can't run on was refused by preparation already; a machine whose
	// family isn't known is read as apt's.
	f.family, _ = prepare.Family(id, like, version)
	f.family = cmp.Or(f.family, prepare.FamilyDebian)
	f.major = prepare.Major(version)
	return f
}

// withoutRecord drops the record from the check's output, for an error message.
func withoutRecord(out string) string {
	var keep []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if !strings.HasPrefix(line, "record=") {
			keep = append(keep, line)
		}
	}
	return strings.Join(keep, "\n")
}
