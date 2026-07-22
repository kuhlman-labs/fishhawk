package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// conventionsFilePath is the repo-relative path of the per-repo
// work-management conventions file the loader fetches (#2022).
const conventionsFilePath = ".fishhawk/work-management.yaml"

// defaultConventionsTTL bounds how long a cached parse is served without a
// refetch. Conventions edits are rare (a committed YAML file), so a few
// minutes of staleness is acceptable in exchange for not putting a forge
// round-trip on every filing.
const defaultConventionsTTL = 5 * time.Minute

// ProviderResolver resolves the ADR-057/ADR-058 tenancy provider
// discriminator for a repo — the SOLE out-of-file hint the per-repo
// conventions loader consults before falling back to the break-glass
// override / workmgmt.Default(). *account.Resolver satisfies it: exactly one
// accounts row for the repo's owner resolves (provider, true); zero rows OR
// an ambiguous key registered under both providers report found=false (a
// clean fall-through, never an arbitrary first row); a query error is
// propagated so the caller fails closed rather than silently selecting a
// different provider on a transient DB fault.
type ProviderResolver interface {
	ResolveProvider(ctx context.Context, repo string) (provider string, found bool, err error)
}

// RepoConventionsLoaderConfig carries the seams RepoConventionsLoader
// resolves through. Nil optional fields degrade per-field: a nil Resolver or
// a nil fetcher for the resolved provider behaves like an unregistered forge
// (fall through to Override/Default); nil Parse/Now and a zero TTL take the
// production defaults (workmgmt.Parse / time.Now / defaultConventionsTTL).
type RepoConventionsLoaderConfig struct {
	// Resolver is the provider discriminator lookup (accounts.provider by
	// the filing repo's owner as account_key).
	Resolver ProviderResolver
	// GitHubFetcher / GitLabFetcher read one file from the resolved forge.
	// serve.go wires each from the forge registry when that forge is
	// registered; nil means the forge is not configured.
	GitHubFetcher forge.FileFetcher
	GitLabFetcher forge.FileFetcher
	// GitHubScope resolves the GitHub App installation scope for owner/name
	// (Server.resolveRepoScope via GitHubRepoScopeResolver). A zero scope
	// with nil error is the not-installed posture; an error is a transient
	// resolution failure and FAILS CLOSED.
	GitHubScope func(ctx context.Context, owner, name string) (forge.CredentialScope, error)
	// GitLabScope is the deployment-level gitlab credential scope (the E45.5
	// static-token path ignores the ref; non-zero simply means "gitlab
	// credentials are configured"). Zero means unconfigured.
	GitLabScope forge.CredentialScope
	// Override is the break-glass FISHHAWKD_WORKMGMT_CONVENTIONS fallback:
	// non-nil and returning ok=true serves those conventions whenever
	// resolution falls through. Nil (or ok=false) falls to
	// workmgmt.Default().
	Override func() (workmgmt.Conventions, bool)
	// Parse parses fetched file bytes; tests inject a counter around it so
	// cache behavior (parse reuse on unchanged SHA) is observable.
	Parse func(r io.Reader) (workmgmt.Conventions, error)
	// Now and TTL gate the cache: within TTL of the last fetch the cached
	// parse is served with no fetch at all.
	Now func() time.Time
	TTL time.Duration
}

// conventionsCacheEntry is one repo's cached parse keyed by the forge blob
// SHA it was parsed from.
type conventionsCacheEntry struct {
	sha       string
	conv      workmgmt.Conventions
	fetchedAt time.Time
}

