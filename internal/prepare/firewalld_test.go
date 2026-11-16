package prepare

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
)

// firewalldNode is a Rocky machine where firewalld runs, with nothing of tow's yet.
func firewalldNode() Facts {
	f := rocky10()
	f.Firewalld, f.FWVersion = true, "1.3.4"
	return f
}

func firewalldNeeds(t *testing.T) (Needs, clusterInfo, firewalldWant) {
	t.Helper()
	n := NeedsFor(cfg(t, "spec: {flavor: homelab, "+byoh+"}"), config.RoleControlPlane)
	info := clusterInfo{name: "lab", peers: []string{"10.0.0.2"}, podCIDR: "10.42.0.0/16", serviceCIDR: "10.43.0.0/16"}
	return n, info, firewalldWanted(n, info)
}

func firewallChanges(changes []Change) []Change {
	var out []Change
	for _, c := range changes {
		if c.Record.What == firewallRecord {
			out = append(out, c)
		}
	}
	return out
}

// With firewalld on, the other nodes and the pod and Service ranges go in tow's zone,
// which accepts everything; the admin address gets the API, NodePorts and the ingress ports
// through tow's policy. Once there, nothing more; what's no longer needed is closed.
func TestPlanFirewalld(t *testing.T) {
	n, info, w := firewalldNeeds(t)
	if strings.Join(w.sources, " ") != "10.0.0.2 10.42.0.0/16 10.43.0.0/16" {
		t.Errorf("sources %v", w.sources)
	}

	changes, problems, _ := planNode(n, firewalldNode(), nil, w, info)
	fw := firewallChanges(changes)
	if len(problems) != 0 || len(fw) != 1 {
		t.Fatalf("problems %+v, firewall changes:\n%s", problems, summaries(fw))
	}
	for _, want := range []string{
		"fc --new-zone=tow-nodes\n", "fc --zone=tow-nodes --set-target=ACCEPT\n",
		"fc --zone=tow-nodes --add-source=10.0.0.2\n", "fc --zone=tow-nodes --add-source=10.42.0.0/16\n",
		"fc --new-policy=tow-admin\nfc --policy=tow-admin --add-ingress-zone=ANY\nfc --policy=tow-admin --add-egress-zone=HOST\n",
		`fc --policy=tow-admin --add-rich-rule="rule family=\"ipv4\" source address=\"$ADMIN\" port port=\"30000-32767\" protocol=\"udp\" accept"`,
		"firewall-cmd -q --reload\n",
	} {
		if !strings.Contains(fw[0].Script, want) {
			t.Errorf("missing %q in:\n%s", want, fw[0].Script)
		}
	}
	if !strings.Contains(fw[0].Summary, "from 10.0.0.2; from 10.42.0.0/16; from 10.43.0.0/16; from 203.0.113.7 to tcp 6443") || fw[0].Record.Revert != firewalldRevert {
		t.Errorf("change: %s\n%+v", fw[0].Summary, fw[0].Record)
	}

	// The script against a stand-in firewall-cmd: this machine's address goes in the rules.
	root := t.TempDir()
	bin := standIn(t, root, "firewall-cmd", `echo "$*" >> `+root+"/log")
	run(t, bin, fw[0].Script, "203.0.113.7")
	log, _ := os.ReadFile(root + "/log")
	if !strings.Contains(string(log), `-q --permanent --policy=tow-admin --add-rich-rule=rule family="ipv4" source address="203.0.113.7" port port="6443" protocol="tcp" accept`) ||
		!strings.HasSuffix(strings.TrimSpace(string(log)), "-q --reload") {
		t.Errorf("firewall-cmd ran:\n%s", log)
	}

	// Once there, as firewalld lists it (a single address as given, with or without /32).
	done := firewalldNode()
	done.FWZone, done.FWZoneTarget, done.FWPolicy = true, "ACCEPT", true
	done.FWZoneSources = []string{"10.0.0.2/32", "10.42.0.0/16", "10.43.0.0/16"}
	for _, r := range w.rules {
		done.FWPolicyRules = append(done.FWPolicyRules, strings.ReplaceAll(r.text, "$ADMIN", "203.0.113.7"))
	}
	done.FWZoneOf = map[string]string{"10.0.0.2": fwZone, "10.42.0.0/16": fwZone, "10.43.0.0/16": fwZone}
	if changes, problems, _ := planNode(n, done, nil, w, info); len(firewallChanges(changes)) != 0 || len(problems) != 0 {
		t.Errorf("nothing more to do: %+v\n%s", problems, summaries(changes))
	}

	// A node that left, and this machine's old address: closed, and only those.
	stale := done
	stale.FWZoneSources = append(stale.FWZoneSources, "10.0.0.9")
	stale.FWPolicyRules = append(stale.FWPolicyRules, `rule family="ipv4" source address="198.51.100.1" port port="6443" protocol="tcp" accept`)
	fw = firewallChanges(changes2fw(n, stale, w, info))
	if len(fw) != 1 || !strings.Contains(fw[0].Script, "fc --zone=tow-nodes --remove-source=10.0.0.9\n") ||
		!strings.Contains(fw[0].Script, `fc --policy=tow-admin --remove-rich-rule='rule family="ipv4" source address="198.51.100.1" port port="6443" protocol="tcp" accept'`) ||
		strings.Count(fw[0].Script, "--remove-") != 2 {
		t.Errorf("stale:\n%s", summaries(fw))
	}
	// Without this machine's address, which admin rule is wanted isn't known: none goes.
	stale.AdminIP = ""
	fw = firewallChanges(changes2fw(n, stale, w, info))
	if len(fw) != 1 || strings.Contains(fw[0].Script, "remove-rich-rule") || !strings.Contains(fw[0].Script, "--remove-source=10.0.0.9") {
		t.Errorf("admin unknown:\n%s", summaries(fw))
	}

	// firewalld off: nothing.
	off := rocky10()
	if fw := firewallChanges(changes2fw(n, off, w, info)); len(fw) != 0 {
		t.Errorf("firewalld off: %s", summaries(fw))
	}
}

