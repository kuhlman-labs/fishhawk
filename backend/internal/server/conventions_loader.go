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

// Conventions resolution states (ADR-110 / ADR-114, #4642). A
// ConventionsResolution reports which one produced its Conventions, so a
// caller can tell "this repo declared its conventions" from "this repo
// declared nothing" (the fetch succeeded and found no file) and from "the
// server could not look" (a pre-fetch fall-through, named by its reason).
const (
	// ConventionsStateDeclared: the repo's committed
	// .fishhawk/work-management.yaml was fetched, parsed and
	// destination-authorized.
	ConventionsStateDeclared = "declared"
	// ConventionsStateNotDeclared: the fetch SUCCEEDED and the forge answered
	// forge.ErrNotFound — the repo commits no conventions file, so the
	// fallback conventions apply.
	ConventionsStateNotDeclared = "not_declared"
	// ConventionsStateDegraded: resolution fell through BEFORE any fetch, so
	// the server could not tell whether the repo declares anything;
	// DegradedReason names why and the fallback conventions apply.
	ConventionsStateDegraded = "degraded"
	// ConventionsStateNoRepo: there was no repo to resolve for (an empty
	// repo); the fallback conventions apply.
	ConventionsStateNoRepo = "no_repo"
)

// Degraded reasons: one per pre-fetch fall-through of the resolution chain,
// plus the seam stub's reason. Each names what an operator has to configure
// for the repo's own conventions to be read.
const (
	// ConventionsDegradedProviderResolverUnconfigured: the loader has no
	// provider discriminator (nil Resolver).
	ConventionsDegradedProviderResolverUnconfigured = "provider_resolver_unconfigured"
	// ConventionsDegradedNoProviderAccount: the discriminator found no
	// accounts row for the repo owner, or an ambiguous one (the same key
	// under two providers).
	ConventionsDegradedNoProviderAccount = "no_provider_account"
	// ConventionsDegradedRepoMalformed: the repo is not "owner/name".
	ConventionsDegradedRepoMalformed = "repo_malformed"
	// ConventionsDegradedForgeUnregistered: the resolved provider has no
	// fetch path — its forge is not registered (nil fetcher, or a nil github
	// scope resolver), or the provider is one the loader does not know.
	ConventionsDegradedForgeUnregistered = "forge_unregistered"
	// ConventionsDegradedAppNotInstalled: the GitHub App is not installed on
	// the repo (a zero installation scope).
	ConventionsDegradedAppNotInstalled = "app_not_installed"
	// ConventionsDegradedForgeCredentialsUnconfigured: the deployment has no
	// gitlab credentials (a zero deployment scope).
	ConventionsDegradedForgeCredentialsUnconfigured = "forge_credentials_unconfigured"
	// ConventionsDegradedLoaderUnconfigured: no per-repo loader is installed
	// behind the conventionsResolver seam, so the server cannot tell whether
	// the repo declares anything (the seam stub's reason).
	ConventionsDegradedLoaderUnconfigured = "conventions_loader_unconfigured"
)

// Fallback kinds: which conventions a not_declared / degraded / no_repo
// resolution carries.
const (
	// ConventionsFallbackDeploymentOverride: the break-glass
	// FISHHAWKD_WORKMGMT_CONVENTIONS override.
	ConventionsFallbackDeploymentOverride = "deployment_override"
	// ConventionsFallbackShippedDefault: workmgmt.Default().
	ConventionsFallbackShippedDefault = "shipped_default"
)

// ConventionsResolution is one repo's resolved work-management conventions
// plus how they were resolved (ADR-114 / #4642). State is one of the
// ConventionsState* values; DegradedReason is set only for
// ConventionsStateDegraded; Fallback is set for every state except
// ConventionsStateDeclared (the seam stub, which cannot know where its
// delegate's conventions came from, leaves it empty). A resolution returned
// alongside a non-nil error carries only Repo.
type ConventionsResolution struct {
	Conventions    workmgmt.Conventions
	Repo           string
	State          string
	DegradedReason string
	Fallback       string
}

// conventionsResolver is the process-wide ConventionsResolution seam, the
// signal-carrying sibling of conventionsLoader (workitems.go). serve.go
// installs RepoConventionsLoader.Resolve on the SAME loader instance whose
// Load it installs as conventionsLoader, so the two share one cache. The
// default stub delegates to conventionsLoader and reports degraded with
// ConventionsDegradedLoaderUnconfigured: with no per-repo loader installed
// the server cannot tell whether the repo declared anything, and a missed
// install then surfaces by name rather than as a silent default. Declared as
// a package var so tests can inject a resolution without a forge round-trip.
var conventionsResolver = func(ctx context.Context, repo string) (ConventionsResolution, error) {
	conv, err := conventionsLoader(ctx, repo)
	if err != nil {
		return ConventionsResolution{Repo: repo}, err
	}
	return ConventionsResolution{
		Conventions:    conv,
		Repo:           repo,
		State:          ConventionsStateDegraded,
		DegradedReason: ConventionsDegradedLoaderUnconfigured,
	}, nil
}

