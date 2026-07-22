package routing_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/directory/internal/routing"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
)

var fixedNow = time.Unix(1_800_000_000, 0).UTC()

// fakeStore is an in-memory routing.Store with injectable failures, so
// every store-error branch in the router has a test.
type fakeStore struct {
	assignments map[string]routing.Assignment
	states      map[string]routing.InstallState
	now         func() time.Time

	assignErr  error
	lookupErr  error
	putErr     error
	consumeErr error

	assignCalls int
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		assignments: map[string]routing.Assignment{},
		states:      map[string]routing.InstallState{},
		now:         func() time.Time { return fixedNow },
	}
}

func key(provider, accountKey string) string { return provider + "/" + accountKey }

func (f *fakeStore) AssignRegion(_ context.Context, provider, accountKey, region string) (routing.Assignment, error) {
	f.assignCalls++
	if f.assignErr != nil {
		return routing.Assignment{}, f.assignErr
	}
	k := key(provider, accountKey)
	if existing, ok := f.assignments[k]; ok {
		return existing, nil // first-write-wins
	}
	a := routing.Assignment{Provider: provider, AccountKey: accountKey, HomeRegion: region}
	f.assignments[k] = a
	return a, nil
}

func (f *fakeStore) LookupRegion(_ context.Context, provider, accountKey string) (routing.Assignment, error) {
	if f.lookupErr != nil {
		return routing.Assignment{}, f.lookupErr
	}
	a, ok := f.assignments[key(provider, accountKey)]
	if !ok {
		return routing.Assignment{}, fmt.Errorf("%w: %s", routing.ErrNotFound, key(provider, accountKey))
	}
	return a, nil
}

func (f *fakeStore) PutInstallState(_ context.Context, st routing.InstallState) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.states[st.Nonce] = st
	return nil
}

func (f *fakeStore) ConsumeInstallState(_ context.Context, nonce string) (routing.InstallState, error) {
	if f.consumeErr != nil {
		return routing.InstallState{}, f.consumeErr
	}
	st, ok := f.states[nonce]
	if !ok {
		return routing.InstallState{}, fmt.Errorf("%w: %s", routing.ErrNotFound, nonce)
	}
	delete(f.states, nonce) // single-use
	if f.now().After(st.ExpiresAt) {
		return routing.InstallState{}, fmt.Errorf("%w: %s", routing.ErrExpired, nonce)
	}
	return st, nil
}

// counterNonce yields deterministic nonces so assertions can name them.
func counterNonce() func() (string, error) {
	n := 0
	return func() (string, error) {
		n++
		return fmt.Sprintf("nonce-%d", n), nil
	}
}

