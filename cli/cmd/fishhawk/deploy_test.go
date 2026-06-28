package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// deployFake is a configurable backend for the deploy end-to-end tests.
// It serves GET stages, GET artifacts, POST approvals, and POST rollback,
// capturing the last request so a test can assert the wiring (method /
// path / body) the CLI issued.
type deployFake struct {
	mu sync.Mutex

	stages       []httpclient.Stage
	stagesStatus int

	artifacts       []httpclient.Artifact
	artifactsStatus int

	approvalStatus  int
	approvalErrCode string
	approvalResp    httpclient.Stage
	approvalRawResp string
	approvedID      string
	approvalBody    httpclient.SubmitApprovalInput

	rollbackStatus  int
	rollbackErrCode string
	rollbackResp    httpclient.RollbackDeploymentResult
	rollbackRunID   string
}

func newDeployFake(t *testing.T) (*deployFake, *httptest.Server) {
	t.Helper()
	fb := &deployFake{
		stagesStatus:    http.StatusOK,
		artifactsStatus: http.StatusOK,
		approvalStatus:  http.StatusOK,
		rollbackStatus:  http.StatusAccepted,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v0/runs/{run_id}/stages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.stagesStatus)
		if fb.stagesStatus >= 400 {
			_ = json.NewEncoder(w).Encode(errEnvelope("internal_error", "boom"))
			return
		}
		_ = json.NewEncoder(w).Encode(httpclient.ListStagesResult{Items: fb.stages})
	})
	mux.HandleFunc("GET /v0/stages/{stage_id}/artifacts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.artifactsStatus)
		if fb.artifactsStatus >= 400 {
			_ = json.NewEncoder(w).Encode(errEnvelope("internal_error", "boom"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": fb.artifacts})
	})
	mux.HandleFunc("POST /v0/stages/{stage_id}/approvals", func(w http.ResponseWriter, r *http.Request) {
		var in httpclient.SubmitApprovalInput
		_ = json.NewDecoder(r.Body).Decode(&in)
		fb.mu.Lock()
		fb.approvedID = r.PathValue("stage_id")
		fb.approvalBody = in
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.approvalStatus)
		if fb.approvalStatus >= 400 {
			code := fb.approvalErrCode
			if code == "" {
				code = "invalid_state_transition"
			}
			details := map[string]any{}
			if code == "insufficient_scope" {
				details["required_scope"] = "write:deploy"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": code, "message": "rejected", "details": details},
			})
			return
		}
		if fb.approvalRawResp != "" {
			_, _ = w.Write([]byte(fb.approvalRawResp))
			return
		}
		_ = json.NewEncoder(w).Encode(fb.approvalResp)
	})
	mux.HandleFunc("POST /v0/runs/{run_id}/deployment/rollback", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.rollbackRunID = r.PathValue("run_id")
		fb.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.rollbackStatus)
		if fb.rollbackStatus >= 400 {
			code := fb.rollbackErrCode
			if code == "" {
				code = "internal_error"
			}
			details := map[string]any{}
			if code == "insufficient_scope" {
				details["required_scope"] = "write:deploy"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": code, "message": "rollback rejected", "details": details},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(fb.rollbackResp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func errEnvelope(code, msg string) map[string]any {
	return map[string]any{"error": map[string]any{"code": code, "message": msg}}
}

// deployStages builds a stage list with a deploy stage in the given
// state, preceded by an implement stage at sequence 1 for shape.
func deployStages(runID uuid.UUID, deployState string) []httpclient.Stage {
	return []httpclient.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: "implement", State: "succeeded",
			Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"}},
		{ID: uuid.New(), RunID: runID, Sequence: 2, Type: "deploy", State: deployState,
			Executor: httpclient.StageExecutor{Kind: "delegate", Ref: "github_actions"}},
	}
}

func deploymentArtifact(stageID uuid.UUID, dep Deployment) httpclient.Artifact {
	body, _ := json.Marshal(dep)
	return httpclient.Artifact{
		ID: uuid.New(), StageID: stageID, Kind: "deployment",
		Content: json.RawMessage(body),
	}
}

