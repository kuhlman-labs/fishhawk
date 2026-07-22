package routing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/directory/internal/store"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
)

const testAdminToken = "operator-token"

// fakeStore is an in-memory RegionStore with the same first-write-wins
// contract as the real one. The router's own behaviour is what these tests
// pin; the SQL guarantee is pinned by the store package's -race concurrency
// test against real Postgres.
type fakeStore struct {
	regions   map[string]string
	assignErr error
	lookupErr error
}

func newFakeStore() *fakeStore { return &fakeStore{regions: map[string]string{}} }

func (f *fakeStore) key(provider, accountKey string) string { return provider + "/" + accountKey }

func (f *fakeStore) AssignRegion(_ context.Context, provider, accountKey, region string) (string, error) {
	if f.assignErr != nil {
		return "", f.assignErr
	}
	if provider == "" || accountKey == "" || region == "" {
		return "", store.ErrInvalidInput
	}
	k := f.key(provider, accountKey)
	if existing, ok := f.regions[k]; ok {
		return existing, nil
	}
	f.regions[k] = region
	return region, nil
}

func (f *fakeStore) Lookup(_ context.Context, provider, accountKey string) (string, error) {
	if f.lookupErr != nil {
		return "", f.lookupErr
	}
	region, ok := f.regions[f.key(provider, accountKey)]
	if !ok {
		return "", fmt.Errorf("%w: %s", store.ErrNotFound, f.key(provider, accountKey))
	}
	return region, nil
}

