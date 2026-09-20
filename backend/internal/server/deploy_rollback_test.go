package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// seedSettledDeployRun stands up a deploy stage in the given terminal state
// (succeeded/failed) plus its run carrying the spec + repo, so the rollback
// handler's "deploy settled" precondition is met. Returns the stage + run.
func seedSettledDeployRun(rr *approvalRunRepo, specYAML string, state run.StageState, installID *int64) (*run.Stage, *run.Run) {
	stage, runRow := seedDeployRun(rr, "release", specYAML)
	rr.mu.Lock()
	stage.State = state
	rr.mu.Unlock()
	runRow.InstallationID = installID
	return stage, runRow
}

// rollbackRequest invokes handleRollbackDeployment for runID with the given
// identity, returning the recorder. A nil identity uses the session-user
// operator (withAuth); otherwise the provided identity is injected verbatim.
func rollbackRequest(t *testing.T, s *Server, runID uuid.UUID, ident *Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/deployment/rollback", runID), nil)
	req.SetPathValue("run_id", runID.String())
	if ident == nil {
		req = withAuth(req)
	} else {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, *ident))
	}
	w := httptest.NewRecorder()
	s.handleRollbackDeployment(w, req)
	return w
}

// (1) github_actions: the rollback re-dispatches the same workflow_ref with the
// rollback marker input, resolves the rollback run handle, and records a
// deployment_rollback_initiated audit DISTINCT from the initial deploy handle.
func TestRollbackDeployment_GitHubActions_DispatchesAndRecordsInitiated(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(99))
	stub.listBody = fmt.Sprintf(`{"workflow_runs":[{"id":515151,"html_url":"https://github.com/kuhlman-labs/example/actions/runs/515151","status":"queued","event":"workflow_dispatch","head_branch":"main","inputs":{"fishhawk_run_id":%q,"fishhawk_stage_id":%q,"fishhawk_rollback":"true"}}]}`,
		stage.RunID.String(), stage.ID.String())

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if stub.dispatchHits != 1 {
		t.Errorf("dispatch hits = %d, want 1", stub.dispatchHits)
	}
	if stub.dispatchInputs[rollbackDispatchInput] != "true" {
		t.Errorf("dispatch inputs %v missing rollback marker", stub.dispatchInputs)
	}
	if stub.dispatchInputs["fishhawk_run_id"] != stage.RunID.String() {
		t.Errorf("dispatch inputs %v missing correlation run id", stub.dispatchInputs)
	}
	p := auditPayload(t, au, CategoryDeploymentRollbackInitiated)
	if p["target"] != "github_actions" {
		t.Errorf("payload target = %v", p["target"])
	}
	if p["gha_run_id"].(float64) != 515151 {
		t.Errorf("payload gha_run_id = %v, want 515151", p["gha_run_id"])
	}
	if p["rollback"] != true {
		t.Errorf("payload rollback = %v, want true", p["rollback"])
	}
	// The rollback handle is DISTINCT — no deployment_dispatched written here.
	if countAppendedCategory(au, CategoryDeploymentDispatched) != 0 {
		t.Errorf("rollback wrote a deployment_dispatched (initial-deploy) handle")
	}
	// _completed is NOT written at initiation (only when the rollback run is
	// terminal, via the deployment callback).
	if countAppendedCategory(au, CategoryDeploymentRollbackCompleted) != 0 {
		t.Errorf("deployment_rollback_completed written at initiation")
	}
}

// (2) webhook: the rollback POSTs the rollback payload to delegate.url and
// records deployment_rollback_initiated with the webhook target as the handle.
func TestRollbackDeployment_Webhook_PostsAndRecordsInitiated(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	var gotBody map[string]any
	hooked := make(chan struct{}, 1)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		hooked <- struct{}{}
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(hook.Close)
	stage, _ := seedSettledDeployRun(rr, fmt.Sprintf(deploySpecWebhookFmt, hook.URL), run.StageStateSucceeded, instID(99))

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	select {
	case <-hooked:
	default:
		t.Fatal("webhook rollback was not POSTed")
	}
	if gotBody[rollbackDispatchInput] != true {
		t.Errorf("webhook body %v missing rollback marker", gotBody)
	}
	vars, _ := gotBody["variables"].(map[string]any)
	if vars["FISHHAWK_ROLLBACK"] != "true" || vars["FISHHAWK_RUN_ID"] != stage.RunID.String() {
		t.Errorf("webhook body variables = %v, want FISHHAWK_ROLLBACK=true + FISHHAWK_RUN_ID", vars)
	}
	p := auditPayload(t, au, CategoryDeploymentRollbackInitiated)
	if p["target"] != "webhook" || p["url"] != hook.URL {
		t.Errorf("payload target/url = %v / %v", p["target"], p["url"])
	}
	if _, has := p["secret_env"]; has {
		t.Errorf("no-secret rollback payload carries secret_env: %v", p)
	}
}

