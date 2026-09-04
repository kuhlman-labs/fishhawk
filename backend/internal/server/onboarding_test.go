package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergegate"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// onboardingReviewersSpecYAML is a valid feature_change spec whose plan stage
// declares a heterogeneous reviewers.agents list (anthropic + codex) so the
// readiness endpoint's reviewer-availability probe has tuples to enumerate.
const onboardingReviewersSpecYAML = `version: "1.0"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agents:
            - provider: anthropic
              model: claude-opus-4-8
            - provider: codex
              model: gpt-5.5
              reasoning_effort: high
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
`

// onboardingInvalidSpecYAML parses against the JSON schema but fails the
// semantic Validate layer: the plan gate's approvers.any_of references a role
// that is not defined at the top level.
const onboardingInvalidSpecYAML = `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
        gates:
          - type: approval
            approvers:
              any_of: [undefined_role]
            sla: 4_business_hours
      - id: implement
        type: implement
        executor:
          agent: claude-code
`

// onboardingMalformedSpecYAML is syntactically broken YAML (an unterminated
// flow mapping) so spec.ParseBytes fails at the YAML-decode layer — a distinct
// arm from onboardingInvalidSpecYAML, which parses but fails semantic Validate.
const onboardingMalformedSpecYAML = "version: \"1.0\"\nworkflows: {unterminated"

// newOnboardingServer builds a Server wired with an optional GitHub client
// (pointing at ghSrv) and an optional reviewer set — the only two
// dependencies the readiness endpoint touches. A nil ghSrv leaves cfg.GitHub
// nil (the "github client not configured" branch); a nil reviewers leaves
// cfg.PlanReviewers nil (the "no reviewer backend wired" branch).
func newOnboardingServer(t *testing.T, ghSrv *httptest.Server, reviewers ReviewerSet) *Server {
	t.Helper()
	cfg := Config{Addr: "127.0.0.1:0", PlanReviewers: reviewers}
	if ghSrv != nil {
		cfg.GitHub = &githubclient.Client{
			BaseURL: ghSrv.URL,
			Tokens:  &ghTokensStub{tok: "ghs_test"},
			HTTP:    &http.Client{Timeout: 5 * time.Second},
			AppJWT:  func() (string, error) { return "gha_app_jwt_test", nil },
		}
	}
	return New(cfg)
}

// onboardingReq builds a GET request for the readiness endpoint, injecting id
// as the caller identity (nil → anonymous). Handlers are invoked directly so
// the injected identity survives (s.Handler() would overwrite it via the auth
// middleware).
func onboardingReq(repo string, id *Identity) *http.Request {
	req := httptest.NewRequest(http.MethodGet,
		"/v0/onboarding/readiness?repo="+url.QueryEscape(repo), nil)
	if id != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, *id))
	}
	return req
}

// decodeReadiness runs the request through the handler and decodes the body.
func decodeReadiness(t *testing.T, s *Server, req *http.Request) (int, onboardingReadinessResponse) {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleGetOnboardingReadiness(w, req)
	var resp onboardingReadinessResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body: %v\n%s", err, w.Body.String())
		}
	}
	return w.Code, resp
}

// tokenIdentity is an authenticated bearer-token caller (non-empty TokenID)
// carrying the given scopes, for the scope-adequacy branch.
func tokenIdentity(scopes ...string) Identity {
	return Identity{Subject: "github:op", TokenID: "tok-1", Scopes: scopes}
}

// TestOnboardingReadiness_Anonymous asserts the auth-only gate: an anonymous
// caller is rejected 401 authentication_required (no write scope required).
func TestOnboardingReadiness_Anonymous(t *testing.T) {
	s := newOnboardingServer(t, nil, nil)
	w := httptest.NewRecorder()
	// No identity injected → IdentityFrom returns the zero (anonymous) value.
	s.handleGetOnboardingReadiness(w, onboardingReq("x/y", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if env.Error.Code != "authentication_required" {
		t.Errorf("code = %q, want authentication_required", env.Error.Code)
	}
}

// TestOnboardingReadiness_AnonymousThroughHandler proves the route is
// registered and the anonymous gate fires through the full middleware stack
// (401, not 404).
func TestOnboardingReadiness_AnonymousThroughHandler(t *testing.T) {
	s := newOnboardingServer(t, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/v0/onboarding/readiness?repo=x/y", nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (route registered, anon gated):\n%s", w.Code, w.Body.String())
	}
}

// TestOnboardingReadiness_MalformedRepo asserts a repo missing the owner/name
// separator is rejected 400 validation_failed.
func TestOnboardingReadiness_MalformedRepo(t *testing.T) {
	s := newOnboardingServer(t, nil, nil)
	id := testOperatorIdentity()
	for _, repo := range []string{"noslash", "", "/name", "owner/", "owner/name/extra"} {
		w := httptest.NewRecorder()
		s.handleGetOnboardingReadiness(w, onboardingReq(repo, &id))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("repo=%q status = %d, want 400:\n%s", repo, w.Code, w.Body.String())
		}
		var env errorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if env.Error.Code != "validation_failed" {
			t.Errorf("repo=%q code = %q, want validation_failed", repo, env.Error.Code)
		}
	}
}

// TestOnboardingReadiness_Installed asserts the installed-repo happy path:
// App.Installed true + InstallationID, the spec is fetched + valid, and the
// declared reviewers are enumerated with an AVAILABLE (For nil) verdict.
func TestOnboardingReadiness_Installed(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	ghSrv := fake.server(t)
	reviewers := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": &fakePlanReviewer{},
		"codex":     &fakePlanReviewer{},
	}}
	s := newOnboardingServer(t, ghSrv, reviewers)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !resp.App.Installed || resp.App.InstallationID != 12345 {
		t.Errorf("App = %+v, want Installed=true InstallationID=12345", resp.App)
	}
	if resp.Spec.Source != "fetched" || !resp.Spec.Valid || resp.Spec.Error != "" {
		t.Errorf("Spec = %+v, want fetched+valid", resp.Spec)
	}
	if len(resp.Reviewers) != 2 {
		t.Fatalf("len(Reviewers) = %d, want 2: %+v", len(resp.Reviewers), resp.Reviewers)
	}
	for _, rv := range resp.Reviewers {
		if !rv.Available || rv.MissingHint != "" {
			t.Errorf("reviewer %q Available=%v MissingHint=%q, want available", rv.Provider, rv.Available, rv.MissingHint)
		}
	}
	// Sorted by provider: anthropic before codex; codex carries reasoning_effort.
	if resp.Reviewers[0].Provider != "anthropic" || resp.Reviewers[1].Provider != "codex" {
		t.Errorf("reviewer order = %q,%q, want anthropic,codex", resp.Reviewers[0].Provider, resp.Reviewers[1].Provider)
	}
	if resp.Reviewers[1].ReasoningEffort != "high" {
		t.Errorf("codex ReasoningEffort = %q, want high", resp.Reviewers[1].ReasoningEffort)
	}
}

