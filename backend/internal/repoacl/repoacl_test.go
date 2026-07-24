package repoacl_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repoacl"
)

// fakeStore records every call so a test can assert that a forge fault wrote
// NOTHING — the "no upsert" half of the fail-closed contract is not observable
// from the return value alone. It also models the purge-watermark generation
// discipline (#2116): a per-(provider, subject) generation map, and a guarded
// Upsert that REJECTS (records nothing, returns nil) when the captured
// generation trails the current one — the in-memory analogue of the FOR SHARE
// guard.
type fakeStore struct {
	entry     repoacl.Entry
	found     bool
	getErr    error
	upsertErr error
	deleteErr error
	ensureErr error
	bumpErr   error

	upserts  []identity.Permission
	deletes  int
	lastKey  [3]string
	getCalls int

	// gens tracks the current purge generation per (provider, subject); absent
	// keys are generation 0. capturedGens/ensureCalls record what Permission
	// threaded into Upsert. calls is an ORDERED log of mutating calls so a test
	// can assert bump-before-delete and ensure-before-upsert by order, not
	// merely counts.
	gens         map[string]int64
	capturedGens []int64
	ensureCalls  int
	calls        []string
}

func (f *fakeStore) key(provider, subject string) string { return provider + "\x00" + subject }

func (f *fakeStore) Get(_ context.Context, provider, subject, repo string) (repoacl.Entry, bool, error) {
	f.getCalls++
	f.lastKey = [3]string{provider, subject, repo}
	if f.getErr != nil {
		return repoacl.Entry{}, false, f.getErr
	}
	return f.entry, f.found, nil
}

func (f *fakeStore) EnsurePurgeGeneration(_ context.Context, provider, subject string) (int64, error) {
	if f.ensureErr != nil {
		return 0, f.ensureErr
	}
	f.ensureCalls++
	f.calls = append(f.calls, "ensure")
	if f.gens == nil {
		f.gens = map[string]int64{}
	}
	return f.gens[f.key(provider, subject)], nil
}

func (f *fakeStore) BumpPurgeWatermark(_ context.Context, provider, subject string) error {
	if f.bumpErr != nil {
		return f.bumpErr
	}
	if f.gens == nil {
		f.gens = map[string]int64{}
	}
	f.gens[f.key(provider, subject)]++
	f.calls = append(f.calls, "bump")
	return nil
}

func (f *fakeStore) Upsert(_ context.Context, provider, subject, _ string, perm identity.Permission, capturedGen int64) error {
	if f.upsertErr != nil {
		return f.upsertErr
	}
	f.capturedGens = append(f.capturedGens, capturedGen)
	// Model the guarded rejection: a purge that bumped the generation past the
	// captured value rejects the write (records nothing, returns nil).
	if f.gens != nil && capturedGen < f.gens[f.key(provider, subject)] {
		return nil
	}
	f.upserts = append(f.upserts, perm)
	f.calls = append(f.calls, "upsert")
	return nil
}

func (f *fakeStore) DeleteForSubject(_ context.Context, _, _ string) error {
	if f.deleteErr != nil {
		return f.deleteErr
	}
	f.deletes++
	f.calls = append(f.calls, "delete")
	return nil
}

type fakeResolver struct {
	perm     identity.Permission
	err      error
	calls    int
	lastRepo string
	lastSubj string
	// onResolve, if set, runs as a side effect of PermissionLevel — used to
	// simulate a purge that bumps the watermark DURING the forge lookup, in the
	// [capture, write] window.
	onResolve func()
}

