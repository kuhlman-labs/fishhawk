package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// releaseAcceptanceSeam is the wiring the release seam test drives: real
// Postgres repositories, a real Orchestrator wired into the server exactly as
// newStageOrchestrator wires it (Runs, GitHub, Artifacts, Audit), and the
// deploy_trigger_test.go GitHub stub as both the server's and the
// orchestrator's forge client.
type releaseAcceptanceSeam struct {
	s         *Server
	orch      *orchestrator.Orchestrator
	runs      run.Repository
	audits    audit.Repository
	artifacts artifact.Repository
	gh        *deployTriggerGitHub
	specBytes []byte
	wf        spec.Workflow
}

func newReleaseAcceptanceSeam(t *testing.T) *releaseAcceptanceSeam {
	t.Helper()
	specPath := filepath.Join("..", "..", "..", "docs", "spec", "examples", "workflow-v2-release-acceptance.yaml")
	specBytes, err := os.ReadFile(specPath) //nolint:gosec // fixed in-repo test fixture path
	if err != nil {
		t.Fatalf("read committed release-acceptance example: %v", err)
	}
	parsed, err := spec.ParseBytes(specBytes)
	if err != nil {
		t.Fatalf("parse committed release-acceptance example: %v", err)
	}
	wf, ok := parsed.Workflows["release"]
	if !ok {
		t.Fatalf("committed example has no release workflow")
	}

	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	artifactRepo := artifact.NewPostgresRepository(pool)
	stub, gh := newDeployTriggerGitHub(t)
	orch := &orchestrator.Orchestrator{Runs: runRepo, GitHub: gh, Artifacts: artifactRepo, Audit: auditRepo}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      runRepo,
		ApprovalRepo: approval.NewPostgresRepository(pool),
		AuditRepo:    auditRepo,
		ArtifactRepo: artifactRepo,
		// SigningRepo is wired as production wires it so the deployment-record
		// handler (POST /v0/runs/{run_id}/deployment) is configured; the
		// rollback-offer seam persists its handle-bearing record through it.
		SigningRepo:  signing.NewPostgresRepository(pool),
		Orchestrator: orch,
		GitHub:       gh,
	})
	return &releaseAcceptanceSeam{
		s: s, orch: orch, runs: runRepo, audits: auditRepo, artifacts: artifactRepo,
		gh: stub, specBytes: specBytes, wf: wf,
	}
}

