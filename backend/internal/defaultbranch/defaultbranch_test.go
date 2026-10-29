package defaultbranch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
)

// fakeGetter is a counting Getter. fn decides each answer; calls counts every
// GetRepository invocation (mutex-guarded, the resolver is concurrent).
type fakeGetter struct {
	mu    sync.Mutex
	calls int
	fn    func(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef) (*forge.Repository, error)
}

func (g *fakeGetter) GetRepository(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef) (*forge.Repository, error) {
	g.mu.Lock()
	g.calls++
	g.mu.Unlock()
	return g.fn(ctx, scope, repo)
}

func (g *fakeGetter) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls
}

func branchGetter(branch string) *fakeGetter {
	return &fakeGetter{fn: func(context.Context, forge.CredentialScope, forge.RepoRef) (*forge.Repository, error) {
		return &forge.Repository{DefaultBranch: branch}, nil
	}}
}

func errGetter(err error) *fakeGetter {
	return &fakeGetter{fn: func(context.Context, forge.CredentialScope, forge.RepoRef) (*forge.Repository, error) {
		return nil, err
	}}
}

// fakeClock is a settable clock for TTL tests.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClocked(g Getter) (*Resolver, *fakeClock) {
	r := New(g)
	clk := &fakeClock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	r.now = clk.now
	return r, clk
}

var (
	scope42 = forge.FromGitHubInstallationID(42)
	widgets = forge.RepoRef{Owner: "acme", Name: "widgets"}
)

func wantReason(t *testing.T, err error, want Reason) {
	t.Helper()
	got, ok := ReasonOf(err)
	if !ok {
		t.Fatalf("err = %v, want an *UnresolvedError with reason %s", err, want)
	}
	if got != want {
		t.Fatalf("reason = %s, want %s (err %v)", got, want, err)
	}
}

func TestResolve_NilSourceIsUnconfigured(t *testing.T) {
	branch, err := Resolve(context.Background(), nil, scope42, widgets)
	if branch != "" {
		t.Errorf("branch = %q, want empty", branch)
	}
	wantReason(t, err, ReasonResolverUnconfigured)
}

func TestResolve_DelegatesToSource(t *testing.T) {
	branch, err := Resolve(context.Background(), New(branchGetter("trunk")), scope42, widgets)
	if err != nil || branch != "trunk" {
		t.Fatalf("Resolve = (%q, %v), want (trunk, nil)", branch, err)
	}
}

func TestResolver_NilGetterIsUnconfigured(t *testing.T) {
	_, err := New(nil).Resolve(context.Background(), scope42, widgets)
	wantReason(t, err, ReasonResolverUnconfigured)

	var nilResolver *Resolver
	_, err = nilResolver.Resolve(context.Background(), scope42, widgets)
	wantReason(t, err, ReasonResolverUnconfigured)
}

func TestResolver_ZeroScopeIsNoInstallation(t *testing.T) {
	g := branchGetter("trunk")
	r := New(g)
	_, err := r.Resolve(context.Background(), forge.CredentialScope{}, widgets)
	wantReason(t, err, ReasonNoInstallation)
	if g.count() != 0 {
		t.Errorf("getter calls = %d, want 0 (decided locally)", g.count())
	}
}

func TestResolver_InvalidRepository(t *testing.T) {
	for name, repo := range map[string]forge.RepoRef{
		"empty owner": {Name: "widgets"},
		"empty name":  {Owner: "acme"},
	} {
		t.Run(name, func(t *testing.T) {
			g := branchGetter("trunk")
			_, err := New(g).Resolve(context.Background(), scope42, repo)
			wantReason(t, err, ReasonRepositoryInvalid)
			if g.count() != 0 {
				t.Errorf("getter calls = %d, want 0 (decided locally)", g.count())
			}
		})
	}
}

func TestResolver_NotFoundIsRepositoryNotVisible(t *testing.T) {
	_, err := New(errGetter(fmt.Errorf("get repository: %w", forge.ErrNotFound))).Resolve(context.Background(), scope42, widgets)
	wantReason(t, err, ReasonRepositoryNotVisible)
	if !errors.Is(err, forge.ErrNotFound) {
		t.Errorf("err = %v, want it to wrap forge.ErrNotFound", err)
	}
}

func TestResolver_ForbiddenIsForbidden(t *testing.T) {
	_, err := New(errGetter(fmt.Errorf("get repository: %w", forge.ErrForbidden))).Resolve(context.Background(), scope42, widgets)
	wantReason(t, err, ReasonForbidden)
}

