package server

import (
	"container/list"
	"errors"
	"fmt"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/oauthas"
)

// oauthCIMDLimiter bounds the outbound CIMD amplification primitive the two
// unauthenticated OAuth AS routes expose (ADR-076 / #2441): an unseen client_id
// URL on GET /v0/oauth/authorize or POST /v0/oauth/token induces one outbound
// HTTPS fetch, and nothing else bounds the rate or volume of those fetches.
//
// It holds TWO token buckets, BOTH of which must admit for a request to pass:
//
//   - a PER-SOURCE bucket keyed by canonicalised source address, and
//   - ONE GLOBAL bucket.
//
// The global bucket is not redundant. A per-source limiter alone is defeated by
// an attacker rotating across an IPv6 /64: each fresh address gets its own
// full-burst bucket, and the evicted ones simply drop out of the LRU below —
// there is no per-source memory of a source once it evicts. The global bucket is
// what bounds the TOTAL outbound fetch rate under that rotation.
//
// Admission is ATOMIC (binding CONDITION 1): both buckets are consulted under
// ONE mutex and a token is consumed from NEITHER unless BOTH admit. A refusal
// that still drained the admitting bucket would make the effective per-source
// rate strictly tighter than configured — a limiter that silently self-tightens
// under load is worse than one that is merely wrong, because it looks correct in
// isolation. The returned wait is the MAXIMUM of the two buckets' waits, never
// the first or the per-source one; understating it invites a client to retry
// straight into a second refusal.
type oauthCIMDLimiter struct {
	// now is the injected clock. Never time.Now directly, so a test can freeze
	// it and drive refill deterministically with zero sleeps (binding
	// constraint 3). New() hands it a closure over s.nowFunc so a test that
	// reassigns s.nowFunc AFTER New still steers this limiter.
	now func() time.Time

	sourceBurst  float64
	sourceRefill time.Duration // time to regain ONE per-source token
	globalBurst  float64
	globalRefill time.Duration // time to regain ONE global token
	maxSources   int

	mu      sync.Mutex
	global  tokenBucket
	entries map[string]*list.Element // source key -> *lruEntry
	order   *list.List               // MRU at front; capped at maxSources
}

// lruEntry is one tracked source's bucket, held by an order-list element. The
// key is an ATTACKER-INFLUENCED string (a rotating source address), so the map
// is bounded by an LRU exactly like oauthas.Fetcher's document cache — an
// unbounded map keyed on attacker input is the memory-exhaustion bug #2429/#2434
// closed for the CIMD cache, and it must not be reintroduced here.
type lruEntry struct {
	key    string
	bucket tokenBucket
}

// tokenBucket is a lazily-refilled token bucket. A zero-valued bucket (last
// unset) initialises to a FULL burst on its first refill, so a freshly-seen
// source starts with its whole allowance.
type tokenBucket struct {
	tokens float64
	last   time.Time
}

// refill advances the bucket to now, adding one token per refillPerToken elapsed
// and clamping at burst. It is idempotent at a fixed now (a second call adds
// nothing), which is what lets Allow refill both buckets before deciding whether
// to consume without the refill itself constituting a consume.
func (b *tokenBucket) refill(now time.Time, burst float64, refillPerToken time.Duration) {
	if b.last.IsZero() {
		b.last = now
		b.tokens = burst
		return
	}
	elapsed := now.Sub(b.last)
	if elapsed <= 0 {
		return
	}
	if refillPerToken > 0 {
		b.tokens += float64(elapsed) / float64(refillPerToken)
		if b.tokens > burst {
			b.tokens = burst
		}
	}
	b.last = now
}

// waitFor is the duration until the bucket holds at least one token, or 0 when
// it already does. Read as STATE by the refusal path (binding constraint 2), so
// the Retry-After the caller emits is derived, not guessed.
func (b *tokenBucket) waitFor(refillPerToken time.Duration) time.Duration {
	if b.tokens >= 1 {
		return 0
	}
	need := 1 - b.tokens
	return time.Duration(need * float64(refillPerToken))
}

