package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// minimalSpecYAML is the smallest valid workflow spec for the
// workflow_spec tests below: one workflow with one implement stage,
// no gates. Mirrors backend/internal/spec/testdata/valid/minimal.yaml.
const minimalSpecYAML = `version: "0.3"
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// gatedSpecYAML carries a plan stage with an approval gate so tests
// can assert that gate metadata (sla, requires_approval) lands on
// the corresponding Stage row.
const gatedSpecYAML = `version: "0.3"
roles:
  tech_lead:
    members: ["@org/tech-leads"]
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
              any_of: [tech_lead]
            sla: 4_business_hours
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`

// TestCreateRun_WorkflowSpec_CreatesStages is the headline #411
// behavior: posting a workflow_spec inline lands one Stage row per
// stage in the spec, in spec order, with the right type +
// executor.
func TestCreateRun_WorkflowSpec_CreatesStages(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"workflow_spec":  gatedSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	stages := repo.stagesFor(got.ID)
	if len(stages) != 2 {
		t.Fatalf("len(stages) = %d, want 2: %#v", len(stages), stages)
	}
	if stages[0].Type != run.StageTypePlan {
		t.Errorf("stage[0].Type = %q, want plan", stages[0].Type)
	}
	if stages[1].Type != run.StageTypeImplement {
		t.Errorf("stage[1].Type = %q, want implement", stages[1].Type)
	}
	// Plan stage carries an approval gate → RequiresApproval true,
	// GateSLA populated from the spec verbatim.
	if !stages[0].RequiresApproval {
		t.Error("stage[0].RequiresApproval = false, want true (plan has approval gate)")
	}
	if stages[0].GateSLA == nil || *stages[0].GateSLA != "4_business_hours" {
		t.Errorf("stage[0].GateSLA = %v, want 4_business_hours", stages[0].GateSLA)
	}
	// Implement has no gate → RequiresApproval false, GateSLA nil.
	if stages[1].RequiresApproval {
		t.Error("stage[1].RequiresApproval = true, want false")
	}
	if stages[1].GateSLA != nil {
		t.Errorf("stage[1].GateSLA = %v, want nil", stages[1].GateSLA)
	}
}

// TestCreateRun_WorkflowSpec_PersistsBytesAndMaxRetries asserts the
// spec bytes are cached on the run row (so the trace handler's
// policy re-evaluation can read constraints without refetching)
// and that MaxRetriesSnapshot is populated from the parsed spec.
// Matches the dispatcher's cache behavior (#280, #283).
func TestCreateRun_WorkflowSpec_PersistsBytesAndMaxRetries(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  minimalSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	if string(repo.lastCreateRunParams.WorkflowSpec) != minimalSpecYAML {
		t.Errorf("WorkflowSpec bytes not cached on row; got len=%d, want len=%d",
			len(repo.lastCreateRunParams.WorkflowSpec), len(minimalSpecYAML))
	}
	// The minimal spec has no on_ci_failure → default applies. The
	// spec package's DefaultMaxRetries is exposed via
	// webhook.WorkflowMaxRetries which the handler calls; assert
	// it's a non-zero default rather than coupling the test to the
	// exact constant.
	if repo.lastCreateRunParams.MaxRetriesSnapshot == 0 {
		t.Error("MaxRetriesSnapshot = 0, want default-from-spec")
	}
}

// TestCreateRun_WorkflowSpec_MalformedYAML rejects the request
// with 400 before any DB write — the run row should NOT exist after
// a parse failure.
func TestCreateRun_WorkflowSpec_MalformedYAML(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  "this: is: not: valid: yaml: ::",
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "workflow_spec") {
		t.Errorf("body should mention workflow_spec: %s", w.Body.String())
	}
	if len(repo.runs) != 0 {
		t.Errorf("expected zero runs created, got %d", len(repo.runs))
	}
}

// TestCreateRun_WorkflowSpec_UnknownWorkflowID rejects when the
// requested workflow_id isn't defined in the supplied spec — same
// 400 the dispatcher would emit on the GHA path.
func TestCreateRun_WorkflowSpec_UnknownWorkflowID(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "does_not_exist",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  minimalSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "workflow_id") {
		t.Errorf("body should reference workflow_id: %s", w.Body.String())
	}
	if len(repo.runs) != 0 {
		t.Errorf("expected zero runs created, got %d", len(repo.runs))
	}
}

// TestCreateRun_NoWorkflowSpec_LegacyPath documents the legacy
// shape: when workflow_spec is absent, the handler creates a run
// row with no stages (the pre-#411 behavior). Kept so integration
// tests and existing scripts that POST without a spec keep
// working.
func TestCreateRun_NoWorkflowSpec_LegacyPath(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)

	body := `{
		"repo": "x/y",
		"workflow_id": "trivial",
		"workflow_sha": "abc",
		"trigger_source": "cli"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(repo.stagesFor(got.ID)) != 0 {
		t.Errorf("legacy path should not create stages; got %d", len(repo.stagesFor(got.ID)))
	}
	if len(repo.lastCreateRunParams.WorkflowSpec) != 0 {
		t.Error("legacy path should NOT cache workflow spec bytes on the row")
	}
}