func TestResolver_EmptyDefaultBranchIsMissing(t *testing.T) {
	cases := map[string]*fakeGetter{
		"nil repository": {fn: func(context.Context, forge.CredentialScope, forge.RepoRef) (*forge.Repository, error) {
			return nil, nil
		}},
		"empty":      branchGetter(""),
		"whitespace": branchGetter("  \t"),
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := New(g).Resolve(context.Background(), scope42, widgets)
			wantReason(t, err, ReasonDefaultBranchMissing)
		})
	}
}

func TestResolver_OtherErrorIsLookupFailed(t *testing.T) {
	_, err := New(errGetter(errors.New("connection reset"))).Resolve(context.Background(), scope42, widgets)
	wantReason(t, err, ReasonLookupFailed)
}

func TestResolver_CachePositive(t *testing.T) {
	g := branchGetter("trunk")
	r, _ := newClocked(g)
	for i := 0; i < 2; i++ {
		branch, err := r.Resolve(context.Background(), scope42, widgets)
		if err != nil || branch != "trunk" {
			t.Fatalf("Resolve #%d = (%q, %v), want (trunk, nil)", i, branch, err)
		}
	}
	if g.count() != 1 {
		t.Fatalf("getter calls = %d, want 1 (second resolve served from cache)", g.count())
	}
}

func TestResolver_CachePositiveExpires(t *testing.T) {
	g := branchGetter("trunk")
	r, clk := newClocked(g)
	if _, err := r.Resolve(context.Background(), scope42, widgets); err != nil {
		t.Fatal(err)
	}
	clk.advance(positiveTTL - time.Second)
	if _, err := r.Resolve(context.Background(), scope42, widgets); err != nil {
		t.Fatal(err)
	}
	if g.count() != 1 {
		t.Fatalf("getter calls inside the window = %d, want 1", g.count())
	}
	clk.advance(2 * time.Second)
	if _, err := r.Resolve(context.Background(), scope42, widgets); err != nil {
		t.Fatal(err)
	}
	if g.count() != 2 {
		t.Fatalf("getter calls past the positive TTL = %d, want 2", g.count())
	}
}

func TestResolver_CacheNegative(t *testing.T) {
	g := errGetter(forge.ErrNotFound)
	r, _ := newClocked(g)
	for i := 0; i < 2; i++ {
		_, err := r.Resolve(context.Background(), scope42, widgets)
		wantReason(t, err, ReasonRepositoryNotVisible)
	}
	if g.count() != 1 {
		t.Fatalf("getter calls = %d, want 1 (failure negative-cached)", g.count())
	}
}

func TestResolver_CacheNegativeExpires(t *testing.T) {
	fail := true
	g := &fakeGetter{fn: func(context.Context, forge.CredentialScope, forge.RepoRef) (*forge.Repository, error) {
		if fail {
			return nil, forge.ErrForbidden
		}
		return &forge.Repository{DefaultBranch: "trunk"}, nil
	}}
	r, clk := newClocked(g)
	_, err := r.Resolve(context.Background(), scope42, widgets)
	wantReason(t, err, ReasonForbidden)
	fail = false
	clk.advance(negativeTTL - time.Second)
	_, err = r.Resolve(context.Background(), scope42, widgets)
	wantReason(t, err, ReasonForbidden)
	clk.advance(2 * time.Second)
	branch, err := r.Resolve(context.Background(), scope42, widgets)
	if err != nil || branch != "trunk" {
		t.Fatalf("Resolve past the negative TTL = (%q, %v), want (trunk, nil)", branch, err)
	}
	if g.count() != 2 {
		t.Fatalf("getter calls = %d, want 2", g.count())
	}
}

// ctxHonouringGetter fails with the ctx error when ctx is done, else answers
// trunk — a forge read that a cancelled request aborts.
func ctxHonouringGetter() *fakeGetter {
	return &fakeGetter{fn: func(ctx context.Context, _ forge.CredentialScope, _ forge.RepoRef) (*forge.Repository, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return &forge.Repository{DefaultBranch: "trunk"}, nil
	}}
}

func TestResolver_CallerCancelNotCached(t *testing.T) {
	g := ctxHonouringGetter()
	r, _ := newClocked(g)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := r.Resolve(cancelled, scope42, widgets)
	wantReason(t, err, ReasonLookupFailed)

	branch, err := r.Resolve(context.Background(), scope42, widgets)
	if err != nil || branch != "trunk" {
		t.Fatalf("Resolve on a live ctx after a caller cancel = (%q, %v), want (trunk, nil): a caller-cancelled failure must not be cached", branch, err)
	}
	if g.count() != 2 {
		t.Fatalf("getter calls = %d, want 2", g.count())
	}
}