// RepoConventionsLoader is the per-repo work-management conventions loader
// (E45.16 / #2022): it fetches .fishhawk/work-management.yaml from the
// filing repo's OWN forge, breaking the chicken-and-egg the deployment
// override sidestepped — the fetch-forge is resolved from OUTSIDE the
// conventions file, via the ADR-057/ADR-058 provider discriminator
// (accounts.provider keyed by the repo owner). Resolution order per filing:
// discriminator → break-glass override → workmgmt.Default(). Once a forge is
// resolved the loader resolves the CredentialScope ITSELF (github: the
// server's repo-installation resolution; gitlab: the deployment scope); no
// resolvable scope is treated exactly like an unregistered forge. Fetch and
// parse failures other than forge.ErrNotFound FAIL CLOSED — an
// auth/transport/server fault must not silently select a different provider.
// Parses are cached per (provider, repo), TTL-gated: within TTL the cached
// parse is served with NO fetch; after TTL a refetch reuses the cached parse
// when the blob SHA is unchanged. The cache key is forge-qualified so a repo
// reassigned to a different provider never serves the prior forge's parse.
type RepoConventionsLoader struct {
	cfg RepoConventionsLoaderConfig

	// mu guards cache and locks. It is held only for the short map
	// operations, NEVER across the forge fetch — a slow/hung round-trip for
	// one repo must not stall filings for every other repo.
	mu    sync.Mutex
	cache map[string]conventionsCacheEntry
	// locks holds one fetch-serialization mutex per forge-qualified cache
	// key. Held across the fetch, it confines the "concurrent same-repo
	// filings do one fetch, not a thundering herd" serialization to that one
	// repo while leaving other repos free to fetch concurrently. Like cache,
	// it never evicts — bounded in practice by distinct authenticated filing
	// targets.
	locks map[string]*sync.Mutex
}

// NewRepoConventionsLoader builds a loader over cfg, defaulting Parse, Now,
// and TTL. Install its Load via SetConventionsLoader.
func NewRepoConventionsLoader(cfg RepoConventionsLoaderConfig) *RepoConventionsLoader {
	if cfg.Parse == nil {
		cfg.Parse = workmgmt.Parse
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.TTL <= 0 {
		cfg.TTL = defaultConventionsTTL
	}
	return &RepoConventionsLoader{
		cfg:   cfg,
		cache: make(map[string]conventionsCacheEntry),
		locks: make(map[string]*sync.Mutex),
	}
}

// Load resolves the work-management conventions for repo ("owner/name"). It
// is the func installed as the process-wide conventionsLoader seam.
func (l *RepoConventionsLoader) Load(ctx context.Context, repo string) (workmgmt.Conventions, error) {
	fetcher, scope, provider, ref, ok, err := l.resolveFetch(ctx, repo)
	if err != nil {
		return workmgmt.Conventions{}, err
	}
	if !ok {
		return l.fallback(), nil
	}
	owner, name, _ := strings.Cut(repo, "/")
	return l.loadCached(ctx, provider, repo, fetcher, scope, forge.RepoRef{Owner: owner, Name: name}, ref)
}

// resolveFetch runs the out-of-file resolution chain: the provider
// discriminator, then that provider's fetcher + self-resolved credential
// scope. ok=false means "fall through to override/Default" (provider
// not-found/ambiguous, unregistered forge, or no resolvable credential
// scope). A resolver or scope-resolution ERROR fails closed instead.
func (l *RepoConventionsLoader) resolveFetch(ctx context.Context, repo string) (forge.FileFetcher, forge.CredentialScope, string, string, bool, error) {
	if l.cfg.Resolver == nil {
		return nil, forge.CredentialScope{}, "", "", false, nil
	}
	provider, found, err := l.cfg.Resolver.ResolveProvider(ctx, repo)
	if err != nil {
		return nil, forge.CredentialScope{}, "", "", false, fmt.Errorf("resolve conventions provider for %q: %w", repo, err)
	}
	if !found {
		return nil, forge.CredentialScope{}, "", "", false, nil
	}
	owner, name, cut := strings.Cut(repo, "/")
	if !cut || owner == "" || name == "" {
		return nil, forge.CredentialScope{}, "", "", false, nil
	}
	switch provider {
	case "github":
		if l.cfg.GitHubFetcher == nil || l.cfg.GitHubScope == nil {
			return nil, forge.CredentialScope{}, "", "", false, nil
		}
		scope, err := l.cfg.GitHubScope(ctx, owner, name)
		if err != nil {
			return nil, forge.CredentialScope{}, "", "", false, fmt.Errorf("resolve github credential scope for %q: %w", repo, err)
		}
		if scope.IsZero() {
			// App not installed on the repo — no credential to fetch
			// with, exactly like an unregistered forge.
			return nil, forge.CredentialScope{}, "", "", false, nil
		}
		return l.cfg.GitHubFetcher, scope, provider, "", true, nil
	case "gitlab":
		if l.cfg.GitLabFetcher == nil || l.cfg.GitLabScope.IsZero() {
			return nil, forge.CredentialScope{}, "", "", false, nil
		}
		// The Repository Files API requires an explicit ref; HEAD is the
		// repo's default branch.
		return l.cfg.GitLabFetcher, l.cfg.GitLabScope, provider, "HEAD", true, nil
	default:
		// A provider the loader has no fetch path for behaves like an
		// unregistered forge.
		return nil, forge.CredentialScope{}, "", "", false, nil
	}
}

// loadCached serves repo's conventions from the TTL-gated cache, fetching
// and (when the blob SHA changed) re-parsing on miss or expiry. The cache is
// keyed by (provider, repo) so a repo reassigned to a different provider
// never serves the prior forge's parse. A per-key mutex serializes concurrent
// loads for the SAME key (one fetch, no thundering herd) without blocking
// loads for any OTHER repo across the forge round-trip.
func (l *RepoConventionsLoader) loadCached(ctx context.Context, provider, repo string, fetcher forge.FileFetcher, scope forge.CredentialScope, ref forge.RepoRef, fetchRef string) (workmgmt.Conventions, error) {
	key := provider + "\x00" + repo

	keyLock := l.keyLock(key)
	keyLock.Lock()
	defer keyLock.Unlock()

	now := l.cfg.Now()
	entry, cached := l.readCache(key)
	if cached && now.Sub(entry.fetchedAt) < l.cfg.TTL {
		return entry.conv, nil
	}

	fc, err := fetcher.FetchFile(ctx, scope, ref, conventionsFilePath, fetchRef)
	if err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			// The repo simply has no committed conventions file.
			return l.fallback(), nil
		}
		// FAIL CLOSED: an auth/transport/server failure must not silently
		// select a different provider via the fallback chain.
		return workmgmt.Conventions{}, fmt.Errorf("fetch %s from %s: %w", conventionsFilePath, repo, err)
	}
	if cached && entry.sha == fc.SHA {
		entry.fetchedAt = now
		l.writeCache(key, entry)
		return entry.conv, nil
	}
	conv, err := l.cfg.Parse(bytes.NewReader(fc.Content))
	if err != nil {
		// FAIL CLOSED: a committed-but-invalid file is an operator error to
		// surface, not a reason to serve some other repo's conventions.
		return workmgmt.Conventions{}, fmt.Errorf("parse %s from %s: %w", conventionsFilePath, repo, err)
	}
	l.writeCache(key, conventionsCacheEntry{sha: fc.SHA, conv: conv, fetchedAt: now})
	return conv, nil
}

