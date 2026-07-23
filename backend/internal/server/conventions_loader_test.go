package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Minimal valid conventions documents, one per provider, so the mixed-forge
// test can prove each repo's resolved conventions carry its OWN committed
// provider.
const testGitHubConventionsYAML = `
spec_version: work-management-v0
provider: github_projects
project:
  owner: acme
  number: 3
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`

// testGitHubConventionsYAMLAlt is the same owner (so it stays
// destination-authorized for an acme-owned repo) with a DIFFERENT project
// number, so a re-parse is observable without crossing the tenancy boundary.
const testGitHubConventionsYAMLAlt = `
spec_version: work-management-v0
provider: github_projects
project:
  owner: acme
  number: 9
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`

// testGitLabConventionsYAMLAcme is the gitlab file for an acme-owned repo:
// its project namespace root matches the repo owner, so it is
// destination-authorized (E44.14 / #2090).
const testGitLabConventionsYAMLAcme = `
spec_version: work-management-v0
provider: gitlab
gitlab:
  project: acme/app
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`

// testGitHubRedirectConventionsYAML is the ATTACK: a file committed to a
// victim-owned repo naming another account's project board.
const testGitHubRedirectConventionsYAML = `
spec_version: work-management-v0
provider: github_projects
project:
  owner: attacker
  number: 1
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`

const testGitLabConventionsYAML = `
spec_version: work-management-v0
provider: gitlab
gitlab:
  project: group/app
required_fields: [Summary, Done-means, complexity]
types: {feature: {body_skeleton: [Summary]}}
`

// fakeProviderResolver is a fixed-answer ProviderResolver.
type fakeProviderResolver struct {
	provider string
	found    bool
	err      error
}

func (f *fakeProviderResolver) ResolveProvider(context.Context, string) (string, bool, error) {
	return f.provider, f.found, f.err
}

// mapProviderResolver resolves per repo — the mixed-forge test's
// discriminator, mirroring accounts rows for two owners under two providers.
type mapProviderResolver map[string]string

func (m mapProviderResolver) ResolveProvider(_ context.Context, repo string) (string, bool, error) {
	p, ok := m[repo]
	return p, ok, nil
}

// fakeFileFetcher records every FetchFile call so tests can assert the
// zero-fetch within-TTL contract and the on-wire path/ref/scope.
type fakeFileFetcher struct {
	calls     int
	lastScope forge.CredentialScope
	lastRepo  forge.RepoRef
	lastPath  string
	lastRef   string
	fc        *forge.FileContent
	err       error
}

func (f *fakeFileFetcher) FetchFile(_ context.Context, scope forge.CredentialScope, repo forge.RepoRef, path, ref string) (*forge.FileContent, error) {
	f.calls++
	f.lastScope, f.lastRepo, f.lastPath, f.lastRef = scope, repo, path, ref
	if f.err != nil {
		return nil, f.err
	}
	return f.fc, nil
}

// countingParse wraps workmgmt.Parse with a counter so cache behavior — a
// reused parse on unchanged SHA — is observable.
func countingParse(n *int) func(io.Reader) (workmgmt.Conventions, error) {
	return func(r io.Reader) (workmgmt.Conventions, error) {
		*n++
		return workmgmt.Parse(r)
	}
}

// breakGlass is a present override serving conventions with a marker
// provider, distinguishable from both Default() and any committed file.
func breakGlass() (workmgmt.Conventions, bool) {
	return workmgmt.Conventions{Provider: "break-glass"}, true
}

func githubScopeFixed(ref string) func(context.Context, string, string) (forge.CredentialScope, error) {
	return func(context.Context, string, string) (forge.CredentialScope, error) {
		return forge.FromRef(ref), nil
	}
}

// TestConventionsLoader_ProviderNotFound: a repo with no accounts row falls
// through to the break-glass override with ZERO fetches on either forge.
func TestConventionsLoader_ProviderNotFound(t *testing.T) {
	gh, gl := &fakeFileFetcher{}, &fakeFileFetcher{}
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{found: false},
		GitHubFetcher: gh,
		GitLabFetcher: gl,
		GitHubScope:   githubScopeFixed("42"),
		GitLabScope:   forge.FromRef("gitlab:deployment"),
		Override:      breakGlass,
	})
	conv, err := l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load err = %v, want nil", err)
	}
	if conv.Provider != "break-glass" {
		t.Errorf("Provider = %q, want the break-glass override", conv.Provider)
	}
	if gh.calls != 0 || gl.calls != 0 {
		t.Errorf("fetch calls = github %d / gitlab %d, want 0/0 on provider-not-found", gh.calls, gl.calls)
	}
}

