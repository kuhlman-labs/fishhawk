package oauthas

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestPublicOnlyAddrGuard(t *testing.T) {
	t.Parallel()
	// One refused case per enumerated special-use prefix.
	blocked := []string{
		"0.0.0.1", "10.0.0.1", "100.64.0.1", "127.0.0.1", "169.254.169.254",
		"172.16.0.1", "192.0.0.1", "192.0.2.1", "192.88.99.1", "192.168.0.1",
		"198.18.0.1", "198.51.100.1", "203.0.113.1", "224.0.0.1", "240.0.0.1",
		"::", "::1", "64:ff9b:1::1", "100::1", "2001::1", "2001:db8::1",
		"fc00::1", "fd00::1", "fe80::1", "fec0::1", "ff00::1",
	}
	for _, s := range blocked {
		t.Run("blocked/"+s, func(t *testing.T) {
			t.Parallel()
			err := PublicOnlyAddrGuard(netip.MustParseAddr(s))
			if err == nil {
				t.Fatalf("PublicOnlyAddrGuard(%s) = nil, want refusal", s)
			}
			if !errors.Is(err, ErrBlockedAddress) {
				t.Fatalf("PublicOnlyAddrGuard(%s) did not wrap ErrBlockedAddress: %v", s, err)
			}
			if got := err.(*Error).Description; got == "" || containsSubstr(got, s) {
				t.Fatalf("client-facing description must not echo the address, got %q", got)
			}
		})
	}
	// Public positives are accepted.
	for _, s := range []string{"8.8.8.8", "2606:4700::1111", "1.1.1.1"} {
		t.Run("public/"+s, func(t *testing.T) {
			t.Parallel()
			if err := PublicOnlyAddrGuard(netip.MustParseAddr(s)); err != nil {
				t.Fatalf("PublicOnlyAddrGuard(%s) = %v, want accept", s, err)
			}
		})
	}
}

// TestPublicOnlyAddrGuard_RefusesIsGlobalUnicastRanges pins the four ranges the
// naive IsGlobalUnicast approach lets through, asserting BOTH that the guard
// refuses them AND that IsGlobalUnicast accepts them, so a regression to the naive
// predicate fails here (#2427, third defect).
func TestPublicOnlyAddrGuard_RefusesIsGlobalUnicastRanges(t *testing.T) {
	t.Parallel()
	for _, s := range []string{"100.64.0.1", "192.0.0.1", "198.18.0.1", "fec0::1"} {
		addr := netip.MustParseAddr(s)
		if !addr.IsGlobalUnicast() {
			t.Fatalf("precondition: %s should satisfy IsGlobalUnicast (documents why the naive predicate is insufficient)", s)
		}
		if err := PublicOnlyAddrGuard(addr); err == nil || !errors.Is(err, ErrBlockedAddress) {
			t.Fatalf("PublicOnlyAddrGuard(%s) = %v, want ErrBlockedAddress", s, err)
		}
	}
}

func TestPublicOnlyAddrGuard_UnmapsV4Mapped(t *testing.T) {
	t.Parallel()
	// ::ffff:127.0.0.1 must be judged as 127.0.0.1 after Unmap.
	addr := netip.MustParseAddr("::ffff:127.0.0.1")
	if err := PublicOnlyAddrGuard(addr); err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("PublicOnlyAddrGuard(::ffff:127.0.0.1) = %v, want ErrBlockedAddress", err)
	}
}

func TestPublicOnlyAddrGuard_RefusesZeroAndZoned(t *testing.T) {
	t.Parallel()
	if err := PublicOnlyAddrGuard(netip.Addr{}); err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("zero address = %v, want ErrBlockedAddress", err)
	}
	zoned := netip.MustParseAddr("2606:4700::1111%eth0")
	if err := PublicOnlyAddrGuard(zoned); err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("zoned public address = %v, want ErrBlockedAddress", err)
	}
}

func TestGuardedDialer_GuardsIPLiteralHost(t *testing.T) {
	t.Parallel()
	gd := newGuardedDialer(PublicOnlyAddrGuard, failLookup(t))
	gd.dialRaw = failDial(t)
	_, err := gd.DialContext(context.Background(), "tcp", "127.0.0.1:80")
	if err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("DialContext to private literal = %v, want ErrBlockedAddress", err)
	}
}

func TestGuardedDialer_RefusesPrivateLookupResult(t *testing.T) {
	t.Parallel()
	lookup := staticLookup(netip.MustParseAddr("10.0.0.5"))
	gd := newGuardedDialer(PublicOnlyAddrGuard, lookup)
	gd.dialRaw = failDial(t)
	_, err := gd.DialContext(context.Background(), "tcp", "internal.example:443")
	if err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("DialContext to private-resolving host = %v, want ErrBlockedAddress", err)
	}
}

func TestGuardedDialer_FailsClosedWhenAnyCandidateBlocked(t *testing.T) {
	t.Parallel()
	// One public plus one private answer → refused, not "skip to the next".
	lookup := staticLookup(netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("192.168.1.5"))
	gd := newGuardedDialer(PublicOnlyAddrGuard, lookup)
	gd.dialRaw = failDial(t)
	_, err := gd.DialContext(context.Background(), "tcp", "dualstack.example:443")
	if err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("DialContext with a mixed answer = %v, want ErrBlockedAddress (fail closed)", err)
	}
}

