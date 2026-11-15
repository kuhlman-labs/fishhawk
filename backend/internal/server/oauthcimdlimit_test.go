package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
)

// frozenClock is a manually-advanced clock. Every limiter test runs on it with
// ZERO sleeps and no elapsed-time upper bounds — no duration here competes with
// a deadline, so the AGENTS.md timescale D(base) rule does not apply (binding
// constraint 3).
type frozenClock struct{ t time.Time }

func (c *frozenClock) now() time.Time          { return c.t }
func (c *frozenClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newFrozen() *frozenClock {
	return &frozenClock{t: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)}
}

// TestOAuthCIMDLimiter_PerSourceBurstExhausted: the (burst+1)th Allow from one
// key is refused. Delete the per-source bucket (make it admit unconditionally)
// and the (burst+1)th call passes → this goes GREEN, so the test pins the
// bucket.
func TestOAuthCIMDLimiter_PerSourceBurstExhausted(t *testing.T) {
	clk := newFrozen()
	// Large global burst so the GLOBAL bucket is not the thing that refuses.
	lim := newOAuthCIMDLimiter(clk.now, 5, 10*time.Second, 1000, time.Second, 100)
	for i := 0; i < 5; i++ {
		if ok, _ := lim.Allow("1.2.3.4"); !ok {
			t.Fatalf("admit %d within burst must pass", i)
		}
	}
	ok, wait := lim.Allow("1.2.3.4")
	if ok {
		t.Fatal("the 6th request from one source must be refused (burst 5)")
	}
	if wait <= 0 {
		t.Fatalf("refusal wait must be > 0, got %v", wait)
	}
}

// TestOAuthCIMDLimiter_RefillsAfterInterval: refused, then admitted after one
// interval, and NOT admitted at interval-minus-a-tick. (CONDITION 3 criterion:
// an exhausted bucket refills.)
func TestOAuthCIMDLimiter_RefillsAfterInterval(t *testing.T) {
	clk := newFrozen()
	lim := newOAuthCIMDLimiter(clk.now, 1, 10*time.Second, 1000, time.Second, 100)
	if ok, _ := lim.Allow("1.2.3.4"); !ok {
		t.Fatal("first admit must pass")
	}
	if ok, _ := lim.Allow("1.2.3.4"); ok {
		t.Fatal("second admit must be refused (burst 1)")
	}
	// One tick short of a full interval: still refused.
	clk.advance(10*time.Second - time.Nanosecond)
	if ok, _ := lim.Allow("1.2.3.4"); ok {
		t.Fatal("must still be refused one tick before the refill interval")
	}
	// Exactly one interval from the last admit: refilled.
	clk.advance(time.Nanosecond)
	if ok, _ := lim.Allow("1.2.3.4"); !ok {
		t.Fatal("must be admitted after a full refill interval")
	}
}

// TestOAuthCIMDLimiter_GlobalCeilingBoundsSourceRotation: each request comes
// from a FRESH source (so no per-source bucket is near empty), yet the global
// ceiling refuses once its burst is spent. This is the /64-rotation residual's
// covering test — delete the global bucket and rotation never refuses.
func TestOAuthCIMDLimiter_GlobalCeilingBoundsSourceRotation(t *testing.T) {
	clk := newFrozen()
	lim := newOAuthCIMDLimiter(clk.now, 5, 10*time.Second, 3, time.Second, 1000)
	for i := 0; i < 3; i++ {
		if ok, _ := lim.Allow(freshSource(i)); !ok {
			t.Fatalf("fresh source %d within global burst must pass", i)
		}
	}
	ok, wait := lim.Allow(freshSource(99))
	if ok {
		t.Fatal("a fresh source must be refused once the global burst is spent")
	}
	if wait <= 0 {
		t.Fatalf("global-refusal wait must be > 0, got %v", wait)
	}
}

