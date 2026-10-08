// Package outbound decides which addresses gitserver may connect to on a
// user's behalf (pull mirrors). Users choose the URL, so without a check
// they could make the server reach what only it can reach: localhost
// services, the private network, or a cloud provider's metadata service
// (169.254.169.254). Only public internet addresses are allowed.
package outbound

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// notPublic are address blocks that IsPublic rejects besides the ones
// netip classifies (loopback, private, link-local, multicast, unspecified).
var notPublic = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // carrier-grade NAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 can lead anywhere in IPv4
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("fec0::/10"),
}

// IsPublic reports whether a is an ordinary internet address.
func IsPublic(a netip.Addr) bool {
	a = a.Unmap()
	if !a.IsValid() || a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range notPublic {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// ErrNotPublic is returned for a host with a non-public address.
var ErrNotPublic = errors.New("only public internet addresses are allowed")

// PublicAddrs resolves host (a name or an IP address) and returns its
// addresses. It fails if any of them is not public, so a name can't point
// half its addresses inside.
func PublicAddrs(ctx context.Context, host string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		if !IsPublic(a) {
			return nil, fmt.Errorf("%s: %w", host, ErrNotPublic)
		}
		return []netip.Addr{a.Unmap()}, nil
	}
	addrs, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("looking up %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s has no addresses", host)
	}
	for i, a := range addrs {
		if !IsPublic(a) {
			return nil, fmt.Errorf("%s (%s): %w", host, a, ErrNotPublic)
		}
		addrs[i] = a.Unmap()
	}
	return addrs, nil
}