func changes2fw(n Needs, f Facts, w firewalldWant, info clusterInfo) []Change {
	c, _, _ := planNode(n, f, nil, w, info)
	return c
}

// What only the user can sort out: a source firewalld already puts in another zone, a
// firewalld too old for policies. Calico's docs ask for no firewall manager: a warning.
func TestFirewalldProblems(t *testing.T) {
	n, info, w := firewalldNeeds(t)
	f := firewalldNode()
	f.FWZoneOf = map[string]string{"10.0.0.2": "internal"}
	if _, p, _ := planNode(n, f, nil, w, info); len(p) != 1 || !strings.Contains(p[0].What, "firewalld already puts 10.0.0.2 in its zone internal") ||
		!strings.Contains(p[0].Hint, "--zone=internal --remove-source=10.0.0.2") {
		t.Errorf("conflict: %+v", p)
	}
	f = firewalldNode()
	f.FWVersion = "0.8.2"
	if _, p, _ := planNode(n, f, nil, w, info); len(p) != 1 || !strings.Contains(p[0].What, "firewalld 0.8.2 is older than 0.9") {
		t.Errorf("old firewalld: %+v", p)
	}

	calico := NeedsFor(cfg(t, "spec: {kubernetes: {distribution: k3s, networking: {cni: calico}}, "+byoh+"}"), config.RoleWorker)
	if _, _, warnings := planNode(calico, firewalldNode(), nil, firewalldWanted(calico, info), info); len(warnings) != 1 || !strings.Contains(warnings[0], "firewalld is on, and Calico's docs ask") {
		t.Errorf("warnings: %v", warnings)
	}

	// The file's adminCIDRs replace this machine's address, and the pool's addresses go too.
	c := cfg(t, "spec: {kubernetes: {distribution: k3s, serviceLoadBalancer: {provider: metallb, metallb: {addresses: [192.168.1.200/32]}}}, infrastructure: {provider: byoh, access: {ssh: {user: pi}, adminCIDRs: [192.168.1.0/24]}}, controlPlane: {machines: [{host: 192.168.1.10}]}}")
	wide := firewalldWanted(NeedsFor(c, config.RoleControlPlane), clusterInfo{name: "lab", admin: c.Spec.Infrastructure.Access.AdminCIDRs})
	var texts []string
	for _, r := range wide.rules {
		if r.needsAdmin {
			t.Errorf("rule with $ADMIN: %s", r.text)
		}
		texts = append(texts, r.text)
	}
	if got := strings.Join(texts, "\n"); !strings.Contains(got, `rule family="ipv4" source address="192.168.1.0/24" port port="6443" protocol="tcp" accept`) ||
		!strings.Contains(got, `rule family="ipv4" source address="192.168.1.0/24" destination address="192.168.1.200" accept`) {
		t.Errorf("rules:\n%s", got)
	}
}