// TestOnboardingReadiness_ReviewerUnavailable asserts the per-reviewer
// capability gap: a provider absent from the reviewer set resolves to
// Available=false with a non-empty MissingHint naming the provider.
func TestOnboardingReadiness_ReviewerUnavailable(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	ghSrv := fake.server(t)
	// codex is NOT wired → For returns an error for it.
	reviewers := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": &fakePlanReviewer{},
	}}
	s := newOnboardingServer(t, ghSrv, reviewers)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	byProvider := map[string]reviewerReadiness{}
	for _, rv := range resp.Reviewers {
		byProvider[rv.Provider] = rv
	}
	if a := byProvider["anthropic"]; !a.Available || a.MissingHint != "" {
		t.Errorf("anthropic = %+v, want available", a)
	}
	c := byProvider["codex"]
	if c.Available {
		t.Errorf("codex Available = true, want false")
	}
	if c.MissingHint == "" {
		t.Errorf("codex MissingHint empty, want the unavailable-provider hint")
	}
}

// TestOnboardingReadiness_NoReviewerBackend asserts that when no reviewer
// backend is wired at all (PlanReviewers nil), every declared reviewer is
// unavailable with the wired-backend hint.
func TestOnboardingReadiness_NoReviewerBackend(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	ghSrv := fake.server(t)
	s := newOnboardingServer(t, ghSrv, nil) // no reviewer set

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if len(resp.Reviewers) != 2 {
		t.Fatalf("len(Reviewers) = %d, want 2", len(resp.Reviewers))
	}
	for _, rv := range resp.Reviewers {
		if rv.Available || rv.MissingHint == "" {
			t.Errorf("reviewer %q = %+v, want unavailable with a hint", rv.Provider, rv)
		}
	}
}

// TestOnboardingReadiness_NotInstalled asserts the not-installed cascade: the
// installation endpoint 404s → App.Installed false + Reason, and the spec
// check short-circuits to unavailable with the app-not-installed note; no
// reviewers are enumerated.
func TestOnboardingReadiness_NotInstalled(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	fake.installationStatus = http.StatusNotFound
	fake.installationBody = `{"message":"Not Found"}`
	ghSrv := fake.server(t)
	s := newOnboardingServer(t, ghSrv, fakeReviewerSet{providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}}})

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.App.Installed {
		t.Errorf("App.Installed = true, want false")
	}
	if resp.App.Reason == "" {
		t.Errorf("App.Reason empty, want a not-installed reason")
	}
	if resp.Spec.Source != "unavailable" || resp.Spec.Note == "" {
		t.Errorf("Spec = %+v, want unavailable with app-not-installed note", resp.Spec)
	}
	if len(resp.Reviewers) != 0 {
		t.Errorf("len(Reviewers) = %d, want 0 (spec unavailable)", len(resp.Reviewers))
	}
	// The spec endpoint must never be hit when the App is not installed.
	if fake.specCalls != 0 {
		t.Errorf("specCalls = %d, want 0 (short-circuit on not-installed)", fake.specCalls)
	}
}

// TestOnboardingReadiness_GitHubUnconfigured asserts that a nil GitHub client
// degrades App to not-installed with the not-configured reason rather than
// panicking or 500ing.
func TestOnboardingReadiness_GitHubUnconfigured(t *testing.T) {
	s := newOnboardingServer(t, nil, nil)
	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.App.Installed || resp.App.Reason == "" {
		t.Errorf("App = %+v, want not-installed with not-configured reason", resp.App)
	}
	if resp.Spec.Source != "unavailable" {
		t.Errorf("Spec.Source = %q, want unavailable", resp.Spec.Source)
	}
}

// TestOnboardingReadiness_InstallResolveError asserts a transient
// installation-resolve error (non-ErrNotInstalled) degrades to not-installed
// with the error as reason — never a 500.
func TestOnboardingReadiness_InstallResolveError(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	fake.installationStatus = http.StatusInternalServerError
	fake.installationBody = `{"message":"boom"}`
	ghSrv := fake.server(t)
	s := newOnboardingServer(t, ghSrv, nil)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (never 500 on resolve error):\n", code)
	}
	if resp.App.Installed || resp.App.Reason == "" {
		t.Errorf("App = %+v, want not-installed with error reason", resp.App)
	}
}

// TestOnboardingReadiness_SpecInvalid asserts a fetched spec that fails the
// semantic Validate layer surfaces Source=fetched, Valid=false, Error set, and
// no reviewers enumerated (the parsed spec is discarded on validate failure).
func TestOnboardingReadiness_SpecInvalid(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingInvalidSpecYAML)
	ghSrv := fake.server(t)
	s := newOnboardingServer(t, ghSrv, fakeReviewerSet{providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}}})

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Spec.Source != "fetched" {
		t.Errorf("Spec.Source = %q, want fetched", resp.Spec.Source)
	}
	if resp.Spec.Valid {
		t.Errorf("Spec.Valid = true, want false")
	}
	if resp.Spec.Error == "" {
		t.Errorf("Spec.Error empty, want the validation failure")
	}
	if len(resp.Reviewers) != 0 {
		t.Errorf("len(Reviewers) = %d, want 0 (invalid spec)", len(resp.Reviewers))
	}
}

// TestOnboardingReadiness_SpecMalformed asserts a fetched but syntactically
// malformed spec fails at the spec.ParseBytes (YAML-decode) arm — distinct from
// the Validate arm SpecInvalid drives — surfacing Source=fetched, Valid=false,
// Error set, and no reviewers enumerated (nil parsedSpec).
func TestOnboardingReadiness_SpecMalformed(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingMalformedSpecYAML)
	ghSrv := fake.server(t)
	s := newOnboardingServer(t, ghSrv, fakeReviewerSet{providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}}})

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Spec.Source != "fetched" {
		t.Errorf("Spec.Source = %q, want fetched", resp.Spec.Source)
	}
	if resp.Spec.Valid {
		t.Errorf("Spec.Valid = true, want false (ParseBytes failure)")
	}
	if resp.Spec.Error == "" {
		t.Errorf("Spec.Error empty, want the parse failure")
	}
	if len(resp.Reviewers) != 0 {
		t.Errorf("len(Reviewers) = %d, want 0 (unparseable spec)", len(resp.Reviewers))
	}
}

// TestOnboardingReadiness_SpecFetchError asserts a generic (non-ErrNotFound)
// spec-fetch failure — here a 500 from the contents endpoint — degrades spec to
// unavailable with the error as Note (the default switch arm), not a 404 note
// and never a hard failure.
func TestOnboardingReadiness_SpecFetchError(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	fake.specStatus = http.StatusInternalServerError
	fake.specBody = `{"message":"boom"}`
	ghSrv := fake.server(t)
	s := newOnboardingServer(t, ghSrv, nil)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (never 500 on fetch error)", code)
	}
	if !resp.App.Installed {
		t.Fatalf("App.Installed = false, want true (installation OK)")
	}
	if resp.Spec.Source != "unavailable" || resp.Spec.Note == "" {
		t.Errorf("Spec = %+v, want unavailable with the fetch-error note", resp.Spec)
	}
	if len(resp.Reviewers) != 0 {
		t.Errorf("len(Reviewers) = %d, want 0 (spec unavailable)", len(resp.Reviewers))
	}
}