// TestOAuthCIMDLimiter_CombinedGlobalEmptyRefusesSourceUnchanged (CONDITION 1):
// per-source has tokens, global is empty → refused, per-source token count
// UNCHANGED (atomic: consume from neither), and the returned wait EQUALS the
// global bucket's wait.
func TestOAuthCIMDLimiter_CombinedGlobalEmptyRefusesSourceUnchanged(t *testing.T) {
	clk := newFrozen()
	lim := newOAuthCIMDLimiter(clk.now, 5, 10*time.Second, 3, time.Second, 1000)
	// Spend the global burst (3) across THREE distinct sources so none of their
	// per-source buckets is near empty.
	for i := 0; i < 3; i++ {
		if ok, _ := lim.Allow(freshSource(i)); !ok {
			t.Fatalf("priming admit %d must pass", i)
		}
	}
	// A fresh target source: its per-source bucket is full (5), global is empty.
	target := freshSource(42)
	before := lim.sourceTokens(target)
	ok, wait := lim.Allow(target)
	if ok {
		t.Fatal("must refuse when the global bucket is empty even though the source has tokens")
	}
	after := lim.sourceTokens(target)
	if before != after {
		t.Fatalf("per-source tokens changed on a global-only refusal: %v -> %v (must consume from neither)", before, after)
	}
	if wait != time.Second { // one global token per 1s; global at 0 → 1s
		t.Fatalf("returned wait = %v, want the global bucket's wait (1s)", wait)
	}
}

// TestOAuthCIMDLimiter_CombinedSourceEmptyRefusesGlobalUnchanged (CONDITION 1,
// reverse): source is empty, global has tokens → refused, global token count
// UNCHANGED, and the returned wait EQUALS the source bucket's wait.
func TestOAuthCIMDLimiter_CombinedSourceEmptyRefusesGlobalUnchanged(t *testing.T) {
	clk := newFrozen()
	lim := newOAuthCIMDLimiter(clk.now, 5, 10*time.Second, 30, time.Second, 1000)
	// Drain ONE source's 5 tokens (also 5 global, of 30).
	for i := 0; i < 5; i++ {
		if ok, _ := lim.Allow("1.2.3.4"); !ok {
			t.Fatalf("priming admit %d must pass", i)
		}
	}
	globalBefore := lim.globalTokens()
	ok, wait := lim.Allow("1.2.3.4")
	if ok {
		t.Fatal("must refuse when the source bucket is empty even though global has tokens")
	}
	globalAfter := lim.globalTokens()
	if globalBefore != globalAfter {
		t.Fatalf("global tokens changed on a source-only refusal: %v -> %v (must consume from neither)", globalBefore, globalAfter)
	}
	if wait != 10*time.Second { // one source token per 10s; source at 0 → 10s
		t.Fatalf("returned wait = %v, want the source bucket's wait (10s)", wait)
	}
}

// TestOAuthCIMDLimiter_CombinedBothEmptyReturnsMaxWait pins CONDITION 1(b)'s
// load-bearing rule directly: when BOTH buckets are empty the returned wait is
// the MAXIMUM of the two, never the per-source one. The independent-exhaustion
// cases above each leave ONE bucket with tokens, so an implementation that
// returned the source wait whenever it was positive (and only otherwise the
// global wait) would pass all of them yet understate Retry-After here whenever
// the global wait is the longer. Both sub-cases drain BOTH buckets from one
// source (equal bursts, so N admits empty both) at a frozen clock, then assert
// the refusal wait equals the LONGER refill regardless of which bucket it is.
func TestOAuthCIMDLimiter_CombinedBothEmptyReturnsMaxWait(t *testing.T) {
	drainBothThenRefuse := func(t *testing.T, sourceRefill, globalRefill time.Duration) time.Duration {
		t.Helper()
		clk := newFrozen()
		// Equal bursts (3) so 3 admits from ONE source empty BOTH buckets.
		lim := newOAuthCIMDLimiter(clk.now, 3, sourceRefill, 3, globalRefill, 100)
		for i := 0; i < 3; i++ {
			if ok, _ := lim.Allow("1.2.3.4"); !ok {
				t.Fatalf("admit %d within both bursts must pass", i)
			}
		}
		ok, wait := lim.Allow("1.2.3.4")
		if ok {
			t.Fatal("must refuse with both buckets empty")
		}
		return wait
	}

	t.Run("global wait longer", func(t *testing.T) {
		// source 1s, global 10s → both empty → max is the global wait (10s).
		if wait := drainBothThenRefuse(t, time.Second, 10*time.Second); wait != 10*time.Second {
			t.Fatalf("returned wait = %v, want the LONGER (global) wait 10s — the MAX of the two", wait)
		}
	})
	t.Run("source wait longer", func(t *testing.T) {
		// Reverse: source 10s, global 1s → both empty → max is the source wait (10s).
		if wait := drainBothThenRefuse(t, 10*time.Second, time.Second); wait != 10*time.Second {
			t.Fatalf("returned wait = %v, want the LONGER (source) wait 10s — the MAX of the two", wait)
		}
	})
}