// (3) anonymous caller → 401.
func TestRollbackDeployment_Unauthenticated(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(99))
	anon := Identity{}
	w := rollbackRequest(t, s, stage.RunID, &anon)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401:\n%s", w.Code, w.Body.String())
	}
}

// (4) authenticated token missing write:runs → 403.
func TestRollbackDeployment_InsufficientScope(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(99))
	tok := Identity{Subject: "token:reader", TokenID: "tok_1", Scopes: []string{"read:runs"}}
	w := rollbackRequest(t, s, stage.RunID, &tok)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
}

// (5) a run-bound MCP token may roll back only its own run → 403 for another.
func TestRollbackDeployment_CrossRunMCPToken(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(99))
	other := Identity{Subject: "mcp:run:" + uuid.NewString(), TokenID: "tok_mcp", Scopes: []string{"write:runs"}}
	w := rollbackRequest(t, s, stage.RunID, &other)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
}

// (4') #1390: an OPERATOR bearer token (non-mcp:run subject) holding write:runs
// but missing write:deploy → 403 insufficient_scope naming write:deploy. This
// is the deploy-scope branch layered on top of the existing write:runs check.
func TestRollbackDeployment_OperatorMissingWriteDeploy_403(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(99))
	tok := Identity{Subject: "operator:test", TokenID: "tok_ops", Scopes: []string{"write:runs"}}
	w := rollbackRequest(t, s, stage.RunID, &tok)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient_scope") ||
		!strings.Contains(w.Body.String(), "write:deploy") {
		t.Errorf("body missing insufficient_scope/write:deploy:\n%s", w.Body.String())
	}
}

// (4”) #1390: an operator bearer token holding BOTH write:runs and write:deploy
// proceeds — the rollback re-dispatches and records the initiated handle (202).
func TestRollbackDeployment_OperatorWithWriteDeploy_Proceeds(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(99))
	stub.listBody = fmt.Sprintf(`{"workflow_runs":[{"id":616161,"html_url":"https://github.com/kuhlman-labs/example/actions/runs/616161","status":"queued","event":"workflow_dispatch","head_branch":"main","inputs":{"fishhawk_run_id":%q,"fishhawk_stage_id":%q,"fishhawk_rollback":"true"}}]}`,
		stage.RunID.String(), stage.ID.String())
	tok := Identity{Subject: "operator:test", TokenID: "tok_ops", Scopes: []string{"write:runs", "write:deploy"}}
	w := rollbackRequest(t, s, stage.RunID, &tok)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if countAppendedCategory(au, CategoryDeploymentRollbackInitiated) != 1 {
		t.Errorf("deployment_rollback_initiated entries = %d, want 1", countAppendedCategory(au, CategoryDeploymentRollbackInitiated))
	}
}

// (5') the mcp:run self-rollback carve-out: a run-bound MCP token rolling back
// ITS OWN run authorizes WITHOUT write:deploy — the deploy-scope gate exempts
// mcp:run subjects (they are constrained instead by the subject-binding guard).
// Proves #1390 preserved the self-rollback path.
func TestRollbackDeployment_OwnRunMCPToken_NoWriteDeploy_Proceeds(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(99))
	stub.listBody = fmt.Sprintf(`{"workflow_runs":[{"id":717171,"html_url":"https://github.com/kuhlman-labs/example/actions/runs/717171","status":"queued","event":"workflow_dispatch","head_branch":"main","inputs":{"fishhawk_run_id":%q,"fishhawk_stage_id":%q,"fishhawk_rollback":"true"}}]}`,
		stage.RunID.String(), stage.ID.String())
	self := Identity{Subject: "mcp:run:" + stage.RunID.String(), TokenID: "tok_mcp", Scopes: []string{"write:runs"}}
	w := rollbackRequest(t, s, stage.RunID, &self)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (mcp:run self-rollback exempt from write:deploy):\n%s", w.Code, w.Body.String())
	}
	if countAppendedCategory(au, CategoryDeploymentRollbackInitiated) != 1 {
		t.Errorf("deployment_rollback_initiated entries = %d, want 1", countAppendedCategory(au, CategoryDeploymentRollbackInitiated))
	}
}

