package providers

import (
	"strings"
	"testing"
)

// A pool entry covered by the fewest CIDRs, for firewalls that take no ranges.
func TestCIDRs(t *testing.T) {
	for entry, want := range map[string]string{
		"192.168.100.200-192.168.100.210": "192.168.100.200/29,192.168.100.208/31,192.168.100.210/32",
		"192.168.1.0-192.168.1.255":       "192.168.1.0/24",
		"192.168.1.240/28":                "192.168.1.240/28",
		"192.168.1.243/28":                "192.168.1.240/28",
		"10.0.0.7":                        "10.0.0.7/32",
		"0.0.0.0-255.255.255.255":         "0.0.0.0/0",
		"255.255.255.254-255.255.255.255": "255.255.255.254/31",
		"nope":                            "",
	} {
		if got := strings.Join(CIDRs(entry), ","); got != want {
			t.Errorf("%s: %s, want %s", entry, got, want)
		}
	}
}

func TestParseRange(t *testing.T) {
	for _, bad := range []string{"10.0.0.5", "10.0.0.9-10.0.0.5", "10.0.0.1-fe80::1", "a-b"} {
		if _, _, err := ParseRange(bad); err == nil {
			t.Errorf("%q should be refused", bad)
		}
	}
	if f, l, err := ParseRange("10.0.0.5 - 10.0.0.9"); err != nil || RangeSize(f, l) != 5 {
		t.Errorf("spaces around the dash: %v, size %d", err, RangeSize(f, l))
	}
}
