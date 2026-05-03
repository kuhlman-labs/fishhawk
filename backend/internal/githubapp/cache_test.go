package githubapp

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// stubClient lets cache tests drive Token return values without
// going through a real httptest.Server. It satisfies the *Client
// shape via field substitution: cache uses Client.IssueInstallation
// Token, but tests can replace the function pointer via clientFunc.
//
// Easier: just use the real Client with a simple fake server.
// We do that for end-to-end realism in newCachedTestProvider.

func newCachedTestProvider(t *testing.T) (*CachedProvider, *fakeGitHub, *time.Time) {
	t.Helper()
	fg, srv := newFakeGitHub(t)
	c := newTestClient(t, srv.URL)
	cp := NewCachedProvider(c)

	// Inject a mutable clock so tests can roll time forward.
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	cp.Now = func() time.Time { return now }
	return cp, fg, &now
}

func setFakeTokenResponse(fg *fakeGitHub, expiresAt time.Time) {
	fg.body = `{"token":"ghs_token_a","expires_at":"` + expiresAt.UTC().Format(time.RFC3339) + `"}`
}

func TestCachedProvider_FirstCallIsMiss(t *testing.T) {
	cp, fg, now := newCachedTestProvider(t)
	setFakeTokenResponse(fg, now.Add(time.Hour))

	tok, err := cp.Token(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Error("token empty")
	}
	hits, misses, refreshes, refreshErrs := cp.Stats.Snapshot()
	if hits != 0 || misses != 1 || refreshes != 0 || refreshErrs != 0 {
		t.Errorf("stats = (%d,%d,%d,%d), want (0,1,0,0)",
			hits, misses, refreshes, refreshErrs)
	}
	if fg.requestCounter != 1 {
		t.Errorf("issued %d times, want 1", fg.requestCounter)
	}
}

