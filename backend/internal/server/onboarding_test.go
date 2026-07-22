package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
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

// ---------------------------------------------------------------------------
// Cell-side region pin (ADR-062, E44.7 / #1831)
// ---------------------------------------------------------------------------

// fakeAccountsQuerier is an in-memory accounts table keyed by (provider,
// account_key), mirroring UpsertAccount's ON CONFLICT semantics closely enough
// for the pin path.
type fakeAccountsQuerier struct {
	rows    map[string]accountdb.Account
	upserts int
}

func newFakeAccountsQuerier() *fakeAccountsQuerier {
	return &fakeAccountsQuerier{rows: map[string]accountdb.Account{}}
}

func (f *fakeAccountsQuerier) k(provider, key string) string { return provider + "\x00" + key }

func (f *fakeAccountsQuerier) seed(provider, key, region string) {
	f.rows[f.k(provider, key)] = accountdb.Account{
		ID: uuid.New(), Provider: provider, AccountKey: key,
		Granularity: "enterprise", HomeRegion: &region,
	}
}

func (f *fakeAccountsQuerier) GetAccountByKey(_ context.Context, arg accountdb.GetAccountByKeyParams) (accountdb.Account, error) {
	a, ok := f.rows[f.k(arg.Provider, arg.AccountKey)]
	if !ok {
		return accountdb.Account{}, pgx.ErrNoRows
	}
	return a, nil
}

func (f *fakeAccountsQuerier) UpsertAccount(_ context.Context, arg accountdb.UpsertAccountParams) (accountdb.Account, error) {
	f.upserts++
	a := accountdb.Account{
		ID: arg.ID, Provider: arg.Provider, AccountKey: arg.AccountKey,
		DisplayName: arg.DisplayName, Granularity: arg.Granularity, HomeRegion: arg.HomeRegion,
	}
	f.rows[f.k(arg.Provider, arg.AccountKey)] = a
	return a, nil
}

var regionPinTestSecret = []byte("region-handoff-secret")

// newRegionPinServer builds a cell serving cellRegion with the shared handoff
// secret wired. A nil querier is the no-account-store deployment.
func newRegionPinServer(t *testing.T, q account.RegionQuerier, cellRegion string, secret []byte) *Server {
	t.Helper()
	srv := New(Config{Addr: "127.0.0.1:0"})
	var pinner *account.RegionPinner
	if q != nil {
		pinner = account.NewRegionPinner(q, cellRegion)
	}
	srv.ConfigureRegionPin(pinner, secret)
	return srv
}

// regionPinURL builds the redirect target the REAL directory codec would emit,
// then returns just its path+query so a test can drive the cell handler with
// exactly the bytes that crossed the wire. This deliberately goes through
// handoff.AppendTo rather than hand-assembling parameters: the serialization
// boundary between the two sides is the thing under test.
func regionPinURL(t *testing.T, secret []byte, p handoff.Params, extra url.Values) string {
	t.Helper()
	loc, err := handoff.AppendTo("https://cell.example.com", "/v0/onboarding/region-pin", extra, secret, p)
	if err != nil {
		t.Fatalf("handoff.AppendTo: %v", err)
	}
	u, err := url.Parse(loc)
	if err != nil {
		t.Fatalf("parse redirect location: %v", err)
	}
	return u.RequestURI()
}

func validPin() handoff.Params {
	return handoff.Params{
		Provider:   "github",
		AccountKey: "acme",
		HomeRegion: "eu",
		ExpiresAt:  time.Now().Add(2 * time.Minute),
		Nonce:      "nonce-1",
	}
}

func decodeRegionPin(t *testing.T, body []byte) regionPinResponse {
	t.Helper()
	var got regionPinResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode region pin response: %v (%s)", err, body)
	}
	return got
}

func regionPinErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var env errorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode error envelope: %v (%s)", err, body)
	}
	return env.Error.Code
}

// The cross-boundary happy path required by binding condition (7): the request
// is built by the REAL directory codec, routed through the registered mux, and
// asserted to have PERSISTED accounts.home_region — handler → account → db.
func TestRegionPin_StampsHomeRegionThroughTheRealCodec(t *testing.T) {
	q := newFakeAccountsQuerier()
	s := newRegionPinServer(t, q, "eu", regionPinTestSecret)

	// The directory's 302 preserves the original App-install parameters; they
	// ride along and must not disturb the pin.
	target := regionPinURL(t, regionPinTestSecret, validPin(), url.Values{
		"installation_id": {"4242"}, "setup_action": {"install"},
	})
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	got := decodeRegionPin(t, w.Body.Bytes())
	if got.HomeRegion != "eu" || got.Provider != "github" || got.AccountKey != "acme" {
		t.Fatalf("response = %+v", got)
	}
	if got.AccountID == "" {
		t.Error("account_id is empty")
	}
	// The observable done-means: the row exists with home_region persisted.
	row, err := q.GetAccountByKey(context.Background(), accountdb.GetAccountByKeyParams{Provider: "github", AccountKey: "acme"})
	if err != nil {
		t.Fatalf("account row not persisted: %v", err)
	}
	if row.HomeRegion == nil || *row.HomeRegion != "eu" {
		t.Fatalf("persisted home_region = %v, want eu", row.HomeRegion)
	}
}

