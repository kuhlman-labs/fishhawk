package githubapp

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// TokenProvider is the surface every backend caller depends on. It
// returns a string ready to set as `Authorization: Bearer <token>`
// for the given installation. Implementations decide on caching,
// refresh policy, and telemetry; the contract is just "give me a
// token I can use right now."
type TokenProvider interface {
	Token(ctx context.Context, installationID int64) (string, error)
}

// Stats tracks observability counters across the lifetime of a
// CachedProvider. All fields are atomic counters so reads from a
// metrics endpoint don't need locks.
type Stats struct {
	hits        atomic.Uint64
	misses      atomic.Uint64
	refreshes   atomic.Uint64
	refreshErrs atomic.Uint64
}

// Snapshot returns a point-in-time view of the counters. Useful
// for log lines or a future /metrics endpoint.
func (s *Stats) Snapshot() (hits, misses, refreshes, refreshErrs uint64) {
	return s.hits.Load(), s.misses.Load(),
		s.refreshes.Load(), s.refreshErrs.Load()
}

// CachedProvider wraps a Client with a TTL-aware in-memory cache.
// Tokens are refreshed when the remaining lifetime drops below
// RefreshLeadTime; concurrent callers for the same installation
// see exactly one in-flight refresh thanks to per-installation
// mutexes.
//
// Cache keys are installation IDs; the same Provider serves
// multiple installations simultaneously.
type CachedProvider struct {
	Client *Client

	// RefreshLeadTime is how early to refresh before expiry.
	// Default 5 minutes; production runs comfortably above that
	// since GitHub's tokens are 1h.
	RefreshLeadTime time.Duration

	// Now returns the current time. Defaults to time.Now;
	// overridable for deterministic tests.
	Now func() time.Time

	Stats Stats

	mu      sync.Mutex
	entries map[int64]*entry
}

// entry holds a cached token and a per-key mutex that callers can
// hold while refreshing. Holding entryMu instead of the global mu
// during the network round-trip prevents one slow installation
// from blocking lookups for others.
type entry struct {
	mu        sync.Mutex
	token     string
	expiresAt time.Time
}

// NewCachedProvider returns a CachedProvider with sensible defaults.
func NewCachedProvider(c *Client) *CachedProvider {
	return &CachedProvider{
		Client:          c,
		RefreshLeadTime: 5 * time.Minute,
		Now:             func() time.Time { return time.Now().UTC() },
		entries:         make(map[int64]*entry),
	}
}

// Token returns a usable installation token, refreshing in the
// background when the cached one is within RefreshLeadTime of
// expiring. Concurrent callers for the same installation are
// serialized at the entry mutex; different installations don't
// block each other.
func (p *CachedProvider) Token(ctx context.Context, installationID int64) (string, error) {
	e := p.getOrCreateEntry(installationID)
	e.mu.Lock()
	defer e.mu.Unlock()

	now := p.Now()
	if e.token != "" && now.Add(p.RefreshLeadTime).Before(e.expiresAt) {
		p.Stats.hits.Add(1)
		return e.token, nil
	}

	p.Stats.misses.Add(1)
	if e.token != "" {
		p.Stats.refreshes.Add(1)
	}

	tok, err := p.Client.IssueInstallationToken(ctx, installationID)
	if err != nil {
		p.Stats.refreshErrs.Add(1)
		return "", err
	}
	e.token = tok.Token
	e.expiresAt = tok.ExpiresAt
	return e.token, nil
}

// getOrCreateEntry returns the cache entry for the given
// installation, creating it under the global mu if absent. Callers
// then lock entry.mu for refresh-vs-read coordination.
func (p *CachedProvider) getOrCreateEntry(installationID int64) *entry {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[installationID]; ok {
		return e
	}
	e := &entry{}
	p.entries[installationID] = e
	return e
}

// Forget evicts the cached entry for an installation, forcing the
// next Token call to issue fresh. Used after we observe an
// authorization failure on the token (the cache may still see it
// as valid; explicit eviction skips the wait).
func (p *CachedProvider) Forget(installationID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.entries, installationID)
}