// keyLock returns the per-key fetch-serialization mutex, creating it on first
// use. The short l.mu critical section here is the only place the map lock is
// taken during a load — never across the fetch itself.
func (l *RepoConventionsLoader) keyLock(key string) *sync.Mutex {
	l.mu.Lock()
	defer l.mu.Unlock()
	lk := l.locks[key]
	if lk == nil {
		lk = &sync.Mutex{}
		l.locks[key] = lk
	}
	return lk
}

// readCache / writeCache access the cache map under the short l.mu lock.
func (l *RepoConventionsLoader) readCache(key string) (conventionsCacheEntry, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.cache[key]
	return entry, ok
}

func (l *RepoConventionsLoader) writeCache(key string, entry conventionsCacheEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cache[key] = entry
}

// fallback is the tail of the resolution chain: the break-glass
// FISHHAWKD_WORKMGMT_CONVENTIONS override when installed, else the shipped
// default.
func (l *RepoConventionsLoader) fallback() workmgmt.Conventions {
	if l.cfg.Override != nil {
		if conv, ok := l.cfg.Override(); ok {
			return conv
		}
	}
	return workmgmt.Default()
}

// GitHubRepoScopeResolver exposes the server's GitHub repo→installation
// credential-scope resolution (resolveRepoScope) as the plain func the
// conventions-loader wiring in serve.go injects, or nil when GitHub App auth
// is unconfigured — the loader then treats a github-resolved repo like an
// unregistered forge.
func (s *Server) GitHubRepoScopeResolver() func(ctx context.Context, owner, name string) (forge.CredentialScope, error) {
	if s.cfg.GitHub == nil {
		return nil
	}
	return s.resolveRepoScope
}