// TestCreateRun_WorkflowSpec_StageCreateFails_Returns500 covers
// the unhappy persistence path: parse + spec validation pass, the
// run row inserts, then CreateStage errors. Server returns 500 and
// the run row is left behind (orphan) — the dispatcher's behavior
// on the same failure shape. v0.x can wrap this in a transaction
// once we have a use case that demands strict atomicity.
func TestCreateRun_WorkflowSpec_StageCreateFails_Returns500(t *testing.T) {
	repo := newFakeRepo()
	repo.createStageErr = errors.New("disk full")
	s := newServer(t, repo)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  minimalSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "create stages failed") {
		t.Errorf("body missing diagnostic: %s", w.Body.String())
	}
}

// gatingReviewSpecYAML carries a plan stage requesting agent-gated
// review (reviewers.agent>0, human==0) — the configuration that must
// be rejected at create time when no PlanReviewer is wired (#574).
const gatingReviewSpecYAML = `version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        reviewers:
          agent: 1
          human: 0
        produces:
          - artifact: plan
            schema: standard_v1
`

// TestCreateRun_GatingReview_NilReviewer_Rejected verifies the #574
// guard: a workflow declaring agent-gated plan review while the server
// has no PlanReviewer wired is rejected with 400 +
// plan_reviewer_unconfigured, a run_rejected_misconfigured global-chain
// audit entry is written, and no run row is created.
func TestCreateRun_GatingReview_NilReviewer_Rejected(t *testing.T) {
	repo := newFakeRepo()
	au := newAuditFake()
	// PlanReviewer intentionally nil.
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au})

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  gatingReviewSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"plan_reviewer_unconfigured"`) {
		t.Errorf("body missing plan_reviewer_unconfigured code: %s", w.Body.String())
	}
	if len(repo.runs) != 0 {
		t.Errorf("expected zero runs created, got %d", len(repo.runs))
	}
	// A run_rejected_misconfigured global-chain entry must be written.
	var found bool
	for _, e := range au.globalAppended {
		if e.Category == "run_rejected_misconfigured" {
			found = true
		}
	}
	if !found {
		t.Errorf("no run_rejected_misconfigured global-chain audit entry written: %#v", au.globalAppended)
	}
}

// TestCreateRun_AdvisoryReview_NilReviewer_Allowed verifies the #574
// guard does NOT block advisory-mode review (agent>0, human>0): the
// human gate remains authoritative, so the run is created normally
// even with no PlanReviewer wired.
func TestCreateRun_AdvisoryReview_NilReviewer_Allowed(t *testing.T) {
	repo := newFakeRepo()
	au := newAuditFake()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au})

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec": strings.Replace(gatingReviewSpecYAML,
			"agent: 1\n          human: 0", "agent: 1\n          human: 1", 1),
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (advisory mode allowed):\n%s", w.Code, w.Body.String())
	}
	for _, e := range au.globalAppended {
		if e.Category == "run_rejected_misconfigured" {
			t.Errorf("advisory mode must not emit run_rejected_misconfigured")
		}
	}
}

// heterogeneousGatingSpecYAML carries a plan stage requesting agent-gated
// review via the #955 heterogeneous agents list (anthropic + codex,
// human==0) — every declared provider must resolve at create time.
const heterogeneousGatingSpecYAML = `version: "0.3"
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
          human: 0
        produces:
          - artifact: plan
            schema: standard_v1