func (f *fakeResolver) PermissionLevel(_ context.Context, repo, subject string) (identity.Permission, error) {
	f.calls++
	f.lastRepo, f.lastSubj = repo, subject
	if f.onResolve != nil {
		f.onResolve()
	}
	if f.err != nil {
		return identity.PermissionNone, f.err
	}
	return f.perm, nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func newTestMirror(t *testing.T, s *fakeStore, r *fakeResolver) *repoacl.Mirror {
	t.Helper()
	return repoacl.NewMirror(s, r, repoacl.DefaultTTL, testLogger())
}

// A fresh mirrored entry is served without consulting the forge.
func TestRepoACLMirror_FreshEntryIsServedFromMirror(t *testing.T) {
	store := &fakeStore{
		entry: repoacl.Entry{Permission: identity.PermissionWrite, CheckedAt: time.Now().Add(-time.Minute)},
		found: true,
	}
	res := &fakeResolver{perm: identity.PermissionNone}
	m := newTestMirror(t, store, res)

	visible, err := m.Visible(context.Background(), "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Visible error: %v", err)
	}
	if !visible {
		t.Errorf("Visible = false, want true (mirrored write tier)")
	}
	if res.calls != 0 {
		t.Errorf("forge calls = %d, want 0 (fresh hit must not consult the forge)", res.calls)
	}
	if len(store.upserts) != 0 {
		t.Errorf("upserts = %d, want 0 (a hit writes nothing)", len(store.upserts))
	}
}

// Failure mode (d): an entry older than the TTL re-resolves, and the stale
// value is NOT served.
func TestRepoACLMirror_ExpiredEntryReResolvesAndDoesNotServeStale(t *testing.T) {
	store := &fakeStore{
		// A stale GRANT — the dangerous direction. It must not be served.
		entry: repoacl.Entry{Permission: identity.PermissionAdmin, CheckedAt: time.Now().Add(-repoacl.DefaultTTL - time.Minute)},
		found: true,
	}
	res := &fakeResolver{perm: identity.PermissionNone}
	m := newTestMirror(t, store, res)

	visible, err := m.Visible(context.Background(), "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Visible error: %v", err)
	}
	if visible {
		t.Errorf("Visible = true, want false (stale admin grant must not be served after TTL)")
	}
	if res.calls != 1 {
		t.Errorf("forge calls = %d, want 1 (expired entry must re-resolve)", res.calls)
	}
	if len(store.upserts) != 1 || store.upserts[0] != identity.PermissionNone {
		t.Errorf("upserts = %v, want [none] (the re-resolved answer is memoized)", store.upserts)
	}
}

// A checked_at AHEAD of the application clock (database/app forward clock
// skew) must NOT be treated as fresh. Age is negative there, and a bare
// `age >= ttl` test would serve the stale row until that future instant plus
// the TTL — extending stale-allow past the bound DefaultTTL documents, by
// exactly the skew. The stale GRANT is the dangerous direction, so that is
// what this drives.
func TestRepoACLMirror_FutureCheckedAtIsExpired(t *testing.T) {
	for _, tc := range []struct {
		name string
		skew time.Duration
	}{
		{"modest forward skew", time.Minute},
		{"clock jump past the TTL", 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{
				entry: repoacl.Entry{Permission: identity.PermissionAdmin,
					CheckedAt: time.Now().Add(tc.skew)},
				found: true,
			}
			res := &fakeResolver{perm: identity.PermissionNone}
			m := newTestMirror(t, store, res)

			visible, err := m.Visible(context.Background(), "github", "octocat", "acme/app")
			if err != nil {
				t.Fatalf("Visible error: %v", err)
			}
			if visible {
				t.Errorf("Visible = true, want false (a future-stamped grant must not be served)")
			}
			if res.calls != 1 {
				t.Errorf("forge calls = %d, want 1 (a future checked_at must re-resolve)", res.calls)
			}
		})
	}
}

// Failure mode (a): a generic forge error denies the repo for this request,
// writes NOTHING, and does NOT error the request (Visible returns nil error so
// the caller proceeds with a shortened page).
func TestRepoACLMirror_ForgeErrorDeniesWithoutCaching(t *testing.T) {
	store := &fakeStore{}
	res := &fakeResolver{err: errors.New("forge: 502 bad gateway")}
	m := newTestMirror(t, store, res)

	visible, err := m.Visible(context.Background(), "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Visible error = %v, want nil (a forge fault must not fail the request)", err)
	}
	if visible {
		t.Errorf("Visible = true, want false (unknown permission is not visible)")
	}
	if len(store.upserts) != 0 {
		t.Errorf("upserts = %v, want none (a transient forge fault must never be memoized)", store.upserts)
	}

	// Permission() surfaces the classification for callers that need it.
	if _, permErr := m.Permission(context.Background(), "github", "octocat", "acme/app"); !errors.Is(permErr, repoacl.ErrForgeUnavailable) {
		t.Errorf("Permission error = %v, want ErrForgeUnavailable", permErr)
	}
}

