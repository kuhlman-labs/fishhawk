package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
)

const (
	testHandoffSecret = "shared-cell-directory-secret"
	testCellRegion    = "us"
)

// pinRecorder is a RegionPinner query surface that records the pin it was
// asked for and returns a programmed error.
type pinRecorder struct {
	calls []accountdb.PinAccountHomeRegionParams
	err   error
	// existing, when non-empty, is the region the account is already homed
	// in — what the classify read reports for a zero-row miss.
	existing string
}

func (p *pinRecorder) PinAccountHomeRegion(_ context.Context, arg accountdb.PinAccountHomeRegionParams) (accountdb.Account, error) {
	p.calls = append(p.calls, arg)
	if p.err != nil {
		return accountdb.Account{}, p.err
	}
	return accountdb.Account{Provider: arg.Provider, AccountKey: arg.AccountKey, HomeRegion: arg.HomeRegion}, nil
}

func (p *pinRecorder) GetAccountByKey(_ context.Context, arg accountdb.GetAccountByKeyParams) (accountdb.Account, error) {
	region := p.existing
	return accountdb.Account{Provider: arg.Provider, AccountKey: arg.AccountKey, HomeRegion: &region}, nil
}

// regionPinServer builds a Server whose region-pin surface is configured per
// the arguments. An empty secret or an empty cellRegion is the DISABLED
// posture, exactly as serve.go would leave it.
func regionPinServer(t *testing.T, secret, cellRegion string, q *pinRecorder) *Server {
	t.Helper()
	cfg := Config{HandoffSecret: secret}
	if q != nil {
		cfg.RegionPinner = account.NewRegionPinner(q, cellRegion)
	}
	return New(cfg)
}

// signedRequest builds a GET for path carrying a handoff signed with secret.
// extra query parameters are merged in first so the test can assert the
// caller's own parameters survive.
func signedRequest(t *testing.T, secret, path string, p handoff.Params, extra url.Values) *http.Request {
	t.Helper()
	signed, err := handoff.Sign(secret, p)
	if err != nil {
		t.Fatalf("sign handoff: %v", err)
	}
	q := url.Values{}
	for k, vs := range extra {
		q[k] = vs
	}
	for k, vs := range signed {
		q[k] = vs
	}
	return httptest.NewRequest(http.MethodGet, path+"?"+q.Encode(), nil)
}

func validParams() handoff.Params {
	return handoff.Params{
		Provider:   "github",
		AccountKey: "acme",
		HomeRegion: testCellRegion,
		ExpiresAt:  time.Now().Add(2 * time.Minute),
		Nonce:      "0123456789abcdef",
	}
}

// errorCode decodes the error envelope's code, failing the test on a body
// that is not an error envelope.
func errorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, w.Body.String())
	}
	return env.Error.Code
}

// sentinel handler that records whether the middleware delegated.
func recordingNext(reached *bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	}
}

// A request with NO fh_* parameters passes through untouched — the
// single-cell deployment must behave exactly as it did before.
func TestWithRegionPin_PassesThroughUnsignedRequest(t *testing.T) {
	q := &pinRecorder{}
	s := regionPinServer(t, testHandoffSecret, testCellRegion, q)

	var reached bool
	w := httptest.NewRecorder()
	s.withRegionPin(recordingNext(&reached))(w, httptest.NewRequest(http.MethodGet, RoutedOnboardingPath+"?provider=github", nil))

	if !reached {
		t.Fatalf("unsigned request was not delegated (status %d): %s", w.Code, w.Body.String())
	}
	if len(q.calls) != 0 {
		t.Fatalf("unsigned request issued %d pins, want 0", len(q.calls))
	}
}

// Approval condition 8, the fail-closed behavioral test: with the secret SET
// but the cell region UNSET, an ACTUALLY SIGNED request is REFUSED. Asserting
// only that the pinner was not constructed would not show that the surface
// refuses rather than bypasses.
func TestWithRegionPin_RefusesSignedRequestWhenRegionUnset(t *testing.T) {
	cases := []struct {
		name       string
		secret     string
		cellRegion string
		withPinner bool
	}{
		{"secret set, region unset", testHandoffSecret, "", true},
		{"secret set, no pinner at all", testHandoffSecret, "", false},
		{"region set, secret unset", "", testCellRegion, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var q *pinRecorder
			if tc.withPinner {
				q = &pinRecorder{}
			}
			s := regionPinServer(t, tc.secret, tc.cellRegion, q)

			var reached bool
			w := httptest.NewRecorder()
			req := signedRequest(t, testHandoffSecret, RoutedOnboardingPath, validParams(), nil)
			s.withRegionPin(recordingNext(&reached))(w, req)

			if reached {
				t.Fatal("a signed request BYPASSED the disabled pin surface and reached the handler")
			}
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
			}
			if got := errorCode(t, w); got != "region_pin_disabled" {
				t.Fatalf("code = %q, want region_pin_disabled", got)
			}
		})
	}
}