// startRelease creates a release run from the committed example, persists its
// stages through the PRODUCTION webhook.CreateStagesFromSpec mapping, lets the
// real orchestrator park the deploy stage at its pre-execution gate, and
// returns the run plus its deploy and acceptance stage rows.
func (f *releaseAcceptanceSeam) startRelease(t *testing.T) (*run.Run, *run.Stage, *run.Stage) {
	t.Helper()
	ctx := context.Background()
	installationID := int64(99)
	runRow, err := f.runs.CreateRun(ctx, run.CreateRunParams{
		Repo:           "kuhlman-labs/example",
		WorkflowID:     "release",
		WorkflowSHA:    "sha",
		TriggerSource:  run.TriggerCLI,
		InstallationID: &installationID,
		WorkflowSpec:   f.specBytes,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stages, err := webhook.CreateStagesFromSpec(ctx, f.runs, runRow.ID, f.wf.Stages)
	if err != nil {
		t.Fatalf("create stages from spec: %v", err)
	}
	var deploy, acceptance *run.Stage
	for _, st := range stages {
		switch st.Type {
		case run.StageTypeDeploy:
			deploy = st
		case run.StageTypeAcceptance:
			acceptance = st
		}
	}
	if deploy == nil || acceptance == nil || deploy.Sequence >= acceptance.Sequence {
		t.Fatalf("example must persist deploy before acceptance; got deploy=%v acceptance=%v", deploy, acceptance)
	}

	// The REAL orchestrator parks the deploy at its pre-execution approval
	// gate — this test does not place the gate by hand.
	if _, err := f.orch.Advance(ctx, runRow.ID); err != nil {
		t.Fatalf("orchestrator advance to the deploy gate: %v", err)
	}
	f.assertStage(t, deploy.ID, run.StageStateAwaitingDeployApproval, "deploy parked at its pre-execution gate")
	f.assertStage(t, acceptance.ID, run.StageStatePending, "acceptance before the deploy approval")
	return runRow, deploy, acceptance
}

// approveDeploy drives the deploy approval through the REAL HTTP approval
// handler — admission (count 1, not [author, agent]), the allowed_environments
// pre-flight via the --environment=staging comment flag, triggerDeploy, and
// finishApprovalAdvance's Orchestrator.Advance re-entry. A non-2xx answer fails
// the test before any stage-state assertion, so a refused approval can never
// make the hold assertions pass vacuously.
func (f *releaseAcceptanceSeam) approveDeploy(t *testing.T, deploy *run.Stage) {
	t.Helper()
	w := submitApproval(t, f.s, deploy.ID, `{"decision":"approve","comment":"ship it --environment=staging"}`)
	if w.Code < 200 || w.Code > 299 {
		t.Fatalf("deploy approval status = %d, want 2xx (the hold assertions below are only meaningful after a real approval):\n%s", w.Code, w.Body.String())
	}
	f.assertStage(t, deploy.ID, run.StageStateAwaitingDeployment, "deploy after approval (triggered, polling the delegate)")
}

func (f *releaseAcceptanceSeam) assertStage(t *testing.T, id uuid.UUID, want run.StageState, what string) {
	t.Helper()
	got, err := f.runs.GetStage(context.Background(), id)
	if err != nil {
		t.Fatalf("reload stage %s: %v", id, err)
	}
	if got.State != want {
		t.Fatalf("%s: stage state = %s, want %s", what, got.State, want)
	}
}

func (f *releaseAcceptanceSeam) acceptanceDispatchedRows(t *testing.T, runID uuid.UUID) int {
	t.Helper()
	entries, err := f.audits.ListForRunByCategory(context.Background(), runID, CategoryAcceptanceDispatched)
	if err != nil {
		t.Fatalf("list acceptance_dispatched rows: %v", err)
	}
	return len(entries)
}

func (f *releaseAcceptanceSeam) dispatchHits() int {
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	return f.gh.dispatchHits
}

// TestReleaseAcceptance_DeployThenAcceptance_EndToEnd_PgBacked is the
// cross-boundary seam test for E35.1 / #1598 (ADR-053). It drives the
// COMMITTED example docs/spec/examples/workflow-v2-release-acceptance.yaml —
// read from disk, so a drifted example reddens this test — through:
//
//	spec.ParseBytes
//	  → webhook.CreateStagesFromSpec      (stage rows in Postgres)
//	  → real Orchestrator.Advance         (deploy parks at awaiting_deploy_approval)
//	  → POST /v0/stages/{deploy}/approvals (admission + --environment pre-flight
//	                                       → triggerDeploy → awaiting_deployment
//	                                       → finishApprovalAdvance → Advance)
//	  → ResolveDeploymentFromPollState     (artifact + audit + stage terminal
//	                                       → Advance)
//
// and asserts FROM THE DATABASE that the acceptance stage stays pending with
// zero acceptance_dispatched rows while the deploy sits at awaiting_deployment,
// and is dispatched only after the deploy SUCCEEDED.
//
// Counterfactual (C4): replacing orchestrator.DeployAheadNotSucceeded's body
// with `return nil` reddens the post-approval assertion — finishApprovalAdvance's
// Advance re-entry dispatches the pending acceptance stage behind the in-flight
// deploy — proving the real approval path is the one exercised.
func TestReleaseAcceptance_DeployThenAcceptance_EndToEnd_PgBacked(t *testing.T) {
	ctx := context.Background()
	f := newReleaseAcceptanceSeam(t)
	runRow, deploy, acceptance := f.startRelease(t)

	f.approveDeploy(t, deploy)

	// THE HOLD: the deploy is in flight, so the acceptance stage behind it must
	// not have been dispatched by finishApprovalAdvance's Advance re-entry.
	f.assertStage(t, acceptance.ID, run.StageStatePending, "acceptance while the deploy is awaiting_deployment")
	if n := f.acceptanceDispatchedRows(t, runRow.ID); n != 0 {
		t.Fatalf("acceptance_dispatched rows while the deploy is in flight = %d, want 0", n)
	}
	if hits := f.dispatchHits(); hits != 1 {
		t.Fatalf("workflow_dispatch hits after the deploy approval = %d, want 1 (the deploy trigger only)", hits)
	}

	if err := f.s.ResolveDeploymentFromPollState(ctx, runRow.ID, deploy.ID, run.DeployOutcomeSucceeded, "main",
		&githubclient.WorkflowRun{ID: 777001, HTMLURL: "https://github.com/kuhlman-labs/example/actions/runs/777001",
			Status: "completed", Conclusion: "success", HeadSHA: releaseDeployedSHA}); err != nil {
		t.Fatalf("resolve deployment succeeded: %v", err)
	}

	f.assertStage(t, deploy.ID, run.StageStateSucceeded, "deploy after a succeeded resolution")
	arts, err := f.artifacts.ListForStage(ctx, deploy.ID)
	if err != nil {
		t.Fatalf("list deploy artifacts: %v", err)
	}
	var deployments int
	for _, a := range arts {
		if a.Kind == artifact.KindDeployment {
			deployments++
		}
	}
	if deployments != 1 {
		t.Fatalf("deployment artifacts on the deploy stage = %d, want 1", deployments)
	}

	got, err := f.runs.GetStage(ctx, acceptance.ID)
	if err != nil {
		t.Fatalf("reload acceptance stage: %v", err)
	}
	// The run is not locked to the local runner, so the orchestrator takes the
	// backend-triggered dispatch path and anchors it itself.
	if got.State != run.StageStateDispatched {
		t.Fatalf("acceptance stage state after the deploy succeeded = %s, want dispatched", got.State)
	}
	if n := f.acceptanceDispatchedRows(t, runRow.ID); n != 1 {
		t.Fatalf("acceptance_dispatched rows after the deploy succeeded = %d, want 1", n)
	}
	if hits := f.dispatchHits(); hits != 2 {
		t.Fatalf("workflow_dispatch hits after the deploy succeeded = %d, want 2 (deploy + acceptance)", hits)
	}
	finalRun, err := f.runs.GetRun(ctx, runRow.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if finalRun.State != run.StateRunning {
		t.Fatalf("run state = %s, want running (the acceptance stage is now in flight)", finalRun.State)
	}

	// DEPLOYED-TARGET IDENTITY (E35.2 / #1599). The reconciler's git_ref is the
	// symbolic "main"; the polled workflow run's head_sha is the fixture's ONLY
	// full-SHA source, so the expectation below can only come from the stored
	// artifact's sha (counterfactual C10) through the release arm (C1, C6). A
	// release run writes no reported-head entry at all, so without the arm both
	// resolve empty.
	f.assertDeployedIdentity(t, runRow.ID, deploy.ID, acceptance.ID, releaseDeployedSHA)
}

// releaseDeployedSHA is the head_sha of the polled delegate workflow run in the
// release seam — the build the deploy put on the staging host.
const releaseDeployedSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// assertDeployedIdentity reads the deploy stage's deployment artifact back
// from Postgres and asserts its stored sha, the acceptance expected head the
// prompt handlers serve, and the head a shipped verdict binds to — all equal
// to want ("" means the release arm must fail closed on every surface).
func (f *releaseAcceptanceSeam) assertDeployedIdentity(t *testing.T, runID, deployID, acceptanceID uuid.UUID, want string) {
	t.Helper()
	ctx := context.Background()
	arts, err := f.artifacts.ListForStage(ctx, deployID)
	if err != nil {
		t.Fatalf("list deploy artifacts: %v", err)
	}
	var stored struct {
		SHA *string `json:"sha"`
	}
	for _, a := range arts {
		if a.Kind == artifact.KindDeployment {
			if err := json.Unmarshal(a.Content, &stored); err != nil {
				t.Fatalf("decode stored deployment artifact: %v", err)
			}
		}
	}
	switch {
	case want == "" && stored.SHA != nil:
		t.Errorf("stored deployment artifact carries sha %q, want no sha key", *stored.SHA)
	case want != "" && (stored.SHA == nil || *stored.SHA != want):
		t.Errorf("stored deployment artifact sha = %v, want %q", stored.SHA, want)
	}
	if got := f.s.resolveAcceptanceExpectedHeadSHA(ctx, runID, acceptanceID); got != want {
		t.Errorf("resolveAcceptanceExpectedHeadSHA = %q, want %q", got, want)
	}
	sha, ok := f.s.acceptanceValidatedHeadSHA(ctx, runID, acceptanceID)
	if sha != want || ok != (want != "") {
		t.Errorf("acceptanceValidatedHeadSHA = (%q, %v), want (%q, %v)", sha, ok, want, want != "")
	}
}

// TestReleaseAcceptance_SymbolicRefOnly_FailsClosed_PgBacked is the fail-closed
// leg of the deployed-target identity seam: the delegate workflow run reports no
// head_sha, so the stored deployment carries only the symbolic ref "main". The
// acceptance stage still dispatches (the deploy succeeded), but both the served
// expectation and the verdict binding resolve empty — never a guess.
func TestReleaseAcceptance_SymbolicRefOnly_FailsClosed_PgBacked(t *testing.T) {
	ctx := context.Background()
	f := newReleaseAcceptanceSeam(t)
	runRow, deploy, acceptance := f.startRelease(t)
	f.approveDeploy(t, deploy)
	if err := f.s.ResolveDeploymentFromPollState(ctx, runRow.ID, deploy.ID, run.DeployOutcomeSucceeded, "main",
		&githubclient.WorkflowRun{ID: 777003, HTMLURL: "https://github.com/kuhlman-labs/example/actions/runs/777003",
			Status: "completed", Conclusion: "success"}); err != nil {
		t.Fatalf("resolve deployment succeeded: %v", err)
	}
	if n := f.acceptanceDispatchedRows(t, runRow.ID); n != 1 {
		t.Fatalf("acceptance_dispatched rows after the deploy succeeded = %d, want 1", n)
	}
	f.assertDeployedIdentity(t, runRow.ID, deploy.ID, acceptance.ID, "")
}

// TestReleaseAcceptance_DeployFailed_NeverDispatchesAcceptance_PgBacked is the
// failed-deploy leg: the same real approval path, then a FAILED resolution. The
// run must end failed and the acceptance stage must never be dispatched.
func TestReleaseAcceptance_DeployFailed_NeverDispatchesAcceptance_PgBacked(t *testing.T) {
	ctx := context.Background()
	f := newReleaseAcceptanceSeam(t)
	runRow, deploy, acceptance := f.startRelease(t)

	f.approveDeploy(t, deploy)
	f.assertStage(t, acceptance.ID, run.StageStatePending, "acceptance while the deploy is awaiting_deployment")

	if err := f.s.ResolveDeploymentFromPollState(ctx, runRow.ID, deploy.ID, run.DeployOutcomeFailed, "main",
		&githubclient.WorkflowRun{ID: 777002, Status: "completed", Conclusion: "failure"}); err != nil {
		t.Fatalf("resolve deployment failed: %v", err)
	}

	f.assertStage(t, deploy.ID, run.StageStateFailed, "deploy after a failed resolution")
	finalRun, err := f.runs.GetRun(ctx, runRow.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if finalRun.State != run.StateFailed {
		t.Fatalf("run state = %s, want failed (a failed deploy fails the run)", finalRun.State)
	}
	got, err := f.runs.GetStage(ctx, acceptance.ID)
	if err != nil {
		t.Fatalf("reload acceptance stage: %v", err)
	}
	if got.State == run.StageStateDispatched || got.State == run.StageStateAwaitingHostDispatch ||
		got.State == run.StageStateRunning || got.State == run.StageStateSucceeded {
		t.Fatalf("acceptance stage state = %s; it must never dispatch behind a failed deploy", got.State)
	}
	if n := f.acceptanceDispatchedRows(t, runRow.ID); n != 0 {
		t.Fatalf("acceptance_dispatched rows = %d, want 0", n)
	}
	if hits := f.dispatchHits(); hits != 1 {
		t.Fatalf("workflow_dispatch hits = %d, want 1 (the deploy trigger only)", hits)
	}
}

// shipDeploymentAsOperator POSTs body through the PRODUCTION deployment-record
// handler (POST /v0/runs/{run_id}/deployment) under an operator bearer carrying
// write:runs + write:deploy, failing the test on anything but 201.
func (f *releaseAcceptanceSeam) shipDeploymentAsOperator(t *testing.T, runID, deployID uuid.UUID, body deploymentBody) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal deployment body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/v0/runs/%s/deployment?stage_id=%s", runID, deployID), bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("run_id", runID.String())
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, Identity{
		Subject: "operator:release-seam", TokenID: "tok-release-seam", Scopes: []string{"write:runs", "write:deploy"},
	}))
	w := httptest.NewRecorder()
	f.s.handleShipDeployment(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("ship deployment status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
}

// chainPayloads reads every category row for runID back from Postgres,
// decoded, oldest first.
func (f *releaseAcceptanceSeam) chainPayloads(t *testing.T, runID uuid.UUID, category string) []map[string]any {
	t.Helper()
	entries, err := f.audits.ListForRunByCategory(context.Background(), runID, category)
	if err != nil {
		t.Fatalf("list %s rows: %v", category, err)
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		var p map[string]any
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s payload: %v\n%s", category, err, e.Payload)
		}
		out = append(out, p)
	}
	return out
}

// TestReleaseAcceptance_FailedVerdictOffersRollbackWithStoredHandle_PgBacked is
// the cross-boundary seam for E35.3 / #1600 (C13). On real Postgres and the
// committed release example it drives the deploy to succeeded through the real
// approval + reconciler path, persists a handle-bearing deployment record
// through the PRODUCTION POST /v0/runs/{run_id}/deployment handler (NEWER than
// the reconciler's handle-less record), settles the acceptance stage, and runs
// triageAcceptanceFailure on a failed failure_mode=error verdict. It asserts
// FROM THE AUDIT CHAIN that triage recorded rollback_offered carrying the
// stored handle and NEVER dispatched (the GitHub stub's count and the
// deployment_rollback_initiated rows are unchanged), then fires the rollback
// through the real POST /v0/runs/{run_id}/deployment/rollback handler and
// asserts the stored handle reached the workflow_dispatch inputs and the
// chained deployment_rollback_initiated row.
//
// It crosses persistence (artifact + audit), triage, the rollback HTTP handler
// and the GitHub dispatch payload. Counterfactuals: deleting triage's
// decidePostDeployRollback call site records fixup_unavailable_paged (no
// offer); deleting the rollback dispatch's handle insertion leaves
// fishhawk_rollback_handle out of the dispatch inputs.
func TestReleaseAcceptance_FailedVerdictOffersRollbackWithStoredHandle_PgBacked(t *testing.T) {
	ctx := context.Background()
	f := newReleaseAcceptanceSeam(t)
	runRow, deploy, acceptance := f.startRelease(t)
	f.approveDeploy(t, deploy)
	if err := f.s.ResolveDeploymentFromPollState(ctx, runRow.ID, deploy.ID, run.DeployOutcomeSucceeded, "main",
		&githubclient.WorkflowRun{ID: 777004, HTMLURL: "https://github.com/kuhlman-labs/example/actions/runs/777004",
			Status: "completed", Conclusion: "success", HeadSHA: releaseDeployedSHA}); err != nil {
		t.Fatalf("resolve deployment succeeded: %v", err)
	}
	f.assertStage(t, deploy.ID, run.StageStateSucceeded, "deploy after a succeeded resolution")
	f.assertStage(t, acceptance.ID, run.StageStateDispatched, "acceptance once the deploy succeeded")

	// The pipeline's own callback carries the rollback_handle.
	f.shipDeploymentAsOperator(t, runRow.ID, deploy.ID, deploymentBody{
		Environment: "staging", Ref: "main", ExternalRunURL: "https://github.com/kuhlman-labs/example/actions/runs/777004",
		Outcome: string(run.DeployOutcomeSucceeded), RollbackHandle: "rev-abc",
	})
	stored, err := f.s.storedRollbackHandleFor(ctx, deploy.ID)
	if err != nil || stored.Handle != "rev-abc" {
		t.Fatalf("storedRollbackHandleFor = (%+v, %v), want handle rev-abc from the shipped record", stored, err)
	}

	// Settle the acceptance stage the way a finished runner would.
	for _, to := range []run.StageState{run.StageStateRunning, run.StageStateSucceeded} {
		if _, err := f.runs.TransitionStage(ctx, acceptance.ID, to, nil); err != nil {
			t.Fatalf("transition acceptance to %s: %v", to, err)
		}
	}
	settled, err := f.runs.GetStage(ctx, acceptance.ID)
	if err != nil {
		t.Fatalf("reload acceptance: %v", err)
	}

	hitsBeforeTriage := f.dispatchHits()
	disposition := f.s.triageAcceptanceFailure(ctx, runRow.ID, settled,
		acceptanceBody{Verdict: "failed", FailureMode: acceptanceFailureError}, uuid.NewString())
	if disposition != acceptanceDispositionRollbackOffered {
		t.Fatalf("triage disposition = %q, want rollback_offered", disposition)
	}

	triage := f.chainPayloads(t, runRow.ID, CategoryAcceptanceTriageDecided)
	if len(triage) != 1 {
		t.Fatalf("acceptance_triage_decided rows = %d, want 1", len(triage))
	}
	if triage[0]["disposition"] != acceptanceDispositionRollbackOffered || triage[0]["class"] != acceptanceClass1 {
		t.Errorf("triage row = %v, want class 1 / rollback_offered", triage[0])
	}
	offer, _ := triage[0]["rollback_offer"].(map[string]any)
	if offer["rollback_handle"] != "rev-abc" || offer["deploy_stage_id"] != deploy.ID.String() ||
		offer["deployment_artifact_id"] != stored.ArtifactID.String() {
		t.Errorf("rollback_offer = %v, want {rollback_handle:rev-abc deploy_stage_id:%s deployment_artifact_id:%s}",
			offer, deploy.ID, stored.ArtifactID)
	}
	if hits := f.dispatchHits(); hits != hitsBeforeTriage {
		t.Fatalf("workflow_dispatch hits after triage = %d, want %d — the offer must never auto-fire", hits, hitsBeforeTriage)
	}
	if rows := f.chainPayloads(t, runRow.ID, CategoryDeploymentRollbackInitiated); len(rows) != 0 {
		t.Fatalf("deployment_rollback_initiated rows after triage = %d, want 0", len(rows))
	}
	f.assertStage(t, deploy.ID, run.StageStateSucceeded, "deploy after the offer (no transition)")

	// The operator fires the offered rollback through the real handler.
	w := rollbackRequest(t, f.s, runRow.ID, nil)
	if w.Code != http.StatusAccepted {
		t.Fatalf("rollback status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
	if hits := f.dispatchHits(); hits != hitsBeforeTriage+1 {
		t.Errorf("workflow_dispatch hits after the rollback = %d, want %d", hits, hitsBeforeTriage+1)
	}
	f.gh.mu.Lock()
	gotHandle := f.gh.dispatchInputs[rollbackHandleDispatchInput]
	f.gh.mu.Unlock()
	if gotHandle != "rev-abc" {
		t.Errorf("rollback dispatch input %s = %q, want rev-abc", rollbackHandleDispatchInput, gotHandle)
	}
	initiated := f.chainPayloads(t, runRow.ID, CategoryDeploymentRollbackInitiated)
	if len(initiated) != 1 {
		t.Fatalf("deployment_rollback_initiated rows = %d, want 1", len(initiated))
	}
	if initiated[0]["rollback_handle"] != "rev-abc" || initiated[0]["deployment_artifact_id"] != stored.ArtifactID.String() {
		t.Errorf("deployment_rollback_initiated = %v, want rollback_handle rev-abc + deployment_artifact_id %s",
			initiated[0], stored.ArtifactID)
	}
}