func TestCachedProvider_SecondCallIsHit(t *testing.T) {
	cp, fg, now := newCachedTestProvider(t)
	setFakeTokenResponse(fg, now.Add(time.Hour))

	_, _ = cp.Token(context.Background(), 42)
	tok, err := cp.Token(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if tok == "" {
		t.Error("token empty")
	}
	hits, misses, _, _ := cp.Stats.Snapshot()
	if hits != 1 || misses != 1 {
		t.Errorf("hits/misses = %d/%d, want 1/1", hits, misses)
	}
	if fg.requestCounter != 1 {
		t.Errorf("issued %d times, want 1 (hit shouldn't refresh)", fg.requestCounter)
	}
}

func TestCachedProvider_RefreshesNearExpiry(t *testing.T) {
	cp, fg, now := newCachedTestProvider(t)
	// Token TTL = 6 minutes; RefreshLeadTime = 5m by default. Fast-
	// forward 2m: still 4m of TTL left, which is < 5m, so the next
	// call refreshes.
	setFakeTokenResponse(fg, now.Add(6*time.Minute))

	_, _ = cp.Token(context.Background(), 42)
	*now = now.Add(2 * time.Minute)
	setFakeTokenResponse(fg, now.Add(time.Hour)) // refreshed token has fresh TTL

	if _, err := cp.Token(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	hits, misses, refreshes, _ := cp.Stats.Snapshot()
	if hits != 0 || misses != 2 || refreshes != 1 {
		t.Errorf("stats = (%d,%d,%d), want (0,2,1)", hits, misses, refreshes)
	}
	if fg.requestCounter != 2 {
		t.Errorf("issued %d times, want 2", fg.requestCounter)
	}
}

func TestCachedProvider_DifferentInstallationsCachedSeparately(t *testing.T) {
	cp, fg, now := newCachedTestProvider(t)
	setFakeTokenResponse(fg, now.Add(time.Hour))

	_, _ = cp.Token(context.Background(), 1)
	_, _ = cp.Token(context.Background(), 2)
	if fg.requestCounter != 2 {
		t.Errorf("issued %d times, want 2 (different installs)", fg.requestCounter)
	}
	// Both should now hit on second call.
	_, _ = cp.Token(context.Background(), 1)
	_, _ = cp.Token(context.Background(), 2)
	if fg.requestCounter != 2 {
		t.Errorf("issued %d times after hits, want 2", fg.requestCounter)
	}
}

func TestCachedProvider_Forget(t *testing.T) {
	cp, fg, now := newCachedTestProvider(t)
	setFakeTokenResponse(fg, now.Add(time.Hour))

	_, _ = cp.Token(context.Background(), 42)
	cp.Forget(42)
	_, _ = cp.Token(context.Background(), 42)
	if fg.requestCounter != 2 {
		t.Errorf("issued %d times, want 2 (Forget should evict)", fg.requestCounter)
	}
}

func TestCachedProvider_RefreshError(t *testing.T) {
	cp, fg, _ := newCachedTestProvider(t)
	fg.status = http.StatusInternalServerError
	fg.body = "boom"

	_, err := cp.Token(context.Background(), 42)
	if err == nil {
		t.Fatal("expected error")
	}
	_, _, _, refreshErrs := cp.Stats.Snapshot()
	if refreshErrs != 1 {
		t.Errorf("refreshErrs = %d, want 1", refreshErrs)
	}
}

func TestCachedProvider_NotFoundBubblesUp(t *testing.T) {
	cp, fg, _ := newCachedTestProvider(t)
	fg.status = http.StatusNotFound

	_, err := cp.Token(context.Background(), 42)
	if !errors.Is(err, ErrInstallationNotFound) {
		t.Errorf("err = %v, want ErrInstallationNotFound", err)
	}
}

func TestCachedProvider_ConcurrentSameInstallation(t *testing.T) {
	// Multiple goroutines hitting the same installation — each
	// observation should produce one shared token, with at most a
	// small number of network requests (the entry mutex serializes
	// the first refresh; subsequent callers may either wait on the
	// mutex or hit the cache once it's populated).
	cp, fg, now := newCachedTestProvider(t)
	setFakeTokenResponse(fg, now.Add(time.Hour))

	var wg sync.WaitGroup
	var seen sync.Map
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := cp.Token(context.Background(), 42)
			if err != nil {
				t.Errorf("goroutine: %v", err)
				return
			}
			seen.Store(tok, true)
		}()
	}
	wg.Wait()

	count := 0
	seen.Range(func(_, _ any) bool { count++; return true })
	if count != 1 {
		t.Errorf("saw %d unique tokens, want 1 (cache should converge)", count)
	}
	// Network requests aren't necessarily 1 (the mutex is per-entry,
	// not a singleflight) but should be bounded — well under 50.
	if fg.requestCounter > 5 {
		t.Errorf("issued %d times under concurrency, want <= 5", fg.requestCounter)
	}
}

func TestStats_Snapshot(t *testing.T) {
	var s Stats
	s.hits.Add(3)
	s.misses.Add(7)
	s.refreshes.Add(2)
	s.refreshErrs.Add(1)
	hits, misses, refreshes, refreshErrs := s.Snapshot()
	if hits != 3 || misses != 7 || refreshes != 2 || refreshErrs != 1 {
		t.Errorf("Snapshot = (%d,%d,%d,%d), want (3,7,2,1)",
			hits, misses, refreshes, refreshErrs)
	}
}

func TestNewCachedProvider_Defaults(t *testing.T) {
	c := &Client{}
	cp := NewCachedProvider(c)
	if cp.RefreshLeadTime != 5*time.Minute {
		t.Errorf("RefreshLeadTime = %v", cp.RefreshLeadTime)
	}
	if cp.Now == nil {
		t.Error("Now is nil")
	}
	if cp.entries == nil {
		t.Error("entries is nil")
	}
}

// TokenProviderInterfaceCheck is a compile-time guarantee that
// CachedProvider satisfies the TokenProvider interface. If the
// interface ever changes, the build breaks here loudly.
func TestCachedProvider_SatisfiesInterface(t *testing.T) {
	var _ TokenProvider = (*CachedProvider)(nil)
}

// uintEqual sanity-checks the atomic counters' generic type.
// Future-proofs against a refactor that swaps the field types.
func TestStats_AtomicShape(t *testing.T) {
	var s Stats
	if !valueIsAtomicUint64(&s.hits) {
		t.Error("hits is not atomic.Uint64")
	}
}

func valueIsAtomicUint64(v any) bool {
	_, ok := v.(*atomic.Uint64)
	return ok
}
