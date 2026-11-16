package prepare

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// asRoot runs a script read from stdin as root: sudo for a login user, as is for root. The
// address the node sees this machine connect from ($SSH_CLIENT) is passed as $1, since sudo
// clears the environment.
const asRoot = `A="${SSH_CLIENT%% *}"; if [ "$(id -u)" -eq 0 ]; then exec sh -s -- "$A"; else exec sudo -n sh -s -- "$A"; fi`

// Facts are what a node has, as far as preparing it goes.
type Facts struct {
	OSID, OSLike, OSVersion string
	Kernel                  string // uname -r
	AdminIP                 string // this machine's address, as the node sees it

	SwapActive  []string // swapon
	SwapInFstab int      // active swap lines in /etc/fstab
	SwapUnits   []string // active systemd swap units
	Dphys       bool     // Raspberry Pi OS's dphys-swapfile is enabled

	SysctlNow, SysctlSaved map[string]string
	ModuleLoaded           map[string]bool
	ModuleSaved            map[string]bool
	Installed              map[string]bool

	UFW       string // active | inactive | none
	UFWRule   map[int]bool
	UFWAdded  []string // the cluster's rules, as 'ufw show added' prints them (ufw on only)
	Firewalld bool
	// firewalld, when it runs: its release, tow's zone (its target and sources) and
	// policy (its rich rules), and the zone each source the cluster needs is in.
	FWVersion      string
	FWZone         bool
	FWZoneTarget   string
	FWZoneSources  []string
	FWPolicy       bool
	FWPolicyRules  []string
	FWZoneOf       map[string]string
	NetworkManager bool
	CalicoNMConf   bool

	Multipathd    bool // multipathd is running
	MultipathMaps int  // devices multipath manages (a SAN)
	MultipathConf bool // tow's blacklist for Longhorn is there

	MemoryCgroup bool
	PiCmdline    string // where a Raspberry Pi's kernel command line lives
	MemoryMB     int    // MemTotal, in MB as kubeadm counts them (MiB)

	SELinux       string // getenforce: Enforcing, Permissive, Disabled; "" without SELinux
	SELinuxConfig string // SELINUX= in /etc/selinux/config: what the next boot gets

	ProductUUID, MAC string
	Prepared         []byte // /etc/tow/prepared.json, as found

	Complete bool // the script ran to the end
}

const (
	calicoNMConf  = "/etc/NetworkManager/conf.d/calico.conf"
	multipathConf = "/etc/multipath/conf.d/longhorn.conf" // multipath-tools reads /etc/multipath/conf.d
	modulesConf   = "/etc/modules-load.d/k8s.conf"
	sysctlConf    = "/etc/sysctl.d/k8s.conf"
)