// SetConventionsResolver installs the process-wide conventions resolver,
// replacing the stub, mirroring SetConventionsLoader. It is not
// concurrency-safe with in-flight requests and is intended to be called once
// at startup.
func SetConventionsResolver(resolver func(ctx context.Context, repo string) (ConventionsResolution, error)) {
	conventionsResolver = resolver
}

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
	// AllowedDestinations is the deployment-controlled (never repo-controlled)
	// set of exceptions to the destination binding (E44.14 / #2090), parsed
	// from FISHHAWKD_WORKMGMT_ALLOWED_DESTINATIONS. Nil/empty means strict
	// binding with no exceptions.
	AllowedDestinations DestinationAllowList
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
// resolvable scope falls through like an unregistered forge, and Resolve
// names every such pre-fetch fall-through by its own degraded reason
// (ADR-114 / #4642). Fetch and
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
// and TTL. Install its Load via SetConventionsLoader and its Resolve via
// SetConventionsResolver, on the same instance so they share one cache.
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
// is the func installed as the process-wide conventionsLoader seam, and a thin
// wrapper over Resolve: every existing caller sees exactly the conventions
// and error it saw before Resolve existed.
func (l *RepoConventionsLoader) Load(ctx context.Context, repo string) (workmgmt.Conventions, error) {
	res, err := l.Resolve(ctx, repo)
	return res.Conventions, err
}

// Resolve resolves the work-management conventions for repo ("owner/name")
// and reports how (ADR-114 / #4642): declared (fetched and parsed),
// not_declared (the fetch succeeded and answered forge.ErrNotFound), degraded
// (a pre-fetch fall-through, with its named reason) or no_repo (an empty repo
// that fell through). not_declared / degraded / no_repo carry the fallback
// conventions and name their Fallback kind. Resolver, scope-resolution, fetch,
// parse and destination-authorization failures return an error (fail
// closed) and a resolution carrying only Repo. It is the func installed as
// the process-wide conventionsResolver seam.
func (l *RepoConventionsLoader) Resolve(ctx context.Context, repo string) (ConventionsResolution, error) {
	fetcher, scope, provider, ref, reason, err := l.resolveFetch(ctx, repo)
	if err != nil {
		return ConventionsResolution{Repo: repo}, err
	}
	if reason != "" {
		conv, kind := l.fallback()
		res := ConventionsResolution{Conventions: conv, Repo: repo, State: ConventionsStateDegraded, DegradedReason: reason, Fallback: kind}
		if repo == "" {
			// Nothing to resolve for: not a degraded lookup of some repo.
			res.State, res.DegradedReason = ConventionsStateNoRepo, ""
		}
		return res, nil
	}
	owner, name, _ := strings.Cut(repo, "/")
	conv, declared, err := l.loadCached(ctx, provider, repo, fetcher, scope, forge.RepoRef{Owner: owner, Name: name}, ref)
	if err != nil {
		return ConventionsResolution{Repo: repo}, err
	}
	if !declared {
		conv, kind := l.fallback()
		return ConventionsResolution{Conventions: conv, Repo: repo, State: ConventionsStateNotDeclared, Fallback: kind}, nil
	}
	return ConventionsResolution{Conventions: conv, Repo: repo, State: ConventionsStateDeclared}, nil
}

// resolveFetch runs the out-of-file resolution chain: the provider
// discriminator, then that provider's fetcher + self-resolved credential
// scope. A non-empty reason means "fall through to override/Default" and
// names the fall-through (one ConventionsDegraded* value per branch). A
// resolver or scope-resolution ERROR fails closed instead.
func (l *RepoConventionsLoader) resolveFetch(ctx context.Context, repo string) (forge.FileFetcher, forge.CredentialScope, string, string, string, error) {
	if l.cfg.Resolver == nil {
		return nil, forge.CredentialScope{}, "", "", ConventionsDegradedProviderResolverUnconfigured, nil
	}
	provider, found, err := l.cfg.Resolver.ResolveProvider(ctx, repo)
	if err != nil {
		return nil, forge.CredentialScope{}, "", "", "", fmt.Errorf("resolve conventions provider for %q: %w", repo, err)
	}
	if !found {
		return nil, forge.CredentialScope{}, "", "", ConventionsDegradedNoProviderAccount, nil
	}
	owner, name, cut := strings.Cut(repo, "/")
	if !cut || owner == "" || name == "" {
		return nil, forge.CredentialScope{}, "", "", ConventionsDegradedRepoMalformed, nil
	}
	switch provider {
	case "github":
		if l.cfg.GitHubFetcher == nil || l.cfg.GitHubScope == nil {
			return nil, forge.CredentialScope{}, "", "", ConventionsDegradedForgeUnregistered, nil
		}
		scope, err := l.cfg.GitHubScope(ctx, owner, name)
		if err != nil {
			return nil, forge.CredentialScope{}, "", "", "", fmt.Errorf("resolve github credential scope for %q: %w", repo, err)
		}
		if scope.IsZero() {
			// App not installed on the repo — no credential to fetch
			// with.
			return nil, forge.CredentialScope{}, "", "", ConventionsDegradedAppNotInstalled, nil
		}
		return l.cfg.GitHubFetcher, scope, provider, "", "", nil
	case "gitlab":
		if l.cfg.GitLabFetcher == nil {
			return nil, forge.CredentialScope{}, "", "", ConventionsDegradedForgeUnregistered, nil
		}
		if l.cfg.GitLabScope.IsZero() {
			return nil, forge.CredentialScope{}, "", "", ConventionsDegradedForgeCredentialsUnconfigured, nil
		}
		// The Repository Files API requires an explicit ref; HEAD is the
		// repo's default branch.
		return l.cfg.GitLabFetcher, l.cfg.GitLabScope, provider, "HEAD", "", nil
	default:
		// A provider the loader has no fetch path for behaves like an
		// unregistered forge.
		return nil, forge.CredentialScope{}, "", "", ConventionsDegradedForgeUnregistered, nil
	}
}