// Failure mode (b): identity.ErrRateLimited takes the SAME path as any other
// forge error — denied, nothing written — and stays distinguishable from a
// hard deny via errors.Is on the wrapped chain.
func TestRepoACLMirror_RateLimitedDeniesWithoutCaching(t *testing.T) {
	store := &fakeStore{}
	res := &fakeResolver{err: identity.ErrRateLimited}
	m := newTestMirror(t, store, res)

	visible, err := m.Visible(context.Background(), "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Visible error = %v, want nil (a rate limit must not fail the whole page)", err)
	}
	if visible {
		t.Errorf("Visible = true, want false")
	}
	if len(store.upserts) != 0 {
		t.Errorf("upserts = %v, want none (a rate limit must never be memoized as a deny)", store.upserts)
	}

	_, permErr := m.Permission(context.Background(), "github", "octocat", "acme/app")
	if !errors.Is(permErr, repoacl.ErrForgeUnavailable) {
		t.Errorf("Permission error = %v, want ErrForgeUnavailable", permErr)
	}
	if !errors.Is(permErr, identity.ErrRateLimited) {
		t.Errorf("Permission error = %v, want the identity.ErrRateLimited cause preserved", permErr)
	}
	if errors.Is(permErr, repoacl.ErrStoreUnavailable) {
		t.Errorf("Permission error = %v, must NOT classify as a store fault", permErr)
	}
}

// The forge-fault WARN is BINDING observability, not decoration: it is the
// only signal that distinguishes a silently shortened page caused by a forge
// outage from one caused by a genuine denial. Weakening or dropping it would
// otherwise leave every behavioral test above green, so capture the logger and
// assert the level, the repo, the reason, and the rate-limit discriminator.
func TestRepoACLMirror_ForgeFaultLogsWarnWithRepoAndReason(t *testing.T) {
	for _, tc := range []struct {
		name            string
		forgeErr        error
		wantReason      string
		wantRateLimited string
	}{
		{"generic forge error", errors.New("forge: 502 bad gateway"),
			"502 bad gateway", "rate_limited=false"},
		{"rate limited", identity.ErrRateLimited,
			identity.ErrRateLimited.Error(), "rate_limited=true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
			m := repoacl.NewMirror(&fakeStore{}, &fakeResolver{err: tc.forgeErr},
				repoacl.DefaultTTL, logger)

			if _, err := m.Visible(context.Background(), "github", "octocat", "acme/app"); err != nil {
				t.Fatalf("Visible error: %v", err)
			}
			out := buf.String()
			for _, want := range []string{
				"level=WARN", "acme/app", "provider=github",
				tc.wantReason, tc.wantRateLimited,
			} {
				if !strings.Contains(out, want) {
					t.Errorf("forge-fault log missing %q; got:\n%s", want, out)
				}
			}
		})
	}
}

// Failure mode (c): a legitimate PermissionNone IS cached, and denies.
func TestRepoACLMirror_PermissionNoneIsCachedAndDenied(t *testing.T) {
	store := &fakeStore{}
	res := &fakeResolver{perm: identity.PermissionNone}
	m := newTestMirror(t, store, res)

	visible, err := m.Visible(context.Background(), "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Visible error: %v", err)
	}
	if visible {
		t.Errorf("Visible = true, want false (PermissionNone)")
	}
	if len(store.upserts) != 1 || store.upserts[0] != identity.PermissionNone {
		t.Errorf("upserts = %v, want [none] (a legitimate deny is worth memoizing)", store.upserts)
	}
}

