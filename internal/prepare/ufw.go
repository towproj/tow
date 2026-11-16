package prepare

import (
	"fmt"
	"strings"
)

// ufwParts are what make two ufw rules the same rule, whatever order their words come in:
// "allow proto tcp from X to any port 22" (how tow adds it) and "allow from X to any port
// 22 proto tcp" (how 'ufw show added' prints it) are one rule.
type ufwParts struct{ action, proto, from, to, port string }

// parseUFWRule reads a rule in ufw's syntax, with or without "ufw" first and a comment last:
// the full form (allow [proto P] [from A] [to B] [port N]) and the short one ufw prints for a
// rule from anywhere to anywhere (allow 22/tcp). ok is false for anything else, such as an
// interface or a direction: tow never adds those, so it leaves such a rule alone.
func parseUFWRule(rule string) (p ufwParts, ok bool) {
	words := strings.Fields(cutComment(rule))
	if len(words) > 0 && words[0] == "ufw" {
		words = words[1:]
	}
	if len(words) < 2 {
		return p, false
	}
	p = ufwParts{action: words[0], from: "any", to: "any"}
	rest := words[1:]
	if !ufwKeywords[rest[0]] {
		// The short form: a port, maybe with its protocol, and nothing after it.
		if len(rest) != 1 {
			return p, false
		}
		p.port, p.proto, _ = strings.Cut(rest[0], "/")
		return p, true
	}
	for i := 0; i < len(rest); i += 2 {
		if i+1 >= len(rest) {
			return p, false
		}
		switch v := rest[i+1]; rest[i] {
		case "proto":
			p.proto = v
		case "from":
			p.from = address(v)
		case "to":
			p.to = address(v)
		case "port":
			p.port = v
		default:
			return p, false
		}
	}
	return p, true
}

var ufwKeywords = map[string]bool{"proto": true, "from": true, "to": true, "port": true}

// address writes an address the way ufw keeps it: a single IPv4 address without /32, and
// every address as "any".
func address(a string) string {
	if a == "0.0.0.0/0" {
		return "any"
	}
	return strings.TrimSuffix(a, "/32")
}

// cutComment drops a rule's trailing comment ('...'), quotes and all.
func cutComment(rule string) string {
	if i := strings.Index(rule, " comment '"); i >= 0 {
		return strings.TrimSpace(rule[:i])
	}
	return strings.TrimSpace(rule)
}

// staleUFWRules lists the cluster's rules (added: as 'ufw show added' prints them; only
// those with the cluster's comment count) that none of the rules it needs now (wanted)
// matches, without their comment: a firewall opened to an address or a port the file no
// longer names. An admin rule depends on this machine's address as the node sees it
// (adminIP); without it, which admin rule is wanted isn't known, so nothing is called stale.
func staleUFWRules(added []string, wanted []ufwRule, adminIP, cluster string) []string {
	want := map[ufwParts]bool{}
	for _, r := range wanted {
		args := r.args
		if r.needsAdmin {
			if adminIP == "" {
				return nil
			}
			args = strings.ReplaceAll(args, `"$ADMIN"`, adminIP)
		}
		if p, ok := parseUFWRule(args); ok {
			want[p] = true
		}
	}
	var stale []string
	for _, line := range added {
		if !strings.HasSuffix(strings.TrimSpace(line), "comment '"+tag(cluster)+"'") {
			continue // another cluster's, or the user's own
		}
		p, ok := parseUFWRule(line)
		if !ok || want[p] {
			continue
		}
		rule := cutComment(line)
		rule = strings.TrimSpace(strings.TrimPrefix(rule, "ufw "))
		stale = append(stale, rule)
	}
	return stale
}

// ufwDeleteScript deletes rules given in ufw's own words: the same rule it printed.
func ufwDeleteScript(rules []string) string {
	var b strings.Builder
	for _, r := range rules {
		fmt.Fprintf(&b, "ufw --force delete %s >/dev/null\n", r)
	}
	return b.String()
}