func TestGuardedDialer_DialsCheckedLiteralNotHostname(t *testing.T) {
	t.Parallel()
	public := netip.MustParseAddr("93.184.216.34")
	lookup := staticLookup(public)
	gd := newGuardedDialer(PublicOnlyAddrGuard, lookup)
	var dialed string
	gd.dialRaw = func(_ context.Context, _, address string) (net.Conn, error) {
		dialed = address
		return nil, errStopDial
	}
	_, err := gd.DialContext(context.Background(), "tcp", "cdn.example:443")
	if !errors.Is(err, errStopDial) {
		t.Fatalf("expected the recorder sentinel, got %v", err)
	}
	want := net.JoinHostPort(public.String(), "443")
	if dialed != want {
		t.Fatalf("dialed %q, want the guarded IP literal %q (transport must not re-resolve)", dialed, want)
	}
	if _, perr := netip.ParseAddr(hostOnly(t, dialed)); perr != nil {
		t.Fatalf("dialed host %q is not an IP literal: %v", dialed, perr)
	}
}

// TestGuardedDialer_ControlGuardsPeerAddress exercises the ControlContext
// enforcement point directly (the load-bearing connect-time hook): a blocked peer
// literal is refused, a public one passes, and a non-IP peer is refused.
func TestGuardedDialer_ControlGuardsPeerAddress(t *testing.T) {
	t.Parallel()
	gd := newGuardedDialer(PublicOnlyAddrGuard, failLookup(t))
	if err := gd.control(context.Background(), "tcp4", "127.0.0.1:80", nil); err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("control(private peer) = %v, want ErrBlockedAddress", err)
	}
	if err := gd.control(context.Background(), "tcp4", "8.8.8.8:443", nil); err != nil {
		t.Fatalf("control(public peer) = %v, want nil", err)
	}
	if err := gd.control(context.Background(), "tcp", "not-an-ip", nil); err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("control(non-IP peer) = %v, want ErrBlockedAddress", err)
	}
}

func TestGuardedDialer_RejectsMalformedAddress(t *testing.T) {
	t.Parallel()
	gd := newGuardedDialer(PublicOnlyAddrGuard, failLookup(t))
	gd.dialRaw = failDial(t)
	if _, err := gd.DialContext(context.Background(), "tcp", "no-port-here"); err == nil {
		t.Fatalf("DialContext(malformed) = nil, want error")
	}
}

func TestGuardedDialer_LookupErrorAndEmpty(t *testing.T) {
	t.Parallel()
	// Lookup returns an error.
	gdErr := newGuardedDialer(PublicOnlyAddrGuard, func(context.Context, string) ([]netip.Addr, error) {
		return nil, errors.New("resolve failed")
	})
	gdErr.dialRaw = failDial(t)
	if _, err := gdErr.DialContext(context.Background(), "tcp", "host.example:443"); err == nil {
		t.Fatalf("DialContext(lookup error) = nil, want error")
	}
	// Lookup returns no addresses.
	gdEmpty := newGuardedDialer(PublicOnlyAddrGuard, staticLookup())
	gdEmpty.dialRaw = failDial(t)
	if _, err := gdEmpty.DialContext(context.Background(), "tcp", "host.example:443"); err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("DialContext(empty resolve) = %v, want ErrBlockedAddress", err)
	}
}

// TestNewGuardedDialer_DefaultsInstalled proves the nil-guard/nil-lookup defaults
// resolve (the default lookup binds the real resolver; the default guard is
// PublicOnlyAddrGuard, asserted here on an IP-literal path that never resolves).
func TestNewGuardedDialer_DefaultsInstalled(t *testing.T) {
	t.Parallel()
	gd := newGuardedDialer(nil, nil)
	if gd.guard == nil || gd.lookup == nil || gd.dialRaw == nil {
		t.Fatalf("newGuardedDialer left a nil seam: guard=%v lookup=%v dialRaw=%v", gd.guard == nil, gd.lookup == nil, gd.dialRaw == nil)
	}
	gd.dialRaw = failDial(t)
	if _, err := gd.DialContext(context.Background(), "tcp", "127.0.0.1:80"); err == nil || !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("default guard = %v, want PublicOnlyAddrGuard refusal", err)
	}
}

var errStopDial = errors.New("stop dial (test)")

func staticLookup(addrs ...netip.Addr) lookupFunc {
	return func(context.Context, string) ([]netip.Addr, error) { return addrs, nil }
}

func failLookup(t *testing.T) lookupFunc {
	return func(context.Context, string) ([]netip.Addr, error) {
		t.Fatalf("lookup should not be called for an IP-literal host")
		return nil, nil
	}
}

func failDial(t *testing.T) func(context.Context, string, string) (net.Conn, error) {
	return func(_ context.Context, _, address string) (net.Conn, error) {
		t.Fatalf("dialRaw should not be called; address=%q", address)
		return nil, nil
	}
}

func hostOnly(t *testing.T, hostport string) string {
	t.Helper()
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", hostport, err)
	}
	return h
}

func containsSubstr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
