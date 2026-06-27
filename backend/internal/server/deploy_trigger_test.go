package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// deployTriggerGitHub is a minimal api.github.com stub for the deploy trigger:
// it answers the workflow_dispatch POST and the actions/runs list the resolver
// reads. Fields configure status + the list body; the mutex guards the captured
// dispatch call.
type deployTriggerGitHub struct {
	mu             sync.Mutex
	dispatchStatus int
	dispatchHits   int
	dispatchInputs map[string]string
	listBody       string
}

func newDeployTriggerGitHub(t *testing.T) (*deployTriggerGitHub, *githubclient.Client) {
	t.Helper()
	stub := &deployTriggerGitHub{dispatchStatus: http.StatusNoContent, listBody: `{"workflow_runs":[]}`}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/{owner}/{repo}/actions/workflows/{file}/dispatches",
		func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Inputs map[string]string `json:"inputs"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			stub.mu.Lock()
			stub.dispatchHits++
			stub.dispatchInputs = body.Inputs
			status := stub.dispatchStatus
			stub.mu.Unlock()
			w.WriteHeader(status)
		})
	mux.HandleFunc("GET /repos/{owner}/{repo}/actions/runs",
		func(w http.ResponseWriter, _ *http.Request) {
			stub.mu.Lock()
			body := stub.listBody
			stub.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return stub, &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// seedDispatchedDeploy stands up a deploy stage already at `dispatched` (the
// state advanceForDecision reaches before triggerDeploy fires) plus its run
// carrying the spec, repo, and installation id.
func seedDispatchedDeploy(rr *approvalRunRepo, specYAML string, installID *int64) (*run.Stage, *run.Run) {
	runID := uuid.New()
	st := &run.Stage{
		ID:               uuid.New(),
		RunID:            runID,
		Type:             run.StageTypeDeploy,
		ExecutorKind:     run.ExecutorAgent,
		ExecutorRef:      "deploy",
		State:            run.StageStateDispatched,
		RequiresApproval: true,
		CreatedAt:        time.Now().UTC(),
		UpdatedAt:        time.Now().UTC(),
	}
	rr.mu.Lock()
	rr.stages[st.ID] = st
	rr.mu.Unlock()
	runRow := &run.Run{
		ID:             runID,
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "release",
		WorkflowSHA:    "sha",
		WorkflowSpec:   []byte(specYAML),
		InstallationID: installID,
	}
	rr.seedRun(runRow)
	return st, runRow
}

// auditPayload finds the most recent appended entry of category and unmarshals
// its payload, or fails the test.
func auditPayload(t *testing.T, au *approvalAuditFake, category string) map[string]any {
	t.Helper()
	au.mu.Lock()
	defer au.mu.Unlock()
	for i := len(au.appended) - 1; i >= 0; i-- {
		if au.appended[i].Category == category {
			var p map[string]any
			if err := json.Unmarshal(au.appended[i].Payload, &p); err != nil {
				t.Fatalf("unmarshal %s payload: %v", category, err)
			}
			return p
		}
	}
	t.Fatalf("no %s audit entry found", category)
	return nil
}

// (1) github_actions: DispatchWorkflow fires with the correlation inputs, the
// resolver stores the run handle, and the stage parks at awaiting_deployment.
func TestTriggerDeploy_GitHubActions_DispatchResolvesAndParks(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, _ := seedDispatchedDeploy(rr, deploySpecNoConstraints, instID(99))
	// The resolver finds the just-dispatched run by its correlation inputs.
	stub.listBody = fmt.Sprintf(`{"workflow_runs":[{"id":424242,"html_url":"https://github.com/kuhlman-labs/example/actions/runs/424242","status":"queued","event":"workflow_dispatch","head_branch":"main","inputs":{"fishhawk_run_id":%q,"fishhawk_stage_id":%q}}]}`,
		stage.RunID.String(), stage.ID.String())

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateAwaitingDeployment {
		t.Fatalf("stage state = %q, want awaiting_deployment", got.State)
	}
	if stub.dispatchHits != 1 {
		t.Errorf("dispatch hits = %d, want 1", stub.dispatchHits)
	}
	if stub.dispatchInputs["fishhawk_run_id"] != stage.RunID.String() ||
		stub.dispatchInputs["fishhawk_stage_id"] != stage.ID.String() {
		t.Errorf("dispatch inputs = %v, want the run+stage correlation", stub.dispatchInputs)
	}
	p := auditPayload(t, au, CategoryDeploymentDispatched)
	if p["target"] != "github_actions" {
		t.Errorf("payload target = %v", p["target"])
	}
	if p["gha_run_id"].(float64) != 424242 {
		t.Errorf("payload gha_run_id = %v, want 424242", p["gha_run_id"])
	}
	if p["external_run_url"] != "https://github.com/kuhlman-labs/example/actions/runs/424242" {
		t.Errorf("payload external_run_url = %v", p["external_run_url"])
	}
}

// (1b) github_actions: dispatch succeeds but the run is not yet resolvable
// (eventual consistency) — the stage STILL parks at awaiting_deployment with a
// zero handle; the reconciler re-resolves later. Indeterminate resolution is
// not a trigger failure.
func TestTriggerDeploy_GitHubActions_UnresolvedStillParks(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stub.listBody = `{"workflow_runs":[]}` // resolver finds nothing yet
	stage, _ := seedDispatchedDeploy(rr, deploySpecNoConstraints, instID(99))

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateAwaitingDeployment {
		t.Fatalf("stage state = %q, want awaiting_deployment", got.State)
	}
	p := auditPayload(t, au, CategoryDeploymentDispatched)
	if p["gha_run_id"].(float64) != 0 {
		t.Errorf("payload gha_run_id = %v, want 0 (unresolved)", p["gha_run_id"])
	}
}

// (2) webhook: the trigger POSTs to delegate.url and parks at
// awaiting_deployment.
func TestTriggerDeploy_Webhook_PostsAndParks(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	var gotBody map[string]string
	hooked := make(chan struct{}, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		hooked <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(hook.Close)

	spec := fmt.Sprintf(deploySpecWebhookFmt, hook.URL)
	stage, _ := seedDispatchedDeploy(rr, spec, instID(99))

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateAwaitingDeployment {
		t.Fatalf("stage state = %q, want awaiting_deployment", got.State)
	}
	select {
	case <-hooked:
	default:
		t.Fatal("webhook was not POSTed")
	}
	if gotBody["fishhawk_run_id"] != stage.RunID.String() {
		t.Errorf("webhook body run_id = %q", gotBody["fishhawk_run_id"])
	}
	p := auditPayload(t, au, CategoryDeploymentDispatched)
	if p["target"] != "webhook" || p["url"] != hook.URL {
		t.Errorf("payload target/url = %v / %v", p["target"], p["url"])
	}
}

// (3) DispatchWorkflow error fails the stage category C (cannot trigger) with a
// deployment_dispatch_failed audit — NOT a silent park.
func TestTriggerDeploy_GitHubActions_DispatchError_FailsStage(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	stub.dispatchStatus = http.StatusUnprocessableEntity
	s.cfg.GitHub = gh
	stage, _ := seedDispatchedDeploy(rr, deploySpecNoConstraints, instID(99))

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy returned error (want failed stage, nil err): %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed", got.State)
	}
	if got.FailureCategory == nil || *got.FailureCategory != run.FailureC {
		t.Errorf("failure category = %v, want C", got.FailureCategory)
	}
	if countAppendedCategory(au, "deployment_dispatch_failed") != 1 {
		t.Errorf("deployment_dispatch_failed audit entries = %d, want 1",
			countAppendedCategory(au, "deployment_dispatch_failed"))
	}
	if countAppendedCategory(au, CategoryDeploymentDispatched) != 0 {
		t.Errorf("deployment_dispatched written despite dispatch error")
	}
}

// (4) webhook non-2xx fails the stage (cannot trigger).
func TestTriggerDeploy_Webhook_Non2xx_FailsStage(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(hook.Close)
	stage, _ := seedDispatchedDeploy(rr, fmt.Sprintf(deploySpecWebhookFmt, hook.URL), instID(99))

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed", got.State)
	}
	if countAppendedCategory(au, "deployment_dispatch_failed") != 1 {
		t.Errorf("deployment_dispatch_failed entries = %d, want 1",
			countAppendedCategory(au, "deployment_dispatch_failed"))
	}
}

// (5) NOT-WIRED posture: a github_actions target with no GitHub client leaves
// the stage at dispatched (demo/un-wired backend), never failing it.
func TestTriggerDeploy_GitHubActions_NilClient_StaysDispatched(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	s.cfg.GitHub = nil
	stage, _ := seedDispatchedDeploy(rr, deploySpecNoConstraints, instID(99))

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateDispatched {
		t.Fatalf("stage state = %q, want dispatched (not-wired posture)", got.State)
	}
	if countAppendedCategory(au, "deployment_dispatch_failed") != 0 {
		t.Errorf("dispatch_failed audit written for the not-wired posture")
	}
}

// (6) a run whose cached spec cannot be parsed for the delegate config fails
// the stage (resolveDeployDelegate's can't-resolve branch). The pre-flight gate
// already parsed the spec, so a failure here is an infrastructure-class
// surprise — fail loud, never park blind.
func TestTriggerDeploy_UnparseableSpec_FailsStage(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	_, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, _ := seedDispatchedDeploy(rr, "this: is: not: valid: yaml: ::::", instID(99))

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed", got.State)
	}
	if countAppendedCategory(au, "deployment_dispatch_failed") != 1 {
		t.Errorf("deployment_dispatch_failed entries = %d, want 1",
			countAppendedCategory(au, "deployment_dispatch_failed"))
	}
}

// (7) github_actions with no installation_id fails the stage (cannot dispatch).
func TestTriggerDeploy_GitHubActions_NoInstallationID_FailsStage(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	_, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, _ := seedDispatchedDeploy(rr, deploySpecNoConstraints, nil)

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed (no installation id)", got.State)
	}
}

// (8) recording the deployment_dispatched handle is fatal to the trigger: if
// the audit append fails the reconciler could never resolve the run, so the
// stage fails rather than parking blind.
func TestTriggerDeploy_AuditAppendFailure_FailsStage(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	au.appendErr = fmt.Errorf("audit store down")
	stage, _ := seedDispatchedDeploy(rr, deploySpecNoConstraints, instID(99))

	got, err := s.triggerDeploy(context.Background(), stage)
	if err != nil {
		t.Fatalf("triggerDeploy: %v", err)
	}
	if got.State != run.StageStateFailed {
		t.Fatalf("stage state = %q, want failed (handle not recorded)", got.State)
	}
	if stub.dispatchHits != 1 {
		t.Errorf("dispatch hits = %d, want 1 (dispatch fired before the audit failure)", stub.dispatchHits)
	}
}

// CROSS-BOUNDARY: an end-to-end deploy approve (gate → advanceForDecision →
// trigger) on a wired GitHub stub parks the stage at awaiting_deployment and
// records the deployment_dispatched handle — the approve→trigger seam a per-unit
// set alone would not exercise.
func TestSubmitApproval_Deploy_EndToEnd_TriggersAndParks(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, runRow := seedDeployRun(rr, "release", deploySpecNoConstraints)
	runRow.InstallationID = instID(99)
	stub.listBody = fmt.Sprintf(`{"workflow_runs":[{"id":777001,"html_url":"https://github.com/kuhlman-labs/example/actions/runs/777001","status":"in_progress","event":"workflow_dispatch","head_branch":"main","inputs":{"fishhawk_run_id":%q,"fishhawk_stage_id":%q}}]}`,
		stage.RunID.String(), stage.ID.String())

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	cur, _ := rr.GetStage(context.Background(), stage.ID)
	if cur.State != run.StageStateAwaitingDeployment {
		t.Fatalf("stage state = %q, want awaiting_deployment", cur.State)
	}
	if stub.dispatchHits != 1 {
		t.Errorf("dispatch hits = %d, want 1", stub.dispatchHits)
	}
	p := auditPayload(t, au, CategoryDeploymentDispatched)
	if p["gha_run_id"].(float64) != 777001 {
		t.Errorf("payload gha_run_id = %v, want 777001", p["gha_run_id"])
	}
}

const deploySpecWebhookFmt = `
version: "1.0"
workflows:
  release:
    stages:
      - id: deploy
        type: deploy
        executor:
          delegate:
            target: webhook
            url: %s
`