// TestConventionsLoader_ProviderAmbiguous drives the loader through the REAL
// account.Resolver over rows seeding the SAME account_key under BOTH github
// AND gitlab (legal under accounts.UNIQUE(provider, account_key)): the
// ambiguity resolves found=false and falls cleanly through to the override —
// never an arbitrary first row.
func TestConventionsLoader_ProviderAmbiguous(t *testing.T) {
	gh, gl := &fakeFileFetcher{}, &fakeFileFetcher{}
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver: account.NewResolver(staticKeyLister{
			{Provider: "github", AccountKey: "acme"},
			{Provider: "gitlab", AccountKey: "acme"},
		}),
		GitHubFetcher: gh,
		GitLabFetcher: gl,
		GitHubScope:   githubScopeFixed("42"),
		GitLabScope:   forge.FromRef("gitlab:deployment"),
		Override:      breakGlass,
	})
	conv, err := l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load err = %v, want nil", err)
	}
	if conv.Provider != "break-glass" {
		t.Errorf("Provider = %q, want the break-glass override on an ambiguous key", conv.Provider)
	}
	if gh.calls != 0 || gl.calls != 0 {
		t.Errorf("fetch calls = github %d / gitlab %d, want 0/0 — an ambiguous key must never pick a forge", gh.calls, gl.calls)
	}
}

// staticKeyLister satisfies account.KeyLister with fixed rows.
type staticKeyLister []accountdb.Account

func (s staticKeyLister) ListAccountsByAccountKey(context.Context, string) ([]accountdb.Account, error) {
	return s, nil
}

// TestConventionsLoader_ResolverError_FailsClosed: a discriminator query
// error (transient DB fault) propagates — the loader must not silently fall
// through and select a different provider's conventions.
func TestConventionsLoader_ResolverError_FailsClosed(t *testing.T) {
	gh := &fakeFileFetcher{}
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{err: errors.New("db down")},
		GitHubFetcher: gh,
		GitHubScope:   githubScopeFixed("42"),
		Override:      breakGlass,
	})
	_, err := l.Load(context.Background(), "acme/widgets")
	if err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("Load err = %v, want the propagated resolver error", err)
	}
	if gh.calls != 0 {
		t.Errorf("github fetch calls = %d, want 0 on a resolver error", gh.calls)
	}
}

// TestConventionsLoader_NoCredentialScope covers BOTH no-scope branches: a
// github repo whose App-installation resolution returns the zero scope (not
// installed), and a gitlab-resolved repo on a deployment with no configured
// gitlab scope. Each is treated exactly like an unregistered forge — fall
// through to the override, zero fetches.
func TestConventionsLoader_NoCredentialScope(t *testing.T) {
	t.Run("github zero scope", func(t *testing.T) {
		gh := &fakeFileFetcher{}
		l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
			Resolver:      &fakeProviderResolver{provider: "github", found: true},
			GitHubFetcher: gh,
			GitHubScope: func(context.Context, string, string) (forge.CredentialScope, error) {
				return forge.CredentialScope{}, nil
			},
			Override: breakGlass,
		})
		conv, err := l.Load(context.Background(), "acme/widgets")
		if err != nil {
			t.Fatalf("Load err = %v, want nil", err)
		}
		if conv.Provider != "break-glass" {
			t.Errorf("Provider = %q, want the break-glass override on a zero github scope", conv.Provider)
		}
		if gh.calls != 0 {
			t.Errorf("github fetch calls = %d, want 0 with no credential scope", gh.calls)
		}
	})

	t.Run("gitlab unconfigured", func(t *testing.T) {
		gl := &fakeFileFetcher{}
		l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
			Resolver:      &fakeProviderResolver{provider: "gitlab", found: true},
			GitLabFetcher: gl,
			// GitLabScope left zero: the deployment has no gitlab credentials.
			Override: breakGlass,
		})
		conv, err := l.Load(context.Background(), "group/lib")
		if err != nil {
			t.Fatalf("Load err = %v, want nil", err)
		}
		if conv.Provider != "break-glass" {
			t.Errorf("Provider = %q, want the break-glass override on an unconfigured gitlab scope", conv.Provider)
		}
		if gl.calls != 0 {
			t.Errorf("gitlab fetch calls = %d, want 0 with no credential scope", gl.calls)
		}
	})
}

