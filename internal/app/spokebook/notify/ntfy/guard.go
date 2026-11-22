package ntfy

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
)

// errBlocked means the server's address is not allowed.
var errBlocked = errors.New("ntfy: address not allowed")

var (
	// never are addresses never connected to: this network, link-local,
	// cloud metadata services (also those outside link-local: AWS and
	// GCP over IPv6, Oracle, Alibaba, Azure WireServer), IETF protocol
	// assignments, site-local, multicast, broadcast and the IPv6 ranges
	// that tunnel or translate to IPv4 elsewhere (6to4, Teredo, SIIT).
	never = prefixes(
		"0.0.0.0/8", "169.254.0.0/16", "100.100.100.200/32", "168.63.129.16/32", "192.0.0.0/24",
		"224.0.0.0/4", "255.255.255.255/32",
		"::/128", "fd00:ec2::254/128", "fd20:ce::254/128", "fd00:c1::a9fe:a9fe/128",
		"fe80::/10", "fec0::/10", "ff00::/8", "2001::/32", "2002::/16", "::ffff:0:0:0/96",
	)
	// private are loopback and private addresses, connected to only if
	// allowed (APP_NOTIFY_ALLOW_PRIVATE), e.g. for a self-hosted server
	// in the home network. 64:ff9b:1::/48 is NAT64 for local use.
	private = prefixes(
		"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10",
		"::1/128", "fc00::/7", "64:ff9b:1::/48",
	)
	// embedding are IPv6 ranges whose last 4 bytes are an IPv4 address,
	// which is checked instead: well-known NAT64 and IPv4-compatible.
	embedding = prefixes("64:ff9b::/96", "::/96")
	loopback6 = netip.IPv6Loopback()
)

func prefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

func within(a netip.Addr, list []netip.Prefix) bool {
	for _, p := range list {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Allowed reports whether a server at the address may be connected to;
// loopback and private addresses only if allowPrivate is set.
func Allowed(a netip.Addr, allowPrivate bool) bool {
	a = a.Unmap().WithZone("")
	if a.Is6() && a != loopback6 && !a.IsUnspecified() && within(a, embedding) {
		b := a.As16()
		a = netip.AddrFrom4([4]byte(b[12:]))
	}
	switch {
	case !a.IsValid() || within(a, never):
		return false
	case within(a, private):
		return allowPrivate
	}
	return true
}

// neverAllowed reports whether host is an IP address that is not allowed
// even with allowPrivate.
func neverAllowed(host string) bool {
	a, err := netip.ParseAddr(host)
	return err == nil && !Allowed(a, true)
}

// control checks the address of each connection after the name was
// resolved, so neither a name nor a changing DNS answer (rebinding) leads
// to a blocked address.
func control(allowPrivate bool) func(network, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return errBlocked
		}
		a, err := netip.ParseAddr(host)
		if err != nil || !Allowed(a, allowPrivate) {
			return errBlocked
		}
		return nil
	}
}
