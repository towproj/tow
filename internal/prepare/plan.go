package prepare

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Change is one thing to do on a node, asked about before it's done and recorded after.
type Change struct {
	Summary string // what the user is asked about
	Script  string // run as root; $1 is this machine's address as the node sees it
	Record  Record
}

// Problem is something tow can't fix by itself; nothing changes until it's solved.
type Problem struct {
	What, Hint string
}

// ufwRule is one 'ufw allow' rule, commented with the cluster's tag so it can be found and
// removed later.
type ufwRule struct {
	args       string // after "ufw": allow from 10.0.0.2 comment 'tow:lab'
	why        string
	needsAdmin bool // uses $ADMIN, this machine's address as the node sees it
}

// clusterInfo is what a node's rules need to know about the rest of the cluster.
type clusterInfo struct {
	name                 string
	peers                []string // the other nodes' addresses
	podCIDR, serviceCIDR string
	admin                []string // the file's adminCIDRs; empty = this machine ($ADMIN)
}

func tag(cluster string) string { return "tow:" + cluster }

// ufwRevert deletes the cluster's ufw rules (those whose comment is its tag, and only them:
// not another cluster's whose name starts the same), one at a time since the numbers shift.
// ufw itself stays on or off as it was.
func ufwRevert(cluster string) string {
	return `if command -v ufw >/dev/null 2>&1; then
  i=0
  while [ $i -lt 1000 ] && n=$(ufw status numbered | sed -n 's/^\[ *\([0-9][0-9]*\)\].*# ` + tag(cluster) + `[[:space:]]*$/\1/p' | head -n1) && [ -n "$n" ]; do
    ufw --force delete "$n" >/dev/null
    i=$((i + 1))
  done
fi
`
}

// ufwRules turns the node's firewall needs into ufw rules.
func ufwRules(n Needs, c clusterInfo) []ufwRule {
	comment := fmt.Sprintf("comment '%s'", tag(c.name))
	var out []ufwRule
	for _, r := range n.Firewall {
		switch r.From {
		case FromNodes:
			for _, ip := range c.peers {
				out = append(out, ufwRule{args: fmt.Sprintf("allow from %s %s", ip, comment), why: r.Why})
			}
		case FromPods:
			out = append(out, ufwRule{args: fmt.Sprintf("allow from %s %s", c.podCIDR, comment), why: r.Why})
		case FromServices:
			out = append(out, ufwRule{args: fmt.Sprintf("allow from %s %s", c.serviceCIDR, comment), why: r.Why})
		case FromAdmin:
			rule := func(from string) string {
				if r.To != "" {
					return fmt.Sprintf("allow from %s to %s %s", from, r.To, comment)
				}
				return fmt.Sprintf("allow proto %s from %s to any port %s %s", r.Proto, from, r.Port, comment)
			}
			if len(c.admin) == 0 {
				out = append(out, ufwRule{args: rule(`"$ADMIN"`), why: r.Why, needsAdmin: true})
			}
			for _, cidr := range c.admin {
				out = append(out, ufwRule{args: rule(cidr), why: r.Why})
			}
		case FromAnywhere:
			out = append(out, ufwRule{args: fmt.Sprintf("allow proto %s to any port %s %s", r.Proto, r.Port, comment), why: r.Why})
		}
	}
	return out
}