`

// TestCreateRun_GatingReview_AgentsList_UnresolvableProvider_Rejected
// verifies the #955 extension of the #574 fail-fast: a gating agents list
// naming a provider the wired ReviewerSet cannot resolve is rejected at
// create time with 400 + plan_reviewer_unconfigured naming the provider,
// and a run_rejected_misconfigured global-chain entry is written.
func TestCreateRun_GatingReview_AgentsList_UnresolvableProvider_Rejected(t *testing.T) {
	repo := newFakeRepo()
	au := newAuditFake()
	// Only anthropic is configured; the spec also names codex.
	def := &fakePlanReviewer{model: "claude-opus-4-8"}
	set := fakeReviewerSet{def: def, providers: map[string]PlanReviewer{"anthropic": def}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, PlanReviewers: set})

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  heterogeneousGatingSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"plan_reviewer_unconfigured"`) {
		t.Errorf("body missing plan_reviewer_unconfigured code: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `codex`) {
		t.Errorf("error must name the unresolvable provider: %s", w.Body.String())
	}
	if len(repo.runs) != 0 {
		t.Errorf("expected zero runs created, got %d", len(repo.runs))
	}
	var found bool
	for _, e := range au.globalAppended {
		if e.Category == "run_rejected_misconfigured" {
			found = true
		}
	}
	if !found {
		t.Errorf("no run_rejected_misconfigured global-chain audit entry written: %#v", au.globalAppended)
	}
}

// TestCreateRun_GatingReview_AgentsList_AllResolvable_Allowed verifies the
// happy path of the #955 pre-check: when every declared provider resolves,
// the gating agents list passes the create-time guard.
func TestCreateRun_GatingReview_AgentsList_AllResolvable_Allowed(t *testing.T) {
	repo := newFakeRepo()
	au := newAuditFake()
	def := &fakePlanReviewer{model: "claude-opus-4-8"}
	set := fakeReviewerSet{def: def, providers: map[string]PlanReviewer{
		"anthropic": def,
		"codex":     &fakePlanReviewer{model: "gpt-5.5"},
	}}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, PlanReviewers: set})

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  heterogeneousGatingSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (all providers resolvable):\n%s", w.Code, w.Body.String())
	}
	for _, e := range au.globalAppended {
		if e.Category == "run_rejected_misconfigured" {
			t.Errorf("resolvable agents list must not emit run_rejected_misconfigured")
		}
	}
}

// --- GitHub fetch fallback for workflow_spec (#413) ---

// ghTokensStub satisfies githubapp.TokenProvider for runs_test GitHub
// client construction without importing the githubapp package.
type ghTokensStub struct{ tok string }

func (s *ghTokensStub) Token(_ context.Context, _ int64) (string, error) { return s.tok, nil }

// fakeGitHubForRuns is a minimal stub server used by the GitHub-fetch
// fallback tests. It handles:
//
//	GET /repos/{owner}/{repo}/installation → returns a canned installation ID
//	GET /repos/{owner}/{repo}/contents/   → returns base64-encoded spec YAML
type fakeGitHubForRuns struct {
	installationStatus int
	installationBody   string
	specStatus         int
	specBody           string

	// call counters let tests assert which endpoints were hit.
	installationCalls int
	specCalls         int
}

func newFakeGitHubForRuns(specYAML string) *fakeGitHubForRuns {
	encoded := base64.StdEncoding.EncodeToString([]byte(specYAML))
	specJSON := `{"path":".fishhawk/workflows.yaml","sha":"spec_sha","content":"` +
		encoded + `","encoding":"base64","type":"file"}`
	return &fakeGitHubForRuns{
		installationStatus: http.StatusOK,
		installationBody:   `{"id":12345,"app_id":1}`,
		specStatus:         http.StatusOK,
		specBody:           specJSON,
	}
}

func (f *fakeGitHubForRuns) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/installation", func(w http.ResponseWriter, r *http.Request) {
		f.installationCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.installationStatus)
		_, _ = io.WriteString(w, f.installationBody)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/contents/", func(w http.ResponseWriter, r *http.Request) {
		f.specCalls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(f.specStatus)
		if f.specBody != "" {
			_, _ = io.WriteString(w, f.specBody)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// newServerWithGitHub builds a Server with a githubclient.Client pointing
// at ghSrv. Used to exercise the GitHub-fetch fallback path (#413).
func newServerWithGitHub(t *testing.T, repo run.Repository, ghSrv *httptest.Server) *Server {
	t.Helper()
	gh := &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt_test", nil },
	}
	return New(Config{Addr: "127.0.0.1:0", RunRepo: repo, GitHub: gh})
}