// TestOAuthCIMDLimiter_TrackedSourcesBounded: maxSources+50 distinct keys leave
// the tracked count at exactly maxSources. Delete the LRU eviction and the count
// climbs past maxSources → RED.
func TestOAuthCIMDLimiter_TrackedSourcesBounded(t *testing.T) {
	clk := newFrozen()
	const maxSources = 8
	lim := newOAuthCIMDLimiter(clk.now, 5, 10*time.Second, 1_000_000, time.Second, maxSources)
	for i := 0; i < maxSources+50; i++ {
		lim.Allow(freshSource(i))
	}
	if got := lim.trackedSources(); got != maxSources {
		t.Fatalf("tracked sources = %d, want exactly maxSources=%d", got, maxSources)
	}
}

// TestOAuthCIMDLimiter_UnparseableSourceSharesOneBucket: two DISTINCT malformed
// keys draw from the SAME shared bucket, so burst is shared. Here both keys map
// to the unknown sentinel; with burst 1, the second's first request is refused.
func TestOAuthCIMDLimiter_UnparseableSourceSharesOneBucket(t *testing.T) {
	clk := newFrozen()
	lim := newOAuthCIMDLimiter(clk.now, 1, 10*time.Second, 1000, time.Second, 100)
	if ok, _ := lim.Allow(oauthCIMDSourceKey(reqWithRemote("garbage-1"))); !ok {
		t.Fatal("first unknown-source request must pass")
	}
	if ok, _ := lim.Allow(oauthCIMDSourceKey(reqWithRemote("garbage-2"))); ok {
		t.Fatal("a second, DIFFERENT unparseable source must share the one unknown bucket and be refused")
	}
}

// TestOAuthCIMDLimiter_IPv4MappedSharesBucketWithIPv4: ::ffff:1.2.3.4 and
// 1.2.3.4 canonicalise (Unmap) to ONE key, so they share a bucket. Delete Unmap
// and they get independent buckets → the second passes → RED.
func TestOAuthCIMDLimiter_IPv4MappedSharesBucketWithIPv4(t *testing.T) {
	clk := newFrozen()
	lim := newOAuthCIMDLimiter(clk.now, 1, 10*time.Second, 1000, time.Second, 100)
	k1 := oauthCIMDSourceKey(reqWithRemote("1.2.3.4:5555"))
	k2 := oauthCIMDSourceKey(reqWithRemote("[::ffff:1.2.3.4]:6666"))
	if k1 != k2 {
		t.Fatalf("Unmap canonicalisation failed: %q != %q", k1, k2)
	}
	if ok, _ := lim.Allow(k1); !ok {
		t.Fatal("first request must pass")
	}
	if ok, _ := lim.Allow(k2); ok {
		t.Fatal("the IPv4-mapped form must share the IPv4 bucket and be refused")
	}
}

// TestOAuthCIMDLimiter_ZeroConfigResolvesToDefaults: a zero-valued construction
// still refuses past the DEFAULT burst — never unbounded. Delete the
// default-resolution and burst 0 would refuse everything OR (if coerced to a
// huge value) never refuse; the default-burst assertion pins the middle.
func TestOAuthCIMDLimiter_ZeroConfigResolvesToDefaults(t *testing.T) {
	clk := newFrozen()
	lim := newOAuthCIMDLimiter(clk.now, 0, 0, 0, 0, 0)
	for i := 0; i < defaultOAuthCIMDSourceBurst; i++ {
		if ok, _ := lim.Allow("1.2.3.4"); !ok {
			t.Fatalf("admit %d within the default burst must pass", i)
		}
	}
	if ok, _ := lim.Allow("1.2.3.4"); ok {
		t.Fatalf("must refuse past the default source burst (%d)", defaultOAuthCIMDSourceBurst)
	}
}