// Failure mode (h): a store READ error is a different class — the filter
// cannot function, so it surfaces as an error the caller turns into a 503, and
// is never silently absorbed into a deny.
func TestRepoACLMirror_StoreReadErrorSurfaces(t *testing.T) {
	store := &fakeStore{getErr: errors.New("db: connection refused")}
	res := &fakeResolver{perm: identity.PermissionAdmin}
	m := newTestMirror(t, store, res)

	visible, err := m.Visible(context.Background(), "github", "octocat", "acme/app")
	if err == nil {
		t.Fatalf("Visible error = nil, want a store error (must not silently allow or silently shorten)")
	}
	if !errors.Is(err, repoacl.ErrStoreUnavailable) {
		t.Errorf("error = %v, want ErrStoreUnavailable", err)
	}
	if errors.Is(err, repoacl.ErrForgeUnavailable) {
		t.Errorf("error = %v, must NOT classify as a forge fault", err)
	}
	if visible {
		t.Errorf("Visible = true, want false on a store fault")
	}
	if res.calls != 0 {
		t.Errorf("forge calls = %d, want 0 (a broken store short-circuits before the forge)", res.calls)
	}
}

// A store WRITE error is likewise a store fault: the mirror could ask the forge
// but cannot memoize, so the caller 503s rather than serving an unpersisted
// answer that would re-hammer the forge on every subsequent repo.
func TestRepoACLMirror_StoreUpsertErrorSurfaces(t *testing.T) {
	store := &fakeStore{upsertErr: errors.New("db: read-only transaction")}
	res := &fakeResolver{perm: identity.PermissionWrite}
	m := newTestMirror(t, store, res)

	_, err := m.Visible(context.Background(), "github", "octocat", "acme/app")
	if !errors.Is(err, repoacl.ErrStoreUnavailable) {
		t.Fatalf("error = %v, want ErrStoreUnavailable", err)
	}
}

// The read-tier boundary, plus the deny-by-default on an unrecognized tier a
// future forge (or a stale row) could leave behind.
func TestRepoACLMirror_VisibleTierBoundary(t *testing.T) {
	cases := []struct {
		perm identity.Permission
		want bool
	}{
		{identity.PermissionNone, false},
		{identity.PermissionRead, true},
		{identity.PermissionTriage, true},
		{identity.PermissionWrite, true},
		{identity.PermissionMaintain, true},
		{identity.PermissionAdmin, true},
		{identity.Permission(""), false},
		{identity.Permission("guest"), false},
	}
	for _, tc := range cases {
		store := &fakeStore{
			entry: repoacl.Entry{Permission: tc.perm, CheckedAt: time.Now()},
			found: true,
		}
		m := newTestMirror(t, store, &fakeResolver{})
		got, err := m.Visible(context.Background(), "github", "octocat", "acme/app")
		if err != nil {
			t.Fatalf("Visible(%q) error: %v", tc.perm, err)
		}
		if got != tc.want {
			t.Errorf("Visible(%q) = %v, want %v", tc.perm, got, tc.want)
		}
	}
}

// A nil / unwired mirror reports ErrNotConfigured on every method — never a
// bare false, which a caller could mistake for a deny-all.
func TestRepoACLMirror_NotConfigured(t *testing.T) {
	var nilMirror *repoacl.Mirror
	unwiredStore := repoacl.NewMirror(nil, &fakeResolver{}, 0, testLogger())
	unwiredResolver := repoacl.NewMirror(&fakeStore{}, nil, 0, testLogger())

	for name, m := range map[string]*repoacl.Mirror{
		"nil":              nilMirror,
		"no store":         unwiredStore,
		"no identity prov": unwiredResolver,
	} {
		visible, err := m.Visible(context.Background(), "github", "octocat", "acme/app")
		if !errors.Is(err, repoacl.ErrNotConfigured) {
			t.Errorf("%s: Visible error = %v, want ErrNotConfigured", name, err)
		}
		if visible {
			t.Errorf("%s: Visible = true, want false", name)
		}
		if err := m.InvalidateSubject(context.Background(), "github", "octocat"); !errors.Is(err, repoacl.ErrNotConfigured) {
			t.Errorf("%s: InvalidateSubject error = %v, want ErrNotConfigured", name, err)
		}
	}
}