// (6) run not found → 404.
func TestRollbackDeployment_RunNotFound(t *testing.T) {
	s, _, _, _ := newApprovalServer(t)
	w := rollbackRequest(t, s, uuid.New(), nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
}

// (7) a run with no deploy stage → 404 deploy_stage_not_found.
func TestRollbackDeployment_NoDeployStage(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	runRow := &run.Run{ID: uuid.New(), Repo: "kuhlman-labs/example", WorkflowID: "release", WorkflowSpec: []byte(deploySpecNoConstraints)}
	rr.seedRun(runRow)
	// A non-deploy stage on the run; deployStageForRun must skip it.
	rr.seedStageOnRun(runRow.ID, run.StageTypeImplement, run.StageStateSucceeded)
	w := rollbackRequest(t, s, runRow.ID, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404:\n%s", w.Code, w.Body.String())
	}
}

// (8) an in-flight (not settled) deploy stage → 409 deploy_not_settled.
func TestRollbackDeployment_NotSettled(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateAwaitingDeployment, instID(99))
	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409:\n%s", w.Code, w.Body.String())
	}
}

// (9) github_actions dispatch error → 502, no deployment_rollback_initiated.
func TestRollbackDeployment_GitHubActions_DispatchError(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	stub.dispatchStatus = http.StatusUnprocessableEntity
	s.cfg.GitHub = gh
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(99))

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	if countAppendedCategory(au, CategoryDeploymentRollbackInitiated) != 0 {
		t.Errorf("deployment_rollback_initiated written despite dispatch error")
	}
}

// (10) webhook non-2xx → 502, no deployment_rollback_initiated.
func TestRollbackDeployment_Webhook_Non2xx(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(hook.Close)
	stage, _ := seedSettledDeployRun(rr, fmt.Sprintf(deploySpecWebhookFmt, hook.URL), run.StageStateSucceeded, instID(99))

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	if countAppendedCategory(au, CategoryDeploymentRollbackInitiated) != 0 {
		t.Errorf("deployment_rollback_initiated written despite webhook failure")
	}
}

// (11) github_actions with no GitHub client (un-wired) → 503 (fails loud, unlike
// slice-1's trigger which parks).
func TestRollbackDeployment_GitHubActions_NilClient(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	s.cfg.GitHub = nil
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(99))
	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
}

// (12) github_actions with no installation_id → 422.
func TestRollbackDeployment_GitHubActions_NoInstallationID(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	_, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, nil)
	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
}

// (13) a deploy stage present but the cached spec has no delegating deploy stage
// → 422 rollback_unconfigured (deployDelegateForRun's can't-resolve branch).
func TestRollbackDeployment_NoDelegateInSpec(t *testing.T) {
	s, _, rr, _ := newApprovalServer(t)
	_, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	// deploySpecNoDeployStage's workflow has only an implement stage, so the
	// delegate resolver finds no deploy delegate — but the run still carries a
	// deploy stage row (seeded directly), so the precondition passes first.
	stage, _ := seedSettledDeployRun(rr, deploySpecNoDeployStage, run.StageStateSucceeded, instID(99))
	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
}

// (14) the deployment_rollback_initiated audit append failing → 500 (the handle
// is the governance record; a dispatched-but-unrecorded rollback fails loud).
func TestRollbackDeployment_AuditAppendFailure(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	stub, gh := newDeployTriggerGitHub(t)
	s.cfg.GitHub = gh
	au.appendErr = fmt.Errorf("audit store down")
	stage, _ := seedSettledDeployRun(rr, deploySpecNoConstraints, run.StageStateSucceeded, instID(99))

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500:\n%s", w.Code, w.Body.String())
	}
	if stub.dispatchHits != 1 {
		t.Errorf("dispatch hits = %d, want 1 (dispatch fired before the audit failure)", stub.dispatchHits)
	}
}

