package oauthas

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
)

// specialUsePrefixes is the explicitly enumerated set of IANA IPv4/IPv6
// special-purpose prefixes the connect guard refuses. It is deliberately NOT
// derived from net.IP.IsGlobalUnicast plus ad-hoc exclusions: IsGlobalUnicast
// returns true for CGNAT (100.64.0.0/10), IETF protocol assignments
// (192.0.0.0/24), benchmarking (198.18.0.0/15) and deprecated IPv6 site-local
// (fec0::/10), so it is not a publicly-routable predicate (#2427, third defect).
var specialUsePrefixes = []netip.Prefix{
	// IPv4 (IANA IPv4 Special-Purpose Address Registry / RFC 6890).
	netip.MustParsePrefix("0.0.0.0/8"),       // "this host on this network"
	netip.MustParsePrefix("10.0.0.0/8"),      // private-use
	netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT (RFC 6598)
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local (subsumes 169.254.169.254 metadata)
	netip.MustParsePrefix("172.16.0.0/12"),   // private-use
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation (TEST-NET-1)
	netip.MustParsePrefix("192.88.99.0/24"),  // 6to4 relay anycast (deprecated)
	netip.MustParsePrefix("192.168.0.0/16"),  // private-use
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"), // documentation (TEST-NET-2)
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation (TEST-NET-3)
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved (subsumes 255.255.255.255 broadcast)
	// IPv6 (IANA IPv6 Special-Purpose Address Registry / RFC 6890).
	netip.MustParsePrefix("::/128"),         // unspecified
	netip.MustParsePrefix("::1/128"),        // loopback
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("2001::/23"),      // IETF protocol assignments (incl. Teredo)
	netip.MustParsePrefix("2001:db8::/32"),  // documentation
	netip.MustParsePrefix("fc00::/7"),       // unique-local (ULA)
	netip.MustParsePrefix("fe80::/10"),      // link-local
	netip.MustParsePrefix("fec0::/10"),      // site-local (deprecated, RFC 3879)
	netip.MustParsePrefix("ff00::/8"),       // multicast
}

// addrGuard reports whether a resolved address may be dialed. It returns a
// non-nil error (carrying ErrBlockedAddress) to refuse.
type addrGuard func(netip.Addr) error

// lookupFunc resolves a host to candidate addresses. It is the injected DNS seam;
// the default binds net.DefaultResolver.LookupNetIP to "ip".
type lookupFunc func(ctx context.Context, host string) ([]netip.Addr, error)

// PublicOnlyAddrGuard refuses any address that is not publicly routable. It unmaps
// an IPv4-mapped IPv6 address first (so ::ffff:127.0.0.1 is judged as 127.0.0.1),
// refuses an invalid/zero address and a zoned address, and refuses any address
// inside a listed special-use prefix. The refused address is carried in Cause,
// never in the client-facing Description.
func PublicOnlyAddrGuard(addr netip.Addr) error {
	addr = addr.Unmap()
	if !addr.IsValid() {
		return newError(ErrCodeInvalidTarget, "destination address is invalid").withCause(ErrBlockedAddress)
	}
	if addr.Zone() != "" {
		return newError(ErrCodeInvalidTarget, "destination address is not permitted").
			withCause(fmt.Errorf("%w: zoned address %s", ErrBlockedAddress, addr))
	}
	for _, p := range specialUsePrefixes {
		if p.Contains(addr) {
			return newError(ErrCodeInvalidTarget, "destination address is not publicly routable").
				withCause(fmt.Errorf("%w: %s in %s", ErrBlockedAddress, addr, p))
		}
	}
	return nil
}

// guardedDialer is a connect-time SSRF-guarding dialer. It resolves a hostname
// ONCE through the injected lookup seam, fails closed if ANY candidate is blocked,
// and dials the guarded IP literal so the transport never re-resolves the
// hostname — closing the DNS-rebinding hole a pre-flight resolve leaves open. Its
// inner net.Dialer also carries a ControlContext that re-runs the guard on the
// socket's actual peer address, the load-bearing connect-time enforcement point.
type guardedDialer struct {
	guard   addrGuard
	lookup  lookupFunc
	dialRaw func(ctx context.Context, network, address string) (net.Conn, error)
}

// newGuardedDialer builds a guardedDialer, defaulting the guard to
// PublicOnlyAddrGuard and the lookup to net.DefaultResolver.LookupNetIP.
func newGuardedDialer(guard addrGuard, lookup lookupFunc) *guardedDialer {
	if guard == nil {
		guard = PublicOnlyAddrGuard
	}
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	d := &guardedDialer{guard: guard, lookup: lookup}
	inner := &net.Dialer{ControlContext: d.control}
	d.dialRaw = inner.DialContext
	return d
}

// control re-runs the guard on the socket's actual peer address after the
// connection is created and before the dial completes (net.Dialer.ControlContext
// contract). This enforces the guard even if something re-resolved the host.
func (d *guardedDialer) control(_ context.Context, _, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return newError(ErrCodeInvalidTarget, "connect address is not an IP literal").
			withCause(fmt.Errorf("%w: %q", ErrBlockedAddress, host))
	}
	return d.guard(addr)
}

// DialContext resolves addr, guards every resolved candidate (failing closed if
// any is blocked), and dials the guarded IP literal.
func (d *guardedDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, newError(ErrCodeInvalidTarget, "destination address is malformed").withCause(err)
	}
	// IP-literal host: guard and dial it directly.
	if addr, perr := netip.ParseAddr(host); perr == nil {
		if gerr := d.guard(addr.Unmap()); gerr != nil {
			return nil, gerr
		}
		return d.dialRaw(ctx, network, net.JoinHostPort(addr.Unmap().String(), port))
	}
	// Hostname: resolve ONCE and fail closed if ANY candidate is blocked. One
	// private A record among public ones is an attack, not a fallback.
	addrs, err := d.lookup(ctx, host)
	if err != nil {
		return nil, newError(ErrCodeInvalidTarget, "destination host could not be resolved").withCause(err)
	}
	if len(addrs) == 0 {
		return nil, newError(ErrCodeInvalidTarget, "destination host resolved to no addresses").
			withCause(fmt.Errorf("%w: %q", ErrBlockedAddress, host))
	}
	for _, a := range addrs {
		if gerr := d.guard(a.Unmap()); gerr != nil {
			return nil, gerr
		}
	}
	// Dial the pinned, guarded literal so the transport cannot re-resolve.
	target := addrs[0].Unmap()
	return d.dialRaw(ctx, network, net.JoinHostPort(target.String(), port))
}