// planNode compares what a node needs with what it has: the changes to make, the problems
// only the user can solve, and warnings.
func planNode(n Needs, f Facts, rules []ufwRule, fw firewalldWant, c clusterInfo) (changes []Change, problems []Problem, warnings []string) {
	family, err := Family(f.OSID, f.OSLike, f.OSVersion)
	if err != nil {
		return nil, []Problem{{What: err.Error(), Hint: "use an Ubuntu, Debian, RHEL, Rocky or AlmaLinux machine (RHEL family: 9 or 10)"}}, nil
	}
	if m := n.MinKernel; m != nil && !kernelAtLeast(f.Kernel, m.Value) {
		hint := "upgrade the kernel (a newer Ubuntu or Debian release), or pick another CNI"
		if family == FamilyRHEL {
			hint = "use RHEL 9 or 10 with its own kernel, or pick another CNI"
		}
		problems = append(problems, Problem{What: fmt.Sprintf("kernel %s is older than %s, the oldest %s supports", f.Kernel, m.Value, m.Why), Hint: hint})
	}
	if n.MinMemoryMB > 0 && f.MemoryMB > 0 && f.MemoryMB < n.MinMemoryMB {
		hint := "give the machine more memory"
		if family == FamilyRHEL {
			hint += " (RHEL-family kernels keep some aside for kdump's crash kernel: a 2 GiB machine often shows less than 1700 MB)"
		}
		problems = append(problems, Problem{What: fmt.Sprintf("%d MB of memory: a kubeadm control plane needs at least %d MB (kubeadm's preflight check)", f.MemoryMB, n.MinMemoryMB), Hint: hint})
	}
	if !f.MemoryCgroup {
		hint := "enable the memory cgroup controller on the kernel command line, then reboot"
		if f.PiCmdline != "" {
			hint = fmt.Sprintf("add 'cgroup_memory=1 cgroup_enable=memory' to the end of the line in %s, then reboot (k3s docs)", f.PiCmdline)
		}
		problems = append(problems, Problem{What: "the memory cgroup is off: Kubernetes can't limit memory without it", Hint: hint})
	}
	var fwChanges []Change
	if f.Firewalld {
		var fwProblems []Problem
		fwChanges, fwProblems = planFirewalld(fw, f)
		problems = append(problems, fwProblems...)
	}
	if n.NoFirewallManager != "" && f.UFW == "active" {
		warnings = append(warnings, fmt.Sprintf("ufw is on, and %s's docs ask for no other iptables manager on its nodes: if pods can't reach each other, turn ufw off ('sudo ufw disable')", n.NoFirewallManager))
	}
	if n.NoFirewallManager != "" && f.Firewalld {
		warnings = append(warnings, fmt.Sprintf("firewalld is on, and %s's docs ask for no other iptables manager on its nodes: if pods can't reach each other, turn firewalld off ('sudo systemctl disable --now firewalld')", n.NoFirewallManager))
	}
	if n.QuietMultipath != "" && f.Multipathd && !f.MultipathConf && f.MultipathMaps > 0 {
		problems = append(problems, Problem{
			What: fmt.Sprintf("multipath manages %d device(s) here, and %s's fix for multipathd blacklists every sd* disk, which would cut those paths", f.MultipathMaps, n.QuietMultipath),
			Hint: "use machines without multipath storage for " + n.QuietMultipath + ", or another storage driver, or blacklist " + n.QuietMultipath + "'s devices yourself (https://longhorn.io/kb/troubleshooting-volume-with-multipath/) and write " + multipathConf})
	}
	if len(problems) > 0 {
		return nil, problems, warnings
	}

	// Packages first: later steps may need them.
	var missing []pkg
	var names, whys []string
	for _, p := range packagesFor(n.Packages, family, Major(f.OSVersion), f.Kernel) {
		if !f.Installed[p.key] {
			missing = append(missing, p)
			names = append(names, p.spec)
			if !slices.Contains(whys, p.why) {
				whys = append(whys, p.why)
			}
		}
	}
	if len(missing) > 0 {
		script, undo, revert := installPackages(family, missing)
		changes = append(changes, Change{
			Summary: fmt.Sprintf("install %s (%s)", strings.Join(names, ", "), strings.Join(whys, "; ")),
			Script:  script,
			Record:  Record{What: "packages", Detail: "installed " + strings.Join(names, " "), Undo: undo, Revert: revert},
		})
	}

	// Modules before sysctls: bridge settings only exist once br_netfilter is loaded.
	var load, save, touched []string
	names = nil
	for _, m := range n.Modules {
		if !f.ModuleLoaded[m.Name] {
			load = append(load, m.Name)
		}
		if !f.ModuleSaved[m.Name] {
			save = append(save, m.Name)
		}
		if !f.ModuleLoaded[m.Name] || !f.ModuleSaved[m.Name] {
			names = append(names, fmt.Sprintf("%s (%s)", m.Name, m.Why))
			touched = append(touched, m.Name)
		}
	}
	if len(names) > 0 {
		var s strings.Builder
		for _, m := range load {
			fmt.Fprintf(&s, "modprobe %s\n", m)
		}
		for _, m := range save {
			fmt.Fprintf(&s, "echo %s >> %s\n", m, modulesConf)
		}
		// Only the lines tow added go; the modules stay loaded until the next reboot.
		revert := "true\n"
		if len(save) > 0 {
			var r strings.Builder
			for _, m := range save {
				fmt.Fprintf(&r, "sed -i '/^%s$/d' %s 2>/dev/null || true\n", m, modulesConf)
			}
			fmt.Fprintf(&r, "[ -s %s ] || rm -f %s\n", modulesConf, modulesConf)
			revert = r.String()
		}
		changes = append(changes, Change{
			Summary: "load kernel module " + strings.Join(names, ", ") + ", now and at boot",
			Script:  s.String(),
			Record: Record{What: "kernel modules", Detail: strings.Join(touched, ", ") + " loaded, and listed in " + modulesConf,
				Undo: "remove them from " + modulesConf + " (they unload at the next reboot)", Revert: revert},
		})
	}

	var settings []string
	var s, revert strings.Builder
	for _, k := range n.Sysctls {
		if f.SysctlNow[k.Name] == k.Value && f.SysctlSaved[k.Name] == k.Value {
			continue
		}
		settings = append(settings, fmt.Sprintf("%s = %s (%s)", k.Name, k.Value, k.Why))
		drop := fmt.Sprintf("sed -i '/^[[:space:]]*%s[[:space:]]*=/d' %s 2>/dev/null || true\n", regexp.QuoteMeta(k.Name), sysctlConf)
		s.WriteString(drop)
		fmt.Fprintf(&s, "echo '%s = %s' >> %s\n", k.Name, k.Value, sysctlConf)
		revert.WriteString(drop)
	}
	if len(settings) > 0 {
		// Only this file: an unrelated bad line elsewhere must not fail the step. The check that
		// follows the changes confirms the values hold.
		fmt.Fprintf(&s, "sysctl -p %s >/dev/null\n", sysctlConf)
		// The values themselves stay until the next reboot.
		fmt.Fprintf(&revert, "[ -s %s ] || rm -f %s\n", sysctlConf, sysctlConf)
		changes = append(changes, Change{
			Summary: "set " + strings.Join(settings, ", ") + ", now and at boot",
			Script:  s.String(),
			Record: Record{What: "kernel settings", Detail: strings.Join(settings, "; ") + " in " + sysctlConf,
				Undo: "remove those lines from " + sysctlConf + " (the old values come back at the next reboot)", Revert: revert.String()},
		})
	}

	if n.SwapOff && (len(f.SwapActive) > 0 || f.SwapInFstab > 0 || len(f.SwapUnits) > 0 || f.Dphys) {
		changes = append(changes, swapOff(f))
	}
	if n.SELinuxPermissive && (f.SELinux == "Enforcing" || f.SELinuxConfig == "enforcing") {
		changes = append(changes, selinuxPermissive(f))
	}

	if len(n.QuietNetworkManager) > 0 && f.NetworkManager && !f.CalicoNMConf {
		changes = append(changes, Change{
			Summary: "tell NetworkManager to leave Calico's interfaces alone (" + calicoNMConf + ", Calico docs)",
			Script: "mkdir -p /etc/NetworkManager/conf.d\n" +
				"printf '[keyfile]\\nunmanaged-devices=" + strings.Join(n.QuietNetworkManager, ";") + "\\n' > " + calicoNMConf + "\n" +
				"systemctl reload NetworkManager\n",
			Record: Record{What: "NetworkManager", Detail: "wrote " + calicoNMConf, Undo: "sudo rm " + calicoNMConf + " && sudo systemctl reload NetworkManager",
				Revert: "if [ -e " + calicoNMConf + " ]; then rm -f " + calicoNMConf + "; systemctl reload NetworkManager || true; fi\n"},
		})
	}

	if n.QuietMultipath != "" && f.Multipathd && !f.MultipathConf {
		changes = append(changes, Change{
			Summary: "keep multipathd off " + n.QuietMultipath + "'s volumes: blacklist sd* disks in " + multipathConf + " (" + n.QuietMultipath + " docs), restart multipathd",
			Script: "mkdir -p /etc/multipath/conf.d\n" +
				"printf 'blacklist {\\n    devnode \"^sd[a-z0-9]+\"\\n}\\n' > " + multipathConf + "\n" +
				"systemctl restart multipathd\n",
			Record: Record{What: "multipathd", Detail: "wrote " + multipathConf + " (blacklist sd* disks)", Undo: "sudo rm " + multipathConf + " && sudo systemctl restart multipathd",
				Revert: "if [ -e " + multipathConf + " ]; then rm -f " + multipathConf + "; systemctl restart multipathd || true; fi\n"},
		})
	}

	changes = append(changes, fwChanges...)
	if f.UFW == "active" {
		var add []ufwRule
		for i, r := range rules {
			if r.needsAdmin && f.AdminIP == "" {
				continue // no address to allow; the rule waits for a run that knows it
			}
			if !f.UFWRule[i] {
				add = append(add, r)
			}
		}
		if len(add) > 0 {
			var lines, whys []string
			var script strings.Builder
			script.WriteString("ADMIN=\"$1\"\n")
			for _, r := range add {
				fmt.Fprintf(&script, "ufw %s >/dev/null\n", r.args)
				lines = append(lines, strings.ReplaceAll(strings.TrimSuffix(r.args, " comment '"+tag(c.name)+"'"), "\"$ADMIN\"", f.AdminIP))
				if !slices.Contains(whys, r.why) {
					whys = append(whys, r.why)
				}
			}
			changes = append(changes, Change{
				Summary: fmt.Sprintf("open ufw (it's on): %s — for %s", strings.Join(lines, "; "), strings.Join(whys, "; ")),
				Script:  script.String(),
				Record: Record{What: firewallRecord, Detail: fmt.Sprintf("ufw rules commented '%s': %s", tag(c.name), strings.Join(lines, "; ")),
					Undo: ufwUndo(c.name), Revert: ufwRevert(c.name)},
			})
		}
		// The cluster's rules nothing needs any more (an old admin address, a port the file
		// dropped), after the new ones are in: no moment without a rule that's needed.
		if stale := staleUFWRules(f.UFWAdded, rules, f.AdminIP, c.name); len(stale) > 0 {
			changes = append(changes, Change{
				Summary: "close ufw rules no longer needed: " + strings.Join(stale, "; "),
				Script:  ufwDeleteScript(stale),
				Record: Record{What: firewallRecord, Detail: fmt.Sprintf("deleted ufw rules commented '%s': %s", tag(c.name), strings.Join(stale, "; ")),
					Undo: ufwUndo(c.name), Revert: ufwRevert(c.name)},
			})
		}
	}
	return changes, nil, warnings
}