// (15) malformed run_id path value → 400.
func TestRollbackDeployment_InvalidRunID(t *testing.T) {
	s, _, _, _ := newApprovalServer(t)
	req := httptest.NewRequest(http.MethodPost, "/v0/runs/not-a-uuid/deployment/rollback", nil)
	req.SetPathValue("run_id", "not-a-uuid")
	req = withAuth(req)
	w := httptest.NewRecorder()
	s.handleRollbackDeployment(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400:\n%s", w.Code, w.Body.String())
	}
}

// (16) RunRepo/AuditRepo unconfigured → 503.
func TestRollbackDeployment_Unconfigured(t *testing.T) {
	s, _, _, _ := newApprovalServer(t)
	s.cfg.RunRepo = nil
	w := rollbackRequest(t, s, uuid.New(), nil)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503:\n%s", w.Code, w.Body.String())
	}
}

// (R1) webhook secret channel on the ROLLBACK arm (E45.57 / #3497): the same
// shared dispatcher places the PRIVATE-TOKEN default header, the body carries
// fishhawk_rollback in flat and variables form, and the
// deployment_rollback_initiated payload records names only.
func TestRollbackDeployment_Webhook_SecretHeader_Sent(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	withSecretLookup(s, map[string]string{"DEPLOY_TRIGGER_TOKEN": webhookTestSecret})
	logBuf := captureLogger(s)
	hook := newCountingHook(t, http.StatusAccepted, nil, "")
	stage, _ := seedSettledDeployRun(rr, fmt.Sprintf(deploySpecWebhookSecretFmt, hook.srv.URL,
		"            secret_env: DEPLOY_TRIGGER_TOKEN"), run.StageStateSucceeded, instID(99))

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	hits, headers, body := hook.snapshot()
	if hits != 1 {
		t.Fatalf("webhook hits = %d, want 1", hits)
	}
	if h := headers.Get("PRIVATE-TOKEN"); h != webhookTestSecret {
		t.Errorf("PRIVATE-TOKEN header = %q, want the injected value", h)
	}
	if body[rollbackDispatchInput] != true {
		t.Errorf("rollback body lacks fishhawk_rollback: %v", body)
	}
	vars, _ := body["variables"].(map[string]any)
	if vars["FISHHAWK_ROLLBACK"] != "true" {
		t.Errorf("rollback body variables = %v, want FISHHAWK_ROLLBACK=true", vars)
	}
	p := auditPayload(t, au, CategoryDeploymentRollbackInitiated)
	if p["secret_env"] != "DEPLOY_TRIGGER_TOKEN" || p["secret_placement"] != "header" {
		t.Errorf("payload secret_env/secret_placement = %v / %v", p["secret_env"], p["secret_placement"])
	}
	assertSecretAbsent(t, au, logBuf, webhookTestSecret, map[string]string{"response body": w.Body.String()})
}

// (R2) secret_env unset on rollback → 502 rollback_dispatch_failed naming the
// variable, NO POST (zero requests on the reachable server), no
// deployment_rollback_initiated row.
func TestRollbackDeployment_Webhook_SecretEnvUnset_RefusesWithoutPost(t *testing.T) {
	s, _, rr, au := newApprovalServer(t)
	withSecretLookup(s, map[string]string{})
	hook := newCountingHook(t, http.StatusAccepted, nil, "")
	stage, _ := seedSettledDeployRun(rr, fmt.Sprintf(deploySpecWebhookSecretFmt, hook.srv.URL,
		"            secret_env: DEPLOY_TRIGGER_TOKEN"), run.StageStateSucceeded, instID(99))

	w := rollbackRequest(t, s, stage.RunID, nil)
	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "rollback_dispatch_failed") || !strings.Contains(w.Body.String(), "DEPLOY_TRIGGER_TOKEN") {
		t.Errorf("body = %s, want rollback_dispatch_failed naming DEPLOY_TRIGGER_TOKEN", w.Body.String())
	}
	if hits, _, _ := hook.snapshot(); hits != 0 {
		t.Errorf("webhook hits = %d, want 0 — the refusal must fire before any POST", hits)
	}
	if countAppendedCategory(au, CategoryDeploymentRollbackInitiated) != 0 {
		t.Errorf("deployment_rollback_initiated written despite the refusal")
	}
}