// TestConventionsLoader_GitHubScopeError_FailsClosed: a TRANSIENT
// scope-resolution failure (non-nil error, unlike the nil-error zero scope of
// the not-installed posture) propagates rather than silently selecting the
// fallback chain.
func TestConventionsLoader_GitHubScopeError_FailsClosed(t *testing.T) {
	gh := &fakeFileFetcher{}
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{provider: "github", found: true},
		GitHubFetcher: gh,
		GitHubScope: func(context.Context, string, string) (forge.CredentialScope, error) {
			return forge.CredentialScope{}, errors.New("installation lookup 502")
		},
		Override: breakGlass,
	})
	_, err := l.Load(context.Background(), "acme/widgets")
	if err == nil || !strings.Contains(err.Error(), "installation lookup 502") {
		t.Fatalf("Load err = %v, want the propagated scope-resolution error", err)
	}
	if gh.calls != 0 {
		t.Errorf("github fetch calls = %d, want 0 on a scope-resolution error", gh.calls)
	}
}

// TestConventionsLoader_UnregisteredForge: a resolved provider whose fetcher
// is nil (forge absent from the registry) and a provider the loader has no
// fetch path for both fall through to the override.
func TestConventionsLoader_UnregisteredForge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
	}{
		{"github forge not registered", "github"},
		{"gitlab forge not registered", "gitlab"},
		{"unknown provider", "bitbucket"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
				Resolver:    &fakeProviderResolver{provider: tc.provider, found: true},
				GitHubScope: githubScopeFixed("42"),
				GitLabScope: forge.FromRef("gitlab:deployment"),
				Override:    breakGlass,
			})
			conv, err := l.Load(context.Background(), "acme/widgets")
			if err != nil {
				t.Fatalf("Load err = %v, want nil", err)
			}
			if conv.Provider != "break-glass" {
				t.Errorf("Provider = %q, want the break-glass override for an unregistered forge", conv.Provider)
			}
		})
	}
}

// TestConventionsLoader_FetchNotFound: forge.ErrNotFound (the repo has no
// committed conventions file) falls through to the override — the one fetch
// error that is a clean fall-through, not fail-closed.
func TestConventionsLoader_FetchNotFound(t *testing.T) {
	gh := &fakeFileFetcher{err: forge.ErrNotFound}
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{provider: "github", found: true},
		GitHubFetcher: gh,
		GitHubScope:   githubScopeFixed("42"),
		Override:      breakGlass,
	})
	conv, err := l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load err = %v, want nil", err)
	}
	if conv.Provider != "break-glass" {
		t.Errorf("Provider = %q, want the break-glass override when the file is absent", conv.Provider)
	}
	if gh.calls != 1 {
		t.Errorf("github fetch calls = %d, want 1", gh.calls)
	}
}

// TestConventionsLoader_FetchOtherError_FailsClosed: ANY FileFetcher error
// other than ErrNotFound (auth, transport, server fault) propagates — zero
// silent provider switch through the fallback chain.
func TestConventionsLoader_FetchOtherError_FailsClosed(t *testing.T) {
	gh := &fakeFileFetcher{err: forge.ErrForbidden}
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{provider: "github", found: true},
		GitHubFetcher: gh,
		GitHubScope:   githubScopeFixed("42"),
		Override:      breakGlass,
	})
	conv, err := l.Load(context.Background(), "acme/widgets")
	if err == nil || !errors.Is(err, forge.ErrForbidden) {
		t.Fatalf("Load err = %v, want the propagated ErrForbidden (fail closed)", err)
	}
	if conv.Provider == "break-glass" {
		t.Error("a non-ErrNotFound fetch error served the override; it must fail closed")
	}
}