// The replay bound (binding condition 3): replaying the same signed pin is
// idempotent, and no pin can move an account's region.
func TestRegionPin_ReplayBound(t *testing.T) {
	t.Run("replay_is_idempotent", func(t *testing.T) {
		q := newFakeAccountsQuerier()
		s := newRegionPinServer(t, q, "eu", regionPinTestSecret)
		target := regionPinURL(t, regionPinTestSecret, validPin(), nil)
		for i := range 2 {
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("replay #%d status = %d, want 200:\n%s", i, w.Code, w.Body.String())
			}
			if got := decodeRegionPin(t, w.Body.Bytes()); got.HomeRegion != "eu" {
				t.Fatalf("replay #%d home_region = %q", i, got.HomeRegion)
			}
		}
	})

	t.Run("cannot_move_an_existing_region", func(t *testing.T) {
		q := newFakeAccountsQuerier()
		q.seed("github", "acme", "eu")
		// Untagged cell, so the residency check cannot be what rejects here.
		s := newRegionPinServer(t, q, "", regionPinTestSecret)
		p := validPin()
		p.HomeRegion = "us"
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, regionPinURL(t, regionPinTestSecret, p, nil), nil))

		if w.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
		}
		if code := regionPinErrorCode(t, w.Body.Bytes()); code != "region_pin_conflict" {
			t.Errorf("code = %q, want region_pin_conflict", code)
		}
		if q.upserts != 0 {
			t.Errorf("a rejected pin must write nothing; got %d upserts", q.upserts)
		}
		row, _ := q.GetAccountByKey(context.Background(), accountdb.GetAccountByKeyParams{Provider: "github", AccountKey: "acme"})
		if row.HomeRegion == nil || *row.HomeRegion != "eu" {
			t.Fatalf("home_region moved to %v", row.HomeRegion)
		}
	})
}

// The residency invariant (binding condition 4): a VALID EU pin reaching a US
// cell fails closed.
func TestRegionPin_ForeignRegionIsMisdirected(t *testing.T) {
	q := newFakeAccountsQuerier()
	s := newRegionPinServer(t, q, "us", regionPinTestSecret)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, regionPinURL(t, regionPinTestSecret, validPin(), nil), nil))

	if w.Code != http.StatusMisdirectedRequest {
		t.Fatalf("status = %d, want 421:\n%s", w.Code, w.Body.String())
	}
	if code := regionPinErrorCode(t, w.Body.Bytes()); code != "region_pin_misdirected" {
		t.Errorf("code = %q, want region_pin_misdirected", code)
	}
	if q.upserts != 0 {
		t.Errorf("a misdirected pin must write nothing; got %d upserts", q.upserts)
	}
}

