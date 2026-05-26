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
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
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
// is provided inline, the GitHub client is never called — even when a
// GitHub client is configured.
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
	if fake.installationCalls != 0 {
		t.Errorf("installation endpoint called %d times; inline spec must bypass GitHub fetch",
			fake.installationCalls)
	}
	if fake.specCalls != 0 {
		t.Errorf("spec endpoint called %d times; inline spec must bypass GitHub fetch", fake.specCalls)
	}
	// Stages are still created from the inline spec.
	var got runResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if len(repo.stagesFor(got.ID)) != 2 {
		t.Errorf("expected 2 stages from inline spec, got %d", len(repo.stagesFor(got.ID)))
	}
}
