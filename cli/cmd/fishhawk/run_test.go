package main

// Coverage division for the applies_to escape hatch (E53.3 / #2226, wired
// into the CLI by E53.11 / #2364).
//
// THESE tests own exactly one seam: flag-to-wire. They drive the real
// `runStart` call site and assert on the JSON the backend RECEIVES — the
// literal keys `applies_to_override` and `applies_to_override_reason`, as
// the backend parses them. The key STRINGS are asserted rather than a
// struct field on purpose: the key is the contract between the two halves,
// and it is the one thing a reader can verify from neither side alone.
//
// The far side is owned elsewhere and is deliberately NOT re-tested here (a
// CLI-side re-test would duplicate it and add a cross-module dependency this
// workspace does not otherwise have):
//   - Admission and the plan gate:
//     backend/internal/server/applies_to_plan_gate_test.go, including
//     TestAppliesToPlanGate_OverrideEntry_SuppressesRejection and
//     TestAppliesToPlanGate_OverrideAbsent_Rejects.
//   - The run_admitted_applies_to_override audit grant on the admission
//     path: the exact-count assertions in backend/internal/server (#2366).
//   - Marshalling from an already-populated CreateRunInput:
//     cli/internal/httpclient's TestStartRun_W2_AppliesToOverride_Serializes.
//
// One local rule has no wire counterpart: passing a reason WITHOUT the
// boolean is rejected here as a usage error. That is a deliberate LOCAL
// strictness, not a wire-contract divergence — the API silently ignores a
// lone reason, so the invocation can only ever be an operator mistake.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/credstore"
)

// startRunCapture is an httptest backend that records the decoded body of
// every POST /v0/runs it receives, plus the total request count so a test
// can assert ZERO round-trips on a local usage error.
type startRunCapture struct {
	srv      *httptest.Server
	requests atomic.Int64
	body     map[string]any
}