// TestOAuthCIMDLimiter_RetryAfterShrinksAsClockAdvances: the returned wait is
// read as STATE (binding constraint 2) — > 0 on refusal and strictly shrinking
// as the clock advances toward the refill.
func TestOAuthCIMDLimiter_RetryAfterShrinksAsClockAdvances(t *testing.T) {
	clk := newFrozen()
	lim := newOAuthCIMDLimiter(clk.now, 1, 10*time.Second, 1000, time.Second, 100)
	if ok, _ := lim.Allow("1.2.3.4"); !ok {
		t.Fatal("first admit must pass")
	}
	_, w0 := lim.Allow("1.2.3.4")
	if w0 <= 0 {
		t.Fatalf("wait must be > 0 on refusal, got %v", w0)
	}
	clk.advance(4 * time.Second)
	_, w1 := lim.Allow("1.2.3.4")
	if w1 >= w0 {
		t.Fatalf("wait must shrink as the clock advances: w0=%v w1=%v", w0, w1)
	}
}

// TestOAuthCIMDLimiter_SourceKeyDerivation pins the source-key helper directly.
func TestOAuthCIMDLimiter_SourceKeyDerivation(t *testing.T) {
	cases := []struct {
		remote, want string
	}{
		{"1.2.3.4:5555", "1.2.3.4"},
		{"[2001:db8::1]:443", "2001:db8::1"},
		{"[::ffff:1.2.3.4]:80", "1.2.3.4"},
		{"", oauthCIMDUnknownSourceKey},
		{"garbage", oauthCIMDUnknownSourceKey},
	}
	for _, c := range cases {
		if got := oauthCIMDSourceKey(reqWithRemote(c.remote)); got != c.want {
			t.Errorf("oauthCIMDSourceKey(%q) = %q, want %q", c.remote, got, c.want)
		}
	}
}

// TestOAuthCIMDLimiter_RateLimitedErrorCarriesTypedWait pins CONDITION 2: the
// wait is recoverable STRUCTURALLY from the typed carrier, and the sentinel axis
// still matches. String-parsing or re-consulting the limiter would not satisfy
// both.
func TestOAuthCIMDLimiter_RateLimitedErrorCarriesTypedWait(t *testing.T) {
	oe := &oauthas.Error{
		Code:  oauthas.ErrCodeTemporarilyUnavailable,
		Cause: &cimdRateLimitedError{retryAfter: 7 * time.Second},
	}
	if !errors.Is(oe, errCIMDRateLimited) {
		t.Fatal("sentinel identity must match via errors.Is")
	}
	var rl *cimdRateLimitedError
	if !errors.As(oe, &rl) {
		t.Fatal("typed carrier must be recoverable via errors.As")
	}
	if rl.retryAfter != 7*time.Second {
		t.Fatalf("retryAfter = %v, want 7s", rl.retryAfter)
	}
}

// TestOAuthCIMDLimiter_RefusalNamesRatesAndRemedy (#5224) drives the REAL
// authorize and token routes against a server whose limiter is configured
// through Config with DISTINCTIVE refill values (7s per source, 3s global, so
// neither matches a default), exhausts the per-source bucket, and asserts the
// 429's error_description names both configured rates, both bursts and the
// pre-registration remedy. Restoring the old constant description, or rendering
// the defaults instead of the configured values, turns this RED.
func TestOAuthCIMDLimiter_RefusalNamesRatesAndRemedy(t *testing.T) {
	newServer := func() *Server {
		return New(Config{
			OAuthASIssuer:               "https://as.example",
			OAuthStore:                  newFakeOAuthStore(),
			OAuthCIMDFetcher:            newCIMDFetcher(newCIMD()),
			OAuthCIMDRateBurst:          1,
			OAuthCIMDRateInterval:       7 * time.Second,
			OAuthCIMDGlobalRateBurst:    40,
			OAuthCIMDGlobalRateInterval: 3 * time.Second,
		})
	}
	wantParts := []string{
		"one per 7s per source, burst 1",
		"one per 3s globally, burst 40",
		"fishhawkd oauth client register",
		"Retry-After",
	}
	check := func(t *testing.T, rr *httptest.ResponseRecorder) {
		t.Helper()
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("status = %d, want 429; body=%s", rr.Code, rr.Body.String())
		}
		var body oauthErrorBody
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode the 429 body: %v (%s)", err, rr.Body.String())
		}
		if body.Error != "temporarily_unavailable" {
			t.Fatalf("error = %q, want temporarily_unavailable", body.Error)
		}
		for _, want := range wantParts {
			if !strings.Contains(body.ErrorDescription, want) {
				t.Errorf("error_description %q does not name %q", body.ErrorDescription, want)
			}
		}
	}

	t.Run("authorize", func(t *testing.T) {
		srv := newServer()
		const src = "198.51.100.7:7777"
		if rr := getAuthorizeFrom(srv, authorizeQuery(map[string]string{"client_id": "https://d0.example/cimd"}), src); rr.Code == http.StatusTooManyRequests {
			t.Fatalf("the first request (burst 1) was rate limited; body=%s", rr.Body.String())
		}
		check(t, getAuthorizeFrom(srv, authorizeQuery(map[string]string{"client_id": "https://d1.example/cimd"}), src))
	})
	t.Run("token", func(t *testing.T) {
		srv := newServer()
		form := func(clientID string) url.Values {
			v := url.Values{}
			v.Set("grant_type", "authorization_code")
			v.Set("code", "irrelevant")
			v.Set("client_id", clientID)
			return v
		}
		if rr := postToken(srv, form("https://t0.example/cimd"), nil); rr.Code == http.StatusTooManyRequests {
			t.Fatalf("the first request (burst 1) was rate limited; body=%s", rr.Body.String())
		}
		check(t, postToken(srv, form("https://t1.example/cimd"), nil))
	})
}