// TestCreateRun_GitHubFetch_HappyPath verifies that omitting
// workflow_spec triggers a GitHub fetch and produces Stage rows
// identical to the inline-spec path.
func TestCreateRun_GitHubFetch_HappyPath(t *testing.T) {
	repo := newFakeRepo()
	fake := newFakeGitHubForRuns(gatedSpecYAML)
	ghSrv := fake.server(t)
	s := newServerWithGitHub(t, repo, ghSrv)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "deadbeef",
		"trigger_source": "cli",
		"runner_kind":    "local",
		// workflow_spec intentionally omitted — triggers GitHub fetch.
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}

	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	// Both GitHub endpoints must have been called exactly once.
	if fake.installationCalls != 1 {
		t.Errorf("installation endpoint called %d times, want 1", fake.installationCalls)
	}
	if fake.specCalls != 1 {
		t.Errorf("spec endpoint called %d times, want 1", fake.specCalls)
	}

	// Stage rows must match the gatedSpecYAML shape (plan + implement).
	stages := repo.stagesFor(got.ID)
	if len(stages) != 2 {
		t.Fatalf("len(stages) = %d, want 2: %#v", len(stages), stages)
	}
	if stages[0].Type != run.StageTypePlan {
		t.Errorf("stage[0].Type = %q, want plan", stages[0].Type)
	}
	if stages[1].Type != run.StageTypeImplement {
		t.Errorf("stage[1].Type = %q, want implement", stages[1].Type)
	}
	if !stages[0].RequiresApproval {
		t.Error("stage[0].RequiresApproval = false, want true")
	}
	if stages[0].GateSLA == nil || *stages[0].GateSLA != "4_business_hours" {
		t.Errorf("stage[0].GateSLA = %v, want 4_business_hours", stages[0].GateSLA)
	}

	// The fetched spec bytes must be cached on the run row.
	if len(repo.lastCreateRunParams.WorkflowSpec) == 0 {
		t.Error("WorkflowSpec not cached on run row after GitHub fetch")
	}
}

// TestCreateRun_GitHubFetch_NotInstalled verifies that a 404 from the
// installation endpoint produces HTTP 422 with code "repo_not_installed".
func TestCreateRun_GitHubFetch_NotInstalled(t *testing.T) {
	fake := newFakeGitHubForRuns(gatedSpecYAML)
	fake.installationStatus = http.StatusNotFound
	fake.installationBody = `{"message":"Not Found"}`
	ghSrv := fake.server(t)
	s := newServerWithGitHub(t, newFakeRepo(), ghSrv)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "deadbeef",
		"trigger_source": "cli",
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"repo_not_installed"`) {
		t.Errorf("body missing repo_not_installed code: %s", w.Body.String())
	}
	// Spec endpoint must NOT have been called.
	if fake.specCalls != 0 {
		t.Errorf("spec endpoint should not be called after installation failure; calls = %d", fake.specCalls)
	}
}

// TestCreateRun_GitHubFetch_SpecNotFound verifies that a 404 from the
// spec-contents endpoint produces HTTP 422 with code "spec_not_found".
func TestCreateRun_GitHubFetch_SpecNotFound(t *testing.T) {
	fake := newFakeGitHubForRuns(gatedSpecYAML)
	fake.specStatus = http.StatusNotFound
	fake.specBody = `{"message":"Not Found"}`
	ghSrv := fake.server(t)
	s := newServerWithGitHub(t, newFakeRepo(), ghSrv)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "deadbeef",
		"trigger_source": "cli",
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"spec_not_found"`) {
		t.Errorf("body missing spec_not_found code: %s", w.Body.String())
	}
}

// TestCreateRun_InlineSpec_BypassesFetch verifies that when workflow_spec
// is provided inline, the heavy GitHub spec-contents fetch is never
// called. The installation endpoint IS called now (#713) — the run-
// create path resolves the App installation best-effort even on the
// inline path so the run row carries it for push_and_open_pr + the merge
// reconciler — but the spec fetch must still be bypassed.
func TestCreateRun_InlineSpec_BypassesFetch(t *testing.T) {
	fake := newFakeGitHubForRuns(gatedSpecYAML)
	ghSrv := fake.server(t)
	repo := newFakeRepo()
	s := newServerWithGitHub(t, repo, ghSrv)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  gatedSpecYAML, // inline — must not trigger fetch
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	// The installation endpoint is hit once for best-effort stamping
	// (#713); the heavy spec-contents fetch must still be bypassed.
	if fake.installationCalls != 1 {
		t.Errorf("installation endpoint called %d times; want 1 (best-effort stamp on inline path)",
			fake.installationCalls)
	}
	if fake.specCalls != 0 {
		t.Errorf("spec endpoint called %d times; inline spec must bypass GitHub spec fetch", fake.specCalls)
	}
	// The resolved installation is stamped onto the run row even on the
	// inline-spec path — this is the seam #713 fixes.
	if repo.lastCreateRunParams.InstallationID == nil || *repo.lastCreateRunParams.InstallationID != 12345 {
		t.Errorf("InstallationID = %v, want stamped 12345 on inline path", repo.lastCreateRunParams.InstallationID)
	}
	// Stages are still created from the inline spec.
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(repo.stagesFor(got.ID)) != 2 {
		t.Errorf("expected 2 stages from inline spec, got %d", len(repo.stagesFor(got.ID)))
	}
}