func testConfig(t *testing.T, kv map[string]string) routing.Config {
	t.Helper()
	cfg, err := routing.LoadConfig(env(kv))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func newRouter(t *testing.T, st routing.Store, opts ...routing.Option) http.Handler {
	t.Helper()
	base := []routing.Option{
		routing.WithClock(func() time.Time { return fixedNow }),
		routing.WithNonceSource(counterNonce()),
	}
	return routing.New(st, testConfig(t, validEnv()), append(base, opts...)...).Handler()
}

func get(t *testing.T, h http.Handler, target string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// ---- happy paths -------------------------------------------------------

// The core acceptance criterion: the onboarding entry RECORDS the
// (provider, account_key → home_region) row AND redirects, and the
// redirect PRESERVES the original path and every original query
// parameter alongside the signed handoff pin.
func TestOnboardingStartRecordsRowAndRedirectsPreservingRequest(t *testing.T) {
	st := newFakeStore()
	h := newRouter(t, st)

	rec := get(t, h, routing.PathOnboardingStart+
		"?provider=github&account_key=kuhlman-labs&region=eu"+
		"&code=abc123&state=oauth-state&installation_id=42&setup_action=install")

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d want %d (body %q)", rec.Code, http.StatusFound, rec.Body.String())
	}

	// The row is recorded.
	got, ok := st.assignments[key("github", "kuhlman-labs")]
	if !ok {
		t.Fatal("no assignment recorded")
	}
	if got.HomeRegion != "eu" {
		t.Fatalf("recorded home_region: got %q want eu", got.HomeRegion)
	}

	loc := mustLocation(t, rec)
	if loc.Host != "eu.app.fishhawk.test" {
		t.Fatalf("redirect host: got %q want eu.app.fishhawk.test", loc.Host)
	}
	// Path preserved (NOT a bare cell_base_url).
	if loc.Path != routing.PathOnboardingStart {
		t.Fatalf("redirect path: got %q want %q", loc.Path, routing.PathOnboardingStart)
	}
	// Every original parameter survives.
	q := loc.Query()
	for k, want := range map[string]string{
		"provider": "github", "account_key": "kuhlman-labs", "region": "eu",
		"code": "abc123", "state": "oauth-state", "installation_id": "42", "setup_action": "install",
	} {
		if got := q.Get(k); got != want {
			t.Fatalf("query %q: got %q want %q", k, got, want)
		}
	}
	// And the signed pin verifies with the shared secret.
	pin, err := handoff.Verify(q, "shared-secret", fixedNow)
	if err != nil {
		t.Fatalf("Verify handoff on redirect: %v", err)
	}
	if pin.Provider != "github" || pin.AccountKey != "kuhlman-labs" || pin.HomeRegion != "eu" {
		t.Fatalf("pin payload: %+v", pin)
	}
	if !pin.ExpiresAt.After(fixedNow) {
		t.Fatalf("pin expires_at %s is not in the future of %s", pin.ExpiresAt, fixedNow)
	}

	// An install-state nonce was minted for the callback leg.
	if len(st.states) != 1 {
		t.Fatalf("install states: got %d want 1", len(st.states))
	}
}

// First-write-wins at the directory: re-onboarding with a different
// region reads back the ORIGINAL region and redirects there.
func TestOnboardingStartIsFirstWriteWins(t *testing.T) {
	st := newFakeStore()
	h := newRouter(t, st)

	get(t, h, routing.PathOnboardingStart+"?provider=github&account_key=acme&region=eu")
	rec := get(t, h, routing.PathOnboardingStart+"?provider=github&account_key=acme&region=us")

	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d", rec.Code)
	}
	if got := st.assignments[key("github", "acme")].HomeRegion; got != "eu" {
		t.Fatalf("region moved: got %q want eu", got)
	}
	loc := mustLocation(t, rec)
	if loc.Host != "eu.app.fishhawk.test" {
		t.Fatalf("redirect host: got %q want eu.app.fishhawk.test", loc.Host)
	}
}

func TestLoginRedirectsToRecordedRegionPreservingQuery(t *testing.T) {
	st := newFakeStore()
	st.assignments[key("github", "kuhlman-labs")] = routing.Assignment{
		Provider: "github", AccountKey: "kuhlman-labs", HomeRegion: "au",
	}
	h := newRouter(t, st)

	rec := get(t, h, routing.PathLogin+"?provider=github&account_key=kuhlman-labs&redirect_uri=%2Fruns%3Ffoo%3Dbar")
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d body %q", rec.Code, rec.Body.String())
	}
	loc := mustLocation(t, rec)
	if loc.Host != "au.app.fishhawk.test" || loc.Path != routing.PathLogin {
		t.Fatalf("location: got %s", loc)
	}
	if got, want := loc.Query().Get("redirect_uri"), "/runs?foo=bar"; got != want {
		t.Fatalf("redirect_uri: got %q want %q (nested query must survive escaping)", got, want)
	}
	if _, err := handoff.Verify(loc.Query(), "shared-secret", fixedNow); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestInstallCallbackConsumesStateAndRedirects(t *testing.T) {
	st := newFakeStore()
	st.states["cb-nonce"] = routing.InstallState{
		Nonce: "cb-nonce", Provider: "github", AccountKey: "acme",
		HomeRegion: "us", ExpiresAt: fixedNow.Add(time.Minute),
	}
	h := newRouter(t, st)

	rec := get(t, h, routing.PathInstallCallback+"?state=cb-nonce&installation_id=99&setup_action=install")
	if rec.Code != http.StatusFound {
		t.Fatalf("status: got %d body %q", rec.Code, rec.Body.String())
	}
	loc := mustLocation(t, rec)
	if loc.Host != "us.app.fishhawk.test" {
		t.Fatalf("host: got %q", loc.Host)
	}
	if got := loc.Query().Get("installation_id"); got != "99" {
		t.Fatalf("installation_id: got %q want 99", got)
	}
	if _, ok := st.states["cb-nonce"]; ok {
		t.Fatal("install state was not consumed")
	}
}