// Two rich rules are one when firewalld writes them its own way.
func TestRichKey(t *testing.T) {
	a := `rule family="ipv4" source address="203.0.113.7/32" port port="6443" protocol="tcp" accept`
	b := `rule family="ipv4" source address="203.0.113.7" port port="6443" protocol="tcp" accept`
	if richKey(a) != richKey(b) {
		t.Errorf("%s != %s", richKey(a), richKey(b))
	}
	if richKey(b) == richKey(strings.Replace(b, "tcp", "udp", 1)) || richKey(b) == richKey(strings.Replace(b, "accept", "drop", 1)) {
		t.Error("protocol and action count")
	}
	if got := describeRich(`rule family="ipv4" source address="10.1.0.0/16" destination address="192.168.1.200/29" accept`); got != "from 10.1.0.0/16 to 192.168.1.200/29" {
		t.Errorf("describe: %s", got)
	}
	for _, v := range []struct {
		release string
		want    bool
	}{{"1.3.4", true}, {"0.9.11", true}, {"2.2.0", true}, {"0.8.2", false}, {"", false}} {
		if atLeast(v.release, 0, 9) != v.want {
			t.Errorf("atLeast(%q)", v.release)
		}
	}
}

// The undo removes tow's zone and policy, with firewalld running or not; the facts
// script reads what firewalld has, in a shell.
func TestFirewalldScripts(t *testing.T) {
	root := t.TempDir()
	bin := standIn(t, root, "firewall-cmd", `echo "$*" >> `+root+"/log")
	run(t, bin, firewalldRevert, "")
	if log, _ := os.ReadFile(root + "/log"); string(log) != "-q --state\n-q --permanent --delete-zone=tow-nodes\n-q --permanent --delete-policy=tow-admin\n-q --reload\n" {
		t.Errorf("online:\n%s", log)
	}
	// firewalld stopped: its offline tool.
	root = t.TempDir()
	bin = standIn(t, root, "firewall-cmd", "exit 252")
	standIn(t, root, "firewall-offline-cmd", `echo "offline $*" >> `+root+"/log")
	run(t, bin, firewalldRevert, "")
	if log, _ := os.ReadFile(root + "/log"); string(log) != "offline -q --delete-zone=tow-nodes\noffline -q --delete-policy=tow-admin\n" {
		t.Errorf("offline:\n%s", log)
	}

	// The facts, from a stand-in firewalld that has tow's zone and policy.
	_, _, w := firewalldNeeds(t)
	root = t.TempDir()
	bin = standIn(t, root, "systemctl", "echo active")
	standIn(t, root, "firewall-cmd", `case "$*" in
  --version) echo 1.3.4 ;;
  "--permanent --get-zones") echo "public trusted tow-nodes" ;;
  "--permanent --get-policies") echo "allow-host-ipv6 tow-admin" ;;
  *--get-target) echo ACCEPT ;;
  *--list-sources) echo "10.0.0.2 10.42.0.0/16" ;;
  *--list-rich-rules) printf '%s\n' 'rule family="ipv4" source address="203.0.113.7" port port="6443" protocol="tcp" accept' ;;
  *--get-zone-of-source=10.0.0.2) echo tow-nodes ;;
  *) exit 2 ;;
esac`)
	cmd := exec.Command("sh", "-c", firewalldFacts(w))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	f := parseFacts(string(out))
	if !f.Firewalld || f.FWVersion != "1.3.4" || !f.FWZone || f.FWZoneTarget != "ACCEPT" || strings.Join(f.FWZoneSources, " ") != "10.0.0.2 10.42.0.0/16" ||
		!f.FWPolicy || len(f.FWPolicyRules) != 1 || f.FWZoneOf["10.0.0.2"] != fwZone || f.FWZoneOf["10.43.0.0/16"] != "" {
		t.Errorf("facts %+v\n%s", f, out)
	}
}

// standIn writes a program into root/bin and returns that folder.
func standIn(t *testing.T, root, name, body string) string {
	t.Helper()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// run runs a script with bin first on PATH, $1 = admin.
func run(t *testing.T, bin, script, admin string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", "set -e\n"+script, "sh", admin)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, script)
	}
}
