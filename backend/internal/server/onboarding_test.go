package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
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