// TestCreateRun_InlineSpec_NotInstalled_Lenient verifies that on the
// inline-spec path a 404 from the installation endpoint does NOT fail
// the create (unlike the GitHub-fetch path's 422). The run is created
// with a nil InstallationID; the runner's `gh` CLI fallback covers the
// push + PR (#713).
func TestCreateRun_InlineSpec_NotInstalled_Lenient(t *testing.T) {
	fake := newFakeGitHubForRuns(gatedSpecYAML)
	fake.installationStatus = http.StatusNotFound
	fake.installationBody = `{"message":"Not Found"}`
	ghSrv := fake.server(t)
	repo := newFakeRepo()
	s := newServerWithGitHub(t, repo, ghSrv)

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"workflow_spec":  gatedSpecYAML, // inline — stamping is best-effort
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (inline path must be lenient on ErrNotInstalled):\n%s", w.Code, w.Body.String())
	}
	if repo.lastCreateRunParams.InstallationID != nil {
		t.Errorf("InstallationID = %v, want nil when App not installed", *repo.lastCreateRunParams.InstallationID)
	}
}

// TestCreateRun_InlineSpec_StampThreadsToTokenEndpoint is the
// cross-boundary seam #713 actually broke: an inline-spec create (the
// path fishhawk_start_run drives) must stamp the resolved installation
// onto the run row so the runner's installation-token call returns 201
// rather than 400 no_installation_for_run. This exercises create →
// persisted run row → token endpoint with a single RunRepo, proving the
// stamped id threads end-to-end (not just that the field is set).
func TestCreateRun_InlineSpec_StampThreadsToTokenEndpoint(t *testing.T) {
	repo := newFakeRepo()
	fake := newFakeGitHubForRuns(gatedSpecYAML) // resolves installation id 12345
	ghSrv := fake.server(t)
	gh := &githubclient.Client{
		BaseURL: ghSrv.URL,
		Tokens:  &ghTokensStub{tok: "ghs_test"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		AppJWT:  func() (string, error) { return "gha_app_jwt_test", nil },
	}
	sf := newSigningFake()
	au := newAuditFake()
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		GitHub:       gh,
		SigningRepo:  sf,
		AuditRepo:    au,
		GitHubTokens: &fakeTokenProvider{tok: "ghs_xyz"},
	})

	// Create the run via the inline-spec path.
	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"workflow_spec":  gatedSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var created runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create body: %v", err)
	}

	// Resolve the implement stage created from the spec.
	stages := repo.stagesFor(created.ID)
	if len(stages) != 2 {
		t.Fatalf("len(stages) = %d, want 2", len(stages))
	}
	stageID := stages[1].ID // implement stage

	// Sanity: the run row actually carries a non-nil stamped id — without
	// this the token endpoint below would 400 regardless of routing.
	gotRun, err := repo.GetRun(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.InstallationID == nil || *gotRun.InstallationID != 12345 {
		t.Fatalf("run row InstallationID = %v, want stamped 12345", gotRun.InstallationID)
	}

	// Issue a signing key for the run, then call the installation-token
	// endpoint exactly as the runner does. It must return 201, not the
	// 400 no_installation_for_run that an unstamped run produces.
	priv, _ := sf.issue(t, created.ID)
	w2 := issueTokenRequest(t, s, created.ID, stageID, priv, []byte(`{}`), "")
	if w2.Code != http.StatusCreated {
		t.Fatalf("installation-token status = %d, want 201 (stamped id must thread through):\n%s", w2.Code, w2.Body.String())
	}
	var tokResp installationTokenResponse
	if err := json.NewDecoder(w2.Body).Decode(&tokResp); err != nil {
		t.Fatalf("decode token body: %v", err)
	}
	if tokResp.Token != "ghs_xyz" {
		t.Errorf("token = %q, want ghs_xyz", tokResp.Token)
	}
}

