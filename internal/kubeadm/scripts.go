package kubeadm

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/runtimes"
)

// What the machine's record calls each part, so a later run knows it's tow's.
const (
	whatPackages   = "Kubernetes packages"
	whatContainerd = "containerd"
	whatCRIO       = "CRI-O"
)

const heredocEnd = "TOW_EOF"

// kubeRepo is the Kubernetes package repository for one minor release (kubeadm docs): deb
// for apt, rpm for dnf.
func kubeRepo(minor int, rhel bool) string {
	return fmt.Sprintf("https://pkgs.k8s.io/core:/stable:/v1.%d/%s/", minor, repoKind(rhel))
}

// crioRepo is CRI-O's package repository for one minor release (CRI-O packaging).
func crioRepo(minor int, rhel bool) string {
	return fmt.Sprintf("https://download.opensuse.org/repositories/isv:/cri-o:/stable:/v1.%d/%s/", minor, repoKind(rhel))
}

func repoKind(rhel bool) string {
	if rhel {
		return "rpm"
	}
	return "deb"
}

// Where each family keeps the repositories tow adds.
const (
	kubeList = "/etc/apt/sources.list.d/kubernetes.list"
	kubeKey  = "/etc/apt/keyrings/kubernetes-apt-keyring.gpg"
	kubeRPM  = "/etc/yum.repos.d/kubernetes.repo"
	crioList = "/etc/apt/sources.list.d/cri-o.list"
	crioKey  = "/etc/apt/keyrings/cri-o-apt-keyring.gpg"
	crioRPM  = "/etc/yum.repos.d/cri-o.repo"
)

// kubePackages are what the Kubernetes repository's exclude line holds (kubeadm's install
// page, Red Hat-based): dnf leaves them alone until a command lifts it.
const kubePackages = "kubelet kubeadm kubectl cri-tools kubernetes-cni"

// rpmRepo writes a dnf repository file as the official install pages do; exclude holds
// packages back from ordinary updates ("" = none).
func rpmRepo(file, id, name, url, exclude string) string {
	s := fmt.Sprintf("cat > %s <<'%s'\n[%s]\nname=%s\nbaseurl=%s\nenabled=1\ngpgcheck=1\ngpgkey=%srepodata/repomd.xml.key\n", file, heredocEnd, id, name, url, url)
	if exclude != "" {
		s += "exclude=" + exclude + "\n"
	}
	return s + heredocEnd + "\n"
}

// dnfKube installs from the Kubernetes repository, its exclude line lifted for this command
// (kubeadm's install page). RHEL 10 and newer leave out weak dependencies, which would pull
// iptables in.
func dnfKube(major int) string {
	s := "dnf install -y -q --setopt=disable_excludes=kubernetes"
	if major >= 10 {
		s += " --setopt=install_weak_deps=False"
	}
	return s
}

// aptHelpers wait for another apt run (unattended-upgrades) instead of failing, and pick a
// package's exact version from the repository: the Debian revision (-1.1) isn't known ahead.
const aptHelpers = `export DEBIAN_FRONTEND=noninteractive
aptget() { apt-get -o DPkg::Lock::Timeout=600 "$@"; }
exact() { apt-cache madison "$1" | awk -v w="$2-" 'index($3, w) == 1 {print $3; exit}'; }
install -d -m 0755 /etc/apt/keyrings
`

// addRepo adds a signed apt repository the way the official install pages do.
func addRepo(name, url string) string {
	keyring := "/etc/apt/keyrings/" + name + "-apt-keyring.gpg"
	return fmt.Sprintf(`curl -fsSL --retry 3 --retry-delay 2 '%sRelease.key' | gpg --batch --yes --dearmor -o %s
echo 'deb [signed-by=%s] %s /' > /etc/apt/sources.list.d/%s.list
`, url, keyring, keyring, url, name)
}

