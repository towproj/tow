package providers

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"net/netip"
	"strings"
)

// ParseRange reads an IPv4 range written first-last, e.g. 192.168.1.20-192.168.1.29.
func ParseRange(s string) (first, last netip.Addr, err error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return first, last, fmt.Errorf("%q is not a range: write it first-last, like 192.168.1.20-192.168.1.29", s)
	}
	if first, err = netip.ParseAddr(strings.TrimSpace(a)); err != nil || !first.Is4() {
		return first, last, fmt.Errorf("%q: %q is not an IPv4 address", s, a)
	}
	if last, err = netip.ParseAddr(strings.TrimSpace(b)); err != nil || !last.Is4() {
		return first, last, fmt.Errorf("%q: %q is not an IPv4 address", s, b)
	}
	if last.Less(first) {
		return first, last, fmt.Errorf("%q: the range ends before it starts", s)
	}
	return first, last, nil
}

// RangeSize is how many addresses first-last holds.
func RangeSize(first, last netip.Addr) int {
	return int(toUint(last)-toUint(first)) + 1
}

// CIDRs covers an address pool entry (first-last, a CIDR, or one IPv4 address) with the
// fewest CIDRs, for firewalls that take no ranges. nil when it's none of those.
func CIDRs(entry string) []string {
	var first, last uint32
	switch a, b, err := ParseRange(entry); {
	case err == nil:
		first, last = toUint(a), toUint(b)
	default:
		if p, err := netip.ParsePrefix(entry); err == nil && p.Addr().Is4() {
			return []string{p.Masked().String()}
		}
		ip, err := netip.ParseAddr(entry)
		if err != nil || !ip.Is4() {
			return nil
		}
		first, last = toUint(ip), toUint(ip)
	}
	var out []string
	start, end := uint64(first), uint64(last)
	for start <= end {
		// The largest block that starts at start: as aligned as start is, and not past end.
		size := uint64(1)
		for size < 1<<32 && start%(size*2) == 0 && start+size*2-1 <= end {
			size *= 2
		}
		out = append(out, netip.PrefixFrom(fromUint(uint32(start)), 32-bits.TrailingZeros64(size)).String())
		start += size
	}
	return out
}

func toUint(a netip.Addr) uint32 {
	b := a.As4()
	return binary.BigEndian.Uint32(b[:])
}

func fromUint(u uint32) netip.Addr {
	var b [4]byte
	binary.BigEndian.PutUint32(b[:], u)
	return netip.AddrFrom4(b)
}