// loadCached serves repo's conventions from the TTL-gated cache, fetching
// and (when the blob SHA changed) re-parsing on miss or expiry. The cache is
// keyed by (provider, repo) so a repo reassigned to a different provider
// never serves the prior forge's parse. A per-key mutex serializes concurrent
// loads for the SAME key (one fetch, no thundering herd) without blocking
// loads for any OTHER repo across the forge round-trip.
//
// A repo-FETCHED parse is destination-authorized (E44.14 / #2090) BEFORE it
// is cached or returned, and a refusal is neither cached nor fallen through
// from. That ordering is the invariant the cached-serve and unchanged-SHA
// branches rest on: nothing enters the cache without having passed
// authorization, so those branches need no re-check — and an edit that writes
// the cache any earlier silently reopens the redirect hole.
//
// declared=false (with a nil error) means the fetch answered
// forge.ErrNotFound: the repo commits no conventions file, and the caller
// serves the fallback. Only a declared parse is ever cached, so every cache
// hit is declared.
func (l *RepoConventionsLoader) loadCached(ctx context.Context, provider, repo string, fetcher forge.FileFetcher, scope forge.CredentialScope, ref forge.RepoRef, fetchRef string) (conv workmgmt.Conventions, declared bool, err error) {
	key := provider + "\x00" + repo

	keyLock := l.keyLock(key)
	keyLock.Lock()
	defer keyLock.Unlock()

	now := l.cfg.Now()
	entry, cached := l.readCache(key)
	if cached && now.Sub(entry.fetchedAt) < l.cfg.TTL {
		return entry.conv, true, nil
	}

	fc, err := fetcher.FetchFile(ctx, scope, ref, conventionsFilePath, fetchRef)
	if err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			// The repo simply has no committed conventions file.
			return workmgmt.Conventions{}, false, nil
		}
		// FAIL CLOSED: an auth/transport/server failure must not silently
		// select a different provider via the fallback chain.
		return workmgmt.Conventions{}, false, fmt.Errorf("fetch %s from %s: %w", conventionsFilePath, repo, err)
	}
	if cached && entry.sha == fc.SHA {
		entry.fetchedAt = now
		l.writeCache(key, entry)
		return entry.conv, true, nil
	}
	conv, err = l.cfg.Parse(bytes.NewReader(fc.Content))
	if err != nil {
		// FAIL CLOSED: a committed-but-invalid file is an operator error to
		// surface, not a reason to serve some other repo's conventions.
		return workmgmt.Conventions{}, false, fmt.Errorf("parse %s from %s: %w", conventionsFilePath, repo, err)
	}
	if err := authorizeConventionsDestination(provider, repo, conv, l.cfg.AllowedDestinations); err != nil {
		// FAIL CLOSED, and deliberately WITHOUT caching and WITHOUT falling
		// through: caching a refused parse would hide the redirect attempt
		// behind the TTL, and falling back to the override/Default() would
		// let repo-committed content SELECT the deployment default.
		return workmgmt.Conventions{}, false, err
	}
	l.writeCache(key, conventionsCacheEntry{sha: fc.SHA, conv: conv, fetchedAt: now})
	return conv, true, nil
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
// default. The second result names which (a ConventionsFallback* value).
func (l *RepoConventionsLoader) fallback() (workmgmt.Conventions, string) {
	if l.cfg.Override != nil {
		if conv, ok := l.cfg.Override(); ok {
			return conv, ConventionsFallbackDeploymentOverride
		}
	}
	return workmgmt.Default(), ConventionsFallbackShippedDefault
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