// A cell base URL carrying a path prefix must be joined, not clobbered.
func TestRedirectJoinsCellBaseURLPathPrefix(t *testing.T) {
	kv := validEnv()
	kv[routing.EnvCellBaseURLs] = "us=https://cells.fishhawk.test/us,eu=https://cells.fishhawk.test/eu,au=https://cells.fishhawk.test/au"
	st := newFakeStore()
	h := routing.New(st, testConfig(t, kv),
		routing.WithClock(func() time.Time { return fixedNow }),
		routing.WithNonceSource(counterNonce()),
	).Handler()

	rec := get(t, h, routing.PathOnboardingStart+"?provider=github&account_key=acme&region=eu")
	loc := mustLocation(t, rec)
	if got, want := loc.Path, "/eu"+routing.PathOnboardingStart; got != want {
		t.Fatalf("path: got %q want %q", got, want)
	}
}

// A caller-supplied fh_* parameter must never survive next to the
// directory's own signed pin.
func TestRedirectOverridesCallerSuppliedHandoffParams(t *testing.T) {
	st := newFakeStore()
	h := newRouter(t, st)

	rec := get(t, h, routing.PathOnboardingStart+
		"?provider=github&account_key=acme&region=eu&"+handoff.ParamHomeRegion+"=us&"+handoff.ParamSignature+"=deadbeef")
	loc := mustLocation(t, rec)
	q := loc.Query()
	if got := q[handoff.ParamHomeRegion]; len(got) != 1 || got[0] != "eu" {
		t.Fatalf("%s: got %v want [eu]", handoff.ParamHomeRegion, got)
	}
	if got := q[handoff.ParamSignature]; len(got) != 1 || got[0] == "deadbeef" {
		t.Fatalf("%s: got %v; caller-supplied signature survived", handoff.ParamSignature, got)
	}
	if _, err := handoff.Verify(q, "shared-secret", fixedNow); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestHealthz(t *testing.T) {
	rec := get(t, newRouter(t, newFakeStore()), routing.PathHealthz)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "us,eu,au") {
		t.Fatalf("body: %q", rec.Body.String())
	}
}

// The routed surfaces are GET-only by construction, so the "302 rewrites
// POST to GET" hazard cannot arise.
func TestRoutedSurfacesAreGetOnly(t *testing.T) {
	h := newRouter(t, newFakeStore())
	for _, path := range []string{routing.PathOnboardingStart, routing.PathInstallCallback, routing.PathLogin} {
		t.Run(path, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path+"?provider=github&account_key=acme&region=eu", nil))
			if rec.Code != http.StatusMethodNotAllowed {
				t.Fatalf("POST status: got %d want %d", rec.Code, http.StatusMethodNotAllowed)
			}
		})
	}
}

// ---- fail-closed branches ---------------------------------------------

func TestOnboardingStartRejectsMissingParams(t *testing.T) {
	for name, target := range map[string]string{
		"no provider":    routing.PathOnboardingStart + "?account_key=acme&region=eu",
		"no account_key": routing.PathOnboardingStart + "?provider=github&region=eu",
		"no region":      routing.PathOnboardingStart + "?provider=github&account_key=acme",
	} {
		t.Run(name, func(t *testing.T) {
			st := newFakeStore()
			rec := get(t, newRouter(t, st), target)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400", rec.Code)
			}
			if st.assignCalls != 0 {
				t.Fatal("store was written for an invalid request")
			}
		})
	}
}

// An unsupported region must be rejected BEFORE any write.
func TestOnboardingStartRejectsUnsupportedRegionWithoutWriting(t *testing.T) {
	st := newFakeStore()
	rec := get(t, newRouter(t, st), routing.PathOnboardingStart+"?provider=github&account_key=acme&region=jp")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
	if st.assignCalls != 0 {
		t.Fatalf("unsupported region was recorded (%d assign calls)", st.assignCalls)
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("unsupported region produced a redirect")
	}
}

func TestOnboardingStartStoreFailures(t *testing.T) {
	boom := errors.New("boom")
	cases := map[string]func(*fakeStore){
		"assign fails":            func(f *fakeStore) { f.assignErr = boom },
		"put install state fails": func(f *fakeStore) { f.putErr = boom },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			st := newFakeStore()
			mutate(st)
			rec := get(t, newRouter(t, st), routing.PathOnboardingStart+"?provider=github&account_key=acme&region=eu")
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status: got %d want 500", rec.Code)
			}
			if rec.Header().Get("Location") != "" {
				t.Fatal("a store failure produced a redirect")
			}
		})
	}
}