// firewallRecord is what the cluster's ufw changes are recorded as.
const firewallRecord = "firewall"

// ufwUndo says how to remove the cluster's ufw rules by hand.
func ufwUndo(cluster string) string {
	return fmt.Sprintf("delete each rule marked '%s' in 'sudo ufw status numbered' ('sudo ufw delete <number>', highest number first)", tag(cluster))
}

// selinuxPermissive sets SELinux to permissive now and at boot, as kubeadm's install page
// does on Red Hat-based systems; undone, it enforces again as it did.
func selinuxPermissive(f Facts) Change {
	var s, revert strings.Builder
	var done, undo []string
	if f.SELinux == "Enforcing" {
		s.WriteString("setenforce 0\n")
		revert.WriteString("setenforce 1 || true\n")
		done = append(done, "set to permissive now (setenforce 0)")
		undo = append(undo, "sudo setenforce 1")
	}
	if f.SELinuxConfig == "enforcing" {
		s.WriteString("sed -i 's/^SELINUX=enforcing$/SELINUX=permissive/' /etc/selinux/config\n")
		revert.WriteString("sed -i 's/^SELINUX=permissive$/SELINUX=enforcing/' /etc/selinux/config\n")
		done = append(done, "SELINUX=permissive in /etc/selinux/config")
		undo = append(undo, "set SELINUX=enforcing again in /etc/selinux/config")
	}
	return Change{
		Summary: "set SELinux to permissive, now and at boot (kubeadm's install page: containers must reach the host's files)",
		Script:  s.String(),
		Record:  Record{What: "SELinux", Detail: strings.Join(done, "; "), Undo: strings.Join(undo, "; "), Revert: revert.String()},
	}
}

