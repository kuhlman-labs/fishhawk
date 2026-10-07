package server

import (
	"context"
	"encoding/json"
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