// Default limiter policy. Per-source: 5 burst, one token per 10s. Global: 30
// burst, one token per 1s. maxSources is an internal constant, NOT a flag —
// like the CIMD cache bound, an operator-disable knob would reintroduce the
// unbounded case a config typo could reach.
const (
	defaultOAuthCIMDSourceBurst  = 5
	defaultOAuthCIMDSourceRefill = 10 * time.Second
	defaultOAuthCIMDGlobalBurst  = 30
	defaultOAuthCIMDGlobalRefill = 1 * time.Second
	defaultOAuthCIMDMaxSources   = 4096
)

// oauthCIMDUnknownSourceKey is the ONE shared bucket every unidentifiable source
// draws from (see oauthCIMDSourceKey).
const oauthCIMDUnknownSourceKey = "unknown"

// newOAuthCIMDLimiter constructs the limiter. Every zero/negative construction
// value resolves to the default — NEVER to unbounded and never to
// limit-everything — the exact convention oauthas.Fetcher.MaxCacheEntries
// documents.
func newOAuthCIMDLimiter(now func() time.Time, sourceBurst int, sourceRefill time.Duration, globalBurst int, globalRefill time.Duration, maxSources int) *oauthCIMDLimiter {
	if now == nil {
		now = time.Now
	}
	sb := float64(sourceBurst)
	if sourceBurst <= 0 {
		sb = defaultOAuthCIMDSourceBurst
	}
	sr := sourceRefill
	if sr <= 0 {
		sr = defaultOAuthCIMDSourceRefill
	}
	gb := float64(globalBurst)
	if globalBurst <= 0 {
		gb = defaultOAuthCIMDGlobalBurst
	}
	gr := globalRefill
	if gr <= 0 {
		gr = defaultOAuthCIMDGlobalRefill
	}
	ms := maxSources
	if ms <= 0 {
		ms = defaultOAuthCIMDMaxSources
	}
	return &oauthCIMDLimiter{
		now:          now,
		sourceBurst:  sb,
		sourceRefill: sr,
		globalBurst:  gb,
		globalRefill: gr,
		maxSources:   ms,
		entries:      map[string]*list.Element{},
		order:        list.New(),
	}
}

// Allow reports whether a CIMD-triggering request from key may proceed and, on
// refusal, the wait until it could. Both buckets are refilled and consulted
// under one lock; a token is consumed from BOTH only when BOTH admit, and from
// NEITHER otherwise (CONDITION 1a). The returned wait is the MAX of the two
// buckets' waits (CONDITION 1b).
func (l *oauthCIMDLimiter) Allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	src := l.sourceBucketLocked(key)
	src.refill(now, l.sourceBurst, l.sourceRefill)
	l.global.refill(now, l.globalBurst, l.globalRefill)

	srcWait := src.waitFor(l.sourceRefill)
	globalWait := l.global.waitFor(l.globalRefill)
	if srcWait == 0 && globalWait == 0 {
		src.tokens--
		l.global.tokens--
		return true, 0
	}

	wait := srcWait
	if globalWait > wait {
		wait = globalWait
	}
	return false, wait
}

// sourceBucketLocked returns key's bucket, creating it (and evicting the LRU
// tail past maxSources) on first sight. The map and list are mutated only
// together under mu — the single capped LRU the doc comment promises.
func (l *oauthCIMDLimiter) sourceBucketLocked(key string) *tokenBucket {
	if el, ok := l.entries[key]; ok {
		l.order.MoveToFront(el)
		return &lruEntryOf(el).bucket
	}
	ent := &lruEntry{key: key}
	el := l.order.PushFront(ent)
	l.entries[key] = el
	for l.order.Len() > l.maxSources {
		back := l.order.Back()
		if back == nil {
			break
		}
		l.order.Remove(back)
		delete(l.entries, lruEntryOf(back).key)
	}
	return &ent.bucket
}

// lruEntryOf recovers the *lruEntry a list element carries. The order list only
// ever holds *lruEntry values, so a failed assertion is a programmer error, not
// a runtime input condition — panic rather than silently mis-key.
func lruEntryOf(el *list.Element) *lruEntry {
	ent, ok := el.Value.(*lruEntry)
	if !ok {
		panic("oauthCIMDLimiter: order list held a non-*lruEntry value")
	}
	return ent
}

// trackedSources is the count of live per-source buckets — a test state read for
// the LRU-cap assertion.
func (l *oauthCIMDLimiter) trackedSources() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.order.Len()
}