// Every handoff rejection mode (binding condition 2), one case each, all
// asserting that nothing was persisted.
func TestRegionPin_HandoffRejections(t *testing.T) {
	mutate := func(t *testing.T, fn func(url.Values)) string {
		t.Helper()
		target := regionPinURL(t, regionPinTestSecret, validPin(), nil)
		u, err := url.Parse(target)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		q := u.Query()
		fn(q)
		u.RawQuery = q.Encode()
		return u.RequestURI()
	}

	expiredPin := validPin()
	expiredPin.ExpiresAt = time.Now().Add(-time.Second)

	tests := []struct {
		name   string
		target func(t *testing.T) string
		status int
		code   string
	}{
		{
			name:   "absent",
			target: func(*testing.T) string { return "/v0/onboarding/region-pin" },
			status: http.StatusBadRequest, code: "validation_failed",
		},
		{
			name:   "unsigned",
			target: func(t *testing.T) string { return mutate(t, func(v url.Values) { v.Del(handoff.ParamSignature) }) },
			status: http.StatusForbidden, code: "region_pin_rejected",
		},
		{
			name: "forged_signature",
			target: func(t *testing.T) string {
				return mutate(t, func(v url.Values) { v.Set(handoff.ParamSignature, strings.Repeat("ab", 32)) })
			},
			status: http.StatusForbidden, code: "region_pin_rejected",
		},
		{
			name: "tampered_region",
			target: func(t *testing.T) string {
				return mutate(t, func(v url.Values) { v.Set(handoff.ParamHomeRegion, "us") })
			},
			status: http.StatusForbidden, code: "region_pin_rejected",
		},
		{
			name: "tampered_account_key",
			target: func(t *testing.T) string {
				return mutate(t, func(v url.Values) { v.Set(handoff.ParamAccountKey, "someone-else") })
			},
			status: http.StatusForbidden, code: "region_pin_rejected",
		},
		{
			name: "signed_by_a_foreign_secret",
			target: func(t *testing.T) string {
				return regionPinURL(t, []byte("attacker-secret"), validPin(), nil)
			},
			status: http.StatusForbidden, code: "region_pin_rejected",
		},
		{
			name:   "expired",
			target: func(t *testing.T) string { return regionPinURL(t, regionPinTestSecret, expiredPin, nil) },
			status: http.StatusForbidden, code: "region_pin_rejected",
		},
		{
			name: "malformed_expiry",
			target: func(t *testing.T) string {
				return mutate(t, func(v url.Values) { v.Set(handoff.ParamExpiresAt, "soon") })
			},
			status: http.StatusBadRequest, code: "validation_failed",
		},
		{
			name: "blank_nonce",
			target: func(t *testing.T) string {
				return mutate(t, func(v url.Values) { v.Set(handoff.ParamNonce, "") })
			},
			status: http.StatusBadRequest, code: "validation_failed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q := newFakeAccountsQuerier()
			s := newRegionPinServer(t, q, "eu", regionPinTestSecret)
			w := httptest.NewRecorder()
			s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.target(t), nil))
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d:\n%s", w.Code, tc.status, w.Body.String())
			}
			if code := regionPinErrorCode(t, w.Body.Bytes()); code != tc.code {
				t.Errorf("code = %q, want %q", code, tc.code)
			}
			if q.upserts != 0 {
				t.Errorf("a rejected pin must write nothing; got %d upserts", q.upserts)
			}
		})
	}
}

// An unsupported region survives signature verification (the directory signed
// it) and must still be refused by the cell's own closed-set check.
func TestRegionPin_UnsupportedRegionIsRejected(t *testing.T) {
	q := newFakeAccountsQuerier()
	// Untagged cell, so the residency check is not what rejects here.
	s := newRegionPinServer(t, q, "", regionPinTestSecret)
	p := validPin()
	p.HomeRegion = "uk"

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, regionPinURL(t, regionPinTestSecret, p, nil), nil))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if code := regionPinErrorCode(t, w.Body.Bytes()); code != "validation_failed" {
		t.Errorf("code = %q, want validation_failed", code)
	}
	if q.upserts != 0 {
		t.Errorf("got %d upserts, want 0", q.upserts)
	}
}

// Both unavailable postures fail closed rather than trusting the parameters.
func TestRegionPin_UnavailableDeployments(t *testing.T) {
	t.Run("no_account_store", func(t *testing.T) {
		s := newRegionPinServer(t, nil, "eu", regionPinTestSecret)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, regionPinURL(t, regionPinTestSecret, validPin(), nil), nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
		}
		if code := regionPinErrorCode(t, w.Body.Bytes()); code != "region_pin_unavailable" {
			t.Errorf("code = %q, want region_pin_unavailable", code)
		}
	})

	t.Run("no_shared_secret", func(t *testing.T) {
		q := newFakeAccountsQuerier()
		s := newRegionPinServer(t, q, "eu", nil)
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, regionPinURL(t, regionPinTestSecret, validPin(), nil), nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
		}
		if code := regionPinErrorCode(t, w.Body.Bytes()); code != "region_pin_unavailable" {
			t.Errorf("code = %q, want region_pin_unavailable", code)
		}
		if q.upserts != 0 {
			t.Errorf("got %d upserts, want 0", q.upserts)
		}
	})
}

// The route is GET-only by construction (binding condition 10): a POST to the
// same path is not routed to the pin handler.
func TestRegionPin_IsGetOnly(t *testing.T) {
	q := newFakeAccountsQuerier()
	s := newRegionPinServer(t, q, "eu", regionPinTestSecret)
	target := regionPinURL(t, regionPinTestSecret, validPin(), nil)

	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodPost, target, nil))
	if w.Code == http.StatusOK {
		t.Fatalf("POST reached the pin handler (status 200); the route must be GET-only")
	}
	if q.upserts != 0 {
		t.Errorf("POST must write nothing; got %d upserts", q.upserts)
	}
}