func newStartRunCapture(t *testing.T) *startRunCapture {
	t.Helper()
	c := &startRunCapture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.requests.Add(1)
		raw, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(raw, &c.body); err != nil {
			t.Errorf("backend received a body that is not JSON: %v", err)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + uuid.NewString() + `","repo":"x/y","workflow_id":"trivial","state":"pending","runner_kind":"github_actions"}`))
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// specDir writes runStartSpecYAML (from issue_fetch_test.go) into a temp
// dir so runStart's discovery finds a spec and proceeds to the round-trip.
func specDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".fishhawk"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".fishhawk", "workflows.yaml"), []byte(runStartSpecYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestRunStart_AppliesToOverride_ReachesTheRequestBody is the headline
// flag-to-wire test. The hand-off from the two flags into CreateRunInput
// could be dropped entirely with every other CLI and httpclient test green:
// the httpclient's W2 test starts from an already-populated struct, and no
// cli/cmd/fishhawk test set the pair before this one. The operator would
// then pass --applies-to-override, see it accepted, and still be refused.
func TestRunStart_AppliesToOverride_ReachesTheRequestBody(t *testing.T) {
	fake := newStartRunCapture(t)
	// Surrounding whitespace is part of the fixture: the reason travels
	// UNTRIMMED so the backend stays the single normalization point.
	const reason = "  one-off backport of the 2.1 hotfix  "

	var stdout, stderr bytes.Buffer
	code := runStart([]string{
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", specDir(t),
		"--backend-url", fake.srv.URL,
		"--token", "tok-test",
		"--applies-to-override",
		"--applies-to-override-reason", reason,
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runStart exit = %d, want exitOK\nstderr: %s", code, stderr.String())
	}
	if got := fake.body["applies_to_override"]; got != true {
		t.Errorf("wire key applies_to_override = %v, want true — body: %+v", got, fake.body)
	}
	if got := fake.body["applies_to_override_reason"]; got != reason {
		t.Errorf("wire key applies_to_override_reason = %q, want %q untrimmed (the backend owns normalization)", got, reason)
	}
}

// TestRunStart_AppliesToOverride_DefaultsOff machine-enforces the omitempty
// contract: an ordinary `run start` must be byte-identical on the wire to
// its pre-change self, so NEITHER key may appear.
func TestRunStart_AppliesToOverride_DefaultsOff(t *testing.T) {
	fake := newStartRunCapture(t)

	var stdout, stderr bytes.Buffer
	code := runStart([]string{
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", specDir(t),
		"--backend-url", fake.srv.URL,
		"--token", "tok-test",
	}, &stdout, &stderr)
	if code != exitOK {
		t.Fatalf("runStart exit = %d, want exitOK\nstderr: %s", code, stderr.String())
	}
	if _, ok := fake.body["applies_to_override"]; ok {
		t.Errorf("applies_to_override present on an ordinary run start: %+v", fake.body)
	}
	if _, ok := fake.body["applies_to_override_reason"]; ok {
		t.Errorf("applies_to_override_reason present on an ordinary run start: %+v", fake.body)
	}
}

// TestRunStart_AppliesToOverride_EmptyReasonIsUsageError covers failure mode
// (a): the override with a whitespace-only reason. The ZERO-request
// assertion is the load-bearing one — a bypass that reached the backend
// unexplained is the actual defect class here. It runs WITHOUT a discovered
// spec, pinning that validation precedes discoverSpec so the refusal is
// identical from any directory.
func TestRunStart_AppliesToOverride_EmptyReasonIsUsageError(t *testing.T) {
	fake := newStartRunCapture(t)

	var stdout, stderr bytes.Buffer
	code := runStart([]string{
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", t.TempDir(), // no .fishhawk/workflows.yaml
		"--backend-url", fake.srv.URL,
		"--token", "tok-test",
		"--applies-to-override",
		"--applies-to-override-reason", "   \t  ",
	}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("runStart exit = %d, want exitUsage\nstderr: %s", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("applies-to-override-reason")) {
		t.Errorf("stderr does not name the offending flag: %s", stderr.String())
	}
	if n := fake.requests.Load(); n != 0 {
		t.Errorf("backend received %d requests, want 0 — a reasonless override must never round-trip", n)
	}
}

// TestRunStart_AppliesToOverrideReason_WithoutFlagIsUsageError covers
// failure mode (b): a reason with no --applies-to-override. Same three
// properties, same no-spec directory.
func TestRunStart_AppliesToOverrideReason_WithoutFlagIsUsageError(t *testing.T) {
	fake := newStartRunCapture(t)

	var stdout, stderr bytes.Buffer
	code := runStart([]string{
		"--repo", "x/y", "--workflow", "trivial",
		"--working-dir", t.TempDir(), // no .fishhawk/workflows.yaml
		"--backend-url", fake.srv.URL,
		"--token", "tok-test",
		"--applies-to-override-reason", "one-off backport",
	}, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("runStart exit = %d, want exitUsage\nstderr: %s", code, stderr.String())
	}
	if !bytes.Contains(stderr.Bytes(), []byte("applies-to-override-reason")) {
		t.Errorf("stderr does not name the offending flag: %s", stderr.String())
	}
	if n := fake.requests.Load(); n != 0 {
		t.Errorf("backend received %d requests, want 0 — a lone reason must never round-trip", n)
	}
}

// --- newClient credential ladder: proactive refresh (#2393 / ADR-076) --------

// withFakeCredLoad / withFakeCredRefresh swap newClient's credstore seams.
func withFakeCredLoad(t *testing.T, fn func(string) (credstore.Credential, error)) {
	t.Helper()
	orig := credLoad
	credLoad = fn
	t.Cleanup(func() { credLoad = orig })
}

func withFakeCredRefresh(t *testing.T, fn func(string) (credstore.Credential, error)) {
	t.Helper()
	orig := credRefresh
	credRefresh = fn
	t.Cleanup(func() { credRefresh = orig })
}

// cliExpiringRefreshable is a credential 90s from expiry on a 15m
// lifetime (inside the derived 2m skew) carrying everything a refresh
// needs.
func cliExpiringRefreshable(tokenEndpoint string) credstore.Credential {
	now := time.Now()
	issued := now.Add(-13*time.Minute - 30*time.Second)
	exp := issued.Add(15 * time.Minute)
	return credstore.Credential{
		Token:         "fho_stale",
		RefreshToken:  "fhr_seed",
		ClientID:      "fishhawk-cli",
		TokenEndpoint: tokenEndpoint,
		IssuedAt:      &issued,
		ExpiresAt:     &exp,
	}
}

func TestResolveStoredToken_MissingStoreDegradesSilently(t *testing.T) {
	withFakeCredLoad(t, func(string) (credstore.Credential, error) { return credstore.Credential{}, credstore.ErrNotFound })
	withFakeCredRefresh(t, func(string) (credstore.Credential, error) {
		t.Fatal("refresh must not run with no stored credential")
		return credstore.Credential{}, nil
	})
	tok, err := resolveStoredToken("http://localhost:8080", time.Now())
	if err != nil || tok != "" {
		t.Fatalf("missing store must degrade to (\"\", nil); got (%q, %v)", tok, err)
	}
}

func TestResolveStoredToken_OutsideSkewUsesStoredTokenWithoutRefresh(t *testing.T) {
	c := cliExpiringRefreshable("http://as/token")
	issued := time.Now().Add(-5 * time.Minute)
	exp := issued.Add(15 * time.Minute)
	c.IssuedAt, c.ExpiresAt = &issued, &exp
	withFakeCredLoad(t, func(string) (credstore.Credential, error) { return c, nil })
	withFakeCredRefresh(t, func(string) (credstore.Credential, error) {
		t.Fatal("refresh must not run outside the skew")
		return credstore.Credential{}, nil
	})
	tok, err := resolveStoredToken("http://localhost:8080", time.Now())
	if err != nil || tok != "fho_stale" {
		t.Fatalf("got (%q, %v), want the stored token with no refresh", tok, err)
	}
}

func TestResolveStoredToken_InsideSkewUsesRefreshedToken(t *testing.T) {
	withFakeCredLoad(t, func(string) (credstore.Credential, error) { return cliExpiringRefreshable("http://as/token"), nil })
	var refreshedURL string
	withFakeCredRefresh(t, func(u string) (credstore.Credential, error) {
		refreshedURL = u
		fresh := cliExpiringRefreshable("http://as/token")
		fresh.Token = "fho_fresh"
		exp := time.Now().Add(15 * time.Minute)
		fresh.ExpiresAt = &exp
		return fresh, nil
	})
	tok, err := resolveStoredToken("http://localhost:8080", time.Now())
	if err != nil || tok != "fho_fresh" {
		t.Fatalf("got (%q, %v), want the REFRESHED token", tok, err)
	}
	if refreshedURL != "http://localhost:8080" {
		t.Errorf("refresh called with %q, want the backend URL", refreshedURL)
	}
}

// A failed refresh is FATAL: the actionable login error, never the
// stale bearer and never an empty one.
func TestResolveStoredToken_RefreshFailureIsFatal(t *testing.T) {
	withFakeCredLoad(t, func(string) (credstore.Credential, error) { return cliExpiringRefreshable("http://as/token"), nil })
	withFakeCredRefresh(t, func(string) (credstore.Credential, error) {
		return credstore.Credential{}, &credstore.RefreshError{StatusCode: 400, Code: "invalid_grant"}
	})
	tok, err := resolveStoredToken("http://localhost:8080", time.Now())
	if err == nil {
		t.Fatalf("a failed refresh must be fatal; got token %q", tok)
	}
	if tok != "" {
		t.Errorf("must not hand back a token on failure; got %q", tok)
	}
	if !strings.Contains(err.Error(), "fishhawk token login --backend-url http://localhost:8080") {
		t.Errorf("error must name the login command with the backend URL; got %q", err)
	}
	if !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("error should carry the AS's code; got %q", err)
	}
}

// An expired credential with nothing to refresh with is also fatal —
// no silent degradation to a stale bearer that 401s.
func TestResolveStoredToken_ExpiredNotRefreshableIsFatal(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	withFakeCredLoad(t, func(string) (credstore.Credential, error) {
		return credstore.Credential{Token: "fhk_old", ExpiresAt: &past}, nil
	})
	withFakeCredRefresh(t, func(string) (credstore.Credential, error) {
		t.Fatal("a non-refreshable credential must never be refreshed")
		return credstore.Credential{}, nil
	})
	tok, err := resolveStoredToken("http://localhost:8080", time.Now())
	if err == nil || tok != "" {
		t.Fatalf("expired non-refreshable must be fatal; got (%q, %v)", tok, err)
	}
	if !strings.Contains(err.Error(), "expired at") || !strings.Contains(err.Error(), "fishhawk token login") {
		t.Errorf("error = %q, want the expiry and the login command", err)
	}
}

// A nil ExpiresAt (a device-flow fhk_ token) is non-expiring and used as-is.
func TestResolveStoredToken_NilExpiryAccepted(t *testing.T) {
	withFakeCredLoad(t, func(string) (credstore.Credential, error) { return credstore.Credential{Token: "fhk_live"}, nil })
	withFakeCredRefresh(t, func(string) (credstore.Credential, error) {
		t.Fatal("a non-expiring credential must never be refreshed")
		return credstore.Credential{}, nil
	})
	tok, err := resolveStoredToken("http://localhost:8080", time.Now())
	if err != nil || tok != "fhk_live" {
		t.Fatalf("got (%q, %v), want fhk_live", tok, err)
	}
}

// TestRunStatus_RefreshFailureIsFatalOnOrdinaryCLIPath is the binding-
// condition-1 behavioural test on the ORDINARY CLI path with the REAL
// credstore and the PRODUCTION refresh seam: a stored, expired,
// refreshable credential whose refresh the AS refuses makes `fishhawk
// run status` exit non-zero with the actionable login error on stderr,
// and the backend receives ZERO requests — no unauthenticated or
// stale-bearer call ever leaves the process.
func TestRunStatus_RefreshFailureIsFatalOnOrdinaryCLIPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FISHHAWK_TOKEN", "")
	backend := newStartRunCapture(t)
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid_grant","error_description":"refresh token revoked"}`))
	}))
	t.Cleanup(as.Close)

	c := cliExpiringRefreshable(as.URL + "/v0/oauth/token")
	issued := time.Now().Add(-time.Hour)
	exp := issued.Add(15 * time.Minute) // expired
	c.IssuedAt, c.ExpiresAt = &issued, &exp
	if err := credstore.Store(backend.srv.URL, c); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runStatus([]string{"--backend-url", backend.srv.URL, uuid.NewString()}, &stdout, &stderr)
	if code == exitOK {
		t.Fatalf("run status must fail when the stored credential cannot be refreshed; stdout: %s", stdout.String())
	}
	if !strings.Contains(stderr.String(), "fishhawk token login --backend-url "+backend.srv.URL) {
		t.Errorf("stderr must name the login command; got %q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "invalid_grant") {
		t.Errorf("stderr should carry the AS's refusal; got %q", stderr.String())
	}
	if n := backend.requests.Load(); n != 0 {
		t.Errorf("backend received %d requests, want 0 — the refusal must precede any dial", n)
	}
}

// The success half of the same ordinary path: a refreshable credential
// inside its skew is refreshed against the AS, the rotation is persisted
// to the real store, and the backend sees the NEW bearer.
func TestRunStatus_RefreshesAndPersistsOnOrdinaryCLIPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("FISHHAWK_TOKEN", "")
	var seenAuth atomic.Value
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	}))
	t.Cleanup(backend.Close)
	as := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.PostForm.Get("refresh_token") != "fhr_seed" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fho_rotated","token_type":"Bearer","expires_in":900,"refresh_token":"fhr_rotated"}`))
	}))
	t.Cleanup(as.Close)
	if err := credstore.Store(backend.URL, cliExpiringRefreshable(as.URL+"/v0/oauth/token")); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	_ = runStatus([]string{"--backend-url", backend.URL, uuid.NewString()}, &stdout, &stderr)
	if got, _ := seenAuth.Load().(string); got != "Bearer fho_rotated" {
		t.Fatalf("backend saw Authorization %q, want the refreshed bearer; stderr: %s", got, stderr.String())
	}
	stored, err := credstore.Load(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	if stored.RefreshToken != "fhr_rotated" || stored.Token != "fho_rotated" {
		t.Fatalf("rotation not persisted: %+v", stored)
	}
}