// modelValiditySpecYAML is a one-workflow spec whose implement stage declares
// executor.model, so the submit-time #1339 validity seam has a model to check.
func modelValiditySpecYAML(model string) string {
	return `version: "0.3"
workflows:
  trivial:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          model: ` + model + `
        produces:
          - artifact: pull_request
`
}

// createRunWithSpec posts an inline workflow_spec for workflow "trivial" and
// returns the recorder.
func createRunWithSpec(t *testing.T, s *Server, specYAML string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "trivial",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"workflow_spec":  specYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	return w
}

// TestCreateRun_ModelValidity_RejectOnFreshAbsence is the submit-seam reject
// (#1339), end-to-end: handler -> spec.ValidateModels -> oracle. A spec model
// absent from a fresh+ok snapshot yields 422 model_invalid and NO run row.
func TestCreateRun_ModelValidity_RejectOnFreshAbsence(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	s.cfg.ModelOracle = modeloracle.Static{
		Models: map[string][]string{"claudecode": {"claude-opus-4-8"}},
		Fresh:  true,
	}

	w := createRunWithSpec(t, s, modelValiditySpecYAML("claude-typo-9"))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"model_invalid"`) {
		t.Errorf("body missing model_invalid: %s", w.Body.String())
	}
	if n := len(repo.runs); n != 0 {
		t.Errorf("run rows after a 422 = %d, want 0 (no half-formed run)", n)
	}
}

// TestCreateRun_ModelValidity_AcceptOnFreshPresent: a spec model present in a
// fresh+ok snapshot creates the run normally.
func TestCreateRun_ModelValidity_AcceptOnFreshPresent(t *testing.T) {
	repo := newFakeRepo()
	s := newServer(t, repo)
	s.cfg.ModelOracle = modeloracle.Static{
		Models: map[string][]string{"claudecode": {"claude-opus-4-8"}},
		Fresh:  true,
	}

	w := createRunWithSpec(t, s, modelValiditySpecYAML("claude-opus-4-8"))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
}

// --- deploy-first creation park (E23.13 / #1429) ---

// releaseSpecYAML is a standalone delegating release workflow (workflow-v1,
// ADR-038): a single deploy stage with a delegating executor and a
// pre-execution approval gate, drive on. Mirrors the `release` workflow in
// .fishhawk/workflows.yaml. The deploy stage's effect IS the side effect, so
// its only stage has no agent or runner — the shape that strands at pending
// without the creation-time Advance this slice adds.
const releaseSpecYAML = `version: "1.0"
roles:
  founder:
    members: ["@kuhlman-labs"]
workflows:
  release:
    drive: true
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: github_actions
            workflow_ref: deploy.yml
            git_ref: main
        produces:
          - artifact: deployment
        gates:
          - type: approval
            approvers:
              any_of: [founder]
`

// driveStageRepo extends fakeRepo with the two repository methods the real
// orchestrator.Advance walks (ListStagesForRun, TransitionStage) plus a
// CreateRun that honors the resolved Drive flag — so the create →
// orchestrator.Advance → stage-transition → drive-audit seam runs end to end.
// fakeRepo leaves these unimplemented (they error) because its handler tests
// never drive a real orchestrator; this in-file extension keeps the seam test
// self-contained.
type driveStageRepo struct {
	*fakeRepo
}

func newDriveStageRepo() *driveStageRepo { return &driveStageRepo{fakeRepo: newFakeRepo()} }

// CreateRun delegates to the embedded fake, then stamps the resolved Drive flag
// (fakeRepo.CreateRun drops it). The map stores the returned pointer, so a later
// GetRun sees the same Drive value.
func (f *driveStageRepo) CreateRun(ctx context.Context, p run.CreateRunParams) (*run.Run, error) {
	r, err := f.fakeRepo.CreateRun(ctx, p)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	r.Drive = p.Drive
	f.mu.Unlock()
	return r, nil
}

func (f *driveStageRepo) ListStagesForRun(_ context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	src := f.stagesByRun[runID]
	out := make([]*run.Stage, len(src))
	copy(out, src)
	sort.Slice(out, func(i, j int) bool { return out[i].Sequence < out[j].Sequence })
	return out, nil
}

func (f *driveStageRepo) TransitionStage(_ context.Context, id uuid.UUID, to run.StageState, _ *run.StageCompletion) (*run.Stage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, stages := range f.stagesByRun {
		for _, st := range stages {
			if st.ID == id {
				st.State = to
				st.UpdatedAt = time.Now().UTC()
				return st, nil
			}
		}
	}
	return nil, run.ErrNotFound
}

// hasDeployInitializationEntry reports whether the audit fake captured a
// run_auto_advanced entry naming the deploy_initialization drive rule.
func hasDeployInitializationEntry(t *testing.T, au *auditFake) bool {
	t.Helper()
	for _, e := range au.appended {
		if e.Category != drive.Category {
			continue
		}
		var adv drive.Advance
		if err := json.Unmarshal(e.Payload, &adv); err != nil {
			continue
		}
		if adv.Rule == drive.RuleDeployInitialization {
			return true
		}
	}
	return false
}

// TestCreateRun_DeployFirst_ParksAtApprovalGate is the cross-boundary
// integration test (#1429, operator binding condition 2): creating a standalone
// release run drives the real create → orchestrator.Advance →
// stage-transition → drive-audit seam. The deploy stage must reach
// awaiting_deploy_approval, the run must reach running, and a
// deploy_initialization run_auto_advanced entry must be recorded — proving the
// creation→Advance edge this slice adds (the per-layer Advance deploy-park unit
// already exists as orchestrator's TestAdvance_DeployStage_ParksPreExecution).
func TestCreateRun_DeployFirst_ParksAtApprovalGate(t *testing.T) {
	repo := newDriveStageRepo()
	au := newAuditFake()
	orch := &orchestrator.Orchestrator{Runs: repo}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, Orchestrator: orch})

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "release",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"workflow_spec":  releaseSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	// The deploy stage reached its pre-execution approval gate.
	stages := repo.stagesFor(got.ID)
	if len(stages) != 1 {
		t.Fatalf("len(stages) = %d, want 1 (single deploy stage): %#v", len(stages), stages)
	}
	if stages[0].Type != run.StageTypeDeploy {
		t.Fatalf("stage[0].Type = %q, want deploy", stages[0].Type)
	}
	if stages[0].State != run.StageStateAwaitingDeployApproval {
		t.Errorf("deploy stage state = %q, want awaiting_deploy_approval (creation-time Advance must park it)", stages[0].State)
	}

	// The run walked pending → running (Advance's top transition).
	gotRun, err := repo.GetRun(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.State != run.StateRunning {
		t.Errorf("run state = %q, want running", gotRun.State)
	}

	// The park is attributable to the deploy_initialization drive rule.
	if !hasDeployInitializationEntry(t, au) {
		t.Errorf("no deploy_initialization run_auto_advanced audit entry recorded: %#v", au.appended)
	}
}

// TestCreateRun_DeployFirst_DriveOff_ParksWithoutAudit pins the
// recordDriveDeployInitialization `!runRow.Drive` guard: the creation-time
// Advance is gated on the first stage being a deploy stage (NOT on drive), so a
// drive-OFF release run still parks the deploy stage at its gate — but no
// deploy_initialization run_auto_advanced entry is recorded (the drive audit is
// drive-only). The workflow spec defaults drive on; the per-run override turns
// it off.
func TestCreateRun_DeployFirst_DriveOff_ParksWithoutAudit(t *testing.T) {
	repo := newDriveStageRepo()
	au := newAuditFake()
	orch := &orchestrator.Orchestrator{Runs: repo}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, Orchestrator: orch})

	driveOff := false
	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "release",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"drive":          driveOff, // override the spec's drive: true
		"workflow_spec":  releaseSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// The deploy stage still parks (Advance is gated on stage type, not drive).
	stages := repo.stagesFor(got.ID)
	if len(stages) != 1 || stages[0].State != run.StageStateAwaitingDeployApproval {
		t.Errorf("deploy stage = %#v, want parked at awaiting_deploy_approval even with drive off", stages)
	}
	// But no drive audit entry — the run is not drive-enabled.
	if hasDeployInitializationEntry(t, au) {
		t.Error("deploy_initialization entry recorded for a drive-OFF run (the !runRow.Drive guard should suppress it)")
	}
}

// TestCreateRun_DeployFirst_AdvanceError_StillCreates pins binding condition 1
// (#1429): the creation-time Advance is BEST-EFFORT — an Advance error WARN-logs
// and does NOT unwind the created-run 201 (consistent with boardTransitionForRun).
// The transitionErr forces orchestrator.Advance's pending→running step to fail
// before the deploy stage is parked; the create must still return 201 with the
// run row present and the deploy stage left pending, and NO deploy_initialization
// entry recorded.
func TestCreateRun_DeployFirst_AdvanceError_StillCreates(t *testing.T) {
	repo := newDriveStageRepo()
	repo.transitionErr = errors.New("boom") // fails Advance's run pending→running step
	au := newAuditFake()
	orch := &orchestrator.Orchestrator{Runs: repo}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, Orchestrator: orch})

	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "release",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"workflow_spec":  releaseSpecYAML,
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (Advance error must not unwind the create):\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	// The run row exists despite the Advance failure.
	if _, err := repo.GetRun(context.Background(), got.ID); err != nil {
		t.Fatalf("run row missing after best-effort Advance failure: %v", err)
	}
	// The deploy stage was not parked (Advance failed before the stage
	// transition), and no drive entry was recorded.
	stages := repo.stagesFor(got.ID)
	if len(stages) != 1 || stages[0].State != run.StageStatePending {
		t.Errorf("deploy stage = %#v, want a single pending stage (Advance failed pre-park)", stages)
	}
	if hasDeployInitializationEntry(t, au) {
		t.Error("deploy_initialization entry recorded despite the Advance failure")
	}
}

// TestCreateRun_PlanFirst_NotAdvancedAtCreation is the load-bearing negative
// (#1429 binding condition 2): the creation-time Advance is gated STRICTLY on
// the first stage being a deploy stage, so an agent-first (plan-first) run is
// NOT advanced at creation — its plan stage stays pending and the run stays
// pending, preserving the operator-driven fishhawk_run_stage entry. A gate that
// was too broad would dispatch the plan stage here and fail this test.
func TestCreateRun_PlanFirst_NotAdvancedAtCreation(t *testing.T) {
	repo := newDriveStageRepo()
	au := newAuditFake()
	orch := &orchestrator.Orchestrator{Runs: repo}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: repo, AuditRepo: au, Orchestrator: orch})

	driveTrue := true
	body, _ := json.Marshal(map[string]any{
		"repo":           "x/y",
		"workflow_id":    "feature_change",
		"workflow_sha":   "abc",
		"trigger_source": "cli",
		"runner_kind":    "local",
		"drive":          driveTrue,
		"workflow_spec":  gatedSpecYAML, // plan-first (plan → implement)
	})
	req := httptest.NewRequest(http.MethodPost, "/v0/runs", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.handleCreateRun(w, withAuth(req))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	var got runResponse
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode body: %v", err)
	}

	// Plan-first run: Advance must NOT have run, so the plan stage stays
	// pending and the run stays pending.
	stages := repo.stagesFor(got.ID)
	if len(stages) != 2 || stages[0].Type != run.StageTypePlan {
		t.Fatalf("stages = %#v, want [plan, implement]", stages)
	}
	if stages[0].State != run.StageStatePending {
		t.Errorf("plan stage state = %q, want pending (a plan-first run must not be advanced at creation)", stages[0].State)
	}
	gotRun, err := repo.GetRun(context.Background(), got.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if gotRun.State != run.StatePending {
		t.Errorf("run state = %q, want pending (creation-time Advance must not fire for plan-first)", gotRun.State)
	}
	// And no deploy_initialization entry was recorded.
	if hasDeployInitializationEntry(t, au) {
		t.Error("deploy_initialization entry recorded for a plan-first run")
	}
}

// TestCreateRun_ModelValidity_FailOpen covers both fail-open submit cases: the
// wired NoData oracle and a nil oracle both create the run despite an unknown
// model (no false rejection in production today).
func TestCreateRun_ModelValidity_FailOpen(t *testing.T) {
	for _, tc := range []struct {
		name   string
		oracle modeloracle.ModelOracle
	}{
		{"nodata", modeloracle.NewNoData()},
		{"nil", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeRepo()
			s := newServer(t, repo)
			s.cfg.ModelOracle = tc.oracle

			w := createRunWithSpec(t, s, modelValiditySpecYAML("anything-goes"))
			if w.Code != http.StatusCreated {
				t.Fatalf("status = %d, want 201 (fail open):\n%s", w.Code, w.Body.String())
			}
		})
	}
}