func TestOnboardingStartNonceFailure(t *testing.T) {
	st := newFakeStore()
	h := routing.New(st, testConfig(t, validEnv()),
		routing.WithClock(func() time.Time { return fixedNow }),
		routing.WithNonceSource(func() (string, error) { return "", errors.New("no entropy") }),
	).Handler()
	rec := get(t, h, routing.PathOnboardingStart+"?provider=github&account_key=acme&region=eu")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func TestLoginRejectsMissingParams(t *testing.T) {
	for name, target := range map[string]string{
		"no provider":    routing.PathLogin + "?account_key=acme",
		"no account_key": routing.PathLogin + "?provider=github",
	} {
		t.Run(name, func(t *testing.T) {
			rec := get(t, newRouter(t, newFakeStore()), target)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status: got %d want 400", rec.Code)
			}
		})
	}
}

func TestLoginUnassignedAccountFailsClosed(t *testing.T) {
	rec := get(t, newRouter(t, newFakeStore()), routing.PathLogin+"?provider=github&account_key=unknown")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status: got %d want 404", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("an unassigned account produced a redirect")
	}
}

func TestLoginStoreFailure(t *testing.T) {
	st := newFakeStore()
	st.lookupErr = errors.New("boom")
	rec := get(t, newRouter(t, st), routing.PathLogin+"?provider=github&account_key=acme")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

// The plan's named failure mode (a): an account whose recorded region has
// NO configured cell gets an explicit error, never a fall-through to some
// other region's cell.
func TestRedirectFailsClosedWhenRegionHasNoConfiguredCell(t *testing.T) {
	st := newFakeStore()
	st.assignments[key("github", "acme")] = routing.Assignment{
		Provider: "github", AccountKey: "acme", HomeRegion: "jp",
	}
	rec := get(t, newRouter(t, st), routing.PathLogin+"?provider=github&account_key=acme")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d want 503", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("fell through to a cell: Location %q", loc)
	}
	if !strings.Contains(rec.Body.String(), "jp") {
		t.Fatalf("error does not name the unroutable region: %q", rec.Body.String())
	}
}

func TestInstallCallbackRejectsMissingState(t *testing.T) {
	rec := get(t, newRouter(t, newFakeStore()), routing.PathInstallCallback+"?installation_id=1")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d want 400", rec.Code)
	}
}

func TestInstallCallbackRejectsUnknownState(t *testing.T) {
	rec := get(t, newRouter(t, newFakeStore()), routing.PathInstallCallback+"?state=never-minted")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Fatal("an unknown state produced a redirect")
	}
}

// Single-use: replaying a consumed nonce fails closed.
func TestInstallCallbackRejectsReplayedState(t *testing.T) {
	st := newFakeStore()
	st.states["cb"] = routing.InstallState{
		Nonce: "cb", Provider: "github", AccountKey: "acme",
		HomeRegion: "us", ExpiresAt: fixedNow.Add(time.Minute),
	}
	h := newRouter(t, st)
	if rec := get(t, h, routing.PathInstallCallback+"?state=cb"); rec.Code != http.StatusFound {
		t.Fatalf("first use: got %d want 302", rec.Code)
	}
	rec := get(t, h, routing.PathInstallCallback+"?state=cb")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("replay: got %d want 403", rec.Code)
	}
}

func TestInstallCallbackRejectsExpiredState(t *testing.T) {
	st := newFakeStore()
	st.states["cb"] = routing.InstallState{
		Nonce: "cb", Provider: "github", AccountKey: "acme",
		HomeRegion: "us", ExpiresAt: fixedNow.Add(-time.Second),
	}
	rec := get(t, newRouter(t, st), routing.PathInstallCallback+"?state=cb")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status: got %d want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "expired") {
		t.Fatalf("body: %q", rec.Body.String())
	}
}

func TestInstallCallbackStoreFailure(t *testing.T) {
	st := newFakeStore()
	st.consumeErr = errors.New("boom")
	rec := get(t, newRouter(t, st), routing.PathInstallCallback+"?state=cb")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d want 500", rec.Code)
	}
}

func mustLocation(t *testing.T, rec *httptest.ResponseRecorder) *url.URL {
	t.Helper()
	raw := rec.Header().Get("Location")
	if raw == "" {
		t.Fatalf("no Location header (status %d, body %q)", rec.Code, rec.Body.String())
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse Location %q: %v", raw, err)
	}
	return u
}