// TestOnboardingReadiness_SpecNotFound asserts a 404 on the contents endpoint
// (ErrNotFound) degrades spec to unavailable with a default-branch note, not a
// hard failure.
func TestOnboardingReadiness_SpecNotFound(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	fake.specStatus = http.StatusNotFound
	fake.specBody = `{"message":"Not Found"}`
	ghSrv := fake.server(t)
	s := newOnboardingServer(t, ghSrv, nil)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !resp.App.Installed {
		t.Fatalf("App.Installed = false, want true (installation OK)")
	}
	if resp.Spec.Source != "unavailable" || resp.Spec.Note == "" {
		t.Errorf("Spec = %+v, want unavailable with a not-found note", resp.Spec)
	}
}

// TestOnboardingReadiness_ScopeMissing asserts a token caller lacking
// write:runs is reported inadequate with the gap in Missing, while a caller
// carrying the full run-drive set is adequate with an empty Missing.
func TestOnboardingReadiness_ScopeMissing(t *testing.T) {
	s := newOnboardingServer(t, nil, nil)

	// Full run-drive set minus write:runs.
	partial := tokenIdentity("read:runs", "read:audit", "write:approvals", "write:stages")
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &partial))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Scopes.Adequate {
		t.Errorf("Scopes.Adequate = true, want false")
	}
	if len(resp.Scopes.Missing) != 1 || resp.Scopes.Missing[0] != "write:runs" {
		t.Errorf("Scopes.Missing = %v, want [write:runs]", resp.Scopes.Missing)
	}
	if len(resp.Scopes.Required) != len(requiredRunScopes) {
		t.Errorf("Scopes.Required = %v, want %v", resp.Scopes.Required, requiredRunScopes)
	}

	// Full set → adequate, empty Missing.
	full := tokenIdentity(requiredRunScopes...)
	code, resp = decodeReadiness(t, s, onboardingReq("x/y", &full))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !resp.Scopes.Adequate {
		t.Errorf("Scopes.Adequate = false, want true")
	}
	if len(resp.Scopes.Missing) != 0 {
		t.Errorf("Scopes.Missing = %v, want empty", resp.Scopes.Missing)
	}
}

// TestOnboardingReadiness_CookieSessionScopeBypass asserts a cookie-session
// caller (empty TokenID) is adequate by construction with a bypass note,
// mirroring requireWriteScope's OAuth-session bypass.
func TestOnboardingReadiness_CookieSessionScopeBypass(t *testing.T) {
	s := newOnboardingServer(t, nil, nil)
	id := testOperatorIdentity() // TokenID == ""
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !resp.Scopes.Adequate {
		t.Errorf("Scopes.Adequate = false, want true (cookie-session bypass)")
	}
	if resp.Scopes.Note == "" {
		t.Errorf("Scopes.Note empty, want a bypass note")
	}
	if len(resp.Scopes.Missing) != 0 {
		t.Errorf("Scopes.Missing = %v, want empty", resp.Scopes.Missing)
	}
}

// --- Repo read-visibility gate (#1512, ADR-057 Amendment A2 / #2071) ---

// newOnboardingVisServer builds an onboarding Server with the repo-visibility
// seams wired (mirror + account-role provider + repo-provider resolver) plus an
// optional GitHub fake. It is the fixture for the #1512 point-read gate tests:
// a nil ghSrv leaves cfg.GitHub nil, so the denied path (which short-circuits
// before any forge call) needs no GitHub wiring, while an admitted path wires
// ghSrv so the full 200 aggregate can be asserted.
func newOnboardingVisServer(t *testing.T, ghSrv *httptest.Server, vis RepoVisibility, roles AccountRoles, providers ProviderResolver, reviewers ReviewerSet) *Server {
	t.Helper()
	cfg := Config{
		Addr:           "127.0.0.1:0",
		RepoVisibility: vis,
		AccountRoles:   roles,
		RepoProviders:  providers,
		PlanReviewers:  reviewers,
	}
	if ghSrv != nil {
		cfg.GitHub = &githubclient.Client{
			BaseURL: ghSrv.URL,
			Tokens:  &ghTokensStub{tok: "ghs_test"},
			HTTP:    &http.Client{Timeout: 5 * time.Second},
			AppJWT:  func() (string, error) { return "gha_app_jwt_test", nil },
		}
	}
	return New(cfg)
}

// runOnboarding invokes the handler directly with id injected and returns the
// recorder so a test can assert on the raw body (the denied path is an error
// envelope, not a decodable onboardingReadinessResponse).
func runOnboarding(s *Server, repo string, id Identity) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.handleGetOnboardingReadiness(w, onboardingReq(repo, &id))
	return w
}

// TestOnboardingReadiness_RepoNotVisible is the primary control (#1512) and the
// counterfactual vehicle: a non-admin cookie session querying a repo the mirror
// denies gets 403 repo_forbidden BEFORE any forge call, and no spec parse/
// validation text reaches the caller. The deny is seeded BY CONSTRUCTION — the
// fake mirror's default answer for an unlisted repo is false — so the RED lands
// on the behavioral assertion, not on fixture setup.
func TestOnboardingReadiness_RepoNotVisible(t *testing.T) {
	// ghSrv is wired so a MISSING short-circuit would reach GitHub and bump the
	// call counters; the gate must keep them at zero.
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	ghSrv := fake.server(t)
	vis := newFakeRepoVisibility(map[string]bool{}) // "x/y" absent → not visible
	s := newOnboardingVisServer(t, ghSrv, vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

	w := runOnboarding(s, "x/y", memberIdentity())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "repo_forbidden" {
		t.Errorf("error.code = %q, want repo_forbidden", code)
	}
	body := w.Body.String()
	if strings.Contains(body, "\"spec\"") || strings.Contains(body, "installation") {
		t.Errorf("denied body leaks readiness state:\n%s", body)
	}
	if fake.installationCalls != 0 || fake.specCalls != 0 {
		t.Errorf("forge calls = install:%d spec:%d, want 0/0 (short-circuit before any forge call)",
			fake.installationCalls, fake.specCalls)
	}
}

// TestOnboardingReadiness_RepoVisible is the admission control: the same
// non-admin cookie identity on a repo the mirror ALLOWS gets the full
// pre-change 200 surface. Without it a guard that denied everything would still
// pass RepoNotVisible.
func TestOnboardingReadiness_RepoVisible(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	ghSrv := fake.server(t)
	reviewers := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": &fakePlanReviewer{},
		"codex":     &fakePlanReviewer{},
	}}
	vis := newFakeRepoVisibility(map[string]bool{"x/y": true})
	s := newOnboardingVisServer(t, ghSrv, vis, fakeAccountRoles{role: account.RoleMember}, nil, reviewers)

	mid := memberIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &mid))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !resp.App.Installed || resp.App.InstallationID != 12345 {
		t.Errorf("App = %+v, want Installed=true InstallationID=12345", resp.App)
	}
	if resp.Spec.Source != "fetched" || !resp.Spec.Valid {
		t.Errorf("Spec = %+v, want fetched+valid", resp.Spec)
	}
	if len(resp.Reviewers) != 2 {
		t.Errorf("len(Reviewers) = %d, want 2", len(resp.Reviewers))
	}
}