// TestOAuthCIMDLimiter_RefusalDescriptionIsRFC6749Charset pins the rendering of
// sub-second intervals: RFC 6749 §5.2 limits error_description to %x20-21 /
// %x23-5B / %x5D-7E. A 500µs refill must render in decimal seconds, not as
// time.Duration.String's "500µs" (U+00B5); rendering with Duration.String turns
// this RED.
func TestOAuthCIMDLimiter_RefusalDescriptionIsRFC6749Charset(t *testing.T) {
	lim := newOAuthCIMDLimiter(newFrozen().now, 2, 500*time.Microsecond, 3, 1500*time.Millisecond, 10)
	desc := lim.refusalDescription()
	for i := 0; i < len(desc); i++ {
		if c := desc[i]; c < 0x20 || c > 0x7e || c == '"' || c == '\\' {
			t.Fatalf("error_description byte %d = %#x is outside the RFC 6749 §5.2 set: %q", i, c, desc)
		}
	}
	for _, want := range []string{"one per 0.0005s per source, burst 2", "one per 1.5s globally, burst 3"} {
		if !strings.Contains(desc, want) {
			t.Errorf("error_description %q does not name %q", desc, want)
		}
	}
}

// TestOAuthCIMDLimiter_RefusalDescriptionDocumented pins the two prose copies
// of the default 429 text (#5224) to the accessor that builds it. Each site
// quotes the default-policy description inside backticks, wrapped across
// lines, so the comparison collapses whitespace. When the text changes, update
// BOTH sites this test names.
func TestOAuthCIMDLimiter_RefusalDescriptionDocumented(t *testing.T) {
	want := newOAuthCIMDLimiter(newFrozen().now, 0, 0, 0, 0, 0).refusalDescription()
	for _, site := range []string{"docs/auth.md", "../../../docs/local-tls.md"} {
		b, err := os.ReadFile(site)
		if err != nil {
			t.Fatalf("read %s: %v", site, err)
		}
		if !strings.Contains(strings.Join(strings.Fields(string(b)), " "), want) {
			t.Errorf("%s does not quote the default CIMD limiter 429 text %q; update backend/internal/server/docs/auth.md (\"In-place 429\") and docs/local-tls.md (\"Known limitation — no loopback CIMD\")", site, want)
		}
	}
}

func freshSource(i int) string {
	// Distinct IPv6 addresses in a /112, one per index — a stand-in for /64
	// rotation. Kept literal (no fmt in a hot helper is fine here, but keep it
	// simple and deterministic).
	return "2001:db8::" + itoa16(i)
}

// itoa16 renders i as a hex nibble-string suffix, enough distinct values for the
// tests without importing fmt into assertions.
func itoa16(i int) string {
	const hexdigits = "0123456789abcdef"
	if i == 0 {
		return "0"
	}
	var buf []byte
	for i > 0 {
		buf = append([]byte{hexdigits[i%16]}, buf...)
		i /= 16
	}
	return string(buf)
}

func reqWithRemote(remote string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = remote
	return r
}
