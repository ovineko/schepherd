package httpfetch

import (
	"net/netip"
	"strings"
)

// Ranges refused in addition to what the netip predicates cover.
// Documentation, benchmarking and reserved ranges are included because a
// public upstream never legitimately lives there and some networks route them
// internally. IPv6 space outside 2000::/3 is refused wholesale by isPublic, so
// only special-purpose blocks inside it are listed.
var blockedPrefixes = mustPrefixes(
	"0.0.0.0/8",
	"10.0.0.0/8",
	"100.64.0.0/10",
	"127.0.0.0/8",
	"169.254.0.0/16",
	"172.16.0.0/12",
	"192.0.0.0/24",
	"192.0.2.0/24",
	"192.88.99.0/24",
	"192.168.0.0/16",
	"198.18.0.0/15",
	"198.51.100.0/24",
	"203.0.113.0/24",
	"224.0.0.0/4",
	"240.0.0.0/4",
	"2001::/23",
	"2001:db8::/32",
	"3fff::/20",
)

var (
	globalUnicast6 = netip.MustParsePrefix("2000::/3")
	nat64          = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour      = netip.MustParsePrefix("2002::/16")
)

func mustPrefixes(cidrs ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(cidrs))
	for _, cidr := range cidrs {
		out = append(out, netip.MustParsePrefix(cidr))
	}

	return out
}

func parseIP(host string) (netip.Addr, bool) {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")

	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}

	return addr.WithZone(""), true
}

// isPublic reports whether addr is a public unicast address. IPv4-mapped,
// NAT64 (64:ff9b::/96) and 6to4 (2002::/16) addresses are judged by the IPv4
// address they embed, so an internal IPv4 target cannot be reached through
// an IPv6 spelling of it. IPv6 addresses outside 2000::/3 are not public.
func isPublic(addr netip.Addr) bool {
	addr = addr.Unmap()

	if addr.Is6() {
		switch {
		case nat64.Contains(addr):
			raw := addr.As16()
			return isPublic(netip.AddrFrom4([4]byte(raw[12:16])))
		case sixToFour.Contains(addr):
			raw := addr.As16()
			return isPublic(netip.AddrFrom4([4]byte(raw[2:6])))
		case !globalUnicast6.Contains(addr):
			return false
		}
	}

	if !addr.IsValid() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsPrivate() ||
		addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() ||
		addr.IsMulticast() {
		return false
	}

	for _, prefix := range blockedPrefixes {
		if prefix.Contains(addr) {
			return false
		}
	}

	return true
}