// packagesScript installs kubelet, kubeadm and kubectl at the release, held there, and
// cri-tools (crictl), as the kubeadm install page does; the kubelet is enabled, and restarts
// every few seconds until kubeadm tells it what to do. On RHEL-family machines the
// repository's exclude line holds them, and dnf installs the release (any build of it).
func packagesScript(version string, f facts) string {
	want := strings.TrimPrefix(version, "v")
	if f.rhel() {
		return "set -e\n" + rpmRepo(kubeRPM, "kubernetes", "Kubernetes", kubeRepo(runtimes.KubeMinor(version), true), kubePackages) +
			fmt.Sprintf("%s kubelet-%[2]s kubeadm-%[2]s kubectl-%[2]s cri-tools\nsystemctl enable kubelet >/dev/null 2>&1\n", dnfKube(f.major), want)
	}
	var b strings.Builder
	b.WriteString("set -e\n" + aptHelpers + addRepo("kubernetes", kubeRepo(runtimes.KubeMinor(version), false)))
	fmt.Fprintf(&b, `aptget update -qq
pkgs=""
for p in kubelet kubeadm kubectl; do
  v=$(exact "$p" '%s')
  [ -n "$v" ] || { echo "pkgs.k8s.io has no $p %s: is v%s a Kubernetes release?"; exit 1; }
  pkgs="$pkgs $p=$v"
done
apt-mark unhold kubelet kubeadm kubectl >/dev/null 2>&1 || true
aptget install -y -qq --allow-downgrades $pkgs cri-tools
apt-mark hold kubelet kubeadm kubectl >/dev/null
systemctl enable kubelet >/dev/null 2>&1
`, want, want, want)
	return b.String()
}

func packagesRecord(version string, f facts) prepare.Record {
	minor := runtimes.KubeMinor(version)
	if f.rhel() {
		return prepare.Record{
			What: whatPackages,
			Detail: fmt.Sprintf("kubelet, kubeadm and kubectl %s (held by the repository's exclude line) and cri-tools, from %s (%s)",
				strings.TrimPrefix(version, "v"), kubeRepo(minor, true), kubeRPM),
			Undo:       "sudo dnf remove " + kubePackages + " && sudo rm " + kubeRPM,
			Revert:     prepare.RemoveRPMs(strings.Fields(kubePackages)) + "rm -f " + kubeRPM + "\n",
			Kubernetes: true,
		}
	}
	return prepare.Record{
		What: whatPackages,
		Detail: fmt.Sprintf("kubelet, kubeadm and kubectl %s (held at that release) and cri-tools, from %s (%s, key %s)",
			strings.TrimPrefix(version, "v"), kubeRepo(minor, false), kubeList, kubeKey),
		Undo: "sudo apt-mark unhold kubelet kubeadm kubectl && sudo apt-get purge " + kubePackages + " && sudo rm " + kubeList + " " + kubeKey,
		Revert: "apt-mark unhold kubelet kubeadm kubectl >/dev/null 2>&1 || true\n" +
			prepare.RemovePackages("purge", strings.Fields(kubePackages)) +
			"rm -f " + kubeList + " " + kubeKey + "\n",
		Kubernetes: true,
	}
}

// image is what a pause image's name may hold, so it can go into the scripts as it is.
var image = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]*$`)

// containerdScript installs a containerd release the way containerd's getting-started page
// does: the release tarball under /usr/local, runc, the systemd unit; its config is
// 'containerd config default' with the two settings Kubernetes asks for (container runtimes
// page): systemd cgroups, and the pause image kubeadm expects. The CRI plugin stays on, as
// the default config has it. A config.toml tow didn't write is kept beside it first.
func containerdScript(cluster, version, arch, pause string) (string, error) {
	if !image.MatchString(pause) {
		return "", fmt.Errorf("kubeadm named an unexpected pause image %q", pause)
	}
	if strings.Contains(runtimes.ContainerdService, heredocEnd) {
		return "", fmt.Errorf("containerd's unit contains the heredoc marker %s", heredocEnd)
	}
	v := strings.TrimPrefix(version, "v")
	tarball := fmt.Sprintf("containerd-%s-linux-%s.tar.gz", v, arch)
	url := runtimes.ContainerdURL(v, arch)

	// The pinned release is checked against the sum tow carries; another one against
	// the .sha256sum published beside it.
	var sums string
	if sum := runtimes.ContainerdSum(v, arch); sum != "" {
		sums = fmt.Sprintf("echo '%s  %s' >> \"$tmp/sums\"\n", sum, tarball)
	} else {
		sums = fmt.Sprintf("get %s.sha256sum '%s.sha256sum'\ncat \"$tmp/%s.sha256sum\" >> \"$tmp/sums\"\n", tarball, url, tarball)
	}
	sums += fmt.Sprintf("echo '%s  runc' >> \"$tmp/sums\"\n", runtimes.RuncSum(arch))

	// The config is claimed first, so a failed install is still known as tow's; one
	// tow already wrote stays whole until the new one is ready.
	m := marker(cluster)
	return `set -e
