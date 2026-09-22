package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergegate"
	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
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

// TestOnboardingReadiness_MalformedRepo asserts a repo failing the shared
// shape rule (account.ProjectPathWellFormed: non-empty namespace, every
// remaining component non-empty) is rejected 400 validation_failed under BOTH
// families — the empty/whitespace-component cases (E45.43 / #3348) are refused
// even with forge=gitlab, where nesting itself is allowed. "owner/name/extra"
// is still 400 on the DEFAULT (github) family, now via the nested-on-github
// branch whose message names forge=gitlab as the remedy.
func TestOnboardingReadiness_MalformedRepo(t *testing.T) {
	s := newOnboardingServer(t, nil, nil)
	id := testOperatorIdentity()
	for _, repo := range []string{"noslash", "", "/name", "owner/", "a//b", "a/ /b", "a/b/", " / "} {
		for _, forgeParam := range []string{"", "gitlab"} {
			w := httptest.NewRecorder()
			s.handleGetOnboardingReadiness(w, onboardingReqForge(repo, forgeParam, &id))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("repo=%q forge=%q status = %d, want 400:\n%s", repo, forgeParam, w.Code, w.Body.String())
			}
			var env errorEnvelope
			if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if env.Error.Code != "validation_failed" {
				t.Errorf("repo=%q forge=%q code = %q, want validation_failed", repo, forgeParam, env.Error.Code)
			}
		}
	}

	w := httptest.NewRecorder()
	s.handleGetOnboardingReadiness(w, onboardingReq("owner/name/extra", &id))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("nested on default family: status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if env.Error.Code != "validation_failed" || !strings.Contains(env.Error.Message, "forge=gitlab") {
		t.Errorf("nested on default family: code = %q message = %q, want validation_failed naming forge=gitlab",
			env.Error.Code, env.Error.Message)
	}
}

// onboardingReqForge is onboardingReq plus an optional `forge` query value
// (omitted from the URL when empty, so the default-family path is exercised
// exactly as a caller that never sends the parameter).
func onboardingReqForge(repo, forgeParam string, id *Identity) *http.Request {
	q := "repo=" + url.QueryEscape(repo)
	if forgeParam != "" {
		q += "&forge=" + url.QueryEscape(forgeParam)
	}
	req := httptest.NewRequest(http.MethodGet, "/v0/onboarding/readiness?"+q, nil)
	if id != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, *id))
	}
	return req
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

// --- #3578: reviewer model-validity honesty fields on the readiness rung ---

// onboardingSingleReviewerSpecYAML declares ONE anthropic reviewer with an
// explicit model, so a model-validity test asserts against exactly one rung.
const onboardingSingleReviewerSpecYAML = `version: "1.0"
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
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
`

// onboardingUnpricedReviewerSpecYAML names a model whose family the shared
// pricing table does not know, so the priced flag reads false.
const onboardingUnpricedReviewerSpecYAML = `version: "1.0"
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
              model: mystery-model-1
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
`

// onboardingReviewerNoModelSpecYAML omits the reviewer model, so the resolved
// deployment default (not the spec) is what gets checked.
const onboardingReviewerNoModelSpecYAML = `version: "1.0"
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
          human: 0
        produces:
          - artifact: plan
            schema: standard_v1
`

// newOnboardingServerWithOracle is newOnboardingServer plus an injected
// Config.ModelOracle, so the model-validity honesty fields (#3578) are exercised.
func newOnboardingServerWithOracle(t *testing.T, ghSrv *httptest.Server, reviewers ReviewerSet, oracle modeloracle.ModelOracle) *Server {
	t.Helper()
	s := newOnboardingServer(t, ghSrv, reviewers)
	s.cfg.ModelOracle = oracle
	return s
}

// oracleReviewerSet is the test analog of the production planReviewerSet.For:
// For() resolves an omitted model to the provider's deployment default, then
// verifies the RESOLVED model against an oracle and returns a
// modeloracle.RejectedError (wrapping the verdict) on authoritative absence —
// exactly the shape the readiness rung and the run-create path key on (#3578).
type oracleReviewerSet struct {
	def       PlanReviewer
	providers map[string]PlanReviewer
	defaults  map[string]string // per-provider deployment default model
	oracle    modeloracle.ModelOracle
}

func (s oracleReviewerSet) Default() PlanReviewer { return s.def }

func (s oracleReviewerSet) For(provider, model string, _ ...string) (PlanReviewer, error) {
	r, ok := s.providers[provider]
	if !ok {
		return nil, fmt.Errorf("reviewer provider %q is not configured", provider)
	}
	if model == "" {
		model = s.defaults[provider]
	}
	if v := modeloracle.Verify(context.Background(), s.oracle, provider, model); v.Status == modeloracle.ModelRejected {
		return nil, fmt.Errorf("reviewer provider %q model %q is not available: %w", provider, model, modeloracle.RejectedError{Verdict: v})
	}
	return r, nil
}

// ResolvedReviewerModel implements the server's reviewerModelDefaulter seam so
// an omitted spec model resolves to the deployment default here exactly as the
// production planReviewerSet exposes it (#3578, condition 2).
func (s oracleReviewerSet) ResolvedReviewerModel(provider string) (string, bool) {
	m, ok := s.defaults[provider]
	return m, ok && m != ""
}

// flippingOracle serves `first` on its first Snapshot call for `provider` and
// `rest` on every later one, so a test can drive the readiness rung's TOCTOU
// window: For() reads the accepting `first` snapshot, and the rung's SECOND read
// (annotateReviewerModel) sees a fresh authoritative absence (#3578 concurrency).
type flippingOracle struct {
	mu       sync.Mutex
	provider string
	first    []string
	rest     []string
	calls    int
}

func (o *flippingOracle) Snapshot(_ context.Context, provider string) ([]string, bool, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if provider != o.provider {
		return nil, true, false
	}
	o.calls++
	if o.calls == 1 {
		return o.first, true, true
	}
	return o.rest, true, true
}

// reviewerByProvider indexes a readiness response's reviewers by provider.
func reviewerByProvider(resp onboardingReadinessResponse) map[string]reviewerReadiness {
	out := map[string]reviewerReadiness{}
	for _, rv := range resp.Reviewers {
		out[rv.Provider] = rv
	}
	return out
}

// TestOnboardingReadiness_ReviewerModelRejectedOnFreshAbsence pins the core
// control: a reviewer whose model is authoritatively absent from a fresh
// snapshot is available=false, model_status=rejected, with a did-you-mean hint.
// Deleting the Available=false override (the errors.As RejectedError arm in
// probeReviewers) turns this RED.
func TestOnboardingReadiness_ReviewerModelRejectedOnFreshAbsence(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingSingleReviewerSpecYAML)
	ghSrv := fake.server(t)
	oracle := modeloracle.Static{Models: map[string][]string{"anthropic": {"claude-opus-4-7"}}, Fresh: true}
	set := oracleReviewerSet{
		def:       &fakePlanReviewer{},
		providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}},
		oracle:    oracle,
	}
	s := newOnboardingServerWithOracle(t, ghSrv, set, oracle)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	rv := reviewerByProvider(resp)["anthropic"]
	if rv.Available {
		t.Errorf("Available = true, want false for an authoritatively-absent model")
	}
	if rv.ModelStatus != "rejected" {
		t.Errorf("ModelStatus = %q, want rejected", rv.ModelStatus)
	}
	if !strings.Contains(rv.ModelHint, `did you mean "claude-opus-4-7"`) {
		t.Errorf("ModelHint = %q, want a did-you-mean naming claude-opus-4-7", rv.ModelHint)
	}
}

// TestOnboardingReadiness_ReviewerModelVerified: a model present in a fresh
// snapshot is available=true, model_status=verified, priced=true.
func TestOnboardingReadiness_ReviewerModelVerified(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingSingleReviewerSpecYAML)
	ghSrv := fake.server(t)
	oracle := modeloracle.Static{Models: map[string][]string{"anthropic": {"claude-opus-4-8"}}, Fresh: true}
	set := fakeReviewerSet{providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}}}
	s := newOnboardingServerWithOracle(t, ghSrv, set, oracle)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	rv := reviewerByProvider(resp)["anthropic"]
	if !rv.Available || rv.ModelStatus != "verified" {
		t.Errorf("rv = %+v, want available + verified", rv)
	}
	if rv.Priced == nil || !*rv.Priced {
		t.Errorf("Priced = %v, want &true (claude-opus is a known family)", rv.Priced)
	}
}

// TestOnboardingReadiness_ReviewerModelUnverifiableWhenNoSnapshot: a nil oracle
// (a key-less deployment) resolves to available=true, model_status=unverifiable,
// with a non-empty hint — the rung's honest state everywhere without a snapshot.
func TestOnboardingReadiness_ReviewerModelUnverifiableWhenNoSnapshot(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingSingleReviewerSpecYAML)
	ghSrv := fake.server(t)
	set := fakeReviewerSet{providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}}}
	s := newOnboardingServerWithOracle(t, ghSrv, set, nil) // no oracle

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	rv := reviewerByProvider(resp)["anthropic"]
	if !rv.Available || rv.ModelStatus != "unverifiable" {
		t.Errorf("rv = %+v, want available + unverifiable", rv)
	}
	if rv.ModelHint == "" {
		t.Errorf("ModelHint empty, want the passed-to-the-vendor-verbatim warning")
	}
}

// TestOnboardingReadiness_ReviewerModelUnpriced: a verified model whose family
// the pricing table does not know is priced=false and the hint carries the $0
// note. Deleting the pricing lookup (modelPriced/Priced assignment) turns this
// RED.
func TestOnboardingReadiness_ReviewerModelUnpriced(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingUnpricedReviewerSpecYAML)
	ghSrv := fake.server(t)
	oracle := modeloracle.Static{Models: map[string][]string{"anthropic": {"mystery-model-1"}}, Fresh: true}
	set := fakeReviewerSet{providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}}}
	s := newOnboardingServerWithOracle(t, ghSrv, set, oracle)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	rv := reviewerByProvider(resp)["anthropic"]
	if rv.ModelStatus != "verified" {
		t.Errorf("ModelStatus = %q, want verified (present in the fresh set)", rv.ModelStatus)
	}
	if rv.Priced == nil || *rv.Priced {
		t.Errorf("Priced = %v, want &false (mystery-model-1 has no pricing family)", rv.Priced)
	}
	if !strings.Contains(rv.ModelHint, "$0") {
		t.Errorf("ModelHint = %q, want the unpriced $0 note", rv.ModelHint)
	}
}

// TestOnboardingReadiness_ReviewerModelOmittedNotComputed: when the spec omits
// the model AND the wired set does not expose its deployment default
// (fakeReviewerSet implements no reviewerModelDefaulter), the honesty fields
// stay unset — priced is nil, ABSENT from the JSON (the "not computed" residual,
// #3578). This is the ONLY omitted-model path that still leaves priced absent.
func TestOnboardingReadiness_ReviewerModelOmittedNotComputed(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewerNoModelSpecYAML)
	ghSrv := fake.server(t)
	oracle := modeloracle.Static{Models: map[string][]string{"anthropic": {"claude-opus-4-8"}}, Fresh: true}
	set := fakeReviewerSet{providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}}}
	s := newOnboardingServerWithOracle(t, ghSrv, set, oracle)

	id := testOperatorIdentity()
	w := httptest.NewRecorder()
	s.handleGetOnboardingReadiness(w, onboardingReq("x/y", &id))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), `"priced"`) {
		t.Errorf("response carries a priced key, want it absent when the default id is not exposed:\n%s", w.Body.String())
	}
	var resp onboardingReadinessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rv := reviewerByProvider(resp)["anthropic"]; rv.Priced != nil {
		t.Errorf("Priced = %v, want nil (default id not exposed)", rv.Priced)
	}
}

// TestOnboardingReadiness_ReviewerModelOmittedDefaultVerified: condition 2 —
// when the spec omits the model and the set exposes the resolved default, the
// honesty fields describe that default. A verified+priced default reads
// available + model_status=verified + priced=&true, NOT a bare "ok" with the
// fields absent. Deleting the omitted-model resolvedReviewerDefault branch in
// probeReviewers turns this RED (model_status would be empty).
func TestOnboardingReadiness_ReviewerModelOmittedDefaultVerified(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewerNoModelSpecYAML)
	ghSrv := fake.server(t)
	oracle := modeloracle.Static{Models: map[string][]string{"anthropic": {"claude-opus-4-8"}}, Fresh: true}
	set := oracleReviewerSet{
		def:       &fakePlanReviewer{},
		providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}},
		defaults:  map[string]string{"anthropic": "claude-opus-4-8"},
		oracle:    oracle,
	}
	s := newOnboardingServerWithOracle(t, ghSrv, set, oracle)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	rv := reviewerByProvider(resp)["anthropic"]
	if !rv.Available || rv.ModelStatus != "verified" {
		t.Errorf("rv = %+v, want available + verified (resolved default)", rv)
	}
	if rv.Priced == nil || !*rv.Priced {
		t.Errorf("Priced = %v, want &true (resolved default is a known family)", rv.Priced)
	}
}