// factsScript reads everything in one round trip, as root, and changes nothing. Output is
// key=value lines; ufw is asked with --dry-run whether each rule already exists, and lists
// the rules it has with the cluster's comment; firewalld lists tow's zone and policy.
func factsScript(n Needs, rules []ufwRule, fw firewalldWant, cluster string) string {
	var b strings.Builder
	b.WriteString(`# tow: read-only preparation check
ADMIN="$1"
[ -r /etc/os-release ] && . /etc/os-release
echo "os_id=${ID:-}"
echo "os_like=${ID_LIKE:-}"
echo "os_version=${VERSION_ID:-}"
echo "kernel=$(uname -r)"
echo "admin_ip=$ADMIN"
echo "swap_active=$(swapon --noheadings --show=NAME 2>/dev/null | tr '\n' ' ')"
echo "swap_fstab=$(awk '!/^[[:space:]]*#/ && $3 == "swap"' /etc/fstab 2>/dev/null | wc -l)"
echo "swap_units=$(systemctl list-units --type=swap --state=active --no-legend --plain 2>/dev/null | awk '{print $1}' | tr '\n' ' ')"
echo "dphys=$(systemctl is-enabled dphys-swapfile 2>/dev/null)"
`)
	for _, s := range n.Sysctls {
		re := regexp.QuoteMeta(s.Name)
		fmt.Fprintf(&b, "echo \"sysctl_now:%s=$(sysctl -n %s 2>/dev/null)\"\n", s.Name, s.Name)
		fmt.Fprintf(&b, "echo \"sysctl_saved:%s=$(grep -hsE '^[[:space:]]*%s[[:space:]]*=' /etc/sysctl.conf /etc/sysctl.d/*.conf /run/sysctl.d/*.conf /usr/lib/sysctl.d/*.conf | tail -n1 | cut -d= -f2 | tr -d ' \\t')\"\n", s.Name, re)
	}
	for _, m := range n.Modules {
		fmt.Fprintf(&b, "echo \"module_now:%s=$([ -d /sys/module/%s ] && echo yes || echo no)\"\n", m.Name, m.Name)
		// -q: without it grep exits 2 when one of the files is missing (a glob that matched
		// nothing, like /run/modules-load.d/*.conf on AWS), even when it found the line.
		fmt.Fprintf(&b, "echo \"module_saved:%s=$(grep -qsxE '[[:space:]]*%s[[:space:]]*' /etc/modules /etc/modules-load.d/*.conf /run/modules-load.d/*.conf /usr/lib/modules-load.d/*.conf && echo yes || echo no)\"\n", m.Name, m.Name)
	}
	b.WriteString(packageQuery(n.Packages))
	b.WriteString(`ufw=none
command -v ufw >/dev/null 2>&1 && ufw=$(ufw status 2>/dev/null | awk 'NR == 1 {print $2}')
echo "ufw=${ufw:-none}"
`)
	fmt.Fprintf(&b, `[ "$ufw" = active ] && echo "ufw_added=$(ufw show added 2>/dev/null | grep -F "comment '%s'" | base64 -w0)"
`, tag(cluster))
	if len(rules) > 0 {
		b.WriteString(`if [ "$ufw" = active ]; then
  has() { if ufw --dry-run "$@" 2>&1 | grep -q 'Skipping adding existing rule'; then echo yes; else echo no; fi; }
`)
		for i, r := range rules {
			if r.needsAdmin {
				fmt.Fprintf(&b, "  [ -n \"$ADMIN\" ] && echo \"ufw_rule_%d=$(has %s)\"\n", i, r.args)
			} else {
				fmt.Fprintf(&b, "  echo \"ufw_rule_%d=$(has %s)\"\n", i, r.args)
			}
		}
		b.WriteString("fi\n")
	}
	b.WriteString(firewalldFacts(fw))
	b.WriteString(`echo "networkmanager=$(systemctl is-active NetworkManager 2>/dev/null)"
echo "calico_nm=$([ -f ` + calicoNMConf + ` ] && echo yes || echo no)"
echo "multipathd=$(systemctl is-active multipathd 2>/dev/null)"
echo "multipath_maps=$(dmsetup ls --target multipath 2>/dev/null | grep -vc '^No devices found')"
echo "multipath_conf=$([ -f ` + multipathConf + ` ] && echo yes || echo no)"
if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
  mem=$(grep -qw memory /sys/fs/cgroup/cgroup.controllers && echo yes || echo no)
else
  mem=$(awk '$1 == "memory" {print ($4 == 1) ? "yes" : "no"}' /proc/cgroups 2>/dev/null)
fi
echo "memory_cgroup=${mem:-no}"
echo "memory_mb=$(awk '/^MemTotal:/ {print int($2 / 1024)}' /proc/meminfo 2>/dev/null)"
echo "selinux=$(getenforce 2>/dev/null)"
echo "selinux_config=$(sed -n 's/^SELINUX=//p' /etc/selinux/config 2>/dev/null | head -n1)"
for f in /boot/firmware/cmdline.txt /boot/cmdline.txt; do [ -f "$f" ] && { echo "pi_cmdline=$f"; break; }; done
echo "product_uuid=$(cat /sys/class/dmi/id/product_uuid 2>/dev/null)"
dev=$(ip -4 route get 1.1.1.1 2>/dev/null | awk '{for (i = 1; i < NF; i++) if ($i == "dev") {print $(i+1); exit}}')
[ -n "$dev" ] && echo "mac=$(cat /sys/class/net/$dev/address 2>/dev/null)"
echo "prepared=$(base64 -w0 /etc/tow/prepared.json 2>/dev/null)"
echo "done=yes"
`)
	return b.String()
}