// TestOnboardingReadiness_BearerTokenUnfiltered: a bearer/MCP identity
// (TokenID != "") is UNFILTERED even on a repo the mirror would deny — the
// fishhawk doctor / fishhawk_doctor MCP posture. 200, mirror never asked.
func TestOnboardingReadiness_BearerTokenUnfiltered(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	ghSrv := fake.server(t)
	vis := newFakeRepoVisibility(map[string]bool{}) // would deny x/y
	s := newOnboardingVisServer(t, ghSrv, vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

	// Bearer identity: Subject "github:op", TokenID "tok-1".
	bid := tokenIdentity("read:runs")
	code, _ := decodeReadiness(t, s, onboardingReq("x/y", &bid))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (bearer identity unfiltered)", code)
	}
	if vis.callCount() != 0 {
		t.Errorf("mirror Visible calls = %d, want 0 (bearer never filtered)", vis.callCount())
	}
}

// TestOnboardingReadiness_AdminCookieBypass: a cookie session whose AccountRoles
// resolves RoleAdmin bypasses filtering on a repo the mirror would deny → 200,
// mirror never asked. This is what makes the NON-ADMIN qualification in every
// doc surface true rather than merely asserted.
func TestOnboardingReadiness_AdminCookieBypass(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	ghSrv := fake.server(t)
	vis := newFakeRepoVisibility(map[string]bool{}) // would deny x/y
	s := newOnboardingVisServer(t, ghSrv, vis, fakeAccountRoles{role: account.RoleAdmin}, nil, nil)

	mid := memberIdentity()
	code, _ := decodeReadiness(t, s, onboardingReq("x/y", &mid))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (admin cookie bypasses filtering)", code)
	}
	if vis.callCount() != 0 {
		t.Errorf("mirror Visible calls = %d, want 0 (admin bypass, mirror never asked)", vis.callCount())
	}
}

// TestOnboardingReadiness_NoMirrorWired: with Config.RepoVisibility == nil the
// endpoint keeps its exact pre-change surface (200), the untenanted-allow
// posture (repoFilterFor's first early return).
func TestOnboardingReadiness_NoMirrorWired(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	ghSrv := fake.server(t)
	s := newOnboardingVisServer(t, ghSrv, nil, fakeAccountRoles{role: account.RoleMember}, nil, nil)

	mid := memberIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &mid))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no mirror wired)", code)
	}
	if !resp.App.Installed {
		t.Errorf("App.Installed = false, want true (pre-change surface preserved)")
	}
}

// TestOnboardingReadiness_VisibilityStoreFault: a mirror STORE fault (Visible
// returns a non-nil error) is 503 service_unavailable — never 403 and never
// 200. The store-fault class must not collapse into the permission-denied class.
func TestOnboardingReadiness_VisibilityStoreFault(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{})
	vis.err = errors.New("mirror store unreachable")
	s := newOnboardingVisServer(t, nil, vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

	w := runOnboarding(s, "x/y", memberIdentity())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "service_unavailable" {
		t.Errorf("error.code = %q, want service_unavailable", code)
	}
}

// TestOnboardingReadiness_RoleResolutionFault: an AccountRoles.MemberRole error
// surfaces as 503 (repoFilterFor propagates it rather than bypassing/denying).
func TestOnboardingReadiness_RoleResolutionFault(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{"x/y": true})
	roles := fakeAccountRoles{role: account.RoleMember, err: errors.New("role store down")}
	s := newOnboardingVisServer(t, nil, vis, roles, nil, nil)

	w := runOnboarding(s, "x/y", memberIdentity())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "service_unavailable" {
		t.Errorf("error.code = %q, want service_unavailable", code)
	}
}

// TestOnboardingReadiness_ProviderResolutionFault: a RepoProviders.
// ResolveProvider error surfaces as 503.
func TestOnboardingReadiness_ProviderResolutionFault(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{"x/y": true})
	providers := &fakeProviderResolver{err: errors.New("provider store down")}
	s := newOnboardingVisServer(t, nil, vis, fakeAccountRoles{role: account.RoleMember}, providers, nil)

	w := runOnboarding(s, "x/y", memberIdentity())
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "service_unavailable" {
		t.Errorf("error.code = %q, want service_unavailable", code)
	}
}

// TestOnboardingReadiness_CrossForgeDeny: a resolver answering a forge different
// from the caller's (caller github:alice, row gitlab) denies 403 with ZERO
// forge calls AND zero mirror Visible calls.
func TestOnboardingReadiness_CrossForgeDeny(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	ghSrv := fake.server(t)
	vis := newFakeRepoVisibility(map[string]bool{"x/y": true}) // would ALLOW if reached
	providers := &fakeProviderResolver{provider: "gitlab", found: true}
	s := newOnboardingVisServer(t, ghSrv, vis, fakeAccountRoles{role: account.RoleMember}, providers, nil)

	w := runOnboarding(s, "x/y", memberIdentity())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "repo_forbidden" {
		t.Errorf("error.code = %q, want repo_forbidden", code)
	}
	if vis.callCount() != 0 {
		t.Errorf("mirror Visible calls = %d, want 0 (cross-forge short-circuits)", vis.callCount())
	}
	if fake.installationCalls != 0 || fake.specCalls != 0 {
		t.Errorf("forge calls = install:%d spec:%d, want 0/0", fake.installationCalls, fake.specCalls)
	}
}

// TestOnboardingReadiness_AmbiguousRowForgeDeny: a resolver answering found=false
// (owner unregistered or dual-registered) fails CLOSED → 403.
func TestOnboardingReadiness_AmbiguousRowForgeDeny(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{"x/y": true}) // would ALLOW if reached
	providers := &fakeProviderResolver{found: false}
	s := newOnboardingVisServer(t, nil, vis, fakeAccountRoles{role: account.RoleMember}, providers, nil)

	w := runOnboarding(s, "x/y", memberIdentity())
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "repo_forbidden" {
		t.Errorf("error.code = %q, want repo_forbidden", code)
	}
	if vis.callCount() != 0 {
		t.Errorf("mirror Visible calls = %d, want 0 (ambiguous row short-circuits)", vis.callCount())
	}
}

// TestOnboardingReadiness_PrefixlessSubjectDenyAll: a cookie subject with no
// "<provider>:" prefix cannot be keyed into the mirror, so repoFilterFor returns
// a deny-all filter → 403, mirror never asked.
func TestOnboardingReadiness_PrefixlessSubjectDenyAll(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{"x/y": true}) // would ALLOW if reached
	s := newOnboardingVisServer(t, nil, vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

	id := memberIdentity()
	id.Subject = "alice" // no provider prefix
	w := runOnboarding(s, "x/y", id)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "repo_forbidden" {
		t.Errorf("error.code = %q, want repo_forbidden", code)
	}
	if vis.callCount() != 0 {
		t.Errorf("mirror Visible calls = %d, want 0 (deny-all never asks the mirror)", vis.callCount())
	}
}