// A handoff whose signature does not authenticate the fields is refused —
// never passed through unpinned.
func TestWithRegionPin_RefusesTamperedSignature(t *testing.T) {
	q := &pinRecorder{}
	s := regionPinServer(t, testHandoffSecret, testCellRegion, q)

	req := signedRequest(t, "some-other-secret", RoutedOnboardingPath, validParams(), nil)
	var reached bool
	w := httptest.NewRecorder()
	s.withRegionPin(recordingNext(&reached))(w, req)

	if reached {
		t.Fatal("a badly-signed handoff reached the handler")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if got := errorCode(t, w); got != "handoff_signature_invalid" {
		t.Fatalf("code = %q, want handoff_signature_invalid", got)
	}
	if len(q.calls) != 0 {
		t.Fatalf("a rejected handoff issued %d pins, want 0", len(q.calls))
	}
}

// An expired handoff is refused. The server clock is driven forward rather
// than slept through.
func TestWithRegionPin_RefusesExpiredHandoff(t *testing.T) {
	s := regionPinServer(t, testHandoffSecret, testCellRegion, &pinRecorder{})
	p := validParams()
	s.nowFunc = func() time.Time { return p.ExpiresAt.Add(time.Second) }

	var reached bool
	w := httptest.NewRecorder()
	s.withRegionPin(recordingNext(&reached))(w, signedRequest(t, testHandoffSecret, RoutedOnboardingPath, p, nil))

	if reached {
		t.Fatal("an expired handoff reached the handler")
	}
	if w.Code != http.StatusForbidden || errorCode(t, w) != "handoff_expired" {
		t.Fatalf("status/code = %d/%s, want 403/handoff_expired:\n%s", w.Code, errorCode(t, w), w.Body.String())
	}
}

// A signature present but a required field missing is a malformed request,
// distinguished from a refused credential.
func TestWithRegionPin_RefusesMalformedHandoff(t *testing.T) {
	s := regionPinServer(t, testHandoffSecret, testCellRegion, &pinRecorder{})

	req := signedRequest(t, testHandoffSecret, RoutedOnboardingPath, validParams(), nil)
	q := req.URL.Query()
	q.Del(handoff.ParamNonce)
	req.URL.RawQuery = q.Encode()

	var reached bool
	w := httptest.NewRecorder()
	s.withRegionPin(recordingNext(&reached))(w, req)

	if reached {
		t.Fatal("a malformed handoff reached the handler")
	}
	if w.Code != http.StatusBadRequest || errorCode(t, w) != "handoff_malformed" {
		t.Fatalf("status/code = %d/%s, want 400/handoff_malformed:\n%s", w.Code, errorCode(t, w), w.Body.String())
	}
}

// A valid handoff naming a DIFFERENT region than this cell is refused by the
// residency self-check with the typed conflict status.
func TestWithRegionPin_RefusesForeignRegion(t *testing.T) {
	q := &pinRecorder{}
	s := regionPinServer(t, testHandoffSecret, testCellRegion, q)

	p := validParams()
	p.HomeRegion = "eu"
	var reached bool
	w := httptest.NewRecorder()
	s.withRegionPin(recordingNext(&reached))(w, signedRequest(t, testHandoffSecret, RoutedOnboardingPath, p, nil))

	if reached {
		t.Fatal("a foreign-region handoff reached the handler")
	}
	if w.Code != http.StatusConflict || errorCode(t, w) != "region_mismatch" {
		t.Fatalf("status/code = %d/%s, want 409/region_mismatch:\n%s", w.Code, errorCode(t, w), w.Body.String())
	}
}

// An account already homed elsewhere yields the typed conflict, and the
// wrapped handler is not reached.
func TestWithRegionPin_RefusesAlreadyPinnedAccount(t *testing.T) {
	q := &pinRecorder{err: pgx.ErrNoRows, existing: "eu"}
	s := regionPinServer(t, testHandoffSecret, testCellRegion, q)

	var reached bool
	w := httptest.NewRecorder()
	s.withRegionPin(recordingNext(&reached))(w, signedRequest(t, testHandoffSecret, RoutedOnboardingPath, validParams(), nil))

	if reached {
		t.Fatal("a conflicting handoff reached the handler")
	}
	if w.Code != http.StatusConflict || errorCode(t, w) != "region_conflict" {
		t.Fatalf("status/code = %d/%s, want 409/region_conflict:\n%s", w.Code, errorCode(t, w), w.Body.String())
	}
}

// A pin failure that is neither a residency conflict nor a missing account is
// a 500 — never a silent success.
func TestWithRegionPin_PinFailurePropagates(t *testing.T) {
	s := regionPinServer(t, testHandoffSecret, testCellRegion, &pinRecorder{err: errors.New("connection reset")})

	var reached bool
	w := httptest.NewRecorder()
	s.withRegionPin(recordingNext(&reached))(w, signedRequest(t, testHandoffSecret, RoutedOnboardingPath, validParams(), nil))

	if reached {
		t.Fatal("a failed pin reached the handler")
	}
	if w.Code != http.StatusInternalServerError || errorCode(t, w) != "region_pin_failed" {
		t.Fatalf("status/code = %d/%s, want 500/region_pin_failed:\n%s", w.Code, errorCode(t, w), w.Body.String())
	}
}

// The happy path: a valid handoff pins the account and delegates, with the
// caller's own query parameters untouched.
func TestWithRegionPin_ValidHandoffPinsAndDelegates(t *testing.T) {
	q := &pinRecorder{}
	s := regionPinServer(t, testHandoffSecret, testCellRegion, q)

	extra := url.Values{"state": {"caller-state"}, "provider": {"github"}, "account_key": {"acme"}}
	req := signedRequest(t, testHandoffSecret, RoutedOnboardingPath, validParams(), extra)

	var gotState string
	var gotHandoff handoff.Params
	var viaHandoff bool
	w := httptest.NewRecorder()
	s.withRegionPin(func(w http.ResponseWriter, r *http.Request) {
		gotState = r.URL.Query().Get("state")
		gotHandoff, viaHandoff = handoffFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if len(q.calls) != 1 {
		t.Fatalf("pins = %d, want 1", len(q.calls))
	}
	got := q.calls[0]
	if got.Provider != "github" || got.AccountKey != "acme" || got.HomeRegion == nil || *got.HomeRegion != testCellRegion {
		t.Fatalf("pin params = %+v, want github/acme/us", got)
	}
	if gotState != "caller-state" {
		t.Fatalf("caller state = %q, want it preserved", gotState)
	}
	if !viaHandoff || gotHandoff.AccountKey != "acme" {
		t.Fatalf("handoff not carried into the handler: %+v (present=%v)", gotHandoff, viaHandoff)
	}
}

// --- GET /v0/onboarding/start ----------------------------------------------

// An anonymous caller with NO handoff is refused: the response names an
// account's region, which is not public.
func TestOnboardingStart_AnonymousWithoutHandoffRefused(t *testing.T) {
	s := regionPinServer(t, testHandoffSecret, testCellRegion, &pinRecorder{})

	w := httptest.NewRecorder()
	s.handleOnboardingStart(w, httptest.NewRequest(http.MethodGet, RoutedOnboardingPath+"?provider=github&account_key=acme", nil))

	if w.Code != http.StatusUnauthorized || errorCode(t, w) != "authentication_required" {
		t.Fatalf("status/code = %d/%s, want 401/authentication_required:\n%s", w.Code, errorCode(t, w), w.Body.String())
	}
}

// An authenticated caller on a single-cell deployment still needs to say
// WHICH account it is asking about.
func TestOnboardingStart_AuthenticatedRequiresAccountIdentity(t *testing.T) {
	s := regionPinServer(t, "", "", nil)

	req := httptest.NewRequest(http.MethodGet, RoutedOnboardingPath, nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{Subject: "github:op"}))
	w := httptest.NewRecorder()
	s.handleOnboardingStart(w, req)

	if w.Code != http.StatusBadRequest || errorCode(t, w) != "validation_failed" {
		t.Fatalf("status/code = %d/%s, want 400/validation_failed:\n%s", w.Code, errorCode(t, w), w.Body.String())
	}
}

// End to end through the middleware: a handoff-bearing request is served
// without any session, and the body reports the pinned region.
func TestOnboardingStart_ReportsPinnedRegion(t *testing.T) {
	s := regionPinServer(t, testHandoffSecret, testCellRegion, &pinRecorder{})

	w := httptest.NewRecorder()
	s.withRegionPin(s.handleOnboardingStart)(w, signedRequest(t, testHandoffSecret, RoutedOnboardingPath, validParams(), nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp onboardingStartResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	if !resp.Pinned || resp.HomeRegion != testCellRegion || resp.CellRegion != testCellRegion {
		t.Fatalf("response = %+v, want pinned in %q", resp, testCellRegion)
	}
	if resp.Provider != "github" || resp.AccountKey != "acme" {
		t.Fatalf("response identity = %s/%s, want github/acme", resp.Provider, resp.AccountKey)
	}
}