// TestConventionsLoader_ParseError_FailsClosed: a committed-but-invalid file
// is an operator error to surface, not a reason to serve the fallback chain.
func TestConventionsLoader_ParseError_FailsClosed(t *testing.T) {
	gh := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte("provider: [broken"), SHA: "abc"}}
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{provider: "github", found: true},
		GitHubFetcher: gh,
		GitHubScope:   githubScopeFixed("42"),
		Override:      breakGlass,
	})
	conv, err := l.Load(context.Background(), "acme/widgets")
	if err == nil || !strings.Contains(err.Error(), conventionsFilePath) {
		t.Fatalf("Load err = %v, want a parse failure naming %s", err, conventionsFilePath)
	}
	if conv.Provider == "break-glass" {
		t.Error("a parse error served the override; it must fail closed")
	}
}

// TestConventionsLoader_Cache pins the mutex+TTL cache contract with an
// observable clock, fetch counter, and parse counter: within TTL the cached
// parse is served with ZERO fetches; after TTL an unchanged blob SHA
// refetches but REUSES the cached parse; a changed SHA re-parses.
func TestConventionsLoader_Cache(t *testing.T) {
	parses := 0
	gh := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitHubConventionsYAML), SHA: "sha-1"}}
	now := time.Unix(1_700_000_000, 0)
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{provider: "github", found: true},
		GitHubFetcher: gh,
		GitHubScope:   githubScopeFixed("42"),
		Parse:         countingParse(&parses),
		Now:           func() time.Time { return now },
		TTL:           time.Minute,
	})

	conv, err := l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load (cold) err = %v, want nil", err)
	}
	if conv.Provider != "github_projects" {
		t.Fatalf("Provider = %q, want github_projects from the committed file", conv.Provider)
	}
	if gh.calls != 1 || parses != 1 {
		t.Fatalf("cold load: fetches = %d, parses = %d, want 1/1", gh.calls, parses)
	}

	// Within TTL: cached parse, NO fetch at all.
	now = now.Add(30 * time.Second)
	conv, err = l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load (within TTL) err = %v, want nil", err)
	}
	if conv.Provider != "github_projects" {
		t.Errorf("within-TTL Provider = %q, want the cached github_projects", conv.Provider)
	}
	if gh.calls != 1 {
		t.Errorf("within-TTL fetches = %d, want 1 (zero new fetches)", gh.calls)
	}
	if parses != 1 {
		t.Errorf("within-TTL parses = %d, want 1", parses)
	}

	// After TTL with an UNCHANGED SHA: the refetch happens, but the parse
	// counter must NOT increment — the cached parse is reused.
	now = now.Add(2 * time.Minute)
	conv, err = l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load (after TTL, same SHA) err = %v, want nil", err)
	}
	if gh.calls != 2 {
		t.Errorf("after-TTL fetches = %d, want 2 (a refetch happened)", gh.calls)
	}
	if parses != 1 {
		t.Errorf("after-TTL unchanged-SHA parses = %d, want 1 (cached parse reused)", parses)
	}
	if conv.Provider != "github_projects" {
		t.Errorf("after-TTL Provider = %q, want github_projects", conv.Provider)
	}

	// After TTL with a CHANGED SHA: the new content is parsed. The new
	// content keeps the SAME project owner as the repo — a cross-owner file
	// would be refused by the destination binding (#2090), which is the
	// subject of TestConventionsLoader_DestinationRedirect_Refused.
	gh.fc = &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitHubConventionsYAMLAlt), SHA: "sha-2"}
	now = now.Add(2 * time.Minute)
	conv, err = l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load (after TTL, new SHA) err = %v, want nil", err)
	}
	if gh.calls != 3 || parses != 2 {
		t.Errorf("changed-SHA: fetches = %d, parses = %d, want 3/2", gh.calls, parses)
	}
	if conv.Project == nil || conv.Project.Number != 9 {
		t.Errorf("changed-SHA Project = %+v, want project number 9 from the re-parsed content", conv.Project)
	}
}