// Defensive: an unkeyable lookup can never be a grant, and never reaches the
// forge or the store.
func TestRepoACLMirror_EmptyKeyDenies(t *testing.T) {
	cases := [][3]string{{"", "octocat", "acme/app"}, {"github", "", "acme/app"}, {"github", "octocat", ""}}
	for _, k := range cases {
		store := &fakeStore{}
		res := &fakeResolver{perm: identity.PermissionAdmin}
		m := newTestMirror(t, store, res)
		visible, err := m.Visible(context.Background(), k[0], k[1], k[2])
		if err == nil {
			t.Errorf("Visible%v error = nil, want an argument error", k)
		}
		if visible {
			t.Errorf("Visible%v = true, want false", k)
		}
		if store.getCalls != 0 || res.calls != 0 {
			t.Errorf("Visible%v touched store/forge (get=%d forge=%d), want neither", k, store.getCalls, res.calls)
		}
	}
}

func TestRepoACLMirror_InvalidateSubject(t *testing.T) {
	store := &fakeStore{}
	m := newTestMirror(t, store, &fakeResolver{})
	if err := m.InvalidateSubject(context.Background(), "github", "octocat"); err != nil {
		t.Fatalf("InvalidateSubject error: %v", err)
	}
	if store.deletes != 1 {
		t.Errorf("deletes = %d, want 1", store.deletes)
	}

	// An empty key is a no-op, not a full-table purge.
	if err := m.InvalidateSubject(context.Background(), "github", ""); err != nil {
		t.Errorf("InvalidateSubject(empty subject) error = %v, want nil", err)
	}
	if store.deletes != 1 {
		t.Errorf("deletes = %d after empty-subject purge, want 1 (must not purge)", store.deletes)
	}

	// A purge failure is classified as a store fault so the OAuth callback can
	// log-and-continue on it deliberately (non-fatal to sign-in) rather than
	// mistaking it for a forge fault.
	failing := repoacl.NewMirror(&fakeStore{deleteErr: errors.New("db: down")}, &fakeResolver{}, 0, testLogger())
	err := failing.InvalidateSubject(context.Background(), "github", "octocat")
	if !errors.Is(err, repoacl.ErrStoreUnavailable) {
		t.Errorf("purge error = %v, want ErrStoreUnavailable", err)
	}
}

// Failure mode (m4): InvalidateSubject must BUMP the watermark STRICTLY BEFORE
// deleting the subject's rows — the security-critical ordering (#2116). Asserted
// via the ordered call log, not counts: a swapped order would keep both counts
// at 1 yet reopen the race the bump-before-delete discipline closes.
func TestRepoACLMirror_InvalidateSubjectBumpsBeforeDelete(t *testing.T) {
	store := &fakeStore{}
	m := newTestMirror(t, store, &fakeResolver{})
	if err := m.InvalidateSubject(context.Background(), "github", "octocat"); err != nil {
		t.Fatalf("InvalidateSubject error: %v", err)
	}
	if len(store.calls) != 2 || store.calls[0] != "bump" || store.calls[1] != "delete" {
		t.Fatalf("call order = %v, want [bump delete] (bump strictly before delete)", store.calls)
	}
	if store.gens[store.key("github", "octocat")] != 1 {
		t.Errorf("generation = %d after purge, want 1 (bump raised it)", store.gens[store.key("github", "octocat")])
	}
}

