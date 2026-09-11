package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures"
	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures/catalog"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// fakeDevApplier is a DevFixtureApplier whose Apply answers from a
// canned result or a canned error, recording the name it was asked for.
type fakeDevApplier struct {
	result  devfixtures.Result
	err     error
	applied []string
}

func (f *fakeDevApplier) Names() []string             { return catalog.Names() }
func (f *fakeDevApplier) Describe(name string) string { return catalog.Description(name) }
func (f *fakeDevApplier) Apply(_ context.Context, name string) (devfixtures.Result, error) {
	f.applied = append(f.applied, name)
	if f.err != nil {
		return devfixtures.Result{}, f.err
	}
	if !catalog.Known(name) {
		return devfixtures.Result{}, devfixtures.ErrUnknownScenario
	}
	return f.result, nil
}

// newDevServer builds a server through New(cfg) so the REAL middleware
// chain (request id, logging, bearer auth, csrf, ...) runs in front of
// the dev routes. No repositories are wired: the fake applier is the
// only backend the routes touch.
func newDevServer(t *testing.T, applier DevFixtureApplier) *Server {
	t.Helper()
	return New(Config{Addr: "127.0.0.1:0", DevFixtures: applier})
}

// devRequest issues a credential-less request from a LOOPBACK peer.
// httptest.NewRequest defaults RemoteAddr to 192.0.2.1:1234, which the
// loopback guard refuses, so every helper pins it explicitly — a test
// that forgets gets a 403, not a false pass.
func devRequest(t *testing.T, s *Server, method, path string, body []byte, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = remoteAddr
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

const devLoopbackPeer = "127.0.0.1:1"

func decodeErrorCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error envelope: %v\n%s", err, w.Body.String())
	}
	return env.Error.Code
}

// devRoutes is every (method, path) the surface registers, so the
// absence / loopback tests cover all three rather than a sample.
var devRoutes = []struct{ method, path string }{
	{http.MethodGet, "/v0/dev/fixtures"},
	{http.MethodPost, "/v0/dev/fixtures"},
	{http.MethodPost, "/v0/dev/sign"},
}

// TestDevFixtures_RouteAbsentWhenUnconfigured pins the registration
// guard: a nil Config.DevFixtures leaves every dev path unknown to the
// mux (router 404), never a 503 or a 403. Counterfactual: delete the
// `!= nil` guard in handlers.go → the routes register and the GET
// answers 200 (or panics on the nil applier) → RED.
func TestDevFixtures_RouteAbsentWhenUnconfigured(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	for _, rt := range devRoutes {
		w := devRequest(t, s, rt.method, rt.path, []byte(`{}`), devLoopbackPeer, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s with DevFixtures nil: status = %d, want 404 (surface must be ABSENT)\n%s",
				rt.method, rt.path, w.Code, w.Body.String())
		}
	}
}

// TestDevFixtures_RefusesNonLoopbackPeer pins the loopback guard on all
// three routes. Counterfactual: drop the devLoopbackOnly wrapper from
// the registration → the GET answers 200 for 10.1.2.3 → RED.
func TestDevFixtures_RefusesNonLoopbackPeer(t *testing.T) {
	s := newDevServer(t, &fakeDevApplier{})
	for _, rt := range devRoutes {
		w := devRequest(t, s, rt.method, rt.path, []byte(`{"scenario":"plan-gate-parked"}`), "10.1.2.3:4", nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s from 10.1.2.3: status = %d, want 403\n%s", rt.method, rt.path, w.Code, w.Body.String())
			continue
		}
		if code := decodeErrorCode(t, w); code != "dev_surface_loopback_only" {
			t.Errorf("%s %s: error code = %q, want dev_surface_loopback_only", rt.method, rt.path, code)
		}
	}
}