// TestConventionsLoader_ProviderReassignment_ForgeQualifiedKey pins the
// forge-qualified cache key: a repo cached under one provider and later
// reassigned to another (ADR-057/ADR-058) must refetch from the NEW forge and
// serve its conventions, never the prior forge's cached parse — even within
// the TTL window. With a repo-only key the second (within-TTL) Load would
// serve the stale github parse; the (provider, repo) key makes it a miss.
func TestConventionsLoader_ProviderReassignment_ForgeQualifiedKey(t *testing.T) {
	gh := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitHubConventionsYAML), SHA: "gh-sha"}}
	gl := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitLabConventionsYAMLAcme), SHA: "gl-sha"}}
	resolver := mapProviderResolver{"acme/widgets": "github"}
	now := time.Unix(1_700_000_000, 0)
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      resolver,
		GitHubFetcher: gh,
		GitLabFetcher: gl,
		GitHubScope:   githubScopeFixed("42"),
		GitLabScope:   forge.FromRef("gitlab:deployment"),
		Now:           func() time.Time { return now },
		TTL:           time.Hour,
	})

	conv, err := l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load (github) err = %v, want nil", err)
	}
	if conv.Provider != "github_projects" {
		t.Fatalf("Provider = %q, want github_projects from the github forge", conv.Provider)
	}

	// Reassign the repo to gitlab WITHIN the TTL: a repo-only cache key would
	// serve the stale github parse with no fetch; the forge-qualified key
	// makes this a fresh miss on the gitlab forge.
	resolver["acme/widgets"] = "gitlab"
	conv, err = l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load (gitlab) err = %v, want nil", err)
	}
	if conv.Provider != "gitlab" {
		t.Errorf("Provider = %q, want gitlab after provider reassignment — the github parse must not be served cross-forge", conv.Provider)
	}
	if gl.calls != 1 {
		t.Errorf("gitlab fetch calls = %d, want 1 (the reassignment forced a fetch from the new forge)", gl.calls)
	}
}

// TestConventionsLoader_NotFoundAfterExpiry: a repo whose conventions file was
// cached and is later deleted takes fetch→ErrNotFound→fallback on the
// post-TTL refetch, and the now-expired cache entry is NEVER served again.
func TestConventionsLoader_NotFoundAfterExpiry(t *testing.T) {
	gh := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitHubConventionsYAML), SHA: "sha-1"}}
	now := time.Unix(1_700_000_000, 0)
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{provider: "github", found: true},
		GitHubFetcher: gh,
		GitHubScope:   githubScopeFixed("42"),
		Override:      breakGlass,
		Now:           func() time.Time { return now },
		TTL:           time.Minute,
	})

	conv, err := l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load (cold) err = %v, want nil", err)
	}
	if conv.Provider != "github_projects" {
		t.Fatalf("cold Provider = %q, want github_projects", conv.Provider)
	}

	// The file is deleted; the next refetch (after TTL) returns ErrNotFound.
	gh.fc, gh.err = nil, forge.ErrNotFound
	now = now.Add(2 * time.Minute)
	conv, err = l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load (after delete) err = %v, want nil (ErrNotFound falls through)", err)
	}
	if conv.Provider != "break-glass" {
		t.Errorf("Provider = %q, want the break-glass fallback — the deleted file's stale parse must not be served", conv.Provider)
	}
	if gh.calls != 2 {
		t.Errorf("fetch calls = %d, want 2 (the expiry forced a refetch that 404'd)", gh.calls)
	}
}

