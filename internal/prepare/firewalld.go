package prepare

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// firewalld, when it runs, gets what ufw gets, in two objects only tow uses, so they can
// be compared with what the cluster needs, changed, and removed whole:
//   - the zone tow-nodes accepts everything from the other nodes and from the pod and
//     Service ranges (k3s's docs put those ranges in the trusted zone; a zone of tow's own
//     does the same and leaves the user's trusted zone alone);
//   - the policy tow-admin accepts the admin addresses on the API, NodePorts and the
//     cluster's open ports, as rich rules, on their way to the machine itself. A policy, not
//     a zone: an address in a zone gets that zone's rules only, so SSH would have to follow,
//     and an admin range holding the nodes' addresses would overlap their zone.
//
// Source: firewalld's docs (zones, policies since 0.9, rich rules), and k3s's requirements
// (read 2026-11-14).
const (
	fwZone   = "tow-nodes"
	fwPolicy = "tow-admin"
)

// fwRule is one rich rule of the policy, with "$ADMIN" standing for this machine's address as
// the node sees it when needsAdmin.
type fwRule struct {
	text       string
	why        string
	needsAdmin bool
}

// firewalldWant is what the node's firewalld must have for the cluster.
type firewalldWant struct {
	sources    []string // the zone's: the other nodes, the pod and Service ranges
	whySources []string
	rules      []fwRule
}

// firewalldWanted turns the node's firewall needs into firewalld's terms.
func firewalldWanted(n Needs, c clusterInfo) firewalldWant {
	var w firewalldWant
	addSource := func(src, why string) {
		if src = address(src); src != "" && !slices.Contains(w.sources, src) {
			w.sources = append(w.sources, src)
		}
		if !slices.Contains(w.whySources, why) {
			w.whySources = append(w.whySources, why)
		}
	}
	for _, r := range n.Firewall {
		switch r.From {
		case FromNodes:
			for _, ip := range c.peers {
				addSource(ip, r.Why)
			}
		case FromPods:
			addSource(c.podCIDR, r.Why)
		case FromServices:
			addSource(c.serviceCIDR, r.Why)
		case FromAdmin:
			rule := func(from string) string {
				if r.To != "" {
					return fmt.Sprintf(`rule family="ipv4" source address="%s" destination address="%s" accept`, from, address(r.To))
				}
				return fmt.Sprintf(`rule family="ipv4" source address="%s" port port="%s" protocol="%s" accept`, from, fwPort(r.Port), r.Proto)
			}
			if len(c.admin) == 0 {
				w.rules = append(w.rules, fwRule{text: rule("$ADMIN"), why: r.Why, needsAdmin: true})
			}
			for _, cidr := range c.admin {
				w.rules = append(w.rules, fwRule{text: rule(address(cidr)), why: r.Why})
			}
		case FromAnywhere:
			w.rules = append(w.rules, fwRule{text: fmt.Sprintf(`rule family="ipv4" port port="%s" protocol="%s" accept`, fwPort(r.Port), r.Proto), why: r.Why})
		}
	}
	return w
}

// fwPort writes a port range firewalld's way: 30000-32767.
func fwPort(p string) string { return strings.ReplaceAll(p, ":", "-") }

// firewalldFacts reads, when firewalld runs, its release, tow's zone and policy, and the
// zone each source the cluster needs is already in.
func firewalldFacts(w firewalldWant) string {
	var b strings.Builder
	b.WriteString(`fw=$(systemctl is-active firewalld 2>/dev/null)
echo "firewalld=$fw"
if [ "$fw" = active ] && command -v firewall-cmd >/dev/null 2>&1; then
  echo "fw_version=$(firewall-cmd --version 2>/dev/null)"
  if firewall-cmd --permanent --get-zones 2>/dev/null | tr ' ' '\n' | grep -qx ` + fwZone + `; then
    echo "fw_zone=yes"
    echo "fw_zone_target=$(firewall-cmd --permanent --zone=` + fwZone + ` --get-target 2>/dev/null)"
    echo "fw_zone_sources=$(firewall-cmd --permanent --zone=` + fwZone + ` --list-sources 2>/dev/null)"
  fi
  if firewall-cmd --permanent --get-policies 2>/dev/null | tr ' ' '\n' | grep -qx ` + fwPolicy + `; then
    echo "fw_policy=yes"
    echo "fw_policy_rules=$(firewall-cmd --permanent --policy=` + fwPolicy + ` --list-rich-rules 2>/dev/null | base64 -w0)"
  fi
`)
	for _, s := range w.sources {
		fmt.Fprintf(&b, "  echo \"fw_zone_of:%s=$(firewall-cmd --permanent --get-zone-of-source=%s 2>/dev/null)\"\n", s, s)
	}
	b.WriteString("fi\n")
	return b.String()
}