// TestOnboardingReadiness_AnonymousBeforeVisibility pins the ordering invariant:
// an anonymous request against a store-faulting mirror still gets 401
// authentication_required, not 503. Note the honest scope (CONDITION 2): the
// visibility gate sits AFTER the handler's anonymous check, and repoFilterFor
// ALSO short-circuits anonymous callers (returning a nil filter before the
// mirror is ever consulted), so the mirror fault is unreachable for an anonymous
// caller by two independent mechanisms. This test therefore documents the
// handler-level ordering it can actually observe — anonymous is gated by auth,
// never by the mirror — rather than serving as a strict hoist counterfactual.
func TestOnboardingReadiness_AnonymousBeforeVisibility(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{})
	vis.err = errors.New("mirror store unreachable")
	s := newOnboardingVisServer(t, nil, vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

	w := httptest.NewRecorder()
	s.handleGetOnboardingReadiness(w, onboardingReq("x/y", nil)) // anonymous
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (auth precedes visibility):\n%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "authentication_required" {
		t.Errorf("error.code = %q, want authentication_required", code)
	}
	if vis.callCount() != 0 {
		t.Errorf("mirror Visible calls = %d, want 0 (never consulted for anonymous)", vis.callCount())
	}
}

// TestOnboardingReadiness_MalformedRepoBeforeVisibility pins the 400-before-403
// ordering: an authenticated non-admin caller sending repo="owner/name/extra"
// against a deny-all mirror still gets 400 validation_failed, and the mirror's
// Visible is never called — the filter must never be handed a malformed key. If
// the guard were hoisted above the format check this would go 403 (or ask the
// mirror the malformed key), so the test discriminates.
func TestOnboardingReadiness_MalformedRepoBeforeVisibility(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{}) // denies everything
	s := newOnboardingVisServer(t, nil, vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

	w := runOnboarding(s, "owner/name/extra", memberIdentity())
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (format check precedes visibility):\n%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "validation_failed" {
		t.Errorf("error.code = %q, want validation_failed", code)
	}
	if vis.callCount() != 0 {
		t.Errorf("mirror Visible calls = %d, want 0 (malformed key never reaches the mirror)", vis.callCount())
	}
}