// TestConventionsLoader_ConcurrentLoads asserts the mutex contract: concurrent
// loads for the SAME repo collapse to one fetch (no thundering herd), and a
// slow/hung fetch for ONE repo does not block a concurrent load for ANOTHER
// repo (the per-key lock, not a process-global one).
func TestConventionsLoader_ConcurrentLoads(t *testing.T) {
	t.Run("same repo does one fetch", func(t *testing.T) {
		gh := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitHubConventionsYAML), SHA: "sha-1"}}
		now := time.Unix(1_700_000_000, 0)
		l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
			Resolver:      &fakeProviderResolver{provider: "github", found: true},
			GitHubFetcher: gh,
			GitHubScope:   githubScopeFixed("42"),
			Now:           func() time.Time { return now },
			TTL:           time.Hour,
		})

		const n = 8
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				conv, err := l.Load(context.Background(), "acme/widgets")
				if err != nil {
					t.Errorf("concurrent Load err = %v, want nil", err)
					return
				}
				if conv.Provider != "github_projects" {
					t.Errorf("concurrent Load Provider = %q, want github_projects", conv.Provider)
				}
			}()
		}
		wg.Wait()
		if gh.calls != 1 {
			t.Errorf("fetch calls = %d, want 1 — concurrent same-repo loads must collapse to one fetch", gh.calls)
		}
	})

	t.Run("a slow repo does not block another repo", func(t *testing.T) {
		// gatedGitHub blocks inside FetchFile until released, holding the
		// acme/widgets key lock across the fetch. A process-global lock would
		// then wedge the concurrent group/lib load below.
		started := make(chan struct{})
		release := make(chan struct{})
		gated := &gatedFetcher{
			started: started,
			release: release,
			fc:      &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitHubConventionsYAML), SHA: "gh-sha"},
		}
		gl := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitLabConventionsYAML), SHA: "gl-sha"}}
		l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
			Resolver:      mapProviderResolver{"acme/widgets": "github", "group/lib": "gitlab"},
			GitHubFetcher: gated,
			GitLabFetcher: gl,
			GitHubScope:   githubScopeFixed("42"),
			GitLabScope:   forge.FromRef("gitlab:deployment"),
		})

		slowDone := make(chan struct{})
		go func() {
			defer close(slowDone)
			_, _ = l.Load(context.Background(), "acme/widgets")
		}()
		<-started // acme/widgets is now inside FetchFile, holding its key lock

		fast := make(chan workmgmt.Conventions, 1)
		go func() {
			conv, err := l.Load(context.Background(), "group/lib")
			if err != nil {
				t.Errorf("Load(group/lib) err = %v, want nil", err)
			}
			fast <- conv
		}()

		// The deadlock guard: group/lib must complete while acme/widgets is
		// still blocked in its fetch. This is a generous liveness bound, not a
		// timing assertion — the fake gitlab fetch returns in microseconds.
		select {
		case conv := <-fast:
			if conv.Provider != "gitlab" {
				t.Errorf("group/lib Provider = %q, want gitlab", conv.Provider)
			}
		case <-time.After(5 * time.Second):
			close(release) // unblock the parked goroutine before failing
			t.Fatal("Load(group/lib) blocked behind acme/widgets' held fetch lock — loads are serialized cross-repo")
		}

		close(release)
		<-slowDone
	})
}

// gatedFetcher blocks inside FetchFile until release is closed, signaling entry
// by closing started — so a test can hold one repo's fetch open across a
// concurrent load for another repo.
type gatedFetcher struct {
	started chan struct{}
	release chan struct{}
	fc      *forge.FileContent
}

func (g *gatedFetcher) FetchFile(context.Context, forge.CredentialScope, forge.RepoRef, string, string) (*forge.FileContent, error) {
	close(g.started)
	<-g.release
	return g.fc, nil
}

// TestConventionsLoader_OverrideAbsent_Default: with no break-glass override
// installed, every fall-through lands on workmgmt.Default().
func TestConventionsLoader_OverrideAbsent_Default(t *testing.T) {
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver: &fakeProviderResolver{found: false},
	})
	conv, err := l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load err = %v, want nil", err)
	}
	if conv.Provider != workmgmt.Default().Provider {
		t.Errorf("Provider = %q, want the Default() provider %q", conv.Provider, workmgmt.Default().Provider)
	}
}

// TestConventionsLoader_DestinationRedirect_Refused is the E44.14 / #2090
// done-means test, driven across the whole resolver → fetch → parse →
// authorize → cache seam: a file committed to a VICTIM-owned repo naming
// ANOTHER account's project board is refused, the caller gets the ZERO
// conventions (never the break-glass override and never workmgmt.Default() —
// falling through would make the deployment default attacker-selectable), and
// NOTHING is cached, so a second Load refetches and re-parses instead of
// serving a cached refusal.
func TestConventionsLoader_DestinationRedirect_Refused(t *testing.T) {
	parses := 0
	gh := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitHubRedirectConventionsYAML), SHA: "sha-1"}}
	now := time.Unix(1_700_000_000, 0)
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{provider: "github", found: true},
		GitHubFetcher: gh,
		GitHubScope:   githubScopeFixed("42"),
		Override:      breakGlass,
		Parse:         countingParse(&parses),
		Now:           func() time.Time { return now },
		TTL:           time.Hour,
	})

	conv, err := l.Load(context.Background(), "victim/widgets")
	if err == nil {
		t.Fatal("Load err = nil, want a destination-authorization refusal")
	}
	if !errors.Is(err, errConventionsDestinationUnauthorized) {
		t.Fatalf("Load err = %v, want it to wrap errConventionsDestinationUnauthorized", err)
	}
	if conv.Provider != "" {
		t.Errorf("Provider = %q, want the ZERO conventions — a refused destination must serve neither the override nor Default()", conv.Provider)
	}
	if gh.calls != 1 || parses != 1 {
		t.Fatalf("first Load: fetches = %d, parses = %d, want 1/1", gh.calls, parses)
	}

	// WITHIN the TTL: nothing was cached, so the second Load must refetch AND
	// re-parse. This is the assertion that stops a future edit which caches
	// before authorizing from silently reopening the redirect hole.
	conv, err = l.Load(context.Background(), "victim/widgets")
	if err == nil || !errors.Is(err, errConventionsDestinationUnauthorized) {
		t.Fatalf("second Load err = %v, want the same refusal", err)
	}
	if conv.Provider != "" {
		t.Errorf("second Load Provider = %q, want the ZERO conventions", conv.Provider)
	}
	if gh.calls != 2 {
		t.Errorf("second Load fetches = %d, want 2 — a refused parse must NOT be cached", gh.calls)
	}
	if parses != 2 {
		t.Errorf("second Load parses = %d, want 2 — a refused parse must NOT be cached", parses)
	}
}