// swapOff turns swap off now and keeps it off after a reboot, as the kubeadm docs ask: swap
// lines in /etc/fstab are commented out (with a backup), swap units masked, and Raspberry Pi
// OS's dphys-swapfile disabled.
func swapOff(f Facts) Change {
	var s strings.Builder
	s.WriteString("swapoff -a\n")
	done := []string{"turned swap off"}
	undo := []string{}
	var revert strings.Builder
	if f.SwapInFstab > 0 {
		revert.WriteString("sed -i 's/^# tow: //' /etc/fstab\n")
		s.WriteString(`[ -e /etc/fstab.tow.bak ] || cp -p /etc/fstab /etc/fstab.tow.bak
awk '!/^[[:space:]]*#/ && $3 == "swap" {print "# tow: " $0; next} {print}' /etc/fstab > /etc/fstab.tow.tmp
cat /etc/fstab.tow.tmp > /etc/fstab && rm -f /etc/fstab.tow.tmp
`)
		done = append(done, "commented out the swap lines of /etc/fstab with '# tow: ' (copy: /etc/fstab.tow.bak)")
		undo = append(undo, "remove '# tow: ' from those lines of /etc/fstab")
	}
	if len(f.SwapUnits) > 0 {
		units := strings.Join(f.SwapUnits, " ")
		fmt.Fprintf(&s, "systemctl mask %s\n", units)
		done = append(done, "masked "+units)
		undo = append(undo, "sudo systemctl unmask "+units)
		fmt.Fprintf(&revert, "systemctl unmask %s\n", units)
	}
	if f.Dphys {
		s.WriteString("systemctl disable --now dphys-swapfile\n")
		done = append(done, "disabled dphys-swapfile")
		undo = append(undo, "sudo systemctl enable dphys-swapfile")
		revert.WriteString("systemctl enable --now dphys-swapfile\n")
	}
	s.WriteString("systemctl daemon-reload\n")
	undo = append(undo, "sudo swapon -a")
	// A swap file deleted since can't come back: that mustn't stop the rest of the delete.
	revert.WriteString("systemctl daemon-reload\nswapon -a || true\n")
	return Change{
		Summary: "turn swap off, now and at boot (kubeadm: the kubelet refuses to start with swap)",
		Script:  s.String(),
		Record:  Record{What: "swap", Detail: strings.Join(done, "; "), Undo: strings.Join(undo, "; "), Revert: revert.String()},
	}
}
