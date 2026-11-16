package prepare

import (
	"slices"
	"testing"
)

// A rule is the same whatever order ufw prints its words in, with or without its comment,
// and a single address with or without /32.
func TestParseUFWRule(t *testing.T) {
	same := [][]string{
		{"allow proto tcp from 203.0.113.7/32 to any port 22 comment 'tow:lab'", "ufw allow from 203.0.113.7 to any port 22 proto tcp comment 'tow:lab'"},
		{"allow from 10.0.0.2 comment 'tow:lab'", "ufw allow from 10.0.0.2"},
		{"allow proto tcp to any port 6443", "ufw allow 6443/tcp comment 'tow:lab'"},
		{"allow from 203.0.113.7 to 192.168.1.200/29", "ufw allow from 203.0.113.7 to 192.168.1.200/29"},
		{"allow proto udp from 0.0.0.0/0 to any port 30000:32767", "ufw allow 30000:32767/udp"},
	}
	for _, pair := range same {
		a, okA := parseUFWRule(pair[0])
		b, okB := parseUFWRule(pair[1])
		if !okA || !okB || a != b {
			t.Errorf("%q and %q: %+v (%v) vs %+v (%v), want the same rule", pair[0], pair[1], a, okA, b, okB)
		}
	}
	for _, other := range []string{"ufw allow in on eth0 from 10.0.0.2", "ufw allow from", "ufw allow", "ufw allow 22/tcp from 10.0.0.2"} {
		if p, ok := parseUFWRule(other); ok {
			t.Errorf("%q read as %+v: rules tow never adds are left alone", other, p)
		}
	}
}

// What the cluster no longer needs is closed; what it needs, another cluster's rules and
// the user's own stay.
func TestStaleUFWRules(t *testing.T) {
	wanted := ufwRules(Needs{Firewall: []Rule{
		{From: FromNodes, Why: "nodes"},
		{From: FromAdmin, Port: "22", Proto: "tcp", Why: "SSH"},
		{From: FromAdmin, Port: "9090", Proto: "tcp", Why: "a port"},
	}}, clusterInfo{name: "lab", peers: []string{"10.0.0.2"}})
	added := []string{
		"ufw allow from 10.0.0.2 comment 'tow:lab'",
		"ufw allow from 10.0.0.9 comment 'tow:lab'",                              // a removed machine
		"ufw allow from 198.51.100.4 to any port 22 proto tcp comment 'tow:lab'", // the old admin address
		"ufw allow from 203.0.113.7 to any port 22 proto tcp comment 'tow:lab'",
		"ufw allow from 203.0.113.7 to any port 9090 proto tcp comment 'tow:lab'",
		"ufw allow from 203.0.113.7 to any port 8080 proto tcp comment 'tow:lab'", // a port dropped
		"ufw allow from 10.0.0.9 comment 'tow:lab2'",                              // another cluster
		"ufw allow from 10.0.0.9",                                                 // the user's own
		"ufw allow in on eth1 from 10.0.0.9 comment 'tow:lab'",                    // not tow's form
	}
	got := staleUFWRules(added, wanted, "203.0.113.7", "lab")
	want := []string{
		"allow from 10.0.0.9",
		"allow from 198.51.100.4 to any port 22 proto tcp",
		"allow from 203.0.113.7 to any port 8080 proto tcp",
	}
	if !slices.Equal(got, want) {
		t.Errorf("stale:\n  %q\nwant\n  %q", got, want)
	}
	if got := staleUFWRules(added, wanted, "", "lab"); got != nil {
		t.Errorf("without this machine's address, which admin rule is wanted isn't known: %q", got)
	}
}