// parseFirewalld reads one line of firewalldFacts' output into f; false when it isn't one.
func (f *Facts) parseFirewalld(k, v string) bool {
	kind, name, _ := strings.Cut(k, ":")
	switch {
	case kind == "fw_zone_of":
		if f.FWZoneOf == nil {
			f.FWZoneOf = map[string]string{}
		}
		f.FWZoneOf[name] = v
	case k == "firewalld":
		f.Firewalld = v == "active"
	case k == "fw_version":
		f.FWVersion = v
	case k == "fw_zone":
		f.FWZone = v == "yes"
	case k == "fw_zone_target":
		f.FWZoneTarget = v
	case k == "fw_zone_sources":
		f.FWZoneSources = strings.Fields(v)
	case k == "fw_policy":
		f.FWPolicy = v == "yes"
	case k == "fw_policy_rules":
		data, _ := base64.StdEncoding.DecodeString(v)
		for _, l := range strings.Split(string(data), "\n") {
			if l = strings.TrimSpace(l); l != "" {
				f.FWPolicyRules = append(f.FWPolicyRules, l)
			}
		}
	default:
		return false
	}
	return true
}

// planFirewalld compares what firewalld has with what the cluster needs: the changes (what's
// missing, then what's no longer needed), and the problems only the user can solve.
func planFirewalld(w firewalldWant, f Facts) ([]Change, []Problem) {
	if !atLeast(f.FWVersion, 0, 9) {
		return nil, []Problem{{What: fmt.Sprintf("firewalld %s is older than 0.9, which brought the policies tow uses", f.FWVersion),
			Hint: "upgrade firewalld, or turn it off ('sudo systemctl disable --now firewalld')"}}
	}
	var problems []Problem
	for _, s := range w.sources {
		if zone := f.FWZoneOf[s]; zone != "" && zone != fwZone {
			problems = append(problems, Problem{What: fmt.Sprintf("firewalld already puts %s in its zone %s, so tow can't let it reach the cluster", s, zone),
				Hint: fmt.Sprintf("take it out of that zone: sudo firewall-cmd --permanent --zone=%s --remove-source=%s && sudo firewall-cmd --reload", zone, s)})
		}
	}
	if len(problems) > 0 {
		return nil, problems
	}

	var add, remove strings.Builder
	var added, removed []string
	if !f.FWZone {
		fmt.Fprintf(&add, "fc --new-zone=%s\n", fwZone)
	}
	if !f.FWZone || f.FWZoneTarget != "ACCEPT" {
		fmt.Fprintf(&add, "fc --zone=%s --set-target=ACCEPT\n", fwZone)
	}
	have := map[string]bool{}
	for _, s := range f.FWZoneSources {
		have[address(s)] = true
	}
	for _, s := range w.sources {
		if !have[s] {
			fmt.Fprintf(&add, "fc --zone=%s --add-source=%s\n", fwZone, s)
			added = append(added, "from "+s)
		}
	}
	if !f.FWPolicy {
		fmt.Fprintf(&add, "fc --new-policy=%s\nfc --policy=%s --add-ingress-zone=ANY\nfc --policy=%s --add-egress-zone=HOST\n", fwPolicy, fwPolicy, fwPolicy)
	}
	haveRules := map[string]bool{}
	for _, r := range f.FWPolicyRules {
		haveRules[richKey(r)] = true
	}
	wantRules := map[string]bool{}
	adminKnown := true
	var whys []string
	for _, r := range w.rules {
		text := r.text
		if r.needsAdmin {
			if f.AdminIP == "" {
				adminKnown = false
				continue // no address to allow; the rule waits for a run that knows it
			}
			text = strings.ReplaceAll(text, "$ADMIN", address(f.AdminIP))
		}
		wantRules[richKey(text)] = true
		if !haveRules[richKey(text)] {
			fmt.Fprintf(&add, "fc --policy=%s --add-rich-rule=%s\n", fwPolicy, shellQuoted(r.text))
			added = append(added, describeRich(text))
			if !slices.Contains(whys, r.why) {
				whys = append(whys, r.why)
			}
		}
	}
	// What tow's zone and policy have that nothing needs any more (a node that left, an
	// old admin address, a port the file dropped). Without this machine's address, which
	// admin rule is wanted isn't known: no rule is called stale then.
	for _, s := range f.FWZoneSources {
		if !slices.Contains(w.sources, address(s)) {
			fmt.Fprintf(&remove, "fc --zone=%s --remove-source=%s\n", fwZone, s)
			removed = append(removed, "from "+s)
		}
	}
	if adminKnown {
		for _, r := range f.FWPolicyRules {
			if !wantRules[richKey(r)] && !strings.Contains(r, "'") {
				fmt.Fprintf(&remove, "fc --policy=%s --remove-rich-rule='%s'\n", fwPolicy, r)
				removed = append(removed, describeRich(r))
			}
		}
	}

	const head = "ADMIN=\"$1\"\nfc() { firewall-cmd -q --permanent \"$@\"; }\n"
	var changes []Change
	if add.Len() > 0 {
		summary := fmt.Sprintf("let the cluster through firewalld (it's on): zone %s accepts the other nodes, pods and Services; policy %s accepts the admin addresses on the cluster's ports", fwZone, fwPolicy)
		if len(added) > 0 {
			summary += ": " + strings.Join(added, "; ")
		}
		changes = append(changes, Change{
			Summary: summary,
			Script:  head + add.String() + "firewall-cmd -q --reload\n",
			Record: Record{What: firewallRecord, Detail: fmt.Sprintf("firewalld zone %s and policy %s: %s", fwZone, fwPolicy, strings.Join(added, "; ")),
				Undo: firewalldUndo, Revert: firewalldRevert},
		})
	}
	if remove.Len() > 0 {
		changes = append(changes, Change{
			Summary: "close what firewalld no longer needs to let through: " + strings.Join(removed, "; "),
			Script:  head + remove.String() + "firewall-cmd -q --reload\n",
			Record: Record{What: firewallRecord, Detail: fmt.Sprintf("removed from firewalld zone %s and policy %s: %s", fwZone, fwPolicy, strings.Join(removed, "; ")),
				Undo: firewalldUndo, Revert: firewalldRevert},
		})
	}
	return changes, nil
}