// --- deploy status ---

func TestDeployStatus_HappyPath(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := deployStages(runID, "succeeded")
	deployStageID := stages[1].ID
	fb.stages = stages
	fb.artifacts = []httpclient.Artifact{deploymentArtifact(deployStageID, Deployment{
		Environment:    "production",
		Ref:            "abc123",
		ExternalRunURL: "https://github.com/x/y/actions/runs/42",
		Outcome:        "succeeded",
		RollbackHandle: "rb-1",
	})}

	var stdout strings.Builder
	got := run([]string{"deploy", "status", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	for _, want := range []string{
		deployStageID.String(), "deploy", "succeeded",
		"production", "abc123", "https://github.com/x/y/actions/runs/42", "rb-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestDeployStatus_NoDeploymentArtifact(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	fb.stages = deployStages(runID, "awaiting_deployment")
	fb.artifacts = nil

	var stdout strings.Builder
	got := run([]string{"deploy", "status", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if !strings.Contains(stdout.String(), "(not yet recorded)") {
		t.Errorf("stdout missing not-yet-recorded line:\n%s", stdout.String())
	}
}

func TestDeployStatus_NoDeployStage(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	// Stages with no deploy stage at all.
	fb.stages = []httpclient.Stage{
		{ID: uuid.New(), RunID: runID, Sequence: 1, Type: "implement", State: "succeeded",
			Executor: httpclient.StageExecutor{Kind: "agent", Ref: "claude-code"}},
	}

	var stderr strings.Builder
	got := run([]string{"deploy", "status", runID.String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "no deploy stage") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestDeployStatus_JSONOutput(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := deployStages(runID, "succeeded")
	deployStageID := stages[1].ID
	fb.stages = stages
	fb.artifacts = []httpclient.Artifact{deploymentArtifact(deployStageID, Deployment{
		Environment: "staging", Ref: "deadbeef",
		ExternalRunURL: "https://example.test/run/1", Outcome: "partial",
	})}

	var stdout strings.Builder
	got := run([]string{"deploy", "status", "--output", "json", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	var decoded deployStatusOutput
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.Stage.ID != deployStageID || decoded.Stage.Type != "deploy" {
		t.Errorf("stage mismatch: %+v", decoded.Stage)
	}
	if decoded.Deployment == nil || decoded.Deployment.Environment != "staging" || decoded.Deployment.Outcome != "partial" {
		t.Errorf("deployment mismatch: %+v", decoded.Deployment)
	}
}

func TestDeployStatus_BadOutputValue(t *testing.T) {
	_, srv := newDeployFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"deploy", "status", "--output", "xml", uuid.New().String()}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "invalid --output") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestDeployStatus_BadUUID(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"deploy", "status", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
	if fb.rollbackRunID != "" || fb.approvedID != "" {
		t.Errorf("backend reached despite local UUID validation failure")
	}
}

func TestDeployStatus_MissingArg(t *testing.T) {
	_, srv := newDeployFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"deploy", "status"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "<run-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

// --- deploy approve / reject ---

func TestDeployApprove_HappyPath(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := deployStages(runID, "awaiting_deploy_approval")
	deployStageID := stages[1].ID
	fb.stages = stages
	fb.approvalResp = httpclient.Stage{
		ID: deployStageID, RunID: runID, Sequence: 2, Type: "deploy", State: "awaiting_deployment",
		Executor: httpclient.StageExecutor{Kind: "delegate", Ref: "github_actions"},
	}

	var stdout strings.Builder
	got := run([]string{"deploy", "approve", "--reason", "ship it", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if fb.approvedID != deployStageID.String() {
		t.Errorf("approved stage_id = %s, want %s", fb.approvedID, deployStageID)
	}
	if fb.approvalBody.Decision != httpclient.ApprovalApprove {
		t.Errorf("decision = %q, want approve", fb.approvalBody.Decision)
	}
	if fb.approvalBody.Comment != "ship it" {
		t.Errorf("comment = %q, want 'ship it'", fb.approvalBody.Comment)
	}
	if !strings.Contains(stdout.String(), "awaiting_deployment") {
		t.Errorf("stdout missing post-approve state: %s", stdout.String())
	}
}

func TestDeployReject_MissingReasonWarns(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := deployStages(runID, "awaiting_deploy_approval")
	deployStageID := stages[1].ID
	fb.stages = stages
	cat := "D"
	fb.approvalResp = httpclient.Stage{
		ID: deployStageID, RunID: runID, Sequence: 2, Type: "deploy", State: "failed",
		FailureCategory: &cat,
		Executor:        httpclient.StageExecutor{Kind: "delegate", Ref: "github_actions"},
	}

	var stdout, stderr strings.Builder
	got := run([]string{"deploy", "reject", runID.String()}, &stdout, &stderr)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK; stderr=%s", got, stderr.String())
	}
	if fb.approvalBody.Decision != httpclient.ApprovalReject {
		t.Errorf("decision = %q, want reject", fb.approvalBody.Decision)
	}
	if !strings.Contains(stderr.String(), "warning: --reason not provided") {
		t.Errorf("stderr missing missing-reason warning: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "failed") {
		t.Errorf("stdout missing failed state: %s", stdout.String())
	}
}

func TestDeployDecide_NoAwaitingDeployStage(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	// Deploy stage already settled — gate window missed.
	fb.stages = deployStages(runID, "succeeded")

	var stderr strings.Builder
	got := run([]string{"deploy", "approve", runID.String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "no deploy stage awaiting approval") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	if fb.approvedID != "" {
		t.Errorf("approvals endpoint reached with no awaiting deploy stage; stage_id=%s", fb.approvedID)
	}
}

func TestDeployDecide_BadUUID(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"deploy", "approve", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
	if fb.approvedID != "" {
		t.Errorf("approvals endpoint reached despite local UUID validation failure")
	}
}

func TestDeployDecide_MissingArg(t *testing.T) {
	_, srv := newDeployFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"deploy", "reject"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "<run-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestDeployRollback_MissingArg(t *testing.T) {
	_, srv := newDeployFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"deploy", "rollback"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "<run-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestDeployApprove_InsufficientScope403(t *testing.T) {
	// write:deploy is enforced server-side; the CLI surfaces the 403
	// insufficient_scope envelope verbatim.
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	fb.stages = deployStages(runID, "awaiting_deploy_approval")
	fb.approvalStatus = http.StatusForbidden
	fb.approvalErrCode = "insufficient_scope"

	var stderr strings.Builder
	got := run([]string{"deploy", "approve", "--reason", "go", runID.String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "insufficient_scope") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

func TestDeployApprove_JSONOutput(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	stages := deployStages(runID, "awaiting_deploy_approval")
	deployStageID := stages[1].ID
	fb.stages = stages
	fb.approvalResp = httpclient.Stage{
		ID: deployStageID, RunID: runID, Sequence: 2, Type: "deploy", State: "awaiting_deployment",
		Executor: httpclient.StageExecutor{Kind: "delegate", Ref: "github_actions"},
	}

	var stdout strings.Builder
	got := run([]string{"deploy", "approve", "--output", "json", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	var decoded httpclient.ApprovalResult
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.ID != deployStageID || decoded.State != "awaiting_deployment" {
		t.Errorf("decoded mismatch: %+v", decoded.Stage)
	}
}

func TestDeployApprove_BadOutputValue(t *testing.T) {
	_, srv := newDeployFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"deploy", "approve", "--output", "xml", uuid.New().String()}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "invalid --output") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

// TestFindAwaitingApprovalDeployStage pins the exact gate-state constant so
// that a future drift back to "awaiting_approval" (the old wrong string) fails
// this test rather than silently passing.
func TestFindAwaitingApprovalDeployStage(t *testing.T) {
	runID := uuid.New()
	makeStage := func(typ, state string) httpclient.Stage {
		return httpclient.Stage{
			ID: uuid.New(), RunID: runID, Sequence: 2, Type: typ, State: state,
			Executor: httpclient.StageExecutor{Kind: "delegate", Ref: "github_actions"},
		}
	}

	cases := []struct {
		name      string
		stages    []httpclient.Stage
		wantFound bool
	}{
		{
			name:      "deploy at awaiting_deploy_approval resolves",
			stages:    []httpclient.Stage{makeStage("deploy", "awaiting_deploy_approval")},
			wantFound: true,
		},
		{
			name:      "deploy at awaiting_approval (old wrong constant) does not resolve",
			stages:    []httpclient.Stage{makeStage("deploy", "awaiting_approval")},
			wantFound: false,
		},
		{
			name:      "settled deploy stage does not resolve",
			stages:    []httpclient.Stage{makeStage("deploy", "succeeded")},
			wantFound: false,
		},
		{
			name:      "non-deploy stage at awaiting_deploy_approval does not resolve",
			stages:    []httpclient.Stage{makeStage("implement", "awaiting_deploy_approval")},
			wantFound: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findAwaitingApprovalDeployStage(tc.stages)
			if tc.wantFound && got == nil {
				t.Errorf("expected a stage to be found, got nil")
			}
			if !tc.wantFound && got != nil {
				t.Errorf("expected nil, got stage id=%s state=%s", got.ID, got.State)
			}
		})
	}
}

// --- deploy rollback ---

func TestDeployRollback_HappyPath(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	stageID := uuid.New()
	fb.rollbackResp = httpclient.RollbackDeploymentResult{
		RunID: runID, StageID: stageID, Target: "github_actions",
		GHARunID: 555, ExternalRunURL: "https://github.com/x/y/actions/runs/555",
		Message: "rollback re-dispatched",
	}

	var stdout strings.Builder
	got := run([]string{"deploy", "rollback", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if fb.rollbackRunID != runID.String() {
		t.Errorf("rollback hit run %s, want %s", fb.rollbackRunID, runID)
	}
	out := stdout.String()
	for _, want := range []string{
		"github_actions", stageID.String(), "555",
		"https://github.com/x/y/actions/runs/555", "rollback re-dispatched",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

func TestDeployRollback_JSONOutput(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	runID := uuid.New()
	stageID := uuid.New()
	fb.rollbackResp = httpclient.RollbackDeploymentResult{
		RunID: runID, StageID: stageID, Target: "webhook",
		ExternalRunURL: "https://hook.example/deploy", Message: "ok",
	}

	var stdout strings.Builder
	got := run([]string{"deploy", "rollback", "--output", "json", runID.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	var decoded httpclient.RollbackDeploymentResult
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.StageID != stageID || decoded.Target != "webhook" {
		t.Errorf("decoded mismatch: %+v", decoded)
	}
}

func TestDeployRollback_NotSettled409(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	fb.rollbackStatus = http.StatusConflict
	fb.rollbackErrCode = "deploy_not_settled"

	var stderr strings.Builder
	got := run([]string{"deploy", "rollback", uuid.New().String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "deploy_not_settled") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

func TestDeployRollback_Unconfigured422(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	fb.rollbackStatus = http.StatusUnprocessableEntity
	fb.rollbackErrCode = "rollback_unconfigured"

	var stderr strings.Builder
	got := run([]string{"deploy", "rollback", uuid.New().String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "rollback_unconfigured") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

func TestDeployRollback_BadOutputValue(t *testing.T) {
	_, srv := newDeployFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"deploy", "rollback", "--output", "xml", uuid.New().String()}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "invalid --output") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestDeployRollback_BadUUID(t *testing.T) {
	fb, srv := newDeployFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"deploy", "rollback", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
	if fb.rollbackRunID != "" {
		t.Errorf("rollback endpoint reached despite local UUID validation failure")
	}
}

// --- dispatcher ---

func TestDeploy_NoSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"deploy"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "subcommand required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestDeploy_UnknownSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"deploy", "frobnicate"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}