func parseFacts(out string) Facts {
	f := Facts{
		SysctlNow: map[string]string{}, SysctlSaved: map[string]string{},
		ModuleLoaded: map[string]bool{}, ModuleSaved: map[string]bool{},
		Installed: map[string]bool{}, UFWRule: map[int]bool{},
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		if f.parseFirewalld(k, v) {
			continue
		}
		kind, name, _ := strings.Cut(k, ":")
		switch {
		case kind == "sysctl_now":
			f.SysctlNow[name] = v
		case kind == "sysctl_saved":
			f.SysctlSaved[name] = v
		case kind == "module_now":
			f.ModuleLoaded[name] = v == "yes"
		case kind == "module_saved":
			f.ModuleSaved[name] = v == "yes"
		case kind == "package":
			f.Installed[name] = v == "installed"
		case strings.HasPrefix(k, "ufw_rule_"):
			if i, err := strconv.Atoi(strings.TrimPrefix(k, "ufw_rule_")); err == nil {
				f.UFWRule[i] = v == "yes"
			}
		}
		switch k {
		case "os_id":
			f.OSID = v
		case "os_like":
			f.OSLike = v
		case "os_version":
			f.OSVersion = v
		case "kernel":
			f.Kernel = v
		case "admin_ip":
			f.AdminIP = v
		case "swap_active":
			f.SwapActive = strings.Fields(v)
		case "swap_fstab":
			f.SwapInFstab, _ = strconv.Atoi(v)
		case "swap_units":
			f.SwapUnits = strings.Fields(v)
		case "dphys":
			f.Dphys = v == "enabled"
		case "ufw":
			f.UFW = v
		case "ufw_added":
			data, _ := base64.StdEncoding.DecodeString(v)
			for _, l := range strings.Split(string(data), "\n") {
				if l = strings.TrimSpace(l); l != "" {
					f.UFWAdded = append(f.UFWAdded, l)
				}
			}
		case "networkmanager":
			f.NetworkManager = v == "active"
		case "calico_nm":
			f.CalicoNMConf = v == "yes"
		case "multipathd":
			f.Multipathd = v == "active"
		case "multipath_maps":
			f.MultipathMaps, _ = strconv.Atoi(v)
		case "multipath_conf":
			f.MultipathConf = v == "yes"
		case "memory_cgroup":
			f.MemoryCgroup = v == "yes"
		case "pi_cmdline":
			f.PiCmdline = v
		case "memory_mb":
			f.MemoryMB, _ = strconv.Atoi(v)
		case "selinux":
			f.SELinux = v
		case "selinux_config":
			f.SELinuxConfig = v
		case "product_uuid":
			f.ProductUUID = strings.ToLower(v)
		case "mac":
			f.MAC = strings.ToLower(v)
		case "prepared":
			f.Prepared, _ = base64.StdEncoding.DecodeString(v)
		case "done":
			f.Complete = v == "yes"
		}
	}
	return f
}

// DebianFamily reports whether /etc/os-release's ID and ID_LIKE name Ubuntu, Debian or a
// derivative (Raspberry Pi OS, Linux Mint, ...): the machines tow can prepare.
func DebianFamily(id, idLike string) bool {
	switch id {
	case "ubuntu", "debian", "raspbian":
		return true
	}
	for _, like := range strings.Fields(idLike) {
		if like == "debian" || like == "ubuntu" {
			return true
		}
	}
	return false
}

// kernelAtLeast compares uname -r (5.15.0-91-generic) with a release like 5.10.
func kernelAtLeast(release, min string) bool {
	parse := func(s string) (int, int) {
		parts := strings.SplitN(s, ".", 3)
		if len(parts) < 2 {
			return 0, 0
		}
		major, _ := strconv.Atoi(parts[0])
		minor := parts[1]
		if i := strings.IndexFunc(minor, func(r rune) bool { return r < '0' || r > '9' }); i >= 0 {
			minor = minor[:i] // "15-generic" in a two-part release
		}
		m, _ := strconv.Atoi(minor)
		return major, m
	}
	maj, mn := parse(release)
	wantMaj, wantMn := parse(min)
	return maj > wantMaj || (maj == wantMaj && mn >= wantMn)
}