// TestConventionsLoader_DestinationAuthorized_Cached is the guard against a
// regression that refuses everything: a file whose destination IS the repo's
// own account is served and cached exactly as before (the second within-TTL
// Load issues zero fetches).
func TestConventionsLoader_DestinationAuthorized_Cached(t *testing.T) {
	gh := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitHubConventionsYAML), SHA: "sha-1"}}
	now := time.Unix(1_700_000_000, 0)
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      &fakeProviderResolver{provider: "github", found: true},
		GitHubFetcher: gh,
		GitHubScope:   githubScopeFixed("42"),
		Override:      breakGlass,
		Now:           func() time.Time { return now },
		TTL:           time.Hour,
	})

	conv, err := l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load err = %v, want nil for a file bound to the repo's own account", err)
	}
	if conv.Provider != "github_projects" {
		t.Fatalf("Provider = %q, want github_projects from the committed file", conv.Provider)
	}
	if _, err = l.Load(context.Background(), "acme/widgets"); err != nil {
		t.Fatalf("second Load err = %v, want nil", err)
	}
	if gh.calls != 1 {
		t.Errorf("fetches = %d, want 1 — an authorized parse is cached as before", gh.calls)
	}
}

// TestConventionsLoader_DestinationAllowListed: the administrator-controlled
// allow-list is the escape hatch for a legitimate cross-namespace
// destination.
func TestConventionsLoader_DestinationAllowListed(t *testing.T) {
	allow, err := ParseWorkMgmtDestinationAllowList("victim:github_projects:attacker")
	if err != nil {
		t.Fatalf("allow-list parse err = %v, want nil", err)
	}
	gh := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitHubRedirectConventionsYAML), SHA: "sha-1"}}
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:            &fakeProviderResolver{provider: "github", found: true},
		GitHubFetcher:       gh,
		GitHubScope:         githubScopeFixed("42"),
		Override:            breakGlass,
		AllowedDestinations: allow,
	})

	conv, err := l.Load(context.Background(), "victim/widgets")
	if err != nil {
		t.Fatalf("Load err = %v, want nil for an allow-listed destination", err)
	}
	if conv.Project == nil || conv.Project.Owner != "attacker" {
		t.Errorf("Project = %+v, want the allow-listed cross-namespace owner", conv.Project)
	}
}