func TestResolver_OwnTimeoutIsNegativeCached(t *testing.T) {
	g := &fakeGetter{fn: func(ctx context.Context, _ forge.CredentialScope, _ forge.RepoRef) (*forge.Repository, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	r, _ := newClocked(g)
	r.timeout = 10 * time.Millisecond
	_, err := r.Resolve(context.Background(), scope42, widgets)
	wantReason(t, err, ReasonLookupFailed)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
	_, err = r.Resolve(context.Background(), scope42, widgets)
	wantReason(t, err, ReasonLookupFailed)
	if g.count() != 1 {
		t.Fatalf("getter calls = %d, want 1 (resolver-imposed timeout negative-cached)", g.count())
	}
}

func TestResolver_KeysByScopeAndRepo(t *testing.T) {
	g := branchGetter("trunk")
	r, _ := newClocked(g)
	ctx := context.Background()
	mustResolve := func(scope forge.CredentialScope, repo forge.RepoRef) {
		t.Helper()
		if _, err := r.Resolve(ctx, scope, repo); err != nil {
			t.Fatal(err)
		}
	}
	mustResolve(scope42, widgets)
	mustResolve(forge.FromGitHubInstallationID(43), widgets)
	mustResolve(scope42, forge.RepoRef{Owner: "acme", Name: "gadgets"})
	if g.count() != 3 {
		t.Fatalf("getter calls = %d, want 3 (distinct scope or repo is a distinct key)", g.count())
	}
	mustResolve(scope42, forge.RepoRef{Owner: "ACME", Name: "Widgets"})
	if g.count() != 3 {
		t.Fatalf("getter calls = %d, want 3 (owner/name case folded)", g.count())
	}
}

func TestResolver_ConcurrentResolve(t *testing.T) {
	g := branchGetter("trunk")
	r := New(g)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			repo := forge.RepoRef{Owner: "acme", Name: fmt.Sprintf("r%d", i%4)}
			if branch, err := r.Resolve(context.Background(), scope42, repo); err != nil || branch != "trunk" {
				t.Errorf("Resolve = (%q, %v)", branch, err)
			}
		}(i)
	}
	wg.Wait()
}

func TestResolver_BoundsCache(t *testing.T) {
	g := branchGetter("trunk")
	r, clk := newClocked(g)
	ctx := context.Background()
	for i := 0; i < maxEntries; i++ {
		if _, err := r.Resolve(ctx, scope42, forge.RepoRef{Owner: "acme", Name: fmt.Sprintf("r%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	// All live: the next store clears the map rather than growing past it.
	if _, err := r.Resolve(ctx, scope42, forge.RepoRef{Owner: "acme", Name: "overflow"}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.entries); n != 1 {
		t.Fatalf("entries after an all-live overflow = %d, want 1", n)
	}
	// Expired entries are swept before a clear is needed.
	for i := 0; i < maxEntries-1; i++ {
		if _, err := r.Resolve(ctx, scope42, forge.RepoRef{Owner: "acme", Name: fmt.Sprintf("s%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	clk.advance(positiveTTL + time.Second)
	if _, err := r.Resolve(ctx, scope42, forge.RepoRef{Owner: "acme", Name: "fresh"}); err != nil {
		t.Fatal(err)
	}
	if n := len(r.entries); n != 1 {
		t.Fatalf("entries after sweeping expired = %d, want 1", n)
	}
}

func TestStatic(t *testing.T) {
	branch, err := Static("trunk").Resolve(context.Background(), forge.CredentialScope{}, forge.RepoRef{})
	if err != nil || branch != "trunk" {
		t.Fatalf("Static(trunk) = (%q, %v)", branch, err)
	}
	_, err = Static("").Resolve(context.Background(), scope42, widgets)
	wantReason(t, err, ReasonResolverUnconfigured)
}

func TestUnresolvedError_ErrorUnwrapReasonOf(t *testing.T) {
	cause := fmt.Errorf("wrapped: %w", forge.ErrNotFound)
	ue := Unresolved(widgets, ReasonRepositoryNotVisible, cause)
	msg := ue.Error()
	for _, want := range []string{"acme/widgets", string(ReasonRepositoryNotVisible), "wrapped"} {
		if !strings.Contains(msg, want) {
			t.Errorf("Error() = %q, want it to contain %q", msg, want)
		}
	}
	if !errors.Is(ue, forge.ErrNotFound) {
		t.Errorf("Unwrap chain does not reach forge.ErrNotFound")
	}
	outer := fmt.Errorf("resolve base: %w", ue)
	if r, ok := ReasonOf(outer); !ok || r != ReasonRepositoryNotVisible {
		t.Errorf("ReasonOf(wrapped) = (%s, %v)", r, ok)
	}
	if _, ok := ReasonOf(errors.New("plain")); ok {
		t.Errorf("ReasonOf(plain) reported a reason")
	}
	if got := Unresolved(widgets, ReasonResolverUnconfigured, nil).Error(); strings.HasSuffix(got, ": ") {
		t.Errorf("causeless Error() = %q has a dangling separator", got)
	}
}
