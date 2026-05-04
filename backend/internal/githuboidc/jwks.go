package githuboidc

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultJWKSCacheTTL is the fallback when the JWKS response
// carries no Cache-Control max-age. GitHub's response typically has
// `Cache-Control: max-age=600`, but we keep a defensive default in
// case the upstream silently changes.
const defaultJWKSCacheTTL = time.Hour

// jwksCache fetches a JWKS document from a URL and caches the
// parsed RSA public keys keyed by `kid`. Concurrent Get calls share
// a single in-flight refresh; per-request fetches against a
// thrashing JWKS endpoint would be a foot-gun.
type jwksCache struct {
	url   string
	http  *http.Client
	clock func() time.Time

	mu        sync.Mutex
	keys      map[string]*rsa.PublicKey
	expiresAt time.Time
	// inflight holds an in-progress refresh so concurrent Gets don't
	// each kick off their own fetch.
	inflight *refreshOp
}

type refreshOp struct {
	done chan struct{}
	err  error
}

func newJWKSCache(url string) *jwksCache {
	return &jwksCache{
		url:   url,
		http:  &http.Client{Timeout: 30 * time.Second},
		clock: func() time.Time { return time.Now() },
		keys:  map[string]*rsa.PublicKey{},
	}
}

// Get returns the RSA key for kid. If the cache is empty or
// expired, Get fetches the JWKS document; if kid still isn't found
// after a fresh fetch, returns ErrUnknownKID. A non-nil error from
// the fetch path bubbles up directly.
//
// Cache misses for a known-good kid are common during GitHub's key
// rollovers: callers may want to retry once after a small delay
// before giving up.
func (c *jwksCache) Get(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if kid == "" {
		return nil, fmt.Errorf("%w: token has no kid", ErrInvalidToken)
	}

	if key, ok := c.cached(kid); ok {
		return key, nil
	}

	if err := c.refresh(ctx); err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if key, ok := c.keys[kid]; ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: kid %q", ErrUnknownKID, kid)
}

// cached returns the key for kid if the cache is fresh; otherwise
// (false, nil). Holds c.mu briefly.
func (c *jwksCache) cached(kid string) (*rsa.PublicKey, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clock().After(c.expiresAt) {
		return nil, false
	}
	if key, ok := c.keys[kid]; ok {
		return key, true
	}
	return nil, false
}

// refresh fetches the JWKS from c.url and replaces the in-memory
// keys. Concurrent refresh callers share a single in-flight op via
// the inflight field; the second caller waits on done and returns
// the first caller's error.
func (c *jwksCache) refresh(ctx context.Context) error {
	c.mu.Lock()
	if c.inflight != nil {
		op := c.inflight
		c.mu.Unlock()
		select {
		case <-op.done:
			return op.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	op := &refreshOp{done: make(chan struct{})}
	c.inflight = op
	c.mu.Unlock()

	defer func() {
		close(op.done)
		c.mu.Lock()
		c.inflight = nil
		c.mu.Unlock()
	}()

	keys, ttl, err := c.fetchJWKS(ctx)
	if err != nil {
		op.err = err
		return err
	}

	c.mu.Lock()
	c.keys = keys
	c.expiresAt = c.clock().Add(ttl)
	c.mu.Unlock()
	return nil
}

// fetchJWKS performs the HTTP GET, parses the JWKS, and returns
// the decoded keys plus the cache TTL extracted from the response's
// Cache-Control max-age (defaulting to defaultJWKSCacheTTL).
func (c *jwksCache) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("oidc: build jwks request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("oidc: fetch jwks: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("oidc: jwks returned %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, fmt.Errorf("oidc: read jwks body: %w", err)
	}

	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, 0, fmt.Errorf("oidc: parse jwks: %w", err)
	}

	out := make(map[string]*rsa.PublicKey, len(doc.Keys))
	for _, k := range doc.Keys {
		key, err := k.toRSA()
		if err != nil {
			// Skip individual bad entries rather than failing the
			// whole refresh — GitHub's JWKS typically holds 2-3
			// keys (current + rolling); one malformed key
			// shouldn't black-hole verification of the others.
			continue
		}
		if k.KID != "" {
			out[k.KID] = key
		}
	}
	if len(out) == 0 {
		return nil, 0, errors.New("oidc: jwks contained no usable RSA keys")
	}

	return out, jwksTTL(resp.Header.Get("Cache-Control")), nil
}

// jwk is the single-key shape we decode from JWKS. Optional fields
// (use, alg) aren't used here — kid + n + e are all we need.
type jwk struct {
	KTY string `json:"kty"`
	KID string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
}

func (k jwk) toRSA() (*rsa.PublicKey, error) {
	if k.KTY != "RSA" {
		return nil, fmt.Errorf("kty %q not RSA", k.KTY)
	}
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	if len(nBytes) == 0 || len(eBytes) == 0 {
		return nil, errors.New("empty n or e")
	}
	n := new(big.Int).SetBytes(nBytes)
	e := new(big.Int).SetBytes(eBytes)
	if !e.IsInt64() {
		return nil, errors.New("e overflows int64")
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

// jwksTTL extracts max-age from a Cache-Control header. Returns
// defaultJWKSCacheTTL when absent or malformed.
func jwksTTL(cacheControl string) time.Duration {
	for _, part := range strings.Split(cacheControl, ",") {
		part = strings.TrimSpace(part)
		const prefix = "max-age="
		if !strings.HasPrefix(part, prefix) {
			continue
		}
		secs, err := strconv.Atoi(part[len(prefix):])
		if err != nil || secs <= 0 {
			break
		}
		return time.Duration(secs) * time.Second
	}
	return defaultJWKSCacheTTL
}