// TestDevFixtures_UnparseableRemoteAddrRefused: a RemoteAddr that does
// not split host:port (or whose host is not an IP) is refused, not
// admitted — fail closed on a malformed peer.
func TestDevFixtures_UnparseableRemoteAddrRefused(t *testing.T) {
	s := newDevServer(t, &fakeDevApplier{})
	for _, addr := range []string{"", "not-an-addr", "127.0.0.1", "localhost:1", "[::1]"} {
		w := devRequest(t, s, http.MethodGet, "/v0/dev/fixtures", nil, addr, nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("RemoteAddr %q: status = %d, want 403\n%s", addr, w.Code, w.Body.String())
			continue
		}
		if code := decodeErrorCode(t, w); code != "dev_surface_loopback_only" {
			t.Errorf("RemoteAddr %q: error code = %q, want dev_surface_loopback_only", addr, code)
		}
	}
	// IPv6 loopback is a loopback peer too.
	w := devRequest(t, s, http.MethodGet, "/v0/dev/fixtures", nil, "[::1]:9", nil)
	if w.Code != http.StatusOK {
		t.Errorf("RemoteAddr [::1]:9: status = %d, want 200\n%s", w.Code, w.Body.String())
	}
}

// TestDevFixtures_ListNamesCatalog: GET renders the catalog in Names()
// order with each description.
func TestDevFixtures_ListNamesCatalog(t *testing.T) {
	s := newDevServer(t, &fakeDevApplier{})
	w := devRequest(t, s, http.MethodGet, "/v0/dev/fixtures", nil, devLoopbackPeer, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	var got devFixturesListResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	want := catalog.Names()
	if len(got.Scenarios) != len(want) {
		t.Fatalf("scenarios = %d, want %d: %+v", len(got.Scenarios), len(want), got.Scenarios)
	}
	for i, n := range want {
		if got.Scenarios[i].Name != n {
			t.Errorf("scenarios[%d].name = %q, want %q (catalog order)", i, got.Scenarios[i].Name, n)
		}
		if got.Scenarios[i].Description != catalog.Description(n) {
			t.Errorf("scenarios[%d].description = %q, want the catalog description", i, got.Scenarios[i].Description)
		}
	}
}

// TestDevFixtures_ApplyReturnsIDs: the 201 body carries the run id and
// stage ids the applier minted. The request carries NO Authorization
// header and NO cookie — it drives New(cfg)'s full middleware chain, so
// a csrf_required 403 here would refute the "no exemption needed"
// reading in devfixtures.go. This IS the CSRF proof.
func TestDevFixtures_ApplyReturnsIDs(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	fake := &fakeDevApplier{result: devfixtures.Result{
		Scenario: "plan-gate-parked",
		Runs:     map[string]devfixtures.RunResult{"run": {ID: runID, Stages: map[string]uuid.UUID{"plan": stageID}}},
	}}
	s := newDevServer(t, fake)
	w := devRequest(t, s, http.MethodPost, "/v0/dev/fixtures", []byte(`{"scenario":"plan-gate-parked"}`), devLoopbackPeer, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201\n%s", w.Code, w.Body.String())
	}
	var got struct {
		Scenario string `json:"scenario"`
		Runs     map[string]struct {
			RunID  uuid.UUID            `json:"run_id"`
			Stages map[string]uuid.UUID `json:"stages"`
		} `json:"runs"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Scenario != "plan-gate-parked" {
		t.Errorf("scenario = %q", got.Scenario)
	}
	if got.Runs["run"].RunID != runID {
		t.Errorf("runs.run.run_id = %s, want %s", got.Runs["run"].RunID, runID)
	}
	if got.Runs["run"].Stages["plan"] != stageID {
		t.Errorf("runs.run.stages.plan = %s, want %s", got.Runs["run"].Stages["plan"], stageID)
	}
	if len(fake.applied) != 1 || fake.applied[0] != "plan-gate-parked" {
		t.Errorf("applier saw %v, want [plan-gate-parked]", fake.applied)
	}
}

// TestDevFixtures_UnknownScenario404NamesKnownSet: an
// ErrUnknownScenario from Apply maps to 404 fixture_scenario_unknown
// with details.known = the catalog. Counterfactual: replace the
// errors.Is branch with the 500 fallthrough → RED on the status.
func TestDevFixtures_UnknownScenario404NamesKnownSet(t *testing.T) {
	s := newDevServer(t, &fakeDevApplier{})
	w := devRequest(t, s, http.MethodPost, "/v0/dev/fixtures", []byte(`{"scenario":"not-a-scenario"}`), devLoopbackPeer, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404\n%s", w.Code, w.Body.String())
	}
	var env struct {
		Error struct {
			Code    string `json:"code"`
			Details struct {
				Scenario string   `json:"scenario"`
				Known    []string `json:"known"`
			} `json:"details"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error.Code != "fixture_scenario_unknown" {
		t.Errorf("code = %q, want fixture_scenario_unknown", env.Error.Code)
	}
	if env.Error.Details.Scenario != "not-a-scenario" {
		t.Errorf("details.scenario = %q", env.Error.Details.Scenario)
	}
	if want := catalog.Names(); strings.Join(env.Error.Details.Known, ",") != strings.Join(want, ",") {
		t.Errorf("details.known = %v, want %v", env.Error.Details.Known, want)
	}
}

// TestDevFixtures_MalformedBody400: invalid JSON and an unknown field
// (DisallowUnknownFields) each answer 400 validation_failed, and the
// applier is never called.
func TestDevFixtures_MalformedBody400(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"invalid_json", `{"scenario":`},
		{"unknown_field", `{"scenario":"plan-gate-parked","extra":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeDevApplier{}
			s := newDevServer(t, fake)
			w := devRequest(t, s, http.MethodPost, "/v0/dev/fixtures", []byte(tc.body), devLoopbackPeer, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400\n%s", w.Code, w.Body.String())
			}
			if code := decodeErrorCode(t, w); code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", code)
			}
			if len(fake.applied) != 0 {
				t.Errorf("applier called with %v on a malformed body", fake.applied)
			}
		})
	}
}

// TestDevFixtures_BodyTooLarge400: a body over devFixturesMaxBodyBytes is
// refused at the decoder (http.MaxBytesReader) with the existing 400
// validation_failed shape, and the applier is never called. The oversized
// body is a VALID one-field object padded with a long scenario value, so
// the refusal is the cap and not the JSON grammar; the control is that the
// same shape one byte under the cap decodes and reaches the applier.
// COUNTERFACTUAL: delete the MaxBytesReader line → the over-cap case
// decodes, reaches the applier, and this test is RED.
func TestDevFixtures_BodyTooLarge400(t *testing.T) {
	// {"scenario":"<pad>"} — the pad is sized so the whole body lands
	// exactly at cap+1 (over) or cap (under).
	bodyOfLen := func(n int) []byte {
		const frame = `{"scenario":""}`
		return []byte(`{"scenario":"` + strings.Repeat("x", n-len(frame)) + `"}`)
	}
	t.Run("over_cap_400", func(t *testing.T) {
		fake := &fakeDevApplier{}
		s := newDevServer(t, fake)
		body := bodyOfLen(devFixturesMaxBodyBytes + 1)
		w := devRequest(t, s, http.MethodPost, "/v0/dev/fixtures", body, devLoopbackPeer, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400\n%s", w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w); code != "validation_failed" {
			t.Errorf("code = %q, want validation_failed", code)
		}
		if len(fake.applied) != 0 {
			t.Errorf("applier called with %v on an over-cap body", fake.applied)
		}
	})
	t.Run("at_cap_reaches_applier", func(t *testing.T) {
		fake := &fakeDevApplier{}
		s := newDevServer(t, fake)
		body := bodyOfLen(devFixturesMaxBodyBytes)
		w := devRequest(t, s, http.MethodPost, "/v0/dev/fixtures", body, devLoopbackPeer, nil)
		if w.Code == http.StatusBadRequest {
			t.Fatalf("an at-cap body was refused 400 — the cap is off by one\n%s", w.Body.String())
		}
		if len(fake.applied) != 1 {
			t.Fatalf("applier called %d times on an at-cap body, want 1 (the cap must not swallow a legal body)", len(fake.applied))
		}
	})
}

// TestDevFixtures_ApplyError500: any non-catalog Apply error is a 500
// internal_error whose body carries the error_ref but NOT the cause
// (the writeError 5xx redaction), with the cause in the operator log.
func TestDevFixtures_ApplyError500(t *testing.T) {
	s := newDevServer(t, &fakeDevApplier{err: errors.New("store exploded: secret-detail")})
	w := devRequest(t, s, http.MethodPost, "/v0/dev/fixtures", []byte(`{"scenario":"plan-gate-parked"}`), devLoopbackPeer, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500\n%s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != "internal_error" {
		t.Errorf("code = %q, want internal_error", code)
	}
	if strings.Contains(w.Body.String(), "secret-detail") {
		t.Errorf("5xx body leaks the cause:\n%s", w.Body.String())
	}
}

// devSignKey returns a fresh Ed25519 keypair plus the base64 header
// value POST /v0/runs/{id}/signing-key would have returned for it.
func devSignKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return pub, priv, base64.StdEncoding.EncodeToString(priv)
}

// TestDevSign_SignatureVerifiesAgainstPublicHalf: the route's hex
// signature verifies under the key's public half via
// signing.VerifyWith over signing.ComputeMessage(body), and a FRESHLY
// GENERATED unrelated public key does not — non-matching by
// construction, so the positive arm cannot be a vacuous pass.
func TestDevSign_SignatureVerifiesAgainstPublicHalf(t *testing.T) {
	pub, _, header := devSignKey(t)
	s := newDevServer(t, &fakeDevApplier{})
	body := []byte("raw-bundle-bytes\x00binary")
	w := devRequest(t, s, http.MethodPost, "/v0/dev/sign", body, devLoopbackPeer,
		map[string]string{devSignPrivateKeyHeader: header, "Content-Type": "application/octet-stream"})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	var got devSignResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	sig, err := hex.DecodeString(got.Signature)
	if err != nil {
		t.Fatalf("signature is not hex: %v", err)
	}
	if err := signing.VerifyWith(pub, signing.ComputeMessage(body), sig); err != nil {
		t.Fatalf("signature does not verify under the issued public half: %v", err)
	}
	otherPub, _, _ := devSignKey(t)
	if err := signing.VerifyWith(otherPub, signing.ComputeMessage(body), sig); err == nil {
		t.Fatal("signature verified under an unrelated public key — the positive arm is vacuous")
	}
	// The key never reaches the response.
	if strings.Contains(w.Body.String(), header) {
		t.Fatal("response body echoes the private key")
	}
}

// TestDevSign_MalformedKey400 pins each named key-validation mode:
// missing, not base64, and wrong length (31 bytes). Each is a 400
// validation_failed whose details.reason names the mode.
func TestDevSign_MalformedKey400(t *testing.T) {
	s := newDevServer(t, &fakeDevApplier{})
	for _, tc := range []struct{ name, header, wantReason string }{
		{"missing", "", "missing"},
		{"not_base64", "!!!not-base64!!!", "not_base64"},
		{"wrong_length", base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 31)), "wrong_length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := map[string]string{}
			if tc.header != "" {
				headers[devSignPrivateKeyHeader] = tc.header
			}
			w := devRequest(t, s, http.MethodPost, "/v0/dev/sign", []byte("body"), devLoopbackPeer, headers)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400\n%s", w.Code, w.Body.String())
			}
			var env struct {
				Error struct {
					Code    string `json:"code"`
					Details struct {
						Reason string `json:"reason"`
					} `json:"details"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if env.Error.Code != "validation_failed" {
				t.Errorf("code = %q, want validation_failed", env.Error.Code)
			}
			if env.Error.Details.Reason != tc.wantReason {
				t.Errorf("details.reason = %q, want %q", env.Error.Details.Reason, tc.wantReason)
			}
		})
	}
}

// TestDevSign_BodyTooLarge413: a body over devSignMaxBodyBytes is
// refused 413 body_too_large rather than signed.
func TestDevSign_BodyTooLarge413(t *testing.T) {
	_, _, header := devSignKey(t)
	s := newDevServer(t, &fakeDevApplier{})
	body := bytes.Repeat([]byte{'x'}, devSignMaxBodyBytes+1)
	w := devRequest(t, s, http.MethodPost, "/v0/dev/sign", body, devLoopbackPeer,
		map[string]string{devSignPrivateKeyHeader: header})
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413\n%s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != "body_too_large" {
		t.Errorf("code = %q, want body_too_large", code)
	}
}