// A BumpPurgeWatermark failure surfaces as a store fault (non-fatal-to-sign-in,
// like the delete failure) AND short-circuits before the delete — the bump is
// the security-critical half, so a failed bump must not silently proceed to a
// delete that reopens the window without raising the watermark.
func TestRepoACLMirror_InvalidateSubjectBumpFailureSurfaces(t *testing.T) {
	store := &fakeStore{bumpErr: errors.New("db: down")}
	m := repoacl.NewMirror(store, &fakeResolver{}, 0, testLogger())
	err := m.InvalidateSubject(context.Background(), "github", "octocat")
	if !errors.Is(err, repoacl.ErrStoreUnavailable) {
		t.Errorf("bump error = %v, want ErrStoreUnavailable", err)
	}
	if store.deletes != 0 {
		t.Errorf("deletes = %d, want 0 (a failed bump must not proceed to delete)", store.deletes)
	}
}

// Failure mode (a): on a MISS, Permission calls EnsurePurgeGeneration BEFORE the
// resolver and threads the captured generation into Upsert. Asserted by both the
// ordered call log (ensure before upsert) and the value threaded through.
func TestRepoACLMirror_PermissionCapturesGenerationBeforeResolve(t *testing.T) {
	store := &fakeStore{gens: map[string]int64{}}
	// Pre-set the subject's generation to 3 so the captured value is
	// distinguishable from the zero value.
	store.gens[store.key("github", "octocat")] = 3
	res := &fakeResolver{perm: identity.PermissionWrite}
	m := newTestMirror(t, store, res)

	perm, err := m.Permission(context.Background(), "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if perm != identity.PermissionWrite {
		t.Errorf("perm = %q, want write", perm)
	}
	if store.ensureCalls != 1 {
		t.Errorf("ensure calls = %d, want 1", store.ensureCalls)
	}
	// EnsurePurgeGeneration must precede the forge resolve, which precedes the
	// upsert. The resolver has no entry in the store call log, so assert
	// ensure-before-upsert directly.
	if len(store.calls) != 2 || store.calls[0] != "ensure" || store.calls[1] != "upsert" {
		t.Fatalf("call order = %v, want [ensure upsert]", store.calls)
	}
	if len(store.capturedGens) != 1 || store.capturedGens[0] != 3 {
		t.Errorf("captured generations = %v, want [3] (the generation read at resolution start)", store.capturedGens)
	}
}

// Failure mode (m7): a guarded-rejection Upsert (a purge bumped the generation
// after capture) still returns the resolved perm to THIS caller (perm, nil) and
// memoizes NOTHING — correct for this request; the next request re-resolves.
func TestRepoACLMirror_GuardedRejectionStillReturnsPerm(t *testing.T) {
	store := &fakeStore{gens: map[string]int64{}}
	res := &fakeResolver{perm: identity.PermissionAdmin}
	m := newTestMirror(t, store, res)

	// EnsurePurgeGeneration captures generation 0 at resolution start. A purge
	// then bumps the generation to 1 DURING the forge lookup — the [capture,
	// write] window — so the guarded Upsert (capturedGen 0 < live 1) is
	// rejected.
	res.onResolve = func() { store.gens[store.key("github", "octocat")] = 1 }

	perm, err := m.Permission(context.Background(), "github", "octocat", "acme/app")
	if err != nil {
		t.Fatalf("Permission: %v", err)
	}
	if perm != identity.PermissionAdmin {
		t.Errorf("perm = %q, want admin (the caller still gets the resolved answer)", perm)
	}
	if len(store.upserts) != 0 {
		t.Errorf("upserts = %v, want none (a guarded rejection memoizes nothing)", store.upserts)
	}
	if len(store.capturedGens) != 1 || store.capturedGens[0] != 0 {
		t.Errorf("captured generations = %v, want [0] (captured before the purge bumped to 1)", store.capturedGens)
	}
}