// TestCollectSpecReviewers_Dedup asserts distinct (provider, model, effort)
// tuples are collected once across stages/workflows, de-duped by the composite
// key and returned in sorted order.
func TestCollectSpecReviewers_Dedup(t *testing.T) {
	sp := &spec.Spec{
		Workflows: map[string]spec.Workflow{
			"feature_change": {
				Stages: []spec.Stage{
					{
						ID:   "plan",
						Type: spec.StageTypePlan,
						Reviewers: &spec.ReviewersConfig{Agents: []spec.AgentReviewer{
							{Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "high"},
							{Provider: "anthropic", Model: "claude-opus-4-8"},
						}},
					},
					{
						ID:   "implement",
						Type: spec.StageTypeImplement,
						Reviewers: &spec.ReviewersConfig{Agents: []spec.AgentReviewer{
							// Duplicate of the plan-stage codex tuple → collapses.
							{Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "high"},
							// Same provider+model, different effort → distinct.
							{Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "low"},
						}},
					},
					{
						ID:        "noreview",
						Type:      spec.StageTypeImplement,
						Reviewers: nil, // nil reviewers block is skipped.
					},
				},
			},
		},
	}
	got := collectSpecReviewers(sp)
	want := []spec.AgentReviewer{
		{Provider: "anthropic", Model: "claude-opus-4-8"},
		{Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "high"},
		{Provider: "codex", Model: "gpt-5.5", ReasoningEffort: "low"},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("tuple[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// --- merge gate readiness, check (5) (#3161) ---
//
// The fixture below is a full GitHub stub — installation, contents, repository
// metadata, classic branch protection and both ruleset endpoints — so these
// tests drive the REAL handler through the REAL githubclient decode and the
// REAL mergegate reconciliation. That is deliberate: scope.files for this
// change spans the forge decode, the reconciliation engine, the HTTP response
// and two client mirrors, so a per-layer unit test is insufficient (cf. #618)
// — a break anywhere between the ruleset JSON and the response's json tag
// fails one of these.

// mergeGateFixture is a GitHub stub covering every endpoint the readiness
// endpoint touches. Zero values are filled in by newMergeGateFixture; each
// field is a knob one degrade test flips.
type mergeGateFixture struct {
	defaultBranch string

	repoStatus int
	repoBody   string

	// protection is keyed by BRANCH NAME: a branch absent from the map draws
	// a 404, which githubclient maps to ErrNotFound ("no classic protection").
	// Keying by branch is what makes the default-branch resolve observable.
	protection       map[string]string
	protectionStatus int
	protectionHang   time.Duration

	rulesetsStatus int
	rulesetsList   string
	// rulesetBodies is keyed by ruleset id as a string.
	rulesetBodies map[string]string

	calls struct {
		repo        int
		protection  int
		rulesetList int
	}
	// protectionBranches records every branch the protection endpoint was
	// asked about, so a test can assert WHICH branch was probed.
	protectionBranches []string
}

// newMergeGateFixture returns a fixture whose default posture is: App
// installed, spec valid, default branch "main", no classic protection, one
// active branch ruleset (id 42) covering ~DEFAULT_BRANCH and requiring
// fishhawk_audit_complete with no bypass entries.
func newMergeGateFixture() *mergeGateFixture {
	return &mergeGateFixture{
		defaultBranch:    "main",
		repoStatus:       http.StatusOK,
		protection:       map[string]string{},
		protectionStatus: http.StatusOK,
		rulesetsStatus:   http.StatusOK,
		rulesetsList:     `[{"id":42,"target":"branch","enforcement":"active"}]`,
		rulesetBodies: map[string]string{
			"42": mergeGateRulesetBody([]string{"~DEFAULT_BRANCH"}, []string{"fishhawk_audit_complete"}, 0),
		},
	}
}

// mergeGateRulesetBody renders a repository-ruleset JSON body with the given
// ref_name includes, required-status-check contexts and bypass_actors count —
// the recorded shape of GET /repos/{o}/{r}/rulesets/{id}.
func mergeGateRulesetBody(include, contexts []string, bypassActors int) string {
	quoted := func(vals []string) string {
		out := make([]string, 0, len(vals))
		for _, v := range vals {
			out = append(out, `"`+v+`"`)
		}
		return strings.Join(out, ",")
	}
	checks := make([]string, 0, len(contexts))
	for _, c := range contexts {
		checks = append(checks, `{"context":"`+c+`"}`)
	}
	actors := make([]string, 0, bypassActors)
	for i := 0; i < bypassActors; i++ {
		actors = append(actors,
			`{"actor_id":`+strconv.Itoa(i+1)+`,"actor_type":"Team","bypass_mode":"always"}`)
	}
	return `{"bypass_actors":[` + strings.Join(actors, ",") + `],` +
		`"conditions":{"ref_name":{"include":[` + quoted(include) + `],"exclude":[]}},` +
		`"rules":[{"type":"required_status_checks","parameters":{"required_status_checks":[` +
		strings.Join(checks, ",") + `]}}]}`
}

// mergeGateProtectionBody renders a classic branch-protection JSON body — the
// recorded shape of GET /repos/{o}/{r}/branches/{b}/protection.
func mergeGateProtectionBody(contexts []string, enforceAdmins bool) string {
	quoted := make([]string, 0, len(contexts))
	for _, c := range contexts {
		quoted = append(quoted, `"`+c+`"`)
	}
	return `{"required_status_checks":{"contexts":[` + strings.Join(quoted, ",") + `]},` +
		`"enforce_admins":{"enabled":` + strconv.FormatBool(enforceAdmins) + `}}`
}

// server wires the fixture onto an httptest mux. The spec + installation
// endpoints reuse the same canned bodies the other readiness tests use, so
// these tests only vary the protection surfaces.
func (f *mergeGateFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":12345}`)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, specContentsBody(onboardingReviewersSpecYAML))
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/branches/{branch}/protection", func(w http.ResponseWriter, r *http.Request) {
		f.calls.protection++
		branch := r.PathValue("branch")
		f.protectionBranches = append(f.protectionBranches, branch)
		if f.protectionHang > 0 {
			// Outlive the (test-shrunk) probe timeout so the ctx deadline
			// fires inside the reconciliation.
			select {
			case <-time.After(f.protectionHang):
			case <-r.Context().Done():
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if f.protectionStatus != http.StatusOK {
			w.WriteHeader(f.protectionStatus)
			_, _ = io.WriteString(w, `{"message":"nope"}`)
			return
		}
		body, ok := f.protection[branch]
		if !ok {
			// GitHub 404s a branch with no classic protection — not an error.
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Branch not protected"}`)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/rulesets/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, ok := f.rulesetBodies[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"message":"Not Found"}`)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/rulesets", func(w http.ResponseWriter, _ *http.Request) {
		f.calls.rulesetList++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.rulesetsStatus)
		if f.rulesetsStatus != http.StatusOK {
			_, _ = io.WriteString(w, `{"message":"nope"}`)
			return
		}
		_, _ = io.WriteString(w, f.rulesetsList)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}", func(w http.ResponseWriter, _ *http.Request) {
		f.calls.repo++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.repoStatus)
		switch {
		case f.repoBody != "":
			_, _ = io.WriteString(w, f.repoBody)
		case f.repoStatus != http.StatusOK:
			_, _ = io.WriteString(w, `{"message":"nope"}`)
		default:
			_, _ = io.WriteString(w, `{"default_branch":"`+f.defaultBranch+`"}`)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// specContentsBody renders the base64 contents payload the spec fetch decodes.
func specContentsBody(yaml string) string {
	return `{"path":".fishhawk/workflows.yaml","content":"` +
		base64.StdEncoding.EncodeToString([]byte(yaml)) + `","encoding":"base64","sha":"deadbeef"}`
}

// mergeGateReadinessFor drives the handler against the fixture and returns the
// decoded merge_gate object.
func mergeGateReadinessFor(t *testing.T, f *mergeGateFixture) mergeGateReadiness {
	t.Helper()
	s := newOnboardingServer(t, f.server(t), nil)
	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	return resp.MergeGate
}

// TestOnboardingReadiness_MergeGate_EndToEnd is the CROSS-BOUNDARY test. It
// drives the real handler over a GitHub stub serving actual protection and
// ruleset JSON, and asserts the DECODED HTTP response's merge_gate object.
//
// The fixture is the conjunction case binding condition 1 turns on: TWO
// independent sources require the check — classic protection with
// enforce_admins TRUE (not bypassable) and a ruleset with two bypass entries
// (bypassable). A summed bypass model would report the gate as bypassable;
// the conjunction reports it as NOT bypassable, because a merger has to get
// past both.
func TestOnboardingReadiness_MergeGate_EndToEnd(t *testing.T) {
	f := newMergeGateFixture()
	f.protection["main"] = mergeGateProtectionBody(
		[]string{"fishhawk_audit_complete", "ci"}, true)
	f.rulesetBodies["42"] = mergeGateRulesetBody(
		[]string{"~DEFAULT_BRANCH"}, []string{"fishhawk_audit_complete"}, 2)

	mg := mergeGateReadinessFor(t, f)

	if mg.Status != "required" {
		t.Fatalf("Status = %q, want required (detail=%q reason=%q)", mg.Status, mg.Detail, mg.Reason)
	}
	if mg.Check != "fishhawk_audit_complete" {
		t.Errorf("Check = %q, want fishhawk_audit_complete", mg.Check)
	}
	if mg.Branch != "main" {
		t.Errorf("Branch = %q, want main", mg.Branch)
	}
	if !mg.Authoritative {
		t.Errorf("Authoritative = false, want true (both surfaces answered)")
	}
	if len(mg.Sources) != 2 {
		t.Fatalf("len(Sources) = %d, want 2: %+v", len(mg.Sources), mg.Sources)
	}
	classic, ruleset := mg.Sources[0], mg.Sources[1]
	if classic.Identity != "branch_protection" || !classic.Classic {
		t.Errorf("Sources[0] = %+v, want the classic branch_protection source", classic)
	}
	if !classic.EnforceAdmins {
		t.Errorf("classic EnforceAdmins = false, want true (decoded from enforce_admins.enabled)")
	}
	if classic.Bypassable {
		t.Errorf("classic Bypassable = true, want false (enforce_admins is on)")
	}
	if classic.BypassEntries != 0 {
		t.Errorf("classic BypassEntries = %d, want 0 (the admin exemption is never a count)", classic.BypassEntries)
	}
	if ruleset.Identity != "ruleset:42" {
		t.Errorf("Sources[1].Identity = %q, want ruleset:42", ruleset.Identity)
	}
	if ruleset.BypassEntries != 2 {
		t.Errorf("ruleset BypassEntries = %d, want 2 (decoded from bypass_actors)", ruleset.BypassEntries)
	}
	if !ruleset.Bypassable {
		t.Errorf("ruleset Bypassable = false, want true (it carries bypass entries)")
	}
	// The conjunction: one bypassable source does NOT make the gate bypassable.
	if mg.Bypassable {
		t.Errorf("Bypassable = true, want false: classic enforces with no bypass path, "+
			"so the gate holds regardless of the ruleset's %d bypass entries", ruleset.BypassEntries)
	}
	if !containsString(mg.RequiredContexts, "ci") {
		t.Errorf("RequiredContexts = %v, want it to carry the sibling 'ci' context", mg.RequiredContexts)
	}
}

// TestOnboardingReadiness_MergeGate_NoGitHubClient_Unknown asserts the
// no-client degrade: unknown with the github_client_unconfigured reason, never
// not_required.
func TestOnboardingReadiness_MergeGate_NoGitHubClient_Unknown(t *testing.T) {
	s := newOnboardingServer(t, nil, nil)
	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.MergeGate.Status != "unknown" {
		t.Fatalf("Status = %q, want unknown", resp.MergeGate.Status)
	}
	if resp.MergeGate.Reason != mergeGateReasonNoGitHubClient {
		t.Errorf("Reason = %q, want %q", resp.MergeGate.Reason, mergeGateReasonNoGitHubClient)
	}
	if resp.MergeGate.Detail == "" {
		t.Errorf("Detail empty, want a naming sentence")
	}
	if resp.MergeGate.Check != "fishhawk_audit_complete" {
		t.Errorf("Check = %q, want the probed context even on a degrade", resp.MergeGate.Check)
	}
}

// TestOnboardingReadiness_MergeGate_AppNotInstalled_Unknown asserts the
// not-installed degrade: both protection reads need an installation token, so
// the probe cannot run — unknown, and NO forge protection call is made.
func TestOnboardingReadiness_MergeGate_AppNotInstalled_Unknown(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	fake.installationStatus = http.StatusNotFound
	fake.installationBody = `{"message":"Not Found"}`
	s := newOnboardingServer(t, fake.server(t), nil)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.MergeGate.Status != "unknown" {
		t.Fatalf("Status = %q, want unknown", resp.MergeGate.Status)
	}
	if resp.MergeGate.Reason != mergeGateReasonAppNotInstalled {
		t.Errorf("Reason = %q, want %q", resp.MergeGate.Reason, mergeGateReasonAppNotInstalled)
	}
	if resp.MergeGate.Remediation == "" {
		t.Errorf("Remediation empty, want the install-the-App step")
	}
}

// TestOnboardingReadiness_MergeGate_DefaultBranchLookupFails_Unknown asserts
// the default-branch degrade: a 500 from GET /repos/{o}/{r} yields unknown
// with the default_branch_unresolved reason, and the probe never falls back to
// guessing "main" (the protection endpoint is never called).
func TestOnboardingReadiness_MergeGate_DefaultBranchLookupFails_Unknown(t *testing.T) {
	f := newMergeGateFixture()
	f.repoStatus = http.StatusInternalServerError

	mg := mergeGateReadinessFor(t, f)
	if mg.Status != "unknown" {
		t.Fatalf("Status = %q, want unknown", mg.Status)
	}
	if mg.Reason != mergeGateReasonDefaultBranch {
		t.Errorf("Reason = %q, want %q", mg.Reason, mergeGateReasonDefaultBranch)
	}
	if f.calls.protection != 0 {
		t.Errorf("protection endpoint called %d times, want 0 (no fallback to a guessed branch)",
			f.calls.protection)
	}
}

// TestOnboardingReadiness_MergeGate_NilRepositoryMetadata_Unknown drives the
// defensive guard the concrete client cannot produce: githubclient's
// GetRepository errors when `default_branch` is absent, so a (nil, nil) return
// is unreachable through it — but removing the guard would leave a nil
// dereference one interface swap away. The seam seeds that return BY
// CONSTRUCTION, so the RED lands on the status assertion.
func TestOnboardingReadiness_MergeGate_NilRepositoryMetadata_Unknown(t *testing.T) {
	orig := mergeGateRepository
	mergeGateRepository = func(_ context.Context, _ *githubclient.Client, _ forge.CredentialScope,
		_ githubclient.RepoRef) (*githubclient.Repository, error) {
		return nil, nil
	}
	t.Cleanup(func() { mergeGateRepository = orig })

	mg := mergeGateReadinessFor(t, newMergeGateFixture())
	if mg.Status != "unknown" {
		t.Fatalf("Status = %q, want unknown", mg.Status)
	}
	if mg.Reason != mergeGateReasonDefaultBranch {
		t.Errorf("Reason = %q, want %q", mg.Reason, mergeGateReasonDefaultBranch)
	}
}

// TestOnboardingReadiness_MergeGate_ReconcileRejects_Unknown drives the
// caller-error branch of the reconciliation. mergegate.Reconcile reserves its
// error return for caller mistakes, all of which probeMergeGate excludes, so
// the branch is unreachable in production — the seam seeds the error BY
// CONSTRUCTION to prove it fails closed rather than rendering a verdict.
func TestOnboardingReadiness_MergeGate_ReconcileRejects_Unknown(t *testing.T) {
	orig := mergeGateReconcile
	mergeGateReconcile = func(_ context.Context, _ mergegate.ProtectionAPI, _ forge.CredentialScope,
		_ forge.RepoRef, _, _, _ string) (mergegate.Reconciliation, error) {
		return mergegate.Reconciliation{}, errors.New("mergegate: nil ProtectionAPI")
	}
	t.Cleanup(func() { mergeGateReconcile = orig })

	mg := mergeGateReadinessFor(t, newMergeGateFixture())
	if mg.Status != "unknown" {
		t.Fatalf("Status = %q, want unknown", mg.Status)
	}
	if mg.Reason != mergeGateReasonProbeFailed {
		t.Errorf("Reason = %q, want %q", mg.Reason, mergeGateReasonProbeFailed)
	}
}

// TestOnboardingReadiness_MergeGate_NonMainDefaultBranch_ResolvesRulesets is
// the default-branch-resolve control. The fixture repo defaults to `trunk` and
// carries TWO active rulesets: id 42 scoped by `~DEFAULT_BRANCH` and id 43 by
// the literal `refs/heads/trunk`.
//
// Deleting the resolve so the probe hardcodes "main" is observable twice over:
// the literal-ref ruleset stops matching (Sources drops to one) and the
// reported branch changes. The `~DEFAULT_BRANCH` ruleset alone would NOT
// discriminate — the probe passes the same value as both branch and
// defaultBranch, so `~DEFAULT_BRANCH` self-matches under either — which is
// exactly why the literal-ref sibling is in the fixture.
func TestOnboardingReadiness_MergeGate_NonMainDefaultBranch_ResolvesRulesets(t *testing.T) {
	f := newMergeGateFixture()
	f.defaultBranch = "trunk"
	f.rulesetsList = `[{"id":42,"target":"branch","enforcement":"active"},` +
		`{"id":43,"target":"branch","enforcement":"active"}]`
	f.rulesetBodies["43"] = mergeGateRulesetBody(
		[]string{"refs/heads/trunk"}, []string{"fishhawk_audit_complete"}, 0)

	mg := mergeGateReadinessFor(t, f)

	if mg.Status != "required" {
		t.Fatalf("Status = %q, want required (detail=%q)", mg.Status, mg.Detail)
	}
	if mg.Branch != "trunk" {
		t.Errorf("Branch = %q, want trunk (the repo's REAL default branch, never a guessed main)", mg.Branch)
	}
	if len(mg.Sources) != 2 {
		t.Fatalf("len(Sources) = %d, want 2 (both the ~DEFAULT_BRANCH and the refs/heads/trunk ruleset): %+v",
			len(mg.Sources), mg.Sources)
	}
	if len(f.protectionBranches) == 0 || f.protectionBranches[0] != "trunk" {
		t.Errorf("protection probed branches %v, want the first to be trunk", f.protectionBranches)
	}
}

// TestOnboardingReadiness_MergeGate_AuthoritativeAndAbsent_NotRequired is the
// ONLY path that may report not_required: both surfaces answered definitively
// (classic 404 = positively unprotected, rulesets listed and evaluated) and
// neither requires the check.
func TestOnboardingReadiness_MergeGate_AuthoritativeAndAbsent_NotRequired(t *testing.T) {
	f := newMergeGateFixture()
	f.rulesetBodies["42"] = mergeGateRulesetBody(
		[]string{"~DEFAULT_BRANCH"}, []string{"ci"}, 0)

	mg := mergeGateReadinessFor(t, f)
	if mg.Status != "not_required" {
		t.Fatalf("Status = %q, want not_required (detail=%q reason=%q)", mg.Status, mg.Detail, mg.Reason)
	}
	if !mg.Authoritative {
		t.Errorf("Authoritative = false; not_required must never be reported off a partial read")
	}
	if mg.Remediation == "" {
		t.Errorf("Remediation empty, want the add-the-check step")
	}
	if !containsString(mg.RequiredContexts, "ci") {
		t.Errorf("RequiredContexts = %v, want the contexts that ARE required", mg.RequiredContexts)
	}
	if mg.Bypassable {
		t.Errorf("Bypassable = true with no requiring source, want false")
	}
}

// TestOnboardingReadiness_MergeGate_RulesetsNotFound_Unknown asserts the
// unread-surface degrade: a 404 from the rulesets endpoint (some GHES
// versions) means that surface was never read, so its silence is not
// evidence — unknown, not not_required.
func TestOnboardingReadiness_MergeGate_RulesetsNotFound_Unknown(t *testing.T) {
	f := newMergeGateFixture()
	f.rulesetsStatus = http.StatusNotFound

	mg := mergeGateReadinessFor(t, f)
	if mg.Status != "unknown" {
		t.Fatalf("Status = %q, want unknown (an unread surface is not an absence)", mg.Status)
	}
	if mg.Reason != mergegate.ReasonRulesetsUnqueryable {
		t.Errorf("Reason = %q, want %q", mg.Reason, mergegate.ReasonRulesetsUnqueryable)
	}
	if mg.Authoritative {
		t.Errorf("Authoritative = true, want false")
	}
}

// TestOnboardingReadiness_MergeGate_ForbiddenAdministrationRead_Unknown
// asserts the missing-scope degrade: a 403 from the protection endpoint (the
// App installation lacking `administration: read`, ADR-017 / #252) yields
// unknown with a reason naming the scope.
func TestOnboardingReadiness_MergeGate_ForbiddenAdministrationRead_Unknown(t *testing.T) {
	f := newMergeGateFixture()
	f.protectionStatus = http.StatusForbidden

	mg := mergeGateReadinessFor(t, f)
	if mg.Status != "unknown" {
		t.Fatalf("Status = %q, want unknown", mg.Status)
	}
	if mg.Reason != mergegate.ReasonScopeMissing {
		t.Errorf("Reason = %q, want %q", mg.Reason, mergegate.ReasonScopeMissing)
	}
	if !strings.Contains(mg.Detail, "administration: read") {
		t.Errorf("Detail = %q, want it to name the administration: read scope", mg.Detail)
	}
}

// TestOnboardingReadiness_MergeGate_UnevaluatableRefName_Unknown asserts the
// non-authoritative degrade: an active ruleset scoped by an fnmatch glob the
// v0 matcher cannot evaluate could be hiding a requirement, so a nothing-found
// sweep resolves to unknown rather than not_required.
func TestOnboardingReadiness_MergeGate_UnevaluatableRefName_Unknown(t *testing.T) {
	f := newMergeGateFixture()
	f.rulesetBodies["42"] = mergeGateRulesetBody(
		[]string{"refs/heads/release/*"}, []string{"ci"}, 0)

	mg := mergeGateReadinessFor(t, f)
	if mg.Status != "unknown" {
		t.Fatalf("Status = %q, want unknown (an unevaluatable condition may hide a requirement)", mg.Status)
	}
	if mg.Reason != mergegate.ReasonNonAuthoritative {
		t.Errorf("Reason = %q, want %q", mg.Reason, mergegate.ReasonNonAuthoritative)
	}
}

// TestOnboardingReadiness_MergeGate_TransportError_Unknown asserts a plain
// forge failure (a 500 from the rulesets list) degrades to unknown with the
// transport_error reason.
func TestOnboardingReadiness_MergeGate_TransportError_Unknown(t *testing.T) {
	f := newMergeGateFixture()
	f.rulesetsStatus = http.StatusInternalServerError

	mg := mergeGateReadinessFor(t, f)
	if mg.Status != "unknown" {
		t.Fatalf("Status = %q, want unknown", mg.Status)
	}
	if mg.Reason != mergegate.ReasonTransportError {
		t.Errorf("Reason = %q, want %q", mg.Reason, mergegate.ReasonTransportError)
	}
}

// TestOnboardingReadiness_MergeGate_ProbeTimeout_Unknown asserts the bounded
// probe: a forge that never answers within mergeGateProbeTimeout resolves to
// unknown rather than hanging the readiness report.
//
// Every deadline-competing duration is derived via timescale.D so the
// discrimination ratio (hang >> timeout) holds at any scale factor (#1984).
func TestOnboardingReadiness_MergeGate_ProbeTimeout_Unknown(t *testing.T) {
	orig := mergeGateProbeTimeout
	mergeGateProbeTimeout = timescale.D(100 * time.Millisecond)
	t.Cleanup(func() { mergeGateProbeTimeout = orig })

	f := newMergeGateFixture()
	f.protectionHang = timescale.D(3 * time.Second)

	mg := mergeGateReadinessFor(t, f)
	if mg.Status != "unknown" {
		t.Fatalf("Status = %q, want unknown (a timed-out probe is not an absence)", mg.Status)
	}
	if mg.Reason != mergegate.ReasonTransportError {
		t.Errorf("Reason = %q, want %q", mg.Reason, mergegate.ReasonTransportError)
	}
}

// TestOnboardingReadiness_MergeGate_RequiredViaClassicBypassable asserts the
// single-source classic case: classic protection requires the check with
// enforce_admins FALSE, so the one requiring source is bypassable and the
// conjunction is therefore true. The admin exemption is carried as its own
// named condition — BypassEntries stays 0, never coerced to 1.
func TestOnboardingReadiness_MergeGate_RequiredViaClassicBypassable(t *testing.T) {
	f := newMergeGateFixture()
	f.protection["main"] = mergeGateProtectionBody([]string{"fishhawk_audit_complete"}, false)
	f.rulesetsList = `[]`

	mg := mergeGateReadinessFor(t, f)
	if mg.Status != "required" {
		t.Fatalf("Status = %q, want required", mg.Status)
	}
	if len(mg.Sources) != 1 || !mg.Sources[0].Classic {
		t.Fatalf("Sources = %+v, want the single classic source", mg.Sources)
	}
	if mg.Sources[0].EnforceAdmins {
		t.Errorf("EnforceAdmins = true, want false (decoded from enforce_admins.enabled:false)")
	}
	if mg.Sources[0].BypassEntries != 0 {
		t.Errorf("BypassEntries = %d, want 0: the admin exemption is a named condition, not a count",
			mg.Sources[0].BypassEntries)
	}
	if !mg.Bypassable {
		t.Errorf("Bypassable = false, want true (the only requiring source exempts admins)")
	}
	if mg.Remediation == "" {
		t.Errorf("Remediation empty, want the narrow-the-bypass step")
	}
}