mkdir -p /etc/containerd
if ! head -n1 ` + containerdConfig + ` 2>/dev/null | grep -qF '` + markerPrefix + `'; then
  [ ! -f ` + containerdConfig + ` ] || cp -p ` + containerdConfig + ` ` + containerdConfig + `.tow.bak
  echo '` + m + `' > ` + containerdConfig + `
fi
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
get() { curl -fsSL --retry 3 --retry-delay 2 -o "$tmp/$1" "$2"; }
get ` + tarball + ` '` + url + `'
get runc '` + runtimes.RuncURL(arch) + `'
` + sums + `(cd "$tmp" && sha256sum -c --quiet sums)
systemctl stop containerd 2>/dev/null || true
tar -C /usr/local -xzf "$tmp/` + tarball + `"
install -m 0755 "$tmp/runc" /usr/local/sbin/runc
mkdir -p /usr/local/lib/systemd/system
cat > /usr/local/lib/systemd/system/containerd.service <<'` + heredocEnd + `'
` + runtimes.ContainerdService + heredocEnd + `
{ echo '` + m + `'; /usr/local/bin/containerd config default; } > "$tmp/config.toml"
sed -i -e 's/^\([[:space:]]*\)SystemdCgroup = false$/\1SystemdCgroup = true/' -e "s|^\([[:space:]]*\)sandbox = '[^']*'|\1sandbox = '` + pause + `'|" "$tmp/config.toml"
grep -qE '^[[:space:]]*SystemdCgroup = true$' "$tmp/config.toml" || { echo "containerd's default config has no SystemdCgroup setting to turn on"; exit 1; }
grep -qF "sandbox = '` + pause + `'" "$tmp/config.toml" || { echo "containerd's default config has no sandbox (pause) image setting"; exit 1; }
install -m 0644 "$tmp/config.toml" ` + containerdConfig + `
[ -e /etc/crictl.yaml ] || printf 'runtime-endpoint: unix:///run/containerd/containerd.sock\n' > /etc/crictl.yaml
systemctl daemon-reload
systemctl enable containerd >/dev/null 2>&1
systemctl restart containerd
`, nil
}

func containerdRecord(version string) prepare.Record {
	v := strings.TrimPrefix(version, "v")
	return prepare.Record{
		What: whatContainerd,
		Detail: fmt.Sprintf("containerd %s and runc %s from their GitHub releases (sha256 checked): /usr/local/bin/containerd*, /usr/local/bin/ctr, /usr/local/sbin/runc, "+
			"/usr/local/lib/systemd/system/containerd.service, %s (systemd cgroups, kubeadm's pause image; one tow didn't write is kept as config.toml.tow.bak), /etc/crictl.yaml if there was none",
			v, runtimes.Runc, containerdConfig),
		Undo: "sudo systemctl disable --now containerd && sudo rm -f /usr/local/bin/containerd /usr/local/bin/containerd-shim-runc-v2 /usr/local/bin/containerd-stress /usr/local/bin/ctr " +
			"/usr/local/sbin/runc /usr/local/lib/systemd/system/containerd.service && sudo rm -rf /etc/containerd /var/lib/containerd && sudo systemctl daemon-reload",
		// Containers' mounts go first (kubeadm reset stopped the containers), or their folders
		// can't be removed. A config that was there before tow comes back.
		Revert: "systemctl disable --now containerd >/dev/null 2>&1 || true\n" +
			"awk '$2 ~ \"^/(run|var/lib)/containerd/\" {print $2}' /proc/self/mounts | sort -r | while read -r m; do umount -l \"$m\" 2>/dev/null || true; done\n" +
			"rm -f /usr/local/bin/containerd /usr/local/bin/containerd-shim-runc-v2 /usr/local/bin/containerd-stress /usr/local/bin/ctr /usr/local/sbin/runc /usr/local/lib/systemd/system/containerd.service\n" +
			"if [ -f " + containerdConfig + ".tow.bak ]; then mv " + containerdConfig + ".tow.bak " + containerdConfig + "; else rm -rf /etc/containerd; fi\n" +
			"rm -rf /var/lib/containerd /run/containerd\n" +
			"[ \"$(cat /etc/crictl.yaml 2>/dev/null)\" != 'runtime-endpoint: unix:///run/containerd/containerd.sock' ] || rm -f /etc/crictl.yaml\n" +
			"systemctl daemon-reload\n",
		Kubernetes: true,
	}
}

// crioScript installs CRI-O from its project's packages for the Kubernetes minor, as CRI-O's
// packaging README does, with kubeadm's pause image in a drop-in (container runtimes page).
// The drop-in is written first: it tells tow's CRI-O from someone else's, even when the
// install stops halfway. version "" is the newest of the minor.
func crioScript(cluster string, minor int, version, pause string, f facts) (string, error) {
	if !image.MatchString(pause) {
		return "", fmt.Errorf("kubeadm named an unexpected pause image %q", pause)
	}
	dropIn := `mkdir -p /etc/crio/crio.conf.d