func testConfig(t *testing.T) Config {
	t.Helper()
	cfg, err := LoadConfig(env(map[string]string{
		EnvRegions:       "us=https://us.cell.example,eu=https://eu.cell.example",
		EnvHandoffSecret: "s3cret",
		EnvAdminToken:    testAdminToken,
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg
}

func newRouter(t *testing.T, cfg Config, s RegionStore) *Router {
	t.Helper()
	rt, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return rt
}

func do(t *testing.T, h http.Handler, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestAssignFirstWriteWins(t *testing.T) {
	rt := newRouter(t, testConfig(t), newFakeStore())

	w := do(t, rt, http.MethodPost, AssignPath, testAdminToken,
		`{"provider":"github","account_key":"acme","region":"us"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", w.Code, w.Body)
	}
	var first assignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if first.HomeRegion != "us" || !first.Assigned {
		t.Fatalf("first assign = %+v, want us/assigned", first)
	}

	// A second caller proposing a different region reads the winner back
	// rather than overwriting it, and is told it did not assign.
	w = do(t, rt, http.MethodPost, AssignPath, testAdminToken,
		`{"provider":"github","account_key":"acme","region":"eu"}`)
	var second assignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &second); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if second.HomeRegion != "us" {
		t.Fatalf("second assign home_region = %q, want us", second.HomeRegion)
	}
	if second.Assigned {
		t.Fatal("second assign reported Assigned=true; the first writer owns the account")
	}
}

func TestAssignRejectsBadRequests(t *testing.T) {
	rt := newRouter(t, testConfig(t), newFakeStore())

	cases := []struct {
		name string
		body string
		want int
		code string
	}{
		{"not json", `{`, http.StatusBadRequest, "invalid_request"},
		{"unknown field", `{"provider":"github","account_key":"a","region":"us","extra":1}`, http.StatusBadRequest, "invalid_request"},
		{"missing provider", `{"account_key":"a","region":"us"}`, http.StatusBadRequest, "invalid_request"},
		{"missing account key", `{"provider":"github","region":"us"}`, http.StatusBadRequest, "invalid_request"},
		{"missing region", `{"provider":"github","account_key":"a"}`, http.StatusBadRequest, "invalid_request"},
		{"unconfigured region", `{"provider":"github","account_key":"a","region":"ap"}`, http.StatusBadRequest, "unknown_region"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, rt, http.MethodPost, AssignPath, testAdminToken, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d (body %s)", w.Code, tc.want, w.Body)
			}
			if got := decodeError(t, w).Code; got != tc.code {
				t.Fatalf("code = %q, want %q", got, tc.code)
			}
		})
	}
}

func TestAssignStoreFailureIsInternalError(t *testing.T) {
	fs := newFakeStore()
	fs.assignErr = errors.New("connection refused")
	rt := newRouter(t, testConfig(t), fs)

	w := do(t, rt, http.MethodPost, AssignPath, testAdminToken,
		`{"provider":"github","account_key":"acme","region":"us"}`)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "connection refused") {
		t.Fatal("response leaked the underlying store error")
	}
}

// --- Authorization on BOTH surfaces (ADR-062 A2.5, condition 4) ---------

func TestBothSurfacesRejectMissingBearer(t *testing.T) {
	fs := newFakeStore()
	fs.regions["github/acme"] = "us"
	rt := newRouter(t, testConfig(t), fs)

	for _, tc := range []struct{ name, method, target, body string }{
		{"assign", http.MethodPost, AssignPath, `{"provider":"github","account_key":"acme","region":"us"}`},
		{"routed", http.MethodGet, DefaultRoutedPath + "?provider=github&account_key=acme", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, rt, tc.method, tc.target, "", tc.body)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
		})
	}
}

func TestBothSurfacesRejectWrongBearer(t *testing.T) {
	fs := newFakeStore()
	fs.regions["github/acme"] = "us"
	rt := newRouter(t, testConfig(t), fs)

	for _, tc := range []struct{ name, method, target, body string }{
		{"assign", http.MethodPost, AssignPath, `{"provider":"github","account_key":"acme","region":"us"}`},
		{"routed", http.MethodGet, DefaultRoutedPath + "?provider=github&account_key=acme", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := do(t, rt, tc.method, tc.target, "not-the-operator-token", tc.body)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", w.Code)
			}
		})
	}
}

// The RIGHT token under the WRONG (or no) scheme must not authenticate: a
// prefix trim leaves a bare `Authorization: <token>` header unchanged and
// would have let it through.
func TestBothSurfacesRequireTheBearerScheme(t *testing.T) {
	fs := newFakeStore()
	fs.regions["github/acme"] = "us"
	rt := newRouter(t, testConfig(t), fs)

	surfaces := []struct{ name, method, target, body string }{
		{"assign", http.MethodPost, AssignPath, `{"provider":"github","account_key":"acme","region":"us"}`},
		{"routed", http.MethodGet, DefaultRoutedPath + "?provider=github&account_key=acme", ""},
	}
	headers := map[string]string{
		"no scheme":      testAdminToken,
		"basic scheme":   "Basic " + testAdminToken,
		"token scheme":   "Token " + testAdminToken,
		"glued":          "Bearer" + testAdminToken,
		"scheme as suff": testAdminToken + " Bearer",
	}
	for _, s := range surfaces {
		for name, header := range headers {
			t.Run(s.name+"/"+name, func(t *testing.T) {
				var r *http.Request
				if s.body == "" {
					r = httptest.NewRequest(s.method, s.target, nil)
				} else {
					r = httptest.NewRequest(s.method, s.target, strings.NewReader(s.body))
				}
				r.Header.Set("Authorization", header)
				w := httptest.NewRecorder()
				rt.ServeHTTP(w, r)

				if w.Code != http.StatusUnauthorized {
					t.Fatalf("Authorization: %q -> status %d, want 401", header, w.Code)
				}
			})
		}
	}

	// Positive control: the same token DOES authenticate under the scheme,
	// case-insensitively (RFC 7235 §2.1).
	for _, header := range []string{"Bearer " + testAdminToken, "bearer " + testAdminToken, "BEARER " + testAdminToken} {
		r := httptest.NewRequest(http.MethodGet, DefaultRoutedPath+"?provider=github&account_key=acme", nil)
		r.Header.Set("Authorization", header)
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, r)
		if w.Code != http.StatusFound {
			t.Fatalf("Authorization: %q -> status %d, want 302", header, w.Code)
		}
	}
}

func TestAssignRefusesWhenAdminTokenUnset(t *testing.T) {
	cfg := testConfig(t)
	cfg.AdminToken = ""
	rt := newRouter(t, cfg, newFakeStore())

	// Not even a request presenting an empty bearer gets through: unset
	// means closed, never open.
	for _, token := range []string{"", "anything"} {
		w := do(t, rt, http.MethodPost, AssignPath, token,
			`{"provider":"github","account_key":"acme","region":"us"}`)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("token %q: status = %d, want 503", token, w.Code)
		}
		if got := decodeError(t, w).Code; got != "credential_unconfigured" {
			t.Fatalf("code = %q", got)
		}
	}
}

func TestRoutedRefusesWhenAdminTokenUnset(t *testing.T) {
	cfg := testConfig(t)
	cfg.AdminToken = ""
	fs := newFakeStore()
	fs.regions["github/acme"] = "us"
	rt := newRouter(t, cfg, fs)

	for _, token := range []string{"", "anything"} {
		w := do(t, rt, http.MethodGet, DefaultRoutedPath+"?provider=github&account_key=acme", token, "")
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("token %q: status = %d, want 503", token, w.Code)
		}
		if loc := w.Header().Get("Location"); loc != "" {
			t.Fatalf("refused request still emitted a Location: %q", loc)
		}
	}
}

// --- The 302 ------------------------------------------------------------

func TestRoutedRedirectPreservesPathAndQuery(t *testing.T) {
	fs := newFakeStore()
	fs.regions["github/acme"] = "eu"
	cfg := testConfig(t)
	rt := newRouter(t, cfg, fs)

	fixedNow := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	rt.Now = func() time.Time { return fixedNow }
	rt.NewNonce = func() (string, error) { return "deadbeef", nil }

	// Deliberately NOT in sorted key order, and with a non-canonical escaping
	// (%7E, which url.Values.Encode would rewrite to a literal "~"): a
	// re-encoding round trip is detectable in both.
	const origQuery = "state=caller%7Eoauth-state&provider=github&install_id=42&account_key=acme"
	target := DefaultRoutedPath + "?" + origQuery
	w := do(t, rt, http.MethodGet, target, testAdminToken, "")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (body %s)", w.Code, w.Body)
	}

	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("Location is unparsable: %v", err)
	}
	// "Verbatim" means the caller's query text survives byte for byte, in its
	// original order, with the handoff appended AFTER it — not merely that
	// each decoded value can still be looked up.
	if !strings.HasPrefix(loc.RawQuery, origQuery+"&") {
		t.Fatalf("redirect query = %q, want it to start with the caller's own query verbatim (%q)", loc.RawQuery, origQuery)
	}
	if loc.Scheme+"://"+loc.Host != "https://eu.cell.example" {
		t.Fatalf("redirect host = %q, want the eu cell", loc.Scheme+"://"+loc.Host)
	}
	if loc.Path != DefaultRoutedPath {
		t.Fatalf("redirect path = %q, want the original %q", loc.Path, DefaultRoutedPath)
	}

	q := loc.Query()
	// The caller's own parameters survive verbatim — including `state`,
	// which is the OAuth correlator and a wholly different role from the
	// handoff's replay-binding nonce.
	for name, want := range map[string]string{
		"provider":    "github",
		"account_key": "acme",
		"state":       "caller~oauth-state",
		"install_id":  "42",
	} {
		if got := q.Get(name); got != want {
			t.Fatalf("query %s = %q, want %q", name, got, want)
		}
	}

	// The appended handoff verifies against the configured secret.
	p, err := handoff.Verify(cfg.HandoffSecret, q, fixedNow)
	if err != nil {
		t.Fatalf("handoff on the emitted Location does not verify: %v", err)
	}
	if p.Provider != "github" || p.AccountKey != "acme" || p.HomeRegion != "eu" {
		t.Fatalf("handoff params = %+v", p)
	}
	if p.Nonce != "deadbeef" {
		t.Fatalf("fh_nonce = %q, want the minted nonce", p.Nonce)
	}
	if want := fixedNow.Add(cfg.HandoffTTL); !p.ExpiresAt.Equal(want) {
		t.Fatalf("fh_expires_at = %s, want %s", p.ExpiresAt, want)
	}
	// The handoff is bound to the TTL, so it is already dead just past it.
	if _, err := handoff.Verify(cfg.HandoffSecret, q, fixedNow.Add(cfg.HandoffTTL+time.Second)); !errors.Is(err, handoff.ErrExpired) {
		t.Fatalf("expired verify = %v, want ErrExpired", err)
	}
}

// A caller cannot smuggle an attacker-chosen handoff through the router:
// inbound fh_* parameters are dropped and replaced, never duplicated.
func TestRoutedDropsInboundHandoffParams(t *testing.T) {
	fs := newFakeStore()
	fs.regions["github/acme"] = "us"
	cfg := testConfig(t)
	rt := newRouter(t, cfg, fs)

	target := DefaultRoutedPath + "?provider=github&account_key=acme" +
		"&" + handoff.ParamRegion + "=eu" +
		"&" + handoff.ParamSignature + "=00" +
		"&" + handoff.ParamAccountKey + "=someone-else"
	w := do(t, rt, http.MethodGet, target, testAdminToken, "")
	if w.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302", w.Code)
	}

	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	q := loc.Query()
	for _, name := range []string{handoff.ParamRegion, handoff.ParamSignature, handoff.ParamAccountKey} {
		if got := len(q[name]); got != 1 {
			t.Fatalf("%s appears %d times, want exactly the router's own value", name, got)
		}
	}
	p, err := handoff.Verify(cfg.HandoffSecret, q, time.Now())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if p.HomeRegion != "us" || p.AccountKey != "acme" {
		t.Fatalf("smuggled values survived: %+v", p)
	}
}

func TestRoutedRequiresAccountIdentity(t *testing.T) {
	rt := newRouter(t, testConfig(t), newFakeStore())

	// Condition (2): a routed surface must carry EXPLICIT identity. A
	// missing provider is refused, never defaulted — guessing it would
	// route an account's traffic on a coin flip.
	for _, target := range []string{
		DefaultRoutedPath,
		DefaultRoutedPath + "?account_key=acme",
		DefaultRoutedPath + "?provider=github",
	} {
		w := do(t, rt, http.MethodGet, target, testAdminToken, "")
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", target, w.Code)
		}
	}
}

func TestRoutedUnknownAccountIsNotFound(t *testing.T) {
	rt := newRouter(t, testConfig(t), newFakeStore())

	w := do(t, rt, http.MethodGet, DefaultRoutedPath+"?provider=github&account_key=nobody", testAdminToken, "")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Fatalf("unknown account was redirected to %q; there is no default cell", loc)
	}
}

func TestRoutedUnconfiguredRegionIsServerErrorNotADefaultCell(t *testing.T) {
	fs := newFakeStore()
	fs.regions["github/acme"] = "ap" // assigned, but absent from the env map
	rt := newRouter(t, testConfig(t), fs)

	w := do(t, rt, http.MethodGet, DefaultRoutedPath+"?provider=github&account_key=acme", testAdminToken, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if got := decodeError(t, w).Code; got != "region_not_configured" {
		t.Fatalf("code = %q", got)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Fatalf("fell back to cell %q; there must be no default cell", loc)
	}
}

func TestRoutedLookupFailureIsInternalError(t *testing.T) {
	fs := newFakeStore()
	fs.lookupErr = errors.New("connection refused")
	rt := newRouter(t, testConfig(t), fs)

	w := do(t, rt, http.MethodGet, DefaultRoutedPath+"?provider=github&account_key=acme", testAdminToken, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "connection refused") {
		t.Fatal("response leaked the underlying store error")
	}
}

func TestRoutedNonceFailureRefusesRatherThanRedirects(t *testing.T) {
	fs := newFakeStore()
	fs.regions["github/acme"] = "us"
	rt := newRouter(t, testConfig(t), fs)
	rt.NewNonce = func() (string, error) { return "", errors.New("no entropy") }

	w := do(t, rt, http.MethodGet, DefaultRoutedPath+"?provider=github&account_key=acme", testAdminToken, "")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "" {
		t.Fatalf("emitted an unbound redirect %q after the nonce failed", loc)
	}
}

// A cell base URL carrying a path prefix keeps that prefix, and the
// caller's path is appended rather than replacing it.
func TestRedirectPreservesCellPathPrefix(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		EnvRegions:       "us=https://gateway.example/cells/us/",
		EnvHandoffSecret: "s3cret",
		EnvAdminToken:    testAdminToken,
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	fs := newFakeStore()
	fs.regions["github/acme"] = "us"
	rt := newRouter(t, cfg, fs)

	w := do(t, rt, http.MethodGet, DefaultRoutedPath+"?provider=github&account_key=acme", testAdminToken, "")
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	if want := "/cells/us" + DefaultRoutedPath; loc.Path != want {
		t.Fatalf("path = %q, want %q", loc.Path, want)
	}
}

// --- Construction -------------------------------------------------------

func TestNewRejectsInvalidConfigAndNilStore(t *testing.T) {
	if _, err := New(Config{}, newFakeStore()); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New with a zero Config = %v, want ErrInvalidConfig", err)
	}
	if _, err := New(testConfig(t), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("New with a nil store = %v, want ErrInvalidConfig", err)
	}
	if _, err := NewPostgres(testConfig(t), nil); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("NewPostgres with a nil pool = %v, want ErrInvalidConfig", err)
	}
}

// Only the configured routed paths are served; an unrouted cell path is a
// 404 from the mux, never an unauthenticated pass-through.
func TestUnroutedPathIsNotServed(t *testing.T) {
	rt := newRouter(t, testConfig(t), newFakeStore())
	for _, p := range []string{"/v0/auth/github/login", "/v0/auth/github/callback"} {
		w := do(t, rt, http.MethodGet, p+"?provider=github&account_key=acme", testAdminToken, "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("%s: status = %d, want 404 (it must not be routed by default)", p, w.Code)
		}
	}
}

func decodeError(t *testing.T, w *httptest.ResponseRecorder) errorBody {
	t.Helper()
	var body errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body %q: %v", w.Body, err)
	}
	return body
}