// TestOnboardingReadiness_ReviewerModelOmittedDefaultUnverifiable: condition 2 —
// an omitted model whose resolved default cannot be verified (nil oracle) reads
// available + model_status=unverifiable with the passed-to-vendor hint, instead
// of a bare "ok" with no caveat (the concern-1 misleading render).
func TestOnboardingReadiness_ReviewerModelOmittedDefaultUnverifiable(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewerNoModelSpecYAML)
	ghSrv := fake.server(t)
	set := oracleReviewerSet{
		def:       &fakePlanReviewer{},
		providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}},
		defaults:  map[string]string{"anthropic": "claude-opus-4-8"},
		oracle:    nil, // no snapshot → For() fails open, second read unverifiable
	}
	s := newOnboardingServerWithOracle(t, ghSrv, set, nil)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	rv := reviewerByProvider(resp)["anthropic"]
	if !rv.Available || rv.ModelStatus != "unverifiable" {
		t.Errorf("rv = %+v, want available + unverifiable (resolved default)", rv)
	}
	if rv.ModelHint == "" {
		t.Errorf("ModelHint empty, want the passed-to-the-vendor-verbatim warning for the default")
	}
	if rv.Priced == nil || !*rv.Priced {
		t.Errorf("Priced = %v, want &true (claude-opus is a known family)", rv.Priced)
	}
}

// TestOnboardingReadiness_ReviewerModelOmittedDefaultUnpriced: condition 2 — an
// omitted model whose resolved default is verified but whose family the pricing
// table does not know reads priced=&false with the $0 note, not a bare "ok".
func TestOnboardingReadiness_ReviewerModelOmittedDefaultUnpriced(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingReviewerNoModelSpecYAML)
	ghSrv := fake.server(t)
	oracle := modeloracle.Static{Models: map[string][]string{"anthropic": {"mystery-model-1"}}, Fresh: true}
	set := oracleReviewerSet{
		def:       &fakePlanReviewer{},
		providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}},
		defaults:  map[string]string{"anthropic": "mystery-model-1"},
		oracle:    oracle,
	}
	s := newOnboardingServerWithOracle(t, ghSrv, set, oracle)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	rv := reviewerByProvider(resp)["anthropic"]
	if rv.ModelStatus != "verified" {
		t.Errorf("ModelStatus = %q, want verified (default present in the fresh set)", rv.ModelStatus)
	}
	if rv.Priced == nil || *rv.Priced {
		t.Errorf("Priced = %v, want &false (resolved default has no pricing family)", rv.Priced)
	}
	if !strings.Contains(rv.ModelHint, "$0") {
		t.Errorf("ModelHint = %q, want the unpriced $0 note for the default", rv.ModelHint)
	}
}