cat > ` + crioDropIn + ` <<'` + heredocEnd + `'
` + marker(cluster) + `
[crio.image]
pause_image = "` + pause + `"
` + heredocEnd + `
`
	const start = "systemctl daemon-reload\nsystemctl enable crio >/dev/null 2>&1\nsystemctl restart crio\n"
	v := strings.TrimPrefix(version, "v")
	if f.rhel() {
		pkg := "cri-o"
		if v != "" {
			pkg = "cri-o-" + v
		}
		return "set -e\n" + dropIn + rpmRepo(crioRPM, "cri-o", "CRI-O", crioRepo(minor, true), "") + "dnf install -y -q " + pkg + "\n" + start, nil
	}
	pkg := "cri-o"
	var pick string
	if version != "" {
		pick = fmt.Sprintf(`v=$(exact cri-o '%s')
[ -n "$v" ] || { echo "CRI-O's v1.%d repository has no cri-o %s"; exit 1; }
`, v, minor, v)
		pkg = `"cri-o=$v"`
	}
	return "set -e\n" + aptHelpers + dropIn + addRepo("cri-o", crioRepo(minor, false)) + "aptget update -qq\n" +
		pick + "aptget install -y -qq --allow-downgrades " + pkg + "\n" + start, nil
}

func crioRecord(minor int, f facts) prepare.Record {
	stop := "systemctl disable --now crio >/dev/null 2>&1 || true\n"
	if f.rhel() {
		return prepare.Record{
			What:       whatCRIO,
			Detail:     fmt.Sprintf("cri-o from %s (%s), kubeadm's pause image in %s", crioRepo(minor, true), crioRPM, crioDropIn),
			Undo:       "sudo systemctl disable --now crio && sudo dnf remove cri-o && sudo rm " + crioDropIn + " " + crioRPM,
			Revert:     stop + prepare.RemoveRPMs([]string{"cri-o"}) + "rm -f " + crioDropIn + " " + crioRPM + "\n",
			Kubernetes: true,
		}
	}
	return prepare.Record{
		What:       whatCRIO,
		Detail:     fmt.Sprintf("cri-o from %s (%s, key %s), kubeadm's pause image in %s", crioRepo(minor, false), crioList, crioKey, crioDropIn),
		Undo:       "sudo systemctl disable --now crio && sudo apt-get purge cri-o && sudo rm " + crioDropIn + " " + crioList + " " + crioKey,
		Revert:     stop + prepare.RemovePackages("purge", []string{"cri-o"}) + "rm -f " + crioDropIn + " " + crioList + " " + crioKey + "\n",
		Kubernetes: true,
	}
}