// (R3)+(R4) rollback arms of the redirect posture and the redaction
// invariant: a 302 carrying the secret in Location is NOT followed (the second
// server records zero requests; the 502 carries status only) and an
// unparsable Location classifies redirect_parse; in every arm the secret is
// absent from the audit rows, the HTTP error body and the log buffer.
func TestRollbackDeployment_Webhook_SecretNeverLeaks(t *testing.T) {
	arms := []struct {
		name      string
		status    int
		location  func(second string) string
		wantCode  int
		wantClass string
		want3xx   bool
	}{
		{
			name: "redirect_not_followed", status: http.StatusFound,
			location: func(second string) string { return second + "/?token=" + url.QueryEscape(webhookTestSecret) },
			wantCode: http.StatusBadGateway, want3xx: true,
		},
		{
			name: "malformed_location", status: http.StatusFound,
			location:  func(string) string { return "http://[" + webhookTestSecret },
			wantCode:  http.StatusBadGateway,
			wantClass: webhookErrClassRedirectParse,
		},
		{
			name: "non_2xx", status: http.StatusInternalServerError,
			location: func(string) string { return "" },
			wantCode: http.StatusBadGateway,
		},
		{
			name: "2xx", status: http.StatusAccepted,
			location: func(string) string { return "" },
			wantCode: http.StatusAccepted,
		},
	}
	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			s, _, rr, au := newApprovalServer(t)
			withSecretLookup(s, map[string]string{"DEPLOY_TRIGGER_TOKEN": webhookTestSecret})
			logBuf := captureLogger(s)
			second := newCountingHook(t, http.StatusAccepted, nil, "")
			respHeaders := map[string]string{"X-Echo": webhookTestSecret}
			if loc := a.location(second.srv.URL); loc != "" {
				respHeaders["Location"] = loc
			}
			hook := newCountingHook(t, a.status, respHeaders, "echo "+webhookTestSecret)
			stage, _ := seedSettledDeployRun(rr, fmt.Sprintf(deploySpecWebhookSecretFmt, hook.srv.URL,
				"            secret_env: DEPLOY_TRIGGER_TOKEN"), run.StageStateSucceeded, instID(99))

			w := rollbackRequest(t, s, stage.RunID, nil)
			if w.Code != a.wantCode {
				t.Fatalf("status = %d, want %d:\n%s", w.Code, a.wantCode, w.Body.String())
			}
			if w.Code != http.StatusAccepted {
				var resp errorEnvelope
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode error body: %v\n%s", err, w.Body.String())
				}
				if resp.Error.Code != "rollback_dispatch_failed" {
					t.Errorf("error code = %q, want rollback_dispatch_failed", resp.Error.Code)
				}
				// The 5xx body is default-deny redacted (E67.15), so the
				// class and status ride the MESSAGE; details never carry a
				// location key in the first place.
				if a.wantClass != "" && !strings.Contains(resp.Error.Message, a.wantClass) {
					t.Errorf("message = %q, want it to carry the class %s", resp.Error.Message, a.wantClass)
				}
				if a.want3xx {
					if !strings.Contains(resp.Error.Message, "(302)") {
						t.Errorf("message = %q, want the 302 status", resp.Error.Message)
					}
					if _, has := resp.Error.Details["location"]; has {
						t.Errorf("details carry location: %v", resp.Error.Details)
					}
					if hits, _, _ := second.snapshot(); hits != 0 {
						t.Errorf("redirect target received %d request(s), want 0", hits)
					}
				}
				if countAppendedCategory(au, CategoryDeploymentRollbackInitiated) != 0 {
					t.Errorf("deployment_rollback_initiated written despite the failure")
				}
			}
			assertSecretAbsent(t, au, logBuf, webhookTestSecret, map[string]string{"response body": w.Body.String()})
		})
	}
}