// firewalldUndo and firewalldRevert remove tow's zone and policy; firewalld itself stays
// on or off as it was. Without firewalld running, its offline tool edits the same files.
const firewalldUndo = "sudo firewall-cmd --permanent --delete-zone=" + fwZone + " && sudo firewall-cmd --permanent --delete-policy=" + fwPolicy + " && sudo firewall-cmd --reload"

const firewalldRevert = `if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd -q --state 2>/dev/null; then
  firewall-cmd -q --permanent --delete-zone=` + fwZone + ` 2>/dev/null || true
  firewall-cmd -q --permanent --delete-policy=` + fwPolicy + ` 2>/dev/null || true
  firewall-cmd -q --reload
elif command -v firewall-offline-cmd >/dev/null 2>&1; then
  firewall-offline-cmd -q --delete-zone=` + fwZone + ` 2>/dev/null || true
  firewall-offline-cmd -q --delete-policy=` + fwPolicy + ` 2>/dev/null || true
fi
`

var richAttr = regexp.MustCompile(`(\w+)="([^"]*)"`)

// richKey is what makes two rich rules the same rule, whatever way they're written: the
// family, the source and destination addresses (a single address with or without /32), the
// port and protocol, and the action.
func richKey(rule string) string {
	element := ""
	got := map[string]string{}
	for _, word := range strings.Fields(rule) {
		if m := richAttr.FindStringSubmatch(word); m != nil {
			v := m[2]
			if m[1] == "address" {
				v = address(v)
			}
			got[element+"."+m[1]] = v
			continue
		}
		switch word {
		case "rule", "source", "destination", "port":
			element = word
		default:
			got["action"] = word
		}
	}
	return fmt.Sprintf("%s|%s|%s|%s|%s|%s", got["rule.family"], got["source.address"], got["destination.address"], got["port.port"], got["port.protocol"], got["action"])
}

// describeRich says what a rule lets in: "from 203.0.113.7 to tcp 6443".
func describeRich(rule string) string {
	parts := strings.Split(richKey(rule), "|")
	from, to := parts[1], ""
	switch {
	case parts[2] != "":
		to = parts[2]
	case parts[3] != "":
		to = parts[4] + " " + parts[3]
	}
	if from == "" {
		from = "anywhere"
	}
	return "from " + from + " to " + to
}

// shellQuoted is a rich rule as one double-quoted shell word, so $ADMIN in it is expanded.
func shellQuoted(rule string) string {
	return `"` + strings.ReplaceAll(rule, `"`, `\"`) + `"`
}

// atLeast compares a release like 1.3.4 with major.minor.
func atLeast(release string, major, minor int) bool {
	parts := strings.SplitN(strings.TrimSpace(release), ".", 3)
	if len(parts) < 2 {
		return false
	}
	maj, err1 := strconv.Atoi(parts[0])
	mn, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return false
	}
	return maj > major || (maj == major && mn >= minor)
}