// TestOnboardingReadiness_ReviewerModelRejectedOnSecondRead pins concern 2's
// TOCTOU control: For() accepts an EXPLICIT model against the first (accepting)
// snapshot, then a background refresh flips the snapshot to authoritative
// absence before annotateReviewerModel's second read. The rung must report
// unavailable + model_status=rejected + the did-you-mean, never a priced "ok".
// Deleting the ModelRejected arm in annotateReviewerModel (which forces
// Available=false) turns this RED — Available stays true.
func TestOnboardingReadiness_ReviewerModelRejectedOnSecondRead(t *testing.T) {
	fake := newFakeGitHubForRuns(onboardingSingleReviewerSpecYAML)
	ghSrv := fake.server(t)
	// First read serves claude-opus-4-8 (For accepts); second read drops it and
	// serves only the near-miss claude-opus-4-7 (fresh authoritative absence).
	oracle := &flippingOracle{
		provider: "anthropic",
		first:    []string{"claude-opus-4-8"},
		rest:     []string{"claude-opus-4-7"},
	}
	set := oracleReviewerSet{
		def:       &fakePlanReviewer{},
		providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}},
		oracle:    oracle,
	}
	s := newOnboardingServerWithOracle(t, ghSrv, set, oracle)

	id := testOperatorIdentity()
	code, resp := decodeReadiness(t, s, onboardingReq("x/y", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	rv := reviewerByProvider(resp)["anthropic"]
	if rv.Available {
		t.Errorf("Available = true, want false for a second-read rejection")
	}
	if rv.ModelStatus != "rejected" {
		t.Errorf("ModelStatus = %q, want rejected", rv.ModelStatus)
	}
	if !strings.Contains(rv.ModelHint, `did you mean "claude-opus-4-7"`) {
		t.Errorf("ModelHint = %q, want a did-you-mean naming claude-opus-4-7", rv.ModelHint)
	}
	if oracle.calls < 2 {
		t.Errorf("oracle Snapshot calls = %d, want >= 2 (For then annotate)", oracle.calls)
	}
}

// TestCreateRun_ReviewerDefaultModelAbsent_AdmitsWithDidYouMeanAudit is binding
// condition (3)'s end-to-end test: through the REAL admission handler + audit
// repo, a spec omitting the reviewer model whose deployment DEFAULT is absent
// from a fresh snapshot is ADMITTED (201, degrade not reject) AND a
// reviewer_capability_unavailable audit entry carries the did-you-mean error.
func TestCreateRun_ReviewerDefaultModelAbsent_AdmitsWithDidYouMeanAudit(t *testing.T) {
	repo := newFakeRepo()
	au := newAuditFake()
	oracle := modeloracle.Static{Models: map[string][]string{"anthropic": {"claude-opus-4-7"}}, Fresh: true}
	// The deployment default (claude-bad-default) is absent from the fresh set,
	// so For() rejects it with a did-you-mean naming claude-opus-4-7.
	set := oracleReviewerSet{
		def:       &fakePlanReviewer{model: "claude-opus-4-7"},
		providers: map[string]PlanReviewer{"anthropic": &fakePlanReviewer{}},
		// The deployment default (a near-miss of the served claude-opus-4-7) is
		// absent from the fresh set, so For() rejects it with a did-you-mean.
		defaults: map[string]string{"anthropic": "claude-opus-4-8"},
		oracle:   oracle,
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, PlanReviewers: set, ModelOracle: oracle})

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  onboardingReviewerNoModelSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (a bad default DEGRADES, not rejects):\n%s", w.Code, w.Body.String())
	}
	if len(repo.runs) != 1 {
		t.Fatalf("runs created = %d, want 1", len(repo.runs))
	}
	caps := capabilityUnavailableEntries(t, au)
	if len(caps) != 1 {
		t.Fatalf("reviewer_capability_unavailable entries = %d, want 1", len(caps))
	}
	if caps[0]["provider"] != "anthropic" {
		t.Errorf("capability audit provider = %v, want anthropic", caps[0]["provider"])
	}
	errText, _ := caps[0]["error"].(string)
	if !strings.Contains(errText, `did you mean "claude-opus-4-7"`) {
		t.Errorf("audit error = %q, want the did-you-mean naming claude-opus-4-7", errText)
	}
	if !strings.Contains(errText, "claude-opus-4-8") {
		t.Errorf("audit error = %q, want it to name the resolved default model", errText)
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
// ordering: an authenticated non-admin caller sending repo="owner//name" (an
// empty component — malformed under BOTH families) against a deny-all mirror
// still gets 400 validation_failed, and the mirror's Visible is never called —
// the filter must never be handed a malformed key. If the guard were hoisted
// above the format check this would go 403 (or ask the mirror the malformed
// key), so the test discriminates. (Since E45.43 / #3348 "owner/name/extra" is
// a WELL-FORMED nested path whose github-family rejection depends on when the
// family is known — see _GitHub_NestedPathRejected_BeforeVisibility and
// _ForgeFromRegistry_GitHub_NestedPathRejected_AfterVisibility for both
// orders — so it is no longer this test's vehicle.)
func TestOnboardingReadiness_MalformedRepoBeforeVisibility(t *testing.T) {
	vis := newFakeRepoVisibility(map[string]bool{}) // denies everything
	s := newOnboardingVisServer(t, nil, vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

	w := runOnboarding(s, "owner//name", memberIdentity())
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
	if resp.MergeGate == nil {
		t.Fatalf("merge_gate absent on the github family, want the probed object")
	}
	return *resp.MergeGate
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

// --- forge-family-aware readiness (E45.43 / #3348) -------------------------

// onboardingNestedGitLabPath is the issue's five-segment project path: the
// shape the pre-#3348 owner/name rule refused at parameter validation.
const onboardingNestedGitLabPath = "gitlab-com/customer-success/solutions-architecture/coe/gitlab-migrator"

// fakeGitLabForOnboarding is a minimal GitLab v4 stub for the readiness
// endpoint's gitlab family: GET /api/v4/projects/<path> (project resolve),
// GET /api/v4/projects/<path>/repository/files/.fishhawk/workflows.yaml (the
// Repository Files API), and — for the gitlab_merge_gate rung (E45.66 /
// #3580) — GET /api/v4/projects/<id> (the project settings, by NUMERIC id)
// and GET /api/v4/projects/<id>/protected_branches (the rule list). It is a
// CATCH-ALL handler switching on the DECODED r.URL.Path rather than a mux
// pattern, because gitlabclient PathEscapes the whole namespaced path into
// one %2F-bearing segment and ServeMux pattern matching on such a path is
// ambiguous. It records the decoded project path each path-lookup call
// addressed, so a test can assert the FULL nested path reached the forge on
// both calls; the by-id and protected-branch calls are counted separately
// (idCalls / listCalls) so the existing path assertions stay byte-identical.
//
// projectSettingsJSON is the by-id body's settings tail (default: main is the
// default branch, pipeline must succeed, skipped pipelines not allowed,
// discussions must be resolved); protectedRulesJSON is the rule list (default:
// one exact `main` rule, push "No one" (0), merge "Maintainers" (40), no force
// push). listHang, when set, makes the list route sleep — the probe-timeout
// vehicle.
type fakeGitLabForOnboarding struct {
	mu                  sync.Mutex
	projectStatus       int
	fileStatus          int
	projectByIDStatus   int
	protectedStatus     int
	specYAML            string
	projectSettingsJSON string
	protectedRulesJSON  string
	listHang            time.Duration
	projectPaths        []string
	filePaths           []string
	idCalls             int
	listCalls           int
}

// fakeGitLabProjectID is the numeric id the path lookup answers; the by-id
// and protected-branch routes are keyed on it.
const fakeGitLabProjectID = "5"

const (
	fakeGitLabDefaultSettingsJSON = `"default_branch":"main","only_allow_merge_if_pipeline_succeeds":true,"allow_merge_on_skipped_pipeline":false,"only_allow_merge_if_all_discussions_are_resolved":true`
	fakeGitLabDefaultRulesJSON    = `[{"id":1,"name":"main","push_access_levels":[{"access_level":0,"access_level_description":"No one"}],"merge_access_levels":[{"access_level":40,"access_level_description":"Maintainers"}],"allow_force_push":false}]`
)

func newFakeGitLabForOnboarding(specYAML string) *fakeGitLabForOnboarding {
	return &fakeGitLabForOnboarding{
		projectStatus:       http.StatusOK,
		fileStatus:          http.StatusOK,
		projectByIDStatus:   http.StatusOK,
		protectedStatus:     http.StatusOK,
		specYAML:            specYAML,
		projectSettingsJSON: fakeGitLabDefaultSettingsJSON,
		protectedRulesJSON:  fakeGitLabDefaultRulesJSON,
	}
}

func (f *fakeGitLabForOnboarding) server(t *testing.T) *httptest.Server {
	t.Helper()
	const prefix = "/api/v4/projects/"
	const fileSuffix = "/repository/files/" + githubclient.WorkflowSpecPath
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, prefix) {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"404 Not Found"}`))
			return
		}
		rest := strings.TrimPrefix(r.URL.Path, prefix)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch rest {
		case fakeGitLabProjectID:
			f.idCalls++
			w.WriteHeader(f.projectByIDStatus)
			if f.projectByIDStatus != http.StatusOK {
				_, _ = w.Write([]byte(`{"message":"` + http.StatusText(f.projectByIDStatus) + `"}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":` + fakeGitLabProjectID + `,"web_url":"` + srvURLPlaceholder + `",` + f.projectSettingsJSON + `}`))
			return
		case fakeGitLabProjectID + "/protected_branches":
			f.listCalls++
			if f.listHang > 0 {
				hang := f.listHang
				f.mu.Unlock()
				select {
				case <-time.After(hang):
				case <-r.Context().Done():
				}
				f.mu.Lock()
			}
			w.WriteHeader(f.protectedStatus)
			if f.protectedStatus != http.StatusOK {
				_, _ = w.Write([]byte(`{"message":"` + http.StatusText(f.protectedStatus) + `"}`))
				return
			}
			_, _ = w.Write([]byte(f.protectedRulesJSON))
			return
		}
		if strings.HasSuffix(rest, fileSuffix) {
			f.filePaths = append(f.filePaths, strings.TrimSuffix(rest, fileSuffix))
			w.WriteHeader(f.fileStatus)
			if f.fileStatus != http.StatusOK {
				_, _ = w.Write([]byte(`{"message":"404 File Not Found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"file_path":"` + githubclient.WorkflowSpecPath + `","blob_id":"blob_1","encoding":"base64","content":"` +
				base64.StdEncoding.EncodeToString([]byte(f.specYAML)) + `"}`))
			return
		}
		f.projectPaths = append(f.projectPaths, rest)
		w.WriteHeader(f.projectStatus)
		if f.projectStatus != http.StatusOK {
			_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":5,"web_url":"` + srvURLPlaceholder + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// srvURLPlaceholder stands in for the project's web_url; the readiness probe
// never reads it.
const srvURLPlaceholder = "https://gitlab.example/p"

func (f *fakeGitLabForOnboarding) calls() (projects, files []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.projectPaths...), append([]string(nil), f.filePaths...)
}

// mergeGateCalls returns how many times the by-id project read and the
// protected-branch list were hit.
func (f *fakeGitLabForOnboarding) mergeGateCalls() (byID, list int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.idCalls, f.listCalls
}

// gitlabForgeOver wraps the stub in the REAL forgegitlab.Forge, the adapter
// the production ForgeResolver hands back for the "gitlab" family.
func gitlabForgeOver(glSrv *httptest.Server) *forgegitlab.Forge {
	return forgegitlab.New(glSrv.URL, forgegitlab.NewStaticCredentialProvider("glpat-test"),
		forgegitlab.WithHTTPClient(glSrv.Client()))
}

// registeredGitLabInstallations is the default registry posture for the
// gitlab-family fixtures (E45.68 / #3582): a row registered under the nested
// path whose installation_ref names the SAME project id the v4 stub resolves
// the path to (fakeGitLabProjectID), so the rung reports registered with
// ref_matches true. Tests needing another posture overwrite
// s.cfg.GitLabInstallations after construction.
func registeredGitLabInstallations() *fakeGitLabInstallations {
	return &fakeGitLabInstallations{
		found: true,
		inst: account.GitLabInstallation{
			InstallationRef: "gitlab:" + fakeGitLabProjectID,
			ProjectPath:     onboardingNestedGitLabPath,
		},
	}
}

// newOnboardingGitLabServer builds a Server whose ForgeResolver answers the
// gitlab family with glForge (or errors when glForge is nil, the
// unconfigured-forge posture), with an optional GitHub fake, provider resolver
// and reviewer set, and the registered fake installations registry
// (registeredGitLabInstallations) wired so the gitlab family's `installed`
// — registered AND resolvable since #3582 — reports true when the project
// resolves.
func newOnboardingGitLabServer(t *testing.T, ghSrv *httptest.Server, glForge forge.Forge,
	providers ProviderResolver, reviewers ReviewerSet) *Server {
	t.Helper()
	cfg := Config{
		Addr:                "127.0.0.1:0",
		RepoProviders:       providers,
		PlanReviewers:       reviewers,
		GitLabInstallations: registeredGitLabInstallations(),
		ForgeResolver: func(id string) (forge.Forge, error) {
			if id == observationForgeGitLab && glForge != nil {
				return glForge, nil
			}
			return nil, errors.New("no forge registered for " + id + " in this test")
		},
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

// rawReadiness runs the request and decodes the RAW 200 body into a map, so a
// test can assert key PRESENCE/ABSENCE (`merge_gate`, `installation_id`)
// rather than only zero values on the typed struct.
func rawReadiness(t *testing.T, s *Server, req *http.Request) map[string]any {
	t.Helper()
	w := httptest.NewRecorder()
	s.handleGetOnboardingReadiness(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var raw map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw body: %v\n%s", err, w.Body.String())
	}
	return raw
}

func rawObject(t *testing.T, raw map[string]any, key string) map[string]any {
	t.Helper()
	obj, ok := raw[key].(map[string]any)
	if !ok {
		t.Fatalf("body[%q] = %v (%T), want an object", key, raw[key], raw[key])
	}
	return obj
}

// TestOnboardingReadiness_GitLab_NestedPath_EndToEnd is the CROSS-BOUNDARY
// test for the gitlab family: HTTP request → handler → the REAL
// forgegitlab.Forge → the v4 stub → decoded JSON. forge=gitlab plus the
// issue's five-segment path answers 200 (the pre-#3348 rule refused it 400);
// the RAW body carries forge=gitlab, app.installed with the gitlab note and NO
// installation_id, a fetched+valid spec, both declared reviewers, scopes — and
// NO merge_gate key at all (the counterfactual vehicle for the omission: a
// zero-valued merge gate assigned on the gitlab arm turns this red). The stub
// must have seen the FULL nested path on both the project and the file call,
// so a RepoRef-splitting regression turns it red too.
func TestOnboardingReadiness_GitLab_NestedPath_EndToEnd(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	reviewers := fakeReviewerSet{providers: map[string]PlanReviewer{
		"anthropic": &fakePlanReviewer{},
		"codex":     &fakePlanReviewer{},
	}}
	s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, reviewers)
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge(onboardingNestedGitLabPath, "gitlab", &id))
	if raw["repo"] != onboardingNestedGitLabPath {
		t.Errorf("repo = %v, want the nested path echoed", raw["repo"])
	}
	if raw["forge"] != "gitlab" {
		t.Errorf("forge = %v, want gitlab", raw["forge"])
	}
	app := rawObject(t, raw, "app")
	if app["installed"] != true {
		t.Errorf("app.installed = %v, want true (registered AND resolvable)", app["installed"])
	}
	if app["resolvable"] != true {
		t.Errorf("app.resolvable = %v, want true", app["resolvable"])
	}
	if app["note"] != onboardingGitLabInstalledNote {
		t.Errorf("app.note = %v, want %q", app["note"], onboardingGitLabInstalledNote)
	}
	if _, present := app["installation_id"]; present {
		t.Errorf("app.installation_id present (%v); no App installation applies on GitLab", app["installation_id"])
	}
	// The registration rung (E45.68 / #3582): registered under the exact
	// path, and the registered ref equals the ref the path resolved to.
	reg := rawObject(t, raw, "gitlab_registration")
	if reg["status"] != gitLabRegistrationStatusRegistered {
		t.Errorf("gitlab_registration.status = %v, want registered", reg["status"])
	}
	if reg["project_path"] != onboardingNestedGitLabPath {
		t.Errorf("gitlab_registration.project_path = %v, want the nested path", reg["project_path"])
	}
	wantRef := "gitlab:" + fakeGitLabProjectID
	if reg["installation_ref"] != wantRef || reg["resolved_ref"] != wantRef {
		t.Errorf("gitlab_registration refs = installation:%v resolved:%v, want both %q", reg["installation_ref"], reg["resolved_ref"], wantRef)
	}
	if reg["ref_matches"] != true {
		t.Errorf("gitlab_registration.ref_matches = %v, want true", reg["ref_matches"])
	}
	if reg["note"] != gitLabRegistrationNote {
		t.Errorf("gitlab_registration.note = %v, want the constant note", reg["note"])
	}
	for _, key := range []string{"reason", "detail", "remediation"} {
		if v, present := reg[key]; present {
			t.Errorf("gitlab_registration.%s present on a matching registration: %v", key, v)
		}
	}
	sp := rawObject(t, raw, "spec")
	if sp["source"] != "fetched" || sp["valid"] != true {
		t.Errorf("spec = %v, want fetched + valid", sp)
	}
	rv, _ := raw["reviewers"].([]any)
	if len(rv) != 2 {
		t.Errorf("reviewers = %v, want the two declared tuples", raw["reviewers"])
	}
	if _, present := raw["scopes"]; !present {
		t.Errorf("scopes absent, want the scope rung on every family")
	}
	if mg, present := raw["merge_gate"]; present {
		t.Errorf("merge_gate present on the gitlab family: %v; the surface is never read there and must not render", mg)
	}
	// The GitLab-shaped sibling rung (E45.66 / #3580) IS present, and it
	// addressed project 5 — the id the nested path resolved to — on both the
	// by-id settings read and the protected-branch list.
	glmg := rawObject(t, raw, "gitlab_merge_gate")
	if glmg["status"] != "pipeline_gated" {
		t.Errorf("gitlab_merge_gate.status = %v, want pipeline_gated", glmg["status"])
	}
	projects, files := gl.calls()
	if len(projects) != 1 || projects[0] != onboardingNestedGitLabPath {
		t.Errorf("project calls = %v, want exactly one with the full nested path", projects)
	}
	if len(files) != 1 || files[0] != onboardingNestedGitLabPath {
		t.Errorf("file calls = %v, want exactly one with the full nested path", files)
	}
	if byID, list := gl.mergeGateCalls(); byID != 1 || list != 1 {
		t.Errorf("merge-gate calls = by-id:%d list:%d, want 1/1 on project %s", byID, list, fakeGitLabProjectID)
	}
}

// TestOnboardingReadiness_GitLab_ProjectNotVisible: a 404 on the project
// resolve (forge.ErrNotInstalled) yields installed:false AND resolvable:false
// with the credential-visibility reason (which since #3582 names the
// credential, NOT the register command — registration is the
// gitlab_registration rung's business), an unavailable spec, no merge_gate
// key, and ZERO file reads. The registry is STILL consulted (once, with the
// trimmed path) and reports registered — with resolved_ref and ref_matches
// ABSENT, because the project id never resolved and an unread comparison is
// never rendered.
func TestOnboardingReadiness_GitLab_ProjectNotVisible(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gl.projectStatus = http.StatusNotFound
	s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, nil)
	registry := s.cfg.GitLabInstallations.(*fakeGitLabInstallations)
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge(onboardingNestedGitLabPath, "gitlab", &id))
	app := rawObject(t, raw, "app")
	if app["installed"] != false || app["reason"] != onboardingGitLabProjectNotVisible {
		t.Errorf("app = %v, want installed:false with the not-visible reason", app)
	}
	if app["resolvable"] != false {
		t.Errorf("app.resolvable = %v, want false (read, not absent)", app["resolvable"])
	}
	if strings.Contains(onboardingGitLabProjectNotVisible, "installation register") {
		t.Errorf("the not-visible reason names the register command; registration is the gitlab_registration rung's business: %q", onboardingGitLabProjectNotVisible)
	}
	if !strings.Contains(onboardingGitLabProjectNotVisible, "FISHHAWKD_GITLAB_TOKEN") {
		t.Errorf("the not-visible reason does not name the credential: %q", onboardingGitLabProjectNotVisible)
	}
	reg := rawObject(t, raw, "gitlab_registration")
	if reg["status"] != gitLabRegistrationStatusRegistered {
		t.Errorf("gitlab_registration.status = %v, want registered (the registry is consulted regardless of visibility)", reg["status"])
	}
	for _, key := range []string{"resolved_ref", "ref_matches"} {
		if v, present := reg[key]; present {
			t.Errorf("gitlab_registration.%s present (%v) on an unresolved project; the comparison was never made", key, v)
		}
	}
	if registry.projectCalls != 1 || registry.lastPath != onboardingNestedGitLabPath {
		t.Errorf("registry calls=%d path=%q, want 1 with the nested path", registry.projectCalls, registry.lastPath)
	}
	sp := rawObject(t, raw, "spec")
	if sp["source"] != "unavailable" || sp["note"] == "" {
		t.Errorf("spec = %v, want unavailable with a note", sp)
	}
	if _, present := raw["merge_gate"]; present {
		t.Errorf("merge_gate present on the gitlab family")
	}
	if rv, _ := raw["reviewers"].([]any); len(rv) != 0 {
		t.Errorf("reviewers = %v, want empty", raw["reviewers"])
	}
	projects, files := gl.calls()
	if len(projects) != 1 || len(files) != 0 {
		t.Errorf("calls = project:%v file:%v, want one project resolve and ZERO file reads", projects, files)
	}
}

// TestOnboardingReadiness_GitLab_SpecNotFound: project resolvable, spec file
// 404 → source unavailable with the not-found note, reviewers empty.
func TestOnboardingReadiness_GitLab_SpecNotFound(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gl.fileStatus = http.StatusNotFound
	s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, nil)
	id := testOperatorIdentity()

	code, resp := decodeReadiness(t, s, onboardingReqForge("acme/platform/widgets", "gitlab", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if !resp.App.Installed {
		t.Errorf("App = %+v, want installed", resp.App)
	}
	if resp.Spec.Source != "unavailable" || !strings.Contains(resp.Spec.Note, "no workflow spec found") {
		t.Errorf("Spec = %+v, want unavailable + not-found note", resp.Spec)
	}
	if len(resp.Reviewers) != 0 {
		t.Errorf("Reviewers = %+v, want empty", resp.Reviewers)
	}
	if resp.MergeGate != nil {
		t.Errorf("MergeGate = %+v, want nil on gitlab", resp.MergeGate)
	}
}

// TestOnboardingReadiness_GitLab_SpecInvalid / _SpecMalformed: the fetched
// bytes reach the shared classifier — semantic-invalid and YAML-broken specs
// are fetched + valid:false with an error, and the reviewer probe never runs.
func TestOnboardingReadiness_GitLab_SpecInvalid(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingInvalidSpecYAML)
	s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, nil)
	id := testOperatorIdentity()

	code, resp := decodeReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Spec.Source != "fetched" || resp.Spec.Valid || resp.Spec.Error == "" {
		t.Errorf("Spec = %+v, want fetched + invalid with an error", resp.Spec)
	}
	if len(resp.Reviewers) != 0 {
		t.Errorf("Reviewers = %+v, want empty on an invalid spec", resp.Reviewers)
	}
}

func TestOnboardingReadiness_GitLab_SpecMalformed(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingMalformedSpecYAML)
	s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, nil)
	id := testOperatorIdentity()

	code, resp := decodeReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
	if code != http.StatusOK {
		t.Fatalf("status = %d, want 200", code)
	}
	if resp.Spec.Source != "fetched" || resp.Spec.Valid || resp.Spec.Error == "" {
		t.Errorf("Spec = %+v, want fetched + invalid with a parse error", resp.Spec)
	}
	if len(resp.Reviewers) != 0 {
		t.Errorf("Reviewers = %+v, want empty on a malformed spec", resp.Reviewers)
	}
}

// TestOnboardingReadiness_GitLab_ForgeUnconfigured: the ForgeResolver errors
// for "gitlab" → still 200, installed:false with the config-gap reason, spec
// unavailable, no merge_gate.
func TestOnboardingReadiness_GitLab_ForgeUnconfigured(t *testing.T) {
	s := newOnboardingGitLabServer(t, nil, nil, nil, nil) // resolver errors for every id
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
	app := rawObject(t, raw, "app")
	if app["installed"] != false || app["reason"] != onboardingGitLabForgeUnconfigured {
		t.Errorf("app = %v, want installed:false with the unconfigured reason", app)
	}
	// The forge was never read, so resolvability is ABSENT (nil), never a
	// false — while the registry, which needs no forge, still answered.
	if v, present := app["resolvable"]; present {
		t.Errorf("app.resolvable present (%v) with the forge unconfigured; it was never read", v)
	}
	if reg := rawObject(t, raw, "gitlab_registration"); reg["status"] != gitLabRegistrationStatusRegistered {
		t.Errorf("gitlab_registration.status = %v, want registered (consulted on every gitlab-family report)", reg["status"])
	}
	sp := rawObject(t, raw, "spec")
	if sp["source"] != "unavailable" || sp["note"] != onboardingGitLabForgeUnconfigured {
		t.Errorf("spec = %v, want unavailable with the unconfigured note", sp)
	}
	if _, present := raw["merge_gate"]; present {
		t.Errorf("merge_gate present on the gitlab family")
	}
}

// TestOnboardingReadiness_GitLab_ForgeResolverTypedNil: a resolver returning a
// typed nil (*forgegitlab.Forge)(nil) inside a non-nil interface is the same
// as unconfigured — never a nil-pointer panic. Counterfactual vehicle for the
// isNilForge guard in onboardingForgeFor.
func TestOnboardingReadiness_GitLab_ForgeResolverTypedNil(t *testing.T) {
	s := newOnboardingGitLabServer(t, nil, nil, nil, nil)
	// Override the resolver so it hands back the typed nil inside a non-nil
	// interface — the shape a plain `f == nil` check misses.
	s.cfg.ForgeResolver = func(string) (forge.Forge, error) { return (*forgegitlab.Forge)(nil), nil }
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
	app := rawObject(t, raw, "app")
	if app["installed"] != false || app["reason"] != onboardingGitLabForgeUnconfigured {
		t.Errorf("app = %v, want installed:false with the unconfigured reason (typed nil)", app)
	}
	if v, present := app["resolvable"]; present {
		t.Errorf("app.resolvable present (%v) with a typed-nil forge; it was never read", v)
	}
	if _, present := raw["gitlab_registration"]; !present {
		t.Errorf("gitlab_registration absent; the rung is present on every gitlab-family report")
	}
}

// TestOnboardingReadiness_ForgeFromRegistry_GitLab: no forge param, the
// account registry answers gitlab → the gitlab family runs (the stub's project
// call is observed, the GitHub fake's installation call is not).
func TestOnboardingReadiness_ForgeFromRegistry_GitLab(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	providers := &fakeProviderResolver{provider: "gitlab", found: true}
	s := newOnboardingGitLabServer(t, gh.server(t), gitlabForgeOver(gl.server(t)), providers, nil)
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge("acme/platform/widgets", "", &id))
	if raw["forge"] != "gitlab" {
		t.Errorf("forge = %v, want gitlab (registry-resolved)", raw["forge"])
	}
	if _, present := raw["merge_gate"]; present {
		t.Errorf("merge_gate present on the registry-resolved gitlab family")
	}
	projects, _ := gl.calls()
	if len(projects) != 1 {
		t.Errorf("gitlab project calls = %v, want 1", projects)
	}
	if gh.installationCalls != 0 {
		t.Errorf("github installation calls = %d, want 0", gh.installationCalls)
	}
}

// TestOnboardingReadiness_ForgeFromRegistry_NotFound_DefaultsGitHub: no forge
// param, the registry does not know the owner → github family (the legacy
// default): the GitHub fake's installation call is observed and the raw body
// carries the merge_gate key.
func TestOnboardingReadiness_ForgeFromRegistry_NotFound_DefaultsGitHub(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	providers := &fakeProviderResolver{found: false}
	s := newOnboardingGitLabServer(t, gh.server(t), gitlabForgeOver(gl.server(t)), providers, nil)
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge("x/y", "", &id))
	if raw["forge"] != "github" {
		t.Errorf("forge = %v, want github (not-found default)", raw["forge"])
	}
	if _, present := raw["merge_gate"]; !present {
		t.Errorf("merge_gate ABSENT on the github family; the pointer change must not drop it")
	}
	if gh.installationCalls != 1 {
		t.Errorf("github installation calls = %d, want 1", gh.installationCalls)
	}
	if projects, _ := gl.calls(); len(projects) != 0 {
		t.Errorf("gitlab project calls = %v, want 0", projects)
	}
}

// TestOnboardingReadiness_ForgeFromRegistry_Fault: a resolver STORE fault after
// visibility → 503 service_unavailable with ZERO forge calls on either family.
// Counterfactual vehicle for the error return in onboardingForgeFamily.
func TestOnboardingReadiness_ForgeFromRegistry_Fault(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	providers := &fakeProviderResolver{err: errors.New("accounts store down")}
	s := newOnboardingGitLabServer(t, gh.server(t), gitlabForgeOver(gl.server(t)), providers, nil)
	id := testOperatorIdentity() // admin-bypass posture: the visibility gate never asks the resolver

	w := httptest.NewRecorder()
	s.handleGetOnboardingReadiness(w, onboardingReqForge("x/y", "", &id))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if code := errorCode(t, w); code != "service_unavailable" {
		t.Errorf("error.code = %q, want service_unavailable", code)
	}
	projects, _ := gl.calls()
	if gh.installationCalls != 0 || len(projects) != 0 {
		t.Errorf("forge calls = github:%d gitlab:%v, want 0/0", gh.installationCalls, projects)
	}
}

// TestOnboardingReadiness_ForgeParam_Invalid: forge=bitbucket → 400
// validation_failed on field forge, BEFORE visibility (mirror never asked) and
// with ZERO forge calls. Counterfactual vehicle for the forge enum check: with
// it deleted the value falls into the github branch and answers 200.
func TestOnboardingReadiness_ForgeParam_Invalid(t *testing.T) {
	gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	vis := newFakeRepoVisibility(map[string]bool{"x/y": true}) // would ALLOW if reached
	s := newOnboardingVisServer(t, gh.server(t), vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

	w := httptest.NewRecorder()
	s.handleGetOnboardingReadiness(w, onboardingReqForge("x/y", "bitbucket", ptrIdentity(memberIdentity())))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if env.Error.Code != "validation_failed" || env.Error.Details["field"] != "forge" {
		t.Errorf("error = %+v, want validation_failed on field forge", env.Error)
	}
	if vis.callCount() != 0 {
		t.Errorf("mirror Visible calls = %d, want 0 (forge validated before visibility)", vis.callCount())
	}
	if gh.installationCalls != 0 {
		t.Errorf("github installation calls = %d, want 0", gh.installationCalls)
	}
}

func ptrIdentity(id Identity) *Identity { return &id }

// TestOnboardingReadiness_GitHub_NestedPathRejected_BeforeVisibility pins
// approval condition 1's first order: with forge=github EXPLICIT, a/b/c is
// rejected 400 validation_failed (message naming forge=gitlab) BEFORE
// enforceRepoVisibility — a deny-all mirror is never asked, and no GitHub call
// is made. Counterfactual vehicle for the explicit-github nested rejection:
// with it deleted the request reaches visibility and goes 403 here.
func TestOnboardingReadiness_GitHub_NestedPathRejected_BeforeVisibility(t *testing.T) {
	gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	vis := newFakeRepoVisibility(map[string]bool{}) // denies everything
	s := newOnboardingVisServer(t, gh.server(t), vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

	w := httptest.NewRecorder()
	s.handleGetOnboardingReadiness(w, onboardingReqForge("a/b/c", "github", ptrIdentity(memberIdentity())))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (explicit github nested check precedes visibility):\n%s", w.Code, w.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if env.Error.Code != "validation_failed" || !strings.Contains(env.Error.Message, "forge=gitlab") {
		t.Errorf("error = %+v, want validation_failed naming forge=gitlab", env.Error)
	}
	if vis.callCount() != 0 {
		t.Errorf("mirror Visible calls = %d, want 0", vis.callCount())
	}
	if gh.installationCalls != 0 || gh.specCalls != 0 {
		t.Errorf("github calls = install:%d spec:%d, want 0/0", gh.installationCalls, gh.specCalls)
	}
}

// TestOnboardingReadiness_ForgeFromRegistry_GitHub_NestedPathRejected_AfterVisibility
// pins approval condition 1's second order: with forge OMITTED the family is
// only known after registry resolution, which follows visibility — so a
// registry-resolved github family rejects a/b/c 400 AFTER the mirror was
// consulted (allowed sub-case: Visible called once, then 400 with zero GitHub
// calls; denied sub-case: 403 wins and the nested check is never reached).
// Counterfactual vehicle for the registry-resolved nested rejection: deleted,
// the allowed sub-case answers 200 with Name "b/c".
func TestOnboardingReadiness_ForgeFromRegistry_GitHub_NestedPathRejected_AfterVisibility(t *testing.T) {
	providers := &fakeProviderResolver{provider: "github", found: true}

	t.Run("visible_then_400", func(t *testing.T) {
		gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
		vis := newFakeRepoVisibility(map[string]bool{"a/b/c": true})
		s := newOnboardingVisServer(t, gh.server(t), vis, fakeAccountRoles{role: account.RoleMember}, providers, nil)

		w := runOnboarding(s, "a/b/c", memberIdentity())
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (registry-resolved github rejects the nested path):\n%s", w.Code, w.Body.String())
		}
		var env errorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if env.Error.Code != "validation_failed" || !strings.Contains(env.Error.Message, "forge=gitlab") {
			t.Errorf("error = %+v, want validation_failed naming forge=gitlab", env.Error)
		}
		if vis.callCount() != 1 {
			t.Errorf("mirror Visible calls = %d, want 1 (visibility ran BEFORE the family was known)", vis.callCount())
		}
		if gh.installationCalls != 0 || gh.specCalls != 0 {
			t.Errorf("github calls = install:%d spec:%d, want 0/0", gh.installationCalls, gh.specCalls)
		}
	})

	t.Run("denied_403_wins", func(t *testing.T) {
		gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
		vis := newFakeRepoVisibility(map[string]bool{}) // denies everything
		s := newOnboardingVisServer(t, gh.server(t), vis, fakeAccountRoles{role: account.RoleMember}, providers, nil)

		w := runOnboarding(s, "a/b/c", memberIdentity())
		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (visibility precedes the registry-resolved nested check):\n%s", w.Code, w.Body.String())
		}
		if gh.installationCalls != 0 {
			t.Errorf("github installation calls = %d, want 0", gh.installationCalls)
		}
	})
}

// TestOnboardingReadiness_GitHub_MergeGateKeyPresent guards the pointer change
// on the response type: the github family still SERIALIZES the merge_gate key
// (raw-body assertion), and the body names forge=github.
func TestOnboardingReadiness_GitHub_MergeGateKeyPresent(t *testing.T) {
	gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	s := newOnboardingServer(t, gh.server(t), nil)
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReq("x/y", &id))
	if raw["forge"] != "github" {
		t.Errorf("forge = %v, want github", raw["forge"])
	}
	mg := rawObject(t, raw, "merge_gate")
	if mg["status"] == "" || mg["status"] == nil {
		t.Errorf("merge_gate.status = %v, want a verdict on the github family", mg["status"])
	}
	// The GitLab-shaped sibling is OMITTED on github (E45.66 / #3580): the
	// counterfactual vehicle for the omission — assigning
	// `&gitLabMergeGateReadiness{}` on the github arm turns this red.
	if g, present := raw["gitlab_merge_gate"]; present {
		t.Errorf("gitlab_merge_gate present on the github family: %v; the rung is gitlab-only", g)
	}
	app := rawObject(t, raw, "app")
	if _, present := app["note"]; present {
		t.Errorf("app.note present on github (%v); the note is gitlab-only", app["note"])
	}
}

// TestOnboardingReadiness_ExplicitForgeOverridesRegistry: forge=github with a
// registry that says gitlab → the github path runs (installation call
// observed, no gitlab project call, merge_gate present).
func TestOnboardingReadiness_ExplicitForgeOverridesRegistry(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	providers := &fakeProviderResolver{provider: "gitlab", found: true}
	s := newOnboardingGitLabServer(t, gh.server(t), gitlabForgeOver(gl.server(t)), providers, nil)
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge("x/y", "github", &id))
	if raw["forge"] != "github" {
		t.Errorf("forge = %v, want github (explicit overrides registry)", raw["forge"])
	}
	if _, present := raw["merge_gate"]; !present {
		t.Errorf("merge_gate absent on the explicit github family")
	}
	if gh.installationCalls != 1 {
		t.Errorf("github installation calls = %d, want 1", gh.installationCalls)
	}
	if projects, _ := gl.calls(); len(projects) != 0 {
		t.Errorf("gitlab project calls = %v, want 0", projects)
	}
}

// --- Validate-then-use trim mismatch (#3488) --------------------------------

// TestOnboardingReadiness_PaddedRepo_TrimmedOnce is the CROSS-BOUNDARY
// counterfactual vehicle for the single upfront strings.TrimSpace in
// handleGetOnboardingReadiness: account.ProjectPathWellFormed trims its input
// INTERNALLY, so a padded repo query passes validation regardless of whether
// the handler itself trims — the only way to observe a missing trim is to
// watch what the padded value reaches DOWNSTREAM of validation (the
// visibility mirror, the forge, and the echoed response field). Deleting the
// handler's TrimSpace and re-running this test must go RED: sub-test
// "github" then draws a spurious 403 (the mirror is asked about "  x/y  ", a
// key its fixture never seeds — the deny is default-false by construction),
// and sub-test "gitlab" then records the padded project path and echoes the
// padded repo.
func TestOnboardingReadiness_PaddedRepo_TrimmedOnce(t *testing.T) {
	t.Run("github", func(t *testing.T) {
		fake := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
		ghSrv := fake.server(t)
		// Keyed ONLY by the trimmed path: a padded value reaching the
		// mirror verbatim is denied by construction (the fake's default
		// answer for an unlisted key is false).
		vis := newFakeRepoVisibility(map[string]bool{"x/y": true})
		s := newOnboardingVisServer(t, ghSrv, vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

		mid := memberIdentity()
		code, resp := decodeReadiness(t, s, onboardingReq("  x/y  ", &mid))
		if code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (padded repo trimmed before visibility)", code)
		}
		if resp.Repo != "x/y" {
			t.Errorf("Repo = %q, want the trimmed value echoed", resp.Repo)
		}
		vis.mu.Lock()
		calls := append([]string(nil), vis.calls...)
		vis.mu.Unlock()
		if len(calls) != 1 || !strings.HasSuffix(calls[0], "|x/y") {
			t.Errorf("mirror calls = %v, want exactly one ending in |x/y (trimmed before the visibility check)", calls)
		}
		if fake.installationCalls != 1 {
			t.Errorf("github installation calls = %d, want 1", fake.installationCalls)
		}
	})

	t.Run("gitlab", func(t *testing.T) {
		gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
		s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, nil)
		id := testOperatorIdentity()

		raw := rawReadiness(t, s, onboardingReqForge("  acme/platform/widgets  ", "gitlab", &id))
		if raw["repo"] != "acme/platform/widgets" {
			t.Errorf("repo = %v, want the trimmed value echoed", raw["repo"])
		}
		projects, _ := gl.calls()
		if len(projects) != 1 || projects[0] != "acme/platform/widgets" {
			t.Errorf("gitlab project calls = %v, want exactly one with the trimmed path "+
				"(a padded Owner would surface as an escaped-space path)", projects)
		}
	})

	// The 400-before-visibility ordering (approval condition 1 / #3348) is
	// unchanged by the trim: a padded EXPLICIT-github nested path still 400s
	// naming forge=gitlab, with zero visibility calls.
	t.Run("padded_nested_github_before_visibility", func(t *testing.T) {
		gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
		vis := newFakeRepoVisibility(map[string]bool{}) // denies everything
		s := newOnboardingVisServer(t, gh.server(t), vis, fakeAccountRoles{role: account.RoleMember}, nil, nil)

		w := httptest.NewRecorder()
		s.handleGetOnboardingReadiness(w, onboardingReqForge("  a/b/c  ", "github", ptrIdentity(memberIdentity())))
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (explicit github nested check precedes visibility):\n%s", w.Code, w.Body.String())
		}
		var env errorEnvelope
		if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode error: %v", err)
		}
		if env.Error.Code != "validation_failed" || !strings.Contains(env.Error.Message, "forge=gitlab") {
			t.Errorf("error = %+v, want validation_failed naming forge=gitlab", env.Error)
		}
		if vis.callCount() != 0 {
			t.Errorf("mirror Visible calls = %d, want 0", vis.callCount())
		}
		if gh.installationCalls != 0 || gh.specCalls != 0 {
			t.Errorf("github calls = install:%d spec:%d, want 0/0", gh.installationCalls, gh.specCalls)
		}
	})
}

// --- probeGitLab degrade arms (#3488, issue Notes item 2) -------------------

// gitlabResolveOnlyForge is a forge.Forge implementing only Name() and
// ResolveRepoScope; the rest embeds a nil forge.Forge, unreachable in these
// tests. Because forge.FileFetcher (forge.go:137) is a SEPARATE interface
// from forge.Forge, this type does NOT implement it — an embedded nil
// forge.Forge cannot satisfy FileFetcher either — so probeGitLab's
// `f.(forge.FileFetcher)` type assertion fails by construction, exercising
// the "gitlab forge does not expose file reads" degrade without a special
// case in the fake.
type gitlabResolveOnlyForge struct {
	forge.Forge
	resolveErr error
}

func (f *gitlabResolveOnlyForge) Name() string { return "gitlab" }

func (f *gitlabResolveOnlyForge) ResolveRepoScope(_ context.Context, _ forge.RepoRef) (forge.CredentialScope, error) {
	if f.resolveErr != nil {
		return forge.CredentialScope{}, f.resolveErr
	}
	return forge.CredentialScope{}, nil
}

// gitlabFetchErrForge adds a failing FetchFile on top of a successfully
// resolving gitlabResolveOnlyForge, so it DOES implement forge.FileFetcher
// (unlike its embedded base) and exercises the fetch-fault degrade.
type gitlabFetchErrForge struct {
	gitlabResolveOnlyForge
	fetchErr error
}

func (f *gitlabFetchErrForge) FetchFile(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, _, _ string) (*forge.FileContent, error) {
	return nil, f.fetchErr
}

// TestOnboardingReadiness_GitLab_ForgeWithoutFileReads: the forge resolves the
// project but does not implement forge.FileFetcher → 200, app installed (the
// resolve succeeded), spec unavailable with the file-reads note, empty
// non-null reviewers, no merge_gate. Counterfactual (run, not reasoned):
// delete the `fetcher, ok := f.(forge.FileFetcher); if !ok {...}` guard in
// probeGitLab and call `f.(forge.FileFetcher).FetchFile` directly — this test
// goes RED with a type-assertion panic recovered/reported by the test
// runner, restore after observing it.
func TestOnboardingReadiness_GitLab_ForgeWithoutFileReads(t *testing.T) {
	f := &gitlabResolveOnlyForge{}
	s := newOnboardingGitLabServer(t, nil, f, nil, nil)
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
	app := rawObject(t, raw, "app")
	if app["installed"] != true || app["note"] != onboardingGitLabInstalledNote {
		t.Errorf("app = %v, want installed:true with the gitlab note", app)
	}
	if _, present := app["reason"]; present {
		t.Errorf("app.reason present on a successful resolve: %v", app["reason"])
	}
	sp := rawObject(t, raw, "spec")
	if sp["source"] != "unavailable" || sp["note"] != "gitlab forge does not expose file reads" {
		t.Errorf("spec = %v, want unavailable + the file-reads note", sp)
	}
	rv, ok := raw["reviewers"].([]any)
	if !ok || len(rv) != 0 {
		t.Errorf("reviewers = %v, want an empty non-null list", raw["reviewers"])
	}
	if _, present := raw["merge_gate"]; present {
		t.Errorf("merge_gate present on the gitlab family")
	}
}

// TestOnboardingReadiness_GitLab_ResolveFault: ResolveRepoScope fails with an
// error that does NOT wrap forge.ErrNotInstalled → 200, app.installed false
// with the raw error text as reason, spec unavailable with the
// resolve-failure note, and a WARN log naming the failure. Counterfactuals
// (run, not reasoned): (1) delete the s.cfg.Logger.Warn call in this arm of
// probeGitLab → the log assertion goes RED; (2) change the `default:` case to
// route through the ErrNotInstalled branch instead → the app.reason
// assertion goes RED (it would read the register-command reason instead of
// the raw error text). Restore after each.
func TestOnboardingReadiness_GitLab_ResolveFault(t *testing.T) {
	resolveErr := errors.New("gitlab 502 bad gateway")
	f := &gitlabResolveOnlyForge{resolveErr: resolveErr}
	s := newOnboardingGitLabServer(t, nil, f, nil, nil)
	logBuf := &bytes.Buffer{}
	s.cfg.Logger = slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
	app := rawObject(t, raw, "app")
	if app["installed"] != false || app["reason"] != resolveErr.Error() {
		t.Errorf("app = %v, want installed:false reason=%q", app, resolveErr.Error())
	}
	sp := rawObject(t, raw, "spec")
	const wantNote = "project is not resolvable with the deployment GitLab credential; cannot fetch the workflow spec"
	if sp["source"] != "unavailable" || sp["note"] != wantNote {
		t.Errorf("spec = %v, want unavailable + %q", sp, wantNote)
	}
	rec := soleLogRecord(t, logBuf, "onboarding readiness: resolve gitlab project failed")
	if rec["level"] != "WARN" {
		t.Errorf("log level = %v, want WARN", rec["level"])
	}
	if rec["repo"] != "acme/widgets" {
		t.Errorf("log repo = %v, want acme/widgets", rec["repo"])
	}
	if rec["error"] != resolveErr.Error() {
		t.Errorf("log error = %v, want %q", rec["error"], resolveErr.Error())
	}
}

// TestOnboardingReadiness_GitLab_FetchFault: resolve succeeds but FetchFile
// fails with an error that does NOT wrap forge.ErrNotFound → 200, app
// installed (resolve succeeded), spec unavailable with the raw error text as
// note, and a WARN log naming the failure. Counterfactual (run, not
// reasoned): delete the s.cfg.Logger.Warn call in this arm of probeGitLab →
// the log assertion goes RED. Restore after observing it.
func TestOnboardingReadiness_GitLab_FetchFault(t *testing.T) {
	fetchErr := errors.New("gitlab 500 on repository files")
	f := &gitlabFetchErrForge{fetchErr: fetchErr}
	s := newOnboardingGitLabServer(t, nil, f, nil, nil)
	logBuf := &bytes.Buffer{}
	s.cfg.Logger = slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
	app := rawObject(t, raw, "app")
	if app["installed"] != true {
		t.Errorf("app = %v, want installed:true (resolve succeeded)", app)
	}
	sp := rawObject(t, raw, "spec")
	if sp["source"] != "unavailable" || sp["note"] != fetchErr.Error() {
		t.Errorf("spec = %v, want unavailable note=%q", sp, fetchErr.Error())
	}
	rec := soleLogRecord(t, logBuf, "onboarding readiness: fetch gitlab workflow spec failed")
	if rec["level"] != "WARN" {
		t.Errorf("log level = %v, want WARN", rec["level"])
	}
	if rec["repo"] != "acme/widgets" {
		t.Errorf("log repo = %v, want acme/widgets", rec["repo"])
	}
	if rec["error"] != fetchErr.Error() {
		t.Errorf("log error = %v, want %q", rec["error"], fetchErr.Error())
	}
}

// --- gitlab_merge_gate rung (E45.66 / #3580) --------------------------------

// gitLabMergeGateSignalKeys are the pointer-bool / list keys the rung renders
// ONLY on an authoritative read. Every `unknown` test asserts each is ABSENT
// from the raw object — an unread signal must never render as false.
var gitLabMergeGateSignalKeys = []string{
	"protected", "matched_rules", "allow_force_push", "push_access_levels", "merge_access_levels",
	"pipeline_must_succeed", "allow_skipped_pipeline", "discussions_must_be_resolved", "branch",
}

// assertGitLabMergeGateUnknown asserts the rung's fail-closed rendering: status
// unknown, the named reason, authoritative false, the constant note, and
// every signal key absent.
func assertGitLabMergeGateUnknown(t *testing.T, raw map[string]any, wantReason string) map[string]any {
	t.Helper()
	glmg := rawObject(t, raw, "gitlab_merge_gate")
	if glmg["status"] != "unknown" {
		t.Errorf("gitlab_merge_gate.status = %v, want unknown", glmg["status"])
	}
	if glmg["reason"] != wantReason {
		t.Errorf("gitlab_merge_gate.reason = %v, want %q", glmg["reason"], wantReason)
	}
	if glmg["authoritative"] != false {
		t.Errorf("gitlab_merge_gate.authoritative = %v, want false", glmg["authoritative"])
	}
	if glmg["note"] != gitLabMergeGateNote {
		t.Errorf("gitlab_merge_gate.note = %v, want the constant note", glmg["note"])
	}
	for _, k := range gitLabMergeGateSignalKeys {
		if v, present := glmg[k]; present {
			t.Errorf("gitlab_merge_gate.%s present (%v) on an unknown verdict; an unread signal must be ABSENT, never false", k, v)
		}
	}
	if _, present := raw["merge_gate"]; present {
		t.Errorf("merge_gate present on the gitlab family")
	}
	return glmg
}

// gitLabMergeGateRaw drives the gitlab family end to end over the stub and
// returns the raw body.
func gitLabMergeGateRaw(t *testing.T, gl *fakeGitLabForOnboarding) map[string]any {
	t.Helper()
	s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, nil)
	id := testOperatorIdentity()
	return rawReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
}

// TestOnboardingReadiness_GitLabMergeGate_EndToEnd is the CROSS-BOUNDARY test
// for the rung: HTTP → handler → the REAL forgegitlab.Forge → the real
// gitlabclient → the v4 stub → the RAW JSON body. The default fixture (one
// exact `main` rule, pipeline must succeed) renders pipeline_gated with every
// signal mapped, the constant note, and NO merge_gate key.
func TestOnboardingReadiness_GitLabMergeGate_EndToEnd(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	raw := gitLabMergeGateRaw(t, gl)

	if _, present := raw["merge_gate"]; present {
		t.Errorf("merge_gate present on the gitlab family; the GitHub-shaped rung must stay omitted")
	}
	glmg := rawObject(t, raw, "gitlab_merge_gate")
	if glmg["status"] != "pipeline_gated" {
		t.Errorf("status = %v, want pipeline_gated", glmg["status"])
	}
	if glmg["branch"] != "main" {
		t.Errorf("branch = %v, want main (the project's real default branch)", glmg["branch"])
	}
	if glmg["protected"] != true {
		t.Errorf("protected = %v, want true", glmg["protected"])
	}
	if rules, _ := glmg["matched_rules"].([]any); len(rules) != 1 || rules[0] != "main" {
		t.Errorf("matched_rules = %v, want [main]", glmg["matched_rules"])
	}
	if glmg["allow_force_push"] != false {
		t.Errorf("allow_force_push = %v, want false (read, not absent)", glmg["allow_force_push"])
	}
	if glmg["pipeline_must_succeed"] != true {
		t.Errorf("pipeline_must_succeed = %v, want true", glmg["pipeline_must_succeed"])
	}
	if glmg["allow_skipped_pipeline"] != false {
		t.Errorf("allow_skipped_pipeline = %v, want false (read, not absent)", glmg["allow_skipped_pipeline"])
	}
	if glmg["discussions_must_be_resolved"] != true {
		t.Errorf("discussions_must_be_resolved = %v, want true", glmg["discussions_must_be_resolved"])
	}
	if glmg["authoritative"] != true {
		t.Errorf("authoritative = %v, want true", glmg["authoritative"])
	}
	if glmg["note"] != gitLabMergeGateNote {
		t.Errorf("note = %v, want the constant note", glmg["note"])
	}
	if !strings.Contains(gitLabMergeGateNote, "no per-context required status check") ||
		!strings.Contains(gitLabMergeGateNote, "UNION across every matching protected-branch rule") {
		t.Errorf("note does not state the per-context caveat and the union semantics: %q", gitLabMergeGateNote)
	}
	push, _ := glmg["push_access_levels"].([]any)
	merge, _ := glmg["merge_access_levels"].([]any)
	if len(push) != 1 || len(merge) != 1 {
		t.Fatalf("access levels = push:%v merge:%v, want one entry each", glmg["push_access_levels"], glmg["merge_access_levels"])
	}
	if lvl, _ := push[0].(map[string]any); lvl["level"] != float64(0) || lvl["description"] != "No one" {
		t.Errorf("push_access_levels[0] = %v, want level 0 / No one", push[0])
	}
	if lvl, _ := merge[0].(map[string]any); lvl["level"] != float64(40) || lvl["description"] != "Maintainers" {
		t.Errorf("merge_access_levels[0] = %v, want level 40 / Maintainers", merge[0])
	}
	if _, present := glmg["reason"]; present {
		t.Errorf("reason present on an authoritative verdict: %v", glmg["reason"])
	}
	if byID, list := gl.mergeGateCalls(); byID != 1 || list != 1 {
		t.Errorf("merge-gate calls = by-id:%d list:%d, want 1/1", byID, list)
	}
}

// TestOnboardingReadiness_GitLabMergeGate_OverlappingRules_Union pins the
// approval-condition-1 wire shape end to end: an exact `main` rule (merge 40,
// no force push) and a wildcard `m*` rule (merge 30, force push allowed) both
// match, so matched_rules is [main, m*], merge levels are the ascending union
// [30, 40] and allow_force_push is the OR (true).
func TestOnboardingReadiness_GitLabMergeGate_OverlappingRules_Union(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gl.protectedRulesJSON = `[
	  {"id":1,"name":"main","push_access_levels":[{"access_level":40,"access_level_description":"Maintainers"}],"merge_access_levels":[{"access_level":40,"access_level_description":"Maintainers"}],"allow_force_push":false},
	  {"id":2,"name":"m*","push_access_levels":[{"access_level":30,"access_level_description":"Developers + Maintainers"}],"merge_access_levels":[{"access_level":30,"access_level_description":"Developers + Maintainers"}],"allow_force_push":true}
	]`
	raw := gitLabMergeGateRaw(t, gl)
	glmg := rawObject(t, raw, "gitlab_merge_gate")
	if glmg["status"] != "pipeline_gated" || glmg["protected"] != true {
		t.Errorf("status/protected = %v/%v, want pipeline_gated/true", glmg["status"], glmg["protected"])
	}
	rules, _ := glmg["matched_rules"].([]any)
	if len(rules) != 2 || rules[0] != "main" || rules[1] != "m*" {
		t.Errorf("matched_rules = %v, want [main m*] (exact first, then wildcards)", glmg["matched_rules"])
	}
	if glmg["allow_force_push"] != true {
		t.Errorf("allow_force_push = %v, want true (OR across matching rules)", glmg["allow_force_push"])
	}
	merge, _ := glmg["merge_access_levels"].([]any)
	if len(merge) != 2 {
		t.Fatalf("merge_access_levels = %v, want the two-level union", glmg["merge_access_levels"])
	}
	l0, _ := merge[0].(map[string]any)
	l1, _ := merge[1].(map[string]any)
	if l0["level"] != float64(30) || l1["level"] != float64(40) {
		t.Errorf("merge_access_levels = %v, want ascending [30, 40] (most permissive first)", merge)
	}
}

// TestOnboardingReadiness_GitLabMergeGate_NotProtected_NotPipelineGated: an
// authoritative EMPTY rule list → not_pipeline_gated, protected false (read,
// present), the project settings still mapped, and detail naming the
// unprotected branch.
func TestOnboardingReadiness_GitLabMergeGate_NotProtected_NotPipelineGated(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gl.protectedRulesJSON = `[]`
	raw := gitLabMergeGateRaw(t, gl)
	glmg := rawObject(t, raw, "gitlab_merge_gate")
	if glmg["status"] != "not_pipeline_gated" {
		t.Errorf("status = %v, want not_pipeline_gated", glmg["status"])
	}
	if glmg["protected"] != false {
		t.Errorf("protected = %v, want false (an authoritative empty list, so PRESENT)", glmg["protected"])
	}
	if _, present := glmg["matched_rules"]; present {
		t.Errorf("matched_rules present with no matching rule: %v", glmg["matched_rules"])
	}
	if glmg["pipeline_must_succeed"] != true {
		t.Errorf("pipeline_must_succeed = %v, want true (still mapped)", glmg["pipeline_must_succeed"])
	}
	if glmg["authoritative"] != true {
		t.Errorf("authoritative = %v, want true", glmg["authoritative"])
	}
	detail, _ := glmg["detail"].(string)
	if !strings.Contains(detail, "default branch main is not covered by any protected-branch rule") {
		t.Errorf("detail = %q, want it to name the unprotected default branch", detail)
	}
	if strings.Contains(detail, "only_allow_merge_if_pipeline_succeeds") {
		t.Errorf("detail = %q names the pipeline setting, which is ON", detail)
	}
	if rem, _ := glmg["remediation"].(string); !strings.Contains(rem, "Protected branches") {
		t.Errorf("remediation = %q, want the GitLab settings path", rem)
	}
}

// TestOnboardingReadiness_GitLabMergeGate_PipelineNotRequired_NotPipelineGated:
// the branch is protected but only_allow_merge_if_pipeline_succeeds is off →
// not_pipeline_gated with detail naming the setting.
func TestOnboardingReadiness_GitLabMergeGate_PipelineNotRequired_NotPipelineGated(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gl.projectSettingsJSON = `"default_branch":"main","only_allow_merge_if_pipeline_succeeds":false,"allow_merge_on_skipped_pipeline":true,"only_allow_merge_if_all_discussions_are_resolved":false`
	raw := gitLabMergeGateRaw(t, gl)
	glmg := rawObject(t, raw, "gitlab_merge_gate")
	if glmg["status"] != "not_pipeline_gated" {
		t.Errorf("status = %v, want not_pipeline_gated", glmg["status"])
	}
	if glmg["protected"] != true {
		t.Errorf("protected = %v, want true", glmg["protected"])
	}
	if glmg["pipeline_must_succeed"] != false {
		t.Errorf("pipeline_must_succeed = %v, want false (read, PRESENT)", glmg["pipeline_must_succeed"])
	}
	if glmg["allow_skipped_pipeline"] != true || glmg["discussions_must_be_resolved"] != false {
		t.Errorf("informational settings = skipped:%v discussions:%v, want true/false", glmg["allow_skipped_pipeline"], glmg["discussions_must_be_resolved"])
	}
	detail, _ := glmg["detail"].(string)
	if !strings.Contains(detail, "only_allow_merge_if_pipeline_succeeds is off") {
		t.Errorf("detail = %q, want it to name the pipeline setting", detail)
	}
	if strings.Contains(detail, "not covered by any protected-branch rule") {
		t.Errorf("detail = %q names the branch as unprotected, but it IS protected", detail)
	}
}

// TestOnboardingReadiness_GitLabMergeGate_ForgeUnconfigured_Unknown: no gitlab
// forge → unknown / gitlab_forge_unconfigured, every signal key ABSENT (pins
// the pointer-bool absence rule).
func TestOnboardingReadiness_GitLabMergeGate_ForgeUnconfigured_Unknown(t *testing.T) {
	s := newOnboardingGitLabServer(t, nil, nil, nil, nil)
	id := testOperatorIdentity()
	raw := rawReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
	glmg := assertGitLabMergeGateUnknown(t, raw, gitLabMergeGateReasonForgeUnconfigured)
	if rem, _ := glmg["remediation"].(string); !strings.Contains(rem, "FISHHAWKD_GITLAB_TOKEN") {
		t.Errorf("remediation = %q, want the env vars named", rem)
	}
}

// TestOnboardingReadiness_GitLabMergeGate_ProjectNotVisible_Unknown: a 404 on
// the path lookup → unknown / project_not_visible with the register command,
// and ZERO by-id / list calls (the probe never reaches the forge).
func TestOnboardingReadiness_GitLabMergeGate_ProjectNotVisible_Unknown(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gl.projectStatus = http.StatusNotFound
	raw := gitLabMergeGateRaw(t, gl)
	glmg := assertGitLabMergeGateUnknown(t, raw, gitLabMergeGateReasonProjectNotVisible)
	if rem, _ := glmg["remediation"].(string); !strings.Contains(rem, "FISHHAWKD_GITLAB_TOKEN) can read acme/widgets") {
		t.Errorf("remediation = %q, want the credential-visibility sentence naming the project", rem)
	}
	if byID, list := gl.mergeGateCalls(); byID != 0 || list != 0 {
		t.Errorf("merge-gate calls = by-id:%d list:%d, want 0/0 on an unresolved project", byID, list)
	}
}

// TestOnboardingReadiness_GitLabMergeGate_Unsupported_Unknown: a forge that
// resolves the project but does not implement forge.MergeProtectionReader
// (gitlabFetchErrForge: FileFetcher only) → unknown /
// merge_protection_unsupported.
func TestOnboardingReadiness_GitLabMergeGate_Unsupported_Unknown(t *testing.T) {
	f := &gitlabFetchErrForge{fetchErr: errors.New("unused")}
	if _, ok := forge.Forge(f).(forge.MergeProtectionReader); ok {
		t.Fatalf("fixture implements MergeProtectionReader; the test needs one that does not")
	}
	s := newOnboardingGitLabServer(t, nil, f, nil, nil)
	id := testOperatorIdentity()
	raw := rawReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
	assertGitLabMergeGateUnknown(t, raw, gitLabMergeGateReasonUnsupported)
}

// TestOnboardingReadiness_GitLabMergeGate_DefaultBranchEmpty_Unknown: the
// project body carries an empty default_branch (an empty repository) →
// unknown / default_branch_unresolved, and the list is never read.
func TestOnboardingReadiness_GitLabMergeGate_DefaultBranchEmpty_Unknown(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gl.projectSettingsJSON = `"default_branch":"","only_allow_merge_if_pipeline_succeeds":true`
	raw := gitLabMergeGateRaw(t, gl)
	assertGitLabMergeGateUnknown(t, raw, gitLabMergeGateReasonDefaultBranch)
	if byID, list := gl.mergeGateCalls(); byID != 1 || list != 0 {
		t.Errorf("merge-gate calls = by-id:%d list:%d, want 1/0 (no default branch, no rule read)", byID, list)
	}
}

// TestOnboardingReadiness_GitLabMergeGate_ProjectByIDForbidden_Unknown: a 403
// on the by-id project read → unknown / forbidden, list never read (pins the
// project-first order through the whole stack).
func TestOnboardingReadiness_GitLabMergeGate_ProjectByIDForbidden_Unknown(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gl.projectByIDStatus = http.StatusForbidden
	raw := gitLabMergeGateRaw(t, gl)
	assertGitLabMergeGateUnknown(t, raw, gitLabMergeGateReasonForbidden)
	if byID, list := gl.mergeGateCalls(); byID != 1 || list != 0 {
		t.Errorf("merge-gate calls = by-id:%d list:%d, want 1/0", byID, list)
	}
}

// TestOnboardingReadiness_GitLabMergeGate_ProtectedListForbidden_Unknown is the
// COUNTERFACTUAL vehicle for the fail-closed control: a 200 project read but a
// 403 on the protected-branch list (the endpoint needs Maintainer) → unknown /
// forbidden with the credential-role detail, `protected` ABSENT, and a WARN
// log. Deleting the `if err != nil` branch in probeGitLabMergeGate so the
// verdict renders anyway turns this red (status not unknown, `protected`
// present).
func TestOnboardingReadiness_GitLabMergeGate_ProtectedListForbidden_Unknown(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gl.protectedStatus = http.StatusForbidden
	s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, nil)
	logBuf := &bytes.Buffer{}
	s.cfg.Logger = slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	id := testOperatorIdentity()
	raw := rawReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))

	glmg := assertGitLabMergeGateUnknown(t, raw, gitLabMergeGateReasonForbidden)
	if detail, _ := glmg["detail"].(string); !strings.Contains(detail, "Maintainer role") {
		t.Errorf("detail = %q, want the credential-role sentence", detail)
	}
	if rem, _ := glmg["remediation"].(string); !strings.Contains(rem, "Maintainer") {
		t.Errorf("remediation = %q, want the role to grant", rem)
	}
	rec := soleLogRecord(t, logBuf, "onboarding readiness: gitlab merge gate read failed")
	if rec["level"] != "WARN" || rec["reason"] != gitLabMergeGateReasonForbidden || rec["repo"] != "acme/widgets" {
		t.Errorf("log = %v, want WARN with reason=forbidden repo=acme/widgets", rec)
	}
	if byID, list := gl.mergeGateCalls(); byID != 1 || list != 1 {
		t.Errorf("merge-gate calls = by-id:%d list:%d, want 1/1", byID, list)
	}
}

// TestOnboardingReadiness_GitLabMergeGate_TransportError_Unknown: the stub is
// closed before the merge-gate reads (the path lookup + spec fetch are served
// by a forge over a DIFFERENT, live stub) → unknown / transport_error.
func TestOnboardingReadiness_GitLabMergeGate_TransportError_Unknown(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	srv := gl.server(t)
	// A forge whose client keeps a connection-less transport to a server that
	// is already gone: every read fails at the dial.
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	f := &gitlabSplitForge{
		live: gitlabForgeOver(srv),
		dead: forgegitlab.New(deadURL, forgegitlab.NewStaticCredentialProvider("glpat-test")),
	}
	s := newOnboardingGitLabServer(t, nil, f, nil, nil)
	id := testOperatorIdentity()
	raw := rawReadiness(t, s, onboardingReqForge("acme/widgets", "gitlab", &id))
	glmg := assertGitLabMergeGateUnknown(t, raw, gitLabMergeGateReasonTransport)
	if rem, _ := glmg["remediation"].(string); !strings.Contains(rem, "once the forge is reachable") {
		t.Errorf("remediation = %q, want the re-run sentence", rem)
	}
	app := rawObject(t, raw, "app")
	if app["installed"] != true {
		t.Errorf("app.installed = %v, want true (the live half resolved the project)", app["installed"])
	}
}

// gitlabSplitForge routes resolve + file reads to a live forgegitlab.Forge and
// the merge-protection read to one pointed at a closed server, so the
// transport-error arm is exercised with rung (1) still installed.
type gitlabSplitForge struct {
	forge.Forge
	live *forgegitlab.Forge
	dead *forgegitlab.Forge
}

func (f *gitlabSplitForge) Name() string { return "gitlab" }

func (f *gitlabSplitForge) ResolveRepoScope(ctx context.Context, repo forge.RepoRef) (forge.CredentialScope, error) {
	return f.live.ResolveRepoScope(ctx, repo)
}

func (f *gitlabSplitForge) FetchFile(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, path, ref string) (*forge.FileContent, error) {
	return f.live.FetchFile(ctx, scope, repo, path, ref)
}

func (f *gitlabSplitForge) ReadMergeProtection(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, branch string) (*forge.MergeProtection, error) {
	return f.dead.ReadMergeProtection(ctx, scope, repo, branch)
}

// TestOnboardingReadiness_GitLabMergeGate_ProbeTimeout_Unknown asserts the
// bounded probe: a list route that never answers within mergeGateProbeTimeout
// resolves to unknown / transport_error rather than hanging the report. Every
// deadline-competing duration is derived via timescale.D (#1984).
func TestOnboardingReadiness_GitLabMergeGate_ProbeTimeout_Unknown(t *testing.T) {
	orig := mergeGateProbeTimeout
	mergeGateProbeTimeout = timescale.D(100 * time.Millisecond)
	t.Cleanup(func() { mergeGateProbeTimeout = orig })

	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	gl.listHang = timescale.D(3 * time.Second)
	raw := gitLabMergeGateRaw(t, gl)
	assertGitLabMergeGateUnknown(t, raw, gitLabMergeGateReasonTransport)
}

// --- gitlab_registration rung (E45.68 / #3582) ---

// gitLabRegistrationRaw runs the nested-path gitlab request against the v4
// stub with the given registry posture and returns the raw body plus the
// registry fake, so each registration test asserts key PRESENCE/ABSENCE.
func gitLabRegistrationRaw(t *testing.T, gl *fakeGitLabForOnboarding, registry GitLabInstallationResolver) map[string]any {
	t.Helper()
	s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, nil)
	s.cfg.GitLabInstallations = registry
	id := testOperatorIdentity()
	return rawReadiness(t, s, onboardingReqForge(onboardingNestedGitLabPath, "gitlab", &id))
}

// TestOnboardingReadiness_GitLab_Registration_NotRegistered is the done-means
// test for #3582: a project that RESOLVES with the deployment credential but
// has NO installations row → app.installed false (the pre-#3582 over-claim
// removed) with app.resolvable true and a reason pointing at
// gitlab_registration; the rung reports not_registered with a remediation
// carrying the REAL resolved project id (`--installation-ref gitlab:5`) and
// the exact `--project-path`; and the cascades key on resolvability — the
// spec is STILL fetched + valid, the merge gate STILL pipeline_gated. The
// registry was consulted once with the trimmed nested path.
//
// Counterfactuals (run, not reasoned): (i) delete the
// `reg.Status == registered &&` conjunction in the handler → RED on
// app.installed; (iv) re-key the spec cascade in probeGitLab on Installed →
// RED on spec.source.
func TestOnboardingReadiness_GitLab_Registration_NotRegistered(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	registry := &fakeGitLabInstallations{found: false}
	raw := gitLabRegistrationRaw(t, gl, registry)

	app := rawObject(t, raw, "app")
	if app["installed"] != false {
		t.Errorf("app.installed = %v, want false (resolvable but not registered)", app["installed"])
	}
	if app["resolvable"] != true {
		t.Errorf("app.resolvable = %v, want true", app["resolvable"])
	}
	if app["reason"] != onboardingGitLabNotRegisteredReason || !strings.Contains(onboardingGitLabNotRegisteredReason, "gitlab_registration") {
		t.Errorf("app.reason = %v, want the not-registered reason naming gitlab_registration", app["reason"])
	}
	if app["note"] != onboardingGitLabInstalledNote {
		t.Errorf("app.note = %v, want the gitlab note", app["note"])
	}
	reg := rawObject(t, raw, "gitlab_registration")
	if reg["status"] != gitLabRegistrationStatusNotRegistered {
		t.Errorf("gitlab_registration.status = %v, want not_registered", reg["status"])
	}
	if _, present := reg["reason"]; present {
		t.Errorf("gitlab_registration.reason present (%v) on a positive not_registered finding", reg["reason"])
	}
	if detail, _ := reg["detail"].(string); !strings.Contains(detail, onboardingNestedGitLabPath) || !strings.Contains(detail, "exact match") {
		t.Errorf("gitlab_registration.detail = %q, want the path and the exact-match sentence", detail)
	}
	rem, _ := reg["remediation"].(string)
	for _, want := range []string{
		"fishhawkd installation register --provider gitlab",
		"--account-key gitlab-com",
		"--installation-ref gitlab:" + fakeGitLabProjectID,
		"--project-path " + onboardingNestedGitLabPath,
	} {
		if !strings.Contains(rem, want) {
			t.Errorf("gitlab_registration.remediation = %q, want it to carry %q", rem, want)
		}
	}
	if strings.Contains(rem, "<project_id>") {
		t.Errorf("remediation carries the placeholder although the project resolved: %q", rem)
	}
	if reg["resolved_ref"] != "gitlab:"+fakeGitLabProjectID {
		t.Errorf("gitlab_registration.resolved_ref = %v, want gitlab:%s", reg["resolved_ref"], fakeGitLabProjectID)
	}
	for _, key := range []string{"installation_ref", "project_path", "ref_matches"} {
		if v, present := reg[key]; present {
			t.Errorf("gitlab_registration.%s present (%v) with no registered row", key, v)
		}
	}
	// The cascades key on resolvability, not on the stricter installed.
	sp := rawObject(t, raw, "spec")
	if sp["source"] != "fetched" || sp["valid"] != true {
		t.Errorf("spec = %v, want fetched + valid (the cascade keys on resolvable)", sp)
	}
	if rv, _ := raw["reviewers"].([]any); len(rv) == 0 {
		t.Errorf("reviewers = %v, want the declared tuples", raw["reviewers"])
	}
	if glmg := rawObject(t, raw, "gitlab_merge_gate"); glmg["status"] != gitLabMergeGateStatusPipelineGated {
		t.Errorf("gitlab_merge_gate.status = %v, want pipeline_gated (the cascade keys on resolvable)", glmg["status"])
	}
	if registry.projectCalls != 1 || registry.lastPath != onboardingNestedGitLabPath {
		t.Errorf("registry calls=%d path=%q, want 1 with the trimmed nested path", registry.projectCalls, registry.lastPath)
	}
}

// TestOnboardingReadiness_GitLab_Registration_RegistryUnwired: no registry on
// this deployment (cfg.GitLabInstallations nil) → unknown / registry_unwired,
// installed false with the registry-unknown app reason naming the code,
// resolvable true, spec still fetched. Counterfactual (ii): delete the nil
// guard in probeGitLabRegistration → RED (nil-dereference panic).
func TestOnboardingReadiness_GitLab_Registration_RegistryUnwired(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	raw := gitLabRegistrationRaw(t, gl, nil)

	reg := rawObject(t, raw, "gitlab_registration")
	if reg["status"] != string(mergegate.StatusUnknown) || reg["reason"] != gitLabRegistrationReasonRegistryUnwired {
		t.Errorf("gitlab_registration = %v, want unknown / registry_unwired", reg)
	}
	if detail, _ := reg["detail"].(string); !strings.Contains(detail, "no installation registry") {
		t.Errorf("detail = %q, want the no-registry sentence", detail)
	}
	if rem, _ := reg["remediation"].(string); !strings.Contains(rem, "FISHHAWKD_DATABASE_URL") {
		t.Errorf("remediation = %q, want FISHHAWKD_DATABASE_URL named", rem)
	}
	app := rawObject(t, raw, "app")
	if app["installed"] != false || app["resolvable"] != true {
		t.Errorf("app = %v, want installed:false resolvable:true", app)
	}
	wantReason := onboardingGitLabRegistryUnknownReasonPrefix + gitLabRegistrationReasonRegistryUnwired + onboardingGitLabRegistryUnknownReasonSuffix
	if app["reason"] != wantReason {
		t.Errorf("app.reason = %v, want %q", app["reason"], wantReason)
	}
	if sp := rawObject(t, raw, "spec"); sp["source"] != "fetched" {
		t.Errorf("spec = %v, want fetched", sp)
	}
}

// TestOnboardingReadiness_GitLab_Registration_LookupFailed: the registry
// faults → unknown / registry_lookup_failed with the error in detail, a WARN
// log, installed false. Counterfactual (3): delete the err branch → RED
// (would report not_registered).
func TestOnboardingReadiness_GitLab_Registration_LookupFailed(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	lookupErr := errors.New("installations store down")
	s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, nil)
	s.cfg.GitLabInstallations = &fakeGitLabInstallations{err: lookupErr}
	logBuf := &bytes.Buffer{}
	s.cfg.Logger = slog.New(slog.NewJSONHandler(logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	id := testOperatorIdentity()
	raw := rawReadiness(t, s, onboardingReqForge(onboardingNestedGitLabPath, "gitlab", &id))

	reg := rawObject(t, raw, "gitlab_registration")
	if reg["status"] != string(mergegate.StatusUnknown) || reg["reason"] != gitLabRegistrationReasonLookupFailed {
		t.Errorf("gitlab_registration = %v, want unknown / registry_lookup_failed", reg)
	}
	if detail, _ := reg["detail"].(string); !strings.Contains(detail, lookupErr.Error()) {
		t.Errorf("detail = %q, want the lookup error", detail)
	}
	app := rawObject(t, raw, "app")
	wantReason := onboardingGitLabRegistryUnknownReasonPrefix + gitLabRegistrationReasonLookupFailed + onboardingGitLabRegistryUnknownReasonSuffix
	if app["installed"] != false || app["reason"] != wantReason {
		t.Errorf("app = %v, want installed:false reason=%q", app, wantReason)
	}
	rec := soleLogRecord(t, logBuf, "onboarding readiness: gitlab registration lookup failed")
	if rec["level"] != "WARN" || rec["repo"] != onboardingNestedGitLabPath || rec["error"] != lookupErr.Error() {
		t.Errorf("log record = %v, want WARN with repo + error", rec)
	}
}

// TestOnboardingReadiness_GitLab_Registration_RefMismatch: a row registered
// under the exact path whose installation_ref names a DIFFERENT project
// (gitlab:99) than the path resolves to (gitlab:5) → status registered,
// ref_matches false, detail naming BOTH refs, a re-register remediation
// carrying the resolved ref — and app.installed TRUE, mirroring run-create,
// which accepts the row without resolving the path. Counterfactual (iii):
// delete the ref comparison → RED (ref_matches absent).
func TestOnboardingReadiness_GitLab_Registration_RefMismatch(t *testing.T) {
	gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
	registry := &fakeGitLabInstallations{found: true, inst: account.GitLabInstallation{
		InstallationRef: "gitlab:99", ProjectPath: onboardingNestedGitLabPath}}
	raw := gitLabRegistrationRaw(t, gl, registry)

	reg := rawObject(t, raw, "gitlab_registration")
	if reg["status"] != gitLabRegistrationStatusRegistered {
		t.Errorf("gitlab_registration.status = %v, want registered", reg["status"])
	}
	if reg["ref_matches"] != false {
		t.Errorf("gitlab_registration.ref_matches = %v, want false", reg["ref_matches"])
	}
	if reg["installation_ref"] != "gitlab:99" || reg["resolved_ref"] != "gitlab:"+fakeGitLabProjectID {
		t.Errorf("refs = installation:%v resolved:%v, want gitlab:99 / gitlab:%s", reg["installation_ref"], reg["resolved_ref"], fakeGitLabProjectID)
	}
	detail, _ := reg["detail"].(string)
	if !strings.Contains(detail, "gitlab:99") || !strings.Contains(detail, "gitlab:"+fakeGitLabProjectID) {
		t.Errorf("detail = %q, want both refs named", detail)
	}
	if rem, _ := reg["remediation"].(string); !strings.Contains(rem, "--installation-ref gitlab:"+fakeGitLabProjectID) {
		t.Errorf("remediation = %q, want the re-register command with the resolved ref", rem)
	}
	if app := rawObject(t, raw, "app"); app["installed"] != true {
		t.Errorf("app.installed = %v, want true (run-create accepts a mismatched row)", app["installed"])
	}
}

// TestOnboardingReadiness_GitLab_Registration_NoteAndUnresolvedRefAbsent pins
// the raw keys: note equals the constant on every posture, and resolved_ref
// is ABSENT whenever the project did not resolve — on a not-visible project
// AND on a not-registered one.
func TestOnboardingReadiness_GitLab_Registration_NoteAndUnresolvedRefAbsent(t *testing.T) {
	for _, tc := range []struct {
		name     string
		registry *fakeGitLabInstallations
		want     string
	}{
		{"registered", registeredGitLabInstallations(), gitLabRegistrationStatusRegistered},
		{"not_registered", &fakeGitLabInstallations{found: false}, gitLabRegistrationStatusNotRegistered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
			gl.projectStatus = http.StatusNotFound
			raw := gitLabRegistrationRaw(t, gl, tc.registry)
			reg := rawObject(t, raw, "gitlab_registration")
			if reg["status"] != tc.want {
				t.Errorf("status = %v, want %s", reg["status"], tc.want)
			}
			if reg["note"] != gitLabRegistrationNote {
				t.Errorf("note = %v, want the constant", reg["note"])
			}
			for _, key := range []string{"resolved_ref", "ref_matches"} {
				if v, present := reg[key]; present {
					t.Errorf("%s present (%v) although the project did not resolve", key, v)
				}
			}
			if tc.want == gitLabRegistrationStatusNotRegistered {
				if rem, _ := reg["remediation"].(string); !strings.Contains(rem, "--installation-ref gitlab:<project_id>") {
					t.Errorf("remediation = %q, want the placeholder ref when the project did not resolve", rem)
				}
			}
		})
	}
}

// TestOnboardingReadiness_GitHub_NoGitLabRegistrationKey: the github family
// carries NEITHER `gitlab_registration` NOR `app.resolvable`, and a wired
// registry fake records ZERO calls — the never-consulted seam. Counterfactual
// (v): consult the registry on both families → RED on projectCalls.
func TestOnboardingReadiness_GitHub_NoGitLabRegistrationKey(t *testing.T) {
	gh := newFakeGitHubForRuns(onboardingReviewersSpecYAML)
	s := newOnboardingGitLabServer(t, gh.server(t), nil, nil, nil)
	registry := s.cfg.GitLabInstallations.(*fakeGitLabInstallations)
	id := testOperatorIdentity()

	raw := rawReadiness(t, s, onboardingReqForge("kuhlman-labs/fishhawk", "github", &id))
	if raw["forge"] != "github" {
		t.Fatalf("forge = %v, want github", raw["forge"])
	}
	if v, present := raw["gitlab_registration"]; present {
		t.Errorf("gitlab_registration present on the github family: %v", v)
	}
	app := rawObject(t, raw, "app")
	if v, present := app["resolvable"]; present {
		t.Errorf("app.resolvable present on the github family: %v", v)
	}
	if app["installed"] != true {
		t.Errorf("app.installed = %v, want true (App installed on the fake)", app["installed"])
	}
	if registry.projectCalls != 0 || registry.refCalls != 0 {
		t.Errorf("registry consulted on the github family: project=%d ref=%d, want 0/0", registry.projectCalls, registry.refCalls)
	}
}

// TestOnboardingReadiness_GitLab_RegistrationAgreesWithRunCreate is the
// CROSS-BOUNDARY control (#3582): the readiness handler and POST /v0/runs on
// ONE Server sharing ONE fakeGitLabInstallations, over every registry
// posture. `gitlab_registration.status == registered` ⇔ the run is admitted
// (201), and each non-registered posture's create refusal matches the rung's
// classification: not_registered ⇔ 422 gitlab_project_not_registered,
// registry_unwired ⇔ 503 gitlab_unconfigured, registry_lookup_failed ⇔ 500
// internal_error. Both handlers must see the SAME path on the same seam.
// Counterfactual (condition 4): point the rung at a different seam or
// normalise the path before the lookup → RED.
func TestOnboardingReadiness_GitLab_RegistrationAgreesWithRunCreate(t *testing.T) {
	const repoPath = "acme/platform/api" // gitlabCreateBody's repo
	cases := []struct {
		name       string
		registry   *fakeGitLabInstallations // nil = unwired
		wantStatus string
		wantReason string
		wantCode   int
		wantErr    string
	}{
		{"registered", &fakeGitLabInstallations{found: true, inst: account.GitLabInstallation{InstallationRef: "gitlab:" + fakeGitLabProjectID, ProjectPath: repoPath}},
			gitLabRegistrationStatusRegistered, "", http.StatusCreated, ""},
		{"not_found", &fakeGitLabInstallations{found: false},
			gitLabRegistrationStatusNotRegistered, "", http.StatusUnprocessableEntity, "gitlab_project_not_registered"},
		{"lookup_error", &fakeGitLabInstallations{err: errors.New("registry down")},
			string(mergegate.StatusUnknown), gitLabRegistrationReasonLookupFailed, http.StatusInternalServerError, "internal_error"},
		{"unwired", nil,
			string(mergegate.StatusUnknown), gitLabRegistrationReasonRegistryUnwired, http.StatusServiceUnavailable, "gitlab_unconfigured"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gl := newFakeGitLabForOnboarding(onboardingReviewersSpecYAML)
			s := newOnboardingGitLabServer(t, nil, gitlabForgeOver(gl.server(t)), nil, nil)
			s.cfg.RunRepo = newFakeRepo()
			if tc.registry == nil {
				s.cfg.GitLabInstallations = nil
			} else {
				s.cfg.GitLabInstallations = tc.registry
			}
			id := testOperatorIdentity()

			raw := rawReadiness(t, s, onboardingReqForge(repoPath, "gitlab", &id))
			reg := rawObject(t, raw, "gitlab_registration")
			if reg["status"] != tc.wantStatus {
				t.Errorf("gitlab_registration.status = %v, want %s", reg["status"], tc.wantStatus)
			}
			if tc.wantReason != "" && reg["reason"] != tc.wantReason {
				t.Errorf("gitlab_registration.reason = %v, want %s", reg["reason"], tc.wantReason)
			}
			app := rawObject(t, raw, "app")
			// Captured BEFORE the create call overwrites it: the path the
			// readiness rung handed the seam.
			var readinessPath string
			if tc.registry != nil {
				readinessPath = tc.registry.lastPath
			}

			w := postForgeCreateRun(t, s, gitlabCreateBody())
			if w.Code != tc.wantCode {
				t.Fatalf("POST /v0/runs status = %d, want %d:\n%s", w.Code, tc.wantCode, w.Body.String())
			}
			admitted := w.Code == http.StatusCreated
			registered := reg["status"] == gitLabRegistrationStatusRegistered
			if admitted != registered {
				t.Errorf("registered=%v but run admitted=%v; the rung must pre-flight exactly the run-create registry check", registered, admitted)
			}
			if app["installed"] != admitted {
				t.Errorf("app.installed = %v, want %v (agrees with run-create on a resolvable project)", app["installed"], admitted)
			}
			if !admitted {
				if code := decodeErrorCode(t, w); code != tc.wantErr {
					t.Errorf("create error code = %q, want %q", code, tc.wantErr)
				}
			}
			if tc.registry != nil {
				// Both handlers hit the SAME seam with the SAME byte-exact
				// path: one readiness lookup + one create lookup, neither
				// normalised — a rung that canonicalised the path before the
				// lookup would answer for a row run-create never consults.
				if tc.registry.projectCalls != 2 {
					t.Errorf("registry calls=%d, want 2 (readiness + create)", tc.registry.projectCalls)
				}
				if readinessPath != repoPath || tc.registry.lastPath != repoPath {
					t.Errorf("seam paths: readiness=%q create=%q, want both exactly %q", readinessPath, tc.registry.lastPath, repoPath)
				}
			}
		})
	}
}