// TestConventionsLoader_FallbacksNotDestinationValidated pins the deliberate
// non-validation of the ADMINISTRATOR-controlled fallbacks: the break-glass
// override and workmgmt.Default() are the trusted deployment inputs whose
// displacement by untrusted repo input is the entire concern, so neither is
// subjected to the destination binding. Both fixtures name a destination that
// would be refused if it came from a repo-fetched file.
func TestConventionsLoader_FallbacksNotDestinationValidated(t *testing.T) {
	t.Run("override served unvalidated on ErrNotFound", func(t *testing.T) {
		gh := &fakeFileFetcher{err: forge.ErrNotFound}
		l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
			Resolver:      &fakeProviderResolver{provider: "github", found: true},
			GitHubFetcher: gh,
			GitHubScope:   githubScopeFixed("42"),
			Override: func() (workmgmt.Conventions, bool) {
				return ghDest("some-other-org"), true
			},
		})
		conv, err := l.Load(context.Background(), "acme/widgets")
		if err != nil {
			t.Fatalf("Load err = %v, want nil", err)
		}
		if conv.Project == nil || conv.Project.Owner != "some-other-org" {
			t.Errorf("Project = %+v, want the override served unvalidated", conv.Project)
		}
	})

	t.Run("override served unvalidated when the resolver finds no account", func(t *testing.T) {
		l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
			Resolver: &fakeProviderResolver{found: false},
			Override: func() (workmgmt.Conventions, bool) {
				return ghDest("some-other-org"), true
			},
		})
		conv, err := l.Load(context.Background(), "acme/widgets")
		if err != nil {
			t.Fatalf("Load err = %v, want nil", err)
		}
		if conv.Project == nil || conv.Project.Owner != "some-other-org" {
			t.Errorf("Project = %+v, want the override served unvalidated", conv.Project)
		}
	})

	t.Run("Default served unvalidated for an unregistered forge", func(t *testing.T) {
		l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
			Resolver: &fakeProviderResolver{provider: "github", found: true},
			// No GitHubFetcher: the forge is unregistered.
		})
		conv, err := l.Load(context.Background(), "acme/widgets")
		if err != nil {
			t.Fatalf("Load err = %v, want nil", err)
		}
		if conv.Provider != workmgmt.Default().Provider {
			t.Errorf("Provider = %q, want workmgmt.Default() served unvalidated", conv.Provider)
		}
	})
}

// TestConventionsLoader_MixedForge is the done-means cross-boundary test: ONE
// loader instance driven across TWO repos — one whose discriminator resolves
// github with a committed github_projects file, one resolving gitlab with a
// committed gitlab file — each repo getting its own provider from its OWN
// committed file, fetched from its OWN forge with its own self-resolved
// credential scope (gitlab pinned to ref=HEAD).
func TestConventionsLoader_MixedForge(t *testing.T) {
	gh := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitHubConventionsYAML), SHA: "gh-sha"}}
	gl := &fakeFileFetcher{fc: &forge.FileContent{Path: conventionsFilePath, Content: []byte(testGitLabConventionsYAML), SHA: "gl-sha"}}
	l := NewRepoConventionsLoader(RepoConventionsLoaderConfig{
		Resolver:      mapProviderResolver{"acme/widgets": "github", "group/lib": "gitlab"},
		GitHubFetcher: gh,
		GitLabFetcher: gl,
		GitHubScope:   githubScopeFixed("777"),
		GitLabScope:   forge.FromRef("gitlab:deployment"),
		Override:      breakGlass,
	})

	ghConv, err := l.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load(github repo) err = %v, want nil", err)
	}
	glConv, err := l.Load(context.Background(), "group/lib")
	if err != nil {
		t.Fatalf("Load(gitlab repo) err = %v, want nil", err)
	}

	if ghConv.Provider != "github_projects" {
		t.Errorf("github repo Provider = %q, want github_projects from its own committed file", ghConv.Provider)
	}
	if glConv.Provider != "gitlab" {
		t.Errorf("gitlab repo Provider = %q, want gitlab from its own committed file", glConv.Provider)
	}

	if gh.calls != 1 || gl.calls != 1 {
		t.Errorf("fetch calls = github %d / gitlab %d, want 1/1 — each repo fetches its OWN forge", gh.calls, gl.calls)
	}
	if gh.lastRepo != (forge.RepoRef{Owner: "acme", Name: "widgets"}) || gh.lastPath != conventionsFilePath {
		t.Errorf("github fetch = %v %q, want acme/widgets %q", gh.lastRepo, gh.lastPath, conventionsFilePath)
	}
	if gh.lastScope.Ref() != "777" {
		t.Errorf("github fetch scope = %q, want the self-resolved installation scope 777", gh.lastScope.Ref())
	}
	if gl.lastRepo != (forge.RepoRef{Owner: "group", Name: "lib"}) || gl.lastPath != conventionsFilePath {
		t.Errorf("gitlab fetch = %v %q, want group/lib %q", gl.lastRepo, gl.lastPath, conventionsFilePath)
	}
	if gl.lastRef != "HEAD" {
		t.Errorf("gitlab fetch ref = %q, want the explicit HEAD the Repository Files API requires", gl.lastRef)
	}
	if gl.lastScope.Ref() != "gitlab:deployment" {
		t.Errorf("gitlab fetch scope = %q, want the deployment scope", gl.lastScope.Ref())
	}
}