// Failure mode (m5): an EnsurePurgeGeneration store error surfaces
// ErrStoreUnavailable from Permission — the filter cannot function, and it must
// not silently proceed to write with a bogus generation.
func TestRepoACLMirror_EnsureGenerationErrorSurfaces(t *testing.T) {
	store := &fakeStore{ensureErr: errors.New("db: connection refused")}
	res := &fakeResolver{perm: identity.PermissionWrite}
	m := newTestMirror(t, store, res)

	_, err := m.Permission(context.Background(), "github", "octocat", "acme/app")
	if !errors.Is(err, repoacl.ErrStoreUnavailable) {
		t.Fatalf("Permission error = %v, want ErrStoreUnavailable", err)
	}
	if errors.Is(err, repoacl.ErrForgeUnavailable) {
		t.Errorf("error = %v, must NOT classify as a forge fault", err)
	}
	if res.calls != 0 {
		t.Errorf("forge calls = %d, want 0 (a failed generation capture short-circuits before the forge)", res.calls)
	}
}

// A non-positive TTL falls back to DefaultTTL — a zero TTL would re-resolve
// every repo of every page against the forge.
func TestRepoACLMirror_TTLFallsBackToDefault(t *testing.T) {
	for _, ttl := range []time.Duration{0, -time.Second} {
		if got := repoacl.NewMirror(&fakeStore{}, &fakeResolver{}, ttl, testLogger()).TTL(); got != repoacl.DefaultTTL {
			t.Errorf("NewMirror(ttl=%v).TTL() = %v, want DefaultTTL %v", ttl, got, repoacl.DefaultTTL)
		}
	}
	if got := repoacl.NewMirror(&fakeStore{}, &fakeResolver{}, time.Minute, testLogger()).TTL(); got != time.Minute {
		t.Errorf("explicit TTL = %v, want 1m", got)
	}
	if got := (*repoacl.Mirror)(nil).TTL(); got != 0 {
		t.Errorf("nil mirror TTL = %v, want 0", got)
	}
}

// The mirror keys on the forge-neutral member ref and passes THAT to the forge
// — the identity subject's "<provider>:" prefix is stripped generically, so a
// gitlab subject resolves exactly as a github one does.
func TestRepoACLSubjectRef_StripsProviderPrefixGenerically(t *testing.T) {
	cases := []struct{ provider, subject, want string }{
		{"github", "github:octocat", "octocat"},
		{"gitlab", "gitlab:octocat", "octocat"},
		{"gitlab", "github:octocat", "github:octocat"}, // wrong prefix: verbatim, never silently accepted as a match
		{"github", "octocat", "octocat"},               // unprefixed: verbatim
		{"", "github:octocat", "github:octocat"},
		{"github", "github:group:sub", "group:sub"}, // only the FIRST prefix is stripped
	}
	for _, tc := range cases {
		if got := repoacl.SubjectRef(tc.provider, tc.subject); got != tc.want {
			t.Errorf("SubjectRef(%q, %q) = %q, want %q", tc.provider, tc.subject, got, tc.want)
		}
	}

	// End-to-end through the mirror: a gitlab identity reaches the forge under
	// its stripped ref.
	store := &fakeStore{}
	res := &fakeResolver{perm: identity.PermissionRead}
	m := repoacl.NewMirror(store, res, repoacl.DefaultTTL, testLogger())
	subject := repoacl.SubjectRef("gitlab", "gitlab:octocat")
	if _, err := m.Visible(context.Background(), "gitlab", subject, "acme/app"); err != nil {
		t.Fatalf("Visible error: %v", err)
	}
	if res.lastSubj != "octocat" {
		t.Errorf("forge subject = %q, want %q", res.lastSubj, "octocat")
	}
	if store.lastKey != [3]string{"gitlab", "octocat", "acme/app"} {
		t.Errorf("store key = %v, want [gitlab octocat acme/app]", store.lastKey)
	}
}

// A nil logger must not panic on the mandated WARN — NewMirror substitutes
// slog.Default().
func TestRepoACLMirror_NilLoggerDoesNotPanic(t *testing.T) {
	m := repoacl.NewMirror(&fakeStore{}, &fakeResolver{err: identity.ErrRateLimited}, 0, nil)
	if _, err := m.Visible(context.Background(), "github", "octocat", "acme/app"); err != nil {
		t.Fatalf("Visible error: %v", err)
	}
}