// sourceTokens is the current token count for key (the full burst when key has
// no bucket yet). A test state read (binding constraint 2): the store-hit path
// must leave it UNCHANGED, and a combined refusal must not drain the admitting
// per-source bucket.
func (l *oauthCIMDLimiter) sourceTokens(key string) float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	if el, ok := l.entries[key]; ok {
		return lruEntryOf(el).bucket.tokens
	}
	return l.sourceBurst
}

// globalTokens is the current global token count — a test state read for the
// reverse combined-refusal case (source empty, global untouched).
func (l *oauthCIMDLimiter) globalTokens() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.global.tokens
}

// oauthCIMDSourceKey canonicalises r.RemoteAddr into a limiter key.
//
// r.RemoteAddr is set by net/http to the peer's network address and is NOT
// derived from any request header, so keying on it cannot be forged by a
// client-supplied X-Forwarded-For. There is DELIBERATELY no XFF read: nothing in
// this backend parses XFF and there is no trusted-proxy configuration, so
// honouring a client-settable header would hand an attacker unlimited free
// buckets. If this deployment later sits behind a reverse proxy, every request
// keys to the proxy address and the per-source bucket degrades into a second
// global bucket — degradation toward MORE limiting, and the global bucket still
// bounds the outbound rate.
//
// Unmap() is load-bearing, not cosmetic: without it 1.2.3.4 and ::ffff:1.2.3.4
// are the same host holding TWO buckets, doubling its per-source budget. Any
// failure to parse (empty, malformed, a non-IP RemoteAddr from a synthetic
// listener) returns the ONE shared sentinel key so every unidentifiable source
// shares a SINGLE bucket — degrading toward more limiting, never toward a
// per-request bucket that would be no limit at all.
func oauthCIMDSourceKey(r *http.Request) string {
	raw := r.RemoteAddr
	if host, _, err := net.SplitHostPort(raw); err == nil {
		raw = host
	}
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		return oauthCIMDUnknownSourceKey
	}
	return addr.Unmap().String()
}

// errCIMDRateLimited is the SENTINEL identity for a CIMD limiter refusal, carried
// in an *oauthas.Error's Cause so errors.Is works on the identity axis — the
// same sentinel-in-Cause pairing this package uses for ErrBlockedAddress /
// ErrPKCEMismatch.
var errCIMDRateLimited = errors.New("oauth CIMD outbound-fetch rate limit exceeded")

// cimdRateLimitedError is the TYPED carrier (binding CONDITION 2): it wraps the
// sentinel (so errors.Is(err, errCIMDRateLimited) matches) AND carries the wait
// as a structured field, so the renderer reads the Retry-After duration
// STRUCTURALLY — never by string-parsing and never by re-consulting the limiter,
// which would consume a second token.
type cimdRateLimitedError struct {
	retryAfter time.Duration
}

func (e *cimdRateLimitedError) Error() string {
	return fmt.Sprintf("%s: retry after %s", errCIMDRateLimited.Error(), e.retryAfter)
}

func (*cimdRateLimitedError) Unwrap() error { return errCIMDRateLimited }

// writeOAuthClientError is the ONE decision point that renders a resolver
// *oauthas.Error to a client. authorize passes defaultStatus 400 (a plain
// invalid_client is a bad request) and token passes 401 (invalid_client is
// unauthorized there); BOTH map the limiter refusal to 429 + Retry-After and a
// store outage to 503, so the two handlers cannot diverge on those two mappings.
// The client_id is never echoed.
func (s *Server) writeOAuthClientError(w http.ResponseWriter, r *http.Request, defaultStatus int, cerr *oauthas.Error) {
	status := defaultStatus
	var rl *cimdRateLimitedError
	switch {
	case errors.As(cerr.Cause, &rl):
		// RFC 9110 §10.2.3 delta-seconds: a non-negative integer, ceil of the
		// wait, floored at 1 so a sub-second wait never renders Retry-After: 0.
		status = http.StatusTooManyRequests
		secs := int64(math.Ceil(rl.retryAfter.Seconds()))
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
	case cerr.Code == oauthas.ErrCodeTemporarilyUnavailable:
		// A store-outage temporarily_unavailable (NOT the limiter) → 503.
		status = http.StatusServiceUnavailable
	}
	s.writeOAuthError(w, r, status, cerr.Code, cerr.Description)
}
