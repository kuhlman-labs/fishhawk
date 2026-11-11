package mcpe2e_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// TestE2E_CIFailed_ObserverToDerivedStatusToNextActions drives the
// product-detected CI-failure seam (#1045) end-to-end across the four
// layers the per-layer units cannot exercise together: the drive
// observer's audit write → GET /v0/runs/{id} derived_status read model →
// the MCP next_actions classifier.
//
// A drive-enabled run is parked at its review gate with a red required
// StageCheck. ObserveParkedReviewForDrive (the mergereconciler-invoked
// observer, called directly here as server_test does) stamps the
// ci_failed run_auto_advanced entry. GET /v0/runs/{id} then surfaces
// derived_status "ci_failed", and the MCP get_run_status next_actions
// block classifies ci_failed_unroutable (no open concerns) naming the
// commit_and_vouch operator-remediation arm (#1044). This is the
// layer-crossing test #618 mandates: a per-layer unit passes while the
// seam (a derived_status literal not matching the classifier switch)
// breaks.
func TestE2E_CIFailed_ObserverToDerivedStatusToNextActions(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Stand up a backend over the SAME pool with the stage-check repo
	// wired (newFixture's server omits it) so the observer can read the
	// red required check. The operator fhk_* token authenticates against
	// the same apitoken rows.
	auditRepo := audit.NewPostgresRepository(fx.pool)
	stageCheckRepo := stagecheck.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        fx.runRepo,
		AuditRepo:      auditRepo,
		SigningRepo:    signing.NewPostgresRepository(fx.pool),
		APITokenRepo:   fx.apitokenRepo,
		StageCheckRepo: stageCheckRepo,
		// The PRODUCTION shape: a deployment with the github_projects feedback
		// provider registered, so GET /v0/runs/{id} advertises a non-empty
		// capabilities block and next_actions offers the real tool (#3628).
		//
		// Injected per-SERVER rather than via workmgmt.RegisterFeedback: the
		// feedback registry is a process-global with no Unregister, so a
		// save/restore cannot give the mirror-image case below a deterministic
		// EMPTY registry and the two walks would depend on test order.
		FeedbackProviders: func() []string { return []string{"github_projects"} },
	})

	const requiredCheck = "ci/required"
	const prURL = "https://github.com/kuhlman-labs/fishhawk/pull/4545"

	// A drive-enabled run with a required-checks snapshot and an open PR.
	// No WorkflowSpec → zero implement reviewers configured, so the review
	// round is vacuously terminal and the observer reaches the checks gate.
	run, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:                   "kuhlman-labs/fishhawk",
		WorkflowID:             "feature_change",
		WorkflowSHA:            "deadbeef",
		TriggerSource:          runpkg.TriggerCLI,
		Drive:                  true,
		RequiredChecksSnapshot: &runpkg.RequiredChecksSnapshot{Contexts: []string{requiredCheck}},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, run.ID, prURL); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	if _, err := fx.runRepo.TransitionRun(ctx, run.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}

	// A review stage parked at its approval gate (the observer's gate point).
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            run.ID,
		Sequence:         1,
		Type:             runpkg.StageTypeReview,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(review): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, stage.ID)

	// The required check concluded red on the review stage.
	failure := "failure"
	if _, err := stageCheckRepo.Append(ctx, stagecheck.AppendParams{
		StageID:    stage.ID,
		Name:       requiredCheck,
		Status:     "completed",
		Conclusion: &failure,
		HeadSHA:    "cafebabe",
		Timestamp:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Append red stage check: %v", err)
	}

	// Drive the observer one tick — it stamps the ci_failed entry.
	parked, err := fx.runRepo.GetStage(ctx, stage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	srv.ObserveParkedReviewForDrive(ctx, parked, prURL)

	// Layer 2 + 3: the MCP get_run_status surfaces derived_status ci_failed.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, mountServer(t, srv))
	if got := getDerivedStatus(t, ctx, session, run.ID); got != "ci_failed" {
		t.Fatalf("drive_status.derived_status = %q, want ci_failed", got)
	}

	// Layer 3b (#3628): the capability block reaches the MCP mirror through the
	// real HTTP read, PRESENT and non-empty — the production shape the filing
	// suggestion below depends on.
	if present, ids := runCapabilitiesView(t, ctx, session, run.ID); !present || len(ids) == 0 {
		t.Fatalf("run.capabilities = (present=%v, ids=%v), want a PRESENT non-empty block "+
			"(json tag drift between the backend and the MCP mirror?)", present, ids)
	}

	// Layer 4: the next_actions classifier names the legal remediation arm.
	na := getNextActions(t, ctx, session, run.ID)
	if na == nil {
		t.Fatal("next_actions absent on the ci_failed run")
	}
	if na.State != "ci_failed_unroutable" {
		t.Fatalf("next_actions.state = %q, want ci_failed_unroutable", na.State)
	}
	// The drive distilled next_action (classify_ci_failure) folds in first,
	// then the classifier's commit_and_vouch operator-remediation arm
	// (#1044) — the legal move with no open concerns to route back.
	if len(na.Actions) == 0 || na.Actions[0].Action != "classify_ci_failure" {
		t.Fatalf("next_actions.actions[0] = %+v, want the drive classify_ci_failure folded first", na.Actions)
	}
	var sawCommitVouch bool
	for _, a := range na.Actions {
		if a.Action == "commit_and_vouch" {
			sawCommitVouch = true
		}
	}
	if !sawCommitVouch {
		t.Fatalf("next_actions.actions = %+v, want commit_and_vouch present (#1044 operator remediation)", na.Actions)
	}

	// Layer 4b (E32.11 / #1737): the SAME end-to-end walk proves the
	// product-directed filing suggestion reaches an operator through the REAL
	// MCP surface, not just the pure classifier — a red required check on an
	// otherwise gate-green run is the shape the issue names. It is appended
	// LAST (the remediation move still leads) and pre-populated with THIS run's
	// id plus kind=bug, so the operator accepts a suggestion rather than
	// hand-assembling the call.
	last := na.Actions[len(na.Actions)-1]
	if last.Action != "fishhawk_report_product_issue" {
		t.Fatalf("next_actions.actions = %+v, want fishhawk_report_product_issue LAST (#1737)", na.Actions)
	}
	if last.Params["run_id"] != run.ID.String() {
		t.Errorf("filing run_id = %q, want this run's id %q", last.Params["run_id"], run.ID)
	}
	if last.Params["kind"] != "bug" {
		t.Errorf("filing kind = %q, want bug", last.Params["kind"])
	}
	if last.Consumes != "none" {
		t.Errorf("filing consumes = %q, want none — filing is operator-gated and spends no budget", last.Consumes)
	}
	// Operator-gated, never a default recommendation.
	if !strings.Contains(last.Precondition, "OPERATOR JUDGEMENT") {
		t.Errorf("filing precondition = %q, want the operator-gated wording", last.Precondition)
	}
}

// TestE2E_CIFailed_NoFeedbackProvider_SubstitutesManualFiling is the
// mirror image of the walk above, and the CROSS-BOUNDARY test for #3628: the
// SAME end-to-end path — server -> HTTP GET /v0/runs/{id} -> the MCP Run wire
// mirror -> next_actions — on a deployment that registered NO feedback
// provider.
//
// It is the only test that spans all three layers at once, so it is what fails
// when the per-layer units are all green but the seam is broken: a json-tag
// drift between runCapabilities and RunCapabilities, or a handleGetRun that
// never populates the block, both decode to a nil block here and the walk
// reverts to recommending the tool.
//
// Expectation: the capabilities block is PRESENT and EMPTY, and the last action
// is file_product_issue_manually — never fishhawk_report_product_issue, which
// would collect the bundle and then refuse with 501 provider_unimplemented.
func TestE2E_CIFailed_NoFeedbackProvider_SubstitutesManualFiling(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const requiredCheck = "ci/required"
	const prURL = "https://github.com/kuhlman-labs/fishhawk/pull/4546"

	auditRepo := audit.NewPostgresRepository(fx.pool)
	stageCheckRepo := stagecheck.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        fx.runRepo,
		AuditRepo:      auditRepo,
		SigningRepo:    signing.NewPostgresRepository(fx.pool),
		APITokenRepo:   fx.apitokenRepo,
		StageCheckRepo: stageCheckRepo,
		// Deterministically EMPTY for THIS server only — see the sibling walk.
		FeedbackProviders: func() []string { return nil },
	})

	run, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:                   "kuhlman-labs/fishhawk",
		WorkflowID:             "feature_change",
		WorkflowSHA:            "deadbeef",
		TriggerSource:          runpkg.TriggerCLI,
		Drive:                  true,
		RequiredChecksSnapshot: &runpkg.RequiredChecksSnapshot{Contexts: []string{requiredCheck}},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, run.ID, prURL); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	if _, err := fx.runRepo.TransitionRun(ctx, run.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            run.ID,
		Sequence:         1,
		Type:             runpkg.StageTypeReview,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(review): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, stage.ID)

	failure := "failure"
	if _, err := stageCheckRepo.Append(ctx, stagecheck.AppendParams{
		StageID:    stage.ID,
		Name:       requiredCheck,
		Status:     "completed",
		Conclusion: &failure,
		HeadSHA:    "cafebabe",
		Timestamp:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Append red stage check: %v", err)
	}
	parked, err := fx.runRepo.GetStage(ctx, stage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	srv.ObserveParkedReviewForDrive(ctx, parked, prURL)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, mountServer(t, srv))
	// PRESENT and EMPTY: the positive "no provider registered" fact. An ABSENT
	// block would be UNDECIDABLE and the gate below would (correctly) fail open,
	// so this assertion is what distinguishes a working seam from a broken one.
	present, ids := runCapabilitiesView(t, ctx, session, run.ID)
	if !present {
		t.Fatal("run.capabilities absent — an empty provider list must still reach the wire " +
			"as a PRESENT block (a nil slice or an accidental omitempty collapses it)")
	}
	if len(ids) != 0 {
		t.Fatalf("run.capabilities.product_feedback_providers = %v, want empty", ids)
	}

	na := getNextActions(t, ctx, session, run.ID)
	if na == nil {
		t.Fatal("next_actions absent on the ci_failed run")
	}
	if na.State != "ci_failed_unroutable" {
		t.Fatalf("next_actions.state = %q, want ci_failed_unroutable", na.State)
	}
	if len(na.Actions) == 0 {
		t.Fatal("the capability gate must SUBSTITUTE, never leave the run action-less")
	}
	for _, a := range na.Actions {
		if a.Action == "fishhawk_report_product_issue" {
			t.Fatalf("next_actions offered fishhawk_report_product_issue on a deployment with "+
				"no feedback provider — it would 501 after assembling the report: %+v", na.Actions)
		}
	}
	last := na.Actions[len(na.Actions)-1]
	if last.Action != "file_product_issue_manually" {
		t.Fatalf("next_actions.actions = %+v, want file_product_issue_manually LAST (#3628)", na.Actions)
	}
	if last.Consumes != "none" {
		t.Errorf("manual step consumes = %q, want none", last.Consumes)
	}
	if !strings.Contains(last.Reason, "NO feedback provider registered") {
		t.Errorf("manual step reason must name the gap: %q", last.Reason)
	}
}

// runCapabilitiesView reads the run's deployment-capability block off the SAME
// fishhawk_get_run_status call next_actions rides on (#3628). present is false
// when the block is absent (the UNDECIDABLE state), which is what a json-tag
// drift between the backend runCapabilities and the MCP RunCapabilities mirror
// looks like from here.
func runCapabilitiesView(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) (present bool, ids []string) {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_run_status",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if result.IsError {
		t.Fatalf("get_run_status tool returned error: %s", toolContentString(t, result))
	}
	var out struct {
		Run struct {
			Capabilities *struct {
				ProductFeedbackProviders []string `json:"product_feedback_providers"`
			} `json:"capabilities"`
		} `json:"run"`
	}
	decodeStructured(t, result, &out)
	if out.Run.Capabilities == nil {
		return false, nil
	}
	return true, out.Run.Capabilities.ProductFeedbackProviders
}

// recoverableRun stands up a backend over the fixture pool with the
// stage-check repo wired, a drive-enabled run with the given required-check
// contexts, an open PR, and a review stage parked at its approval gate — the
// shared #3414 recovery scaffold. It returns the server, the run, the parked
// review stage, and the wired stage-check + audit repositories.
func recoverableRun(t *testing.T, ctx context.Context, fx *e2eFixture, contexts []string, prURL string) (*server.Server, *runpkg.Run, *runpkg.Stage, stagecheck.Repository, audit.Repository) {
	t.Helper()
	auditRepo := audit.NewPostgresRepository(fx.pool)
	stageCheckRepo := stagecheck.NewPostgresRepository(fx.pool)
	srv := server.New(server.Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        fx.runRepo,
		AuditRepo:      auditRepo,
		SigningRepo:    signing.NewPostgresRepository(fx.pool),
		APITokenRepo:   fx.apitokenRepo,
		StageCheckRepo: stageCheckRepo,
	})
	run, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:                   "kuhlman-labs/fishhawk",
		WorkflowID:             "feature_change",
		WorkflowSHA:            "deadbeef",
		TriggerSource:          runpkg.TriggerCLI,
		Drive:                  true,
		RequiredChecksSnapshot: &runpkg.RequiredChecksSnapshot{Contexts: contexts},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, run.ID, prURL); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	if _, err := fx.runRepo.TransitionRun(ctx, run.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}
	stage, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:            run.ID,
		Sequence:         1,
		Type:             runpkg.StageTypeReview,
		ExecutorKind:     runpkg.ExecutorAgent,
		ExecutorRef:      "fishhawk/runner@v1",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("CreateStage(review): %v", err)
	}
	parkAtGate(t, ctx, fx.runRepo, stage.ID)
	return srv, run, stage, stageCheckRepo, auditRepo
}

// appendCheckRow appends one stage_checks row for the review stage.
func appendCheckRow(t *testing.T, ctx context.Context, repo stagecheck.Repository, stageID uuid.UUID, name, status, conclusion, headSHA string, ts time.Time) {
	t.Helper()
	p := stagecheck.AppendParams{
		StageID:   stageID,
		Name:      name,
		Status:    status,
		HeadSHA:   headSHA,
		Timestamp: ts,
	}
	if conclusion != "" {
		p.Conclusion = &conclusion
	}
	if _, err := repo.Append(ctx, p); err != nil {
		t.Fatalf("Append stage check %q/%s: %v", name, conclusion, err)
	}
}

// observeTick re-reads the parked stage and drives one observer tick.
func observeTick(t *testing.T, ctx context.Context, srv *server.Server, repo runpkg.Repository, stageID uuid.UUID, prURL string) {
	t.Helper()
	parked, err := repo.GetStage(ctx, stageID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	srv.ObserveParkedReviewForDrive(ctx, parked, prURL)
}

// TestE2E_CIFailed_RecoversWhenNewerHeadGreen drives the #3414 awaiting_merge
// RE-ASSERT path end to end across the real postgres stage-check repository,
// in three phases so the checks_green_awaiting_merge carve-out is genuinely
// load-bearing (binding condition 1): the run FIRST reaches awaiting_merge
// (recording the checks_green stamp), THEN a newer head's `failure` re-parks
// it ci_failed, THEN a still-newer `success` outranks the red (ts-DESC via
// GetStageCheckLatest) and the observer must RE-ASSERT awaiting_merge although
// it was ever-recorded — the exact re-assert the plain-Recorded guard cannot
// do. Reverting the guard to plain Recorded leaves derived_status stuck at
// ci_failed here, so this is an attainable counterfactual for control (d).
func TestE2E_CIFailed_RecoversWhenNewerHeadGreen(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const requiredCheck = "ci/required"
	const prURL = "https://github.com/kuhlman-labs/fishhawk/pull/4546"
	srv, run, stage, stageCheckRepo, _ := recoverableRun(t, ctx, fx, []string{requiredCheck}, prURL)
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, mountServer(t, srv))

	t0 := time.Now().UTC().Add(-3 * time.Minute)
	// Phase 1: the check is green on the first head → the run reaches
	// awaiting_merge, RECORDING checks_green_awaiting_merge for this stage.
	appendCheckRow(t, ctx, stageCheckRepo, stage.ID, requiredCheck, "completed", "success", "cafebabe", t0)
	observeTick(t, ctx, srv, fx.runRepo, stage.ID, prURL)
	if got := getDerivedStatus(t, ctx, session, run.ID); got != "awaiting_merge" {
		t.Fatalf("phase 1 derived_status = %q, want awaiting_merge", got)
	}

	// Phase 2: a newer head fails → the run re-parks ci_failed (the LatestRuleIs
	// ci_failed guard stamps because the latest entry is checks_green, not ci_failed).
	appendCheckRow(t, ctx, stageCheckRepo, stage.ID, requiredCheck, "completed", "failure", "deadbee2", t0.Add(1*time.Minute))
	observeTick(t, ctx, srv, fx.runRepo, stage.ID, prURL)
	if got := getDerivedStatus(t, ctx, session, run.ID); got != "ci_failed" {
		t.Fatalf("phase 2 derived_status = %q, want ci_failed", got)
	}
	if na := getNextActions(t, ctx, session, run.ID); na == nil || na.State != "ci_failed_unroutable" {
		t.Fatalf("phase 2 next_actions.state = %+v, want ci_failed_unroutable", na)
	}

	// Phase 3: a still-newer head's success supersedes the red → the observer
	// must RE-ASSERT awaiting_merge although checks_green was ever-recorded.
	appendCheckRow(t, ctx, stageCheckRepo, stage.ID, requiredCheck, "completed", "success", "beadfeed", t0.Add(2*time.Minute))
	observeTick(t, ctx, srv, fx.runRepo, stage.ID, prURL)
	if got := getDerivedStatus(t, ctx, session, run.ID); got != "awaiting_merge" {
		t.Fatalf("phase 3 derived_status = %q, want awaiting_merge (must re-assert after the ci_failed park went green)", got)
	}
	na := getNextActions(t, ctx, session, run.ID)
	if na == nil {
		t.Fatal("next_actions absent after recovery")
	}
	if strings.HasPrefix(na.State, "ci_failed") {
		t.Fatalf("phase 3 next_actions.state = %q, want a non-ci_failed_* arm after recovery", na.State)
	}
	// The awaiting_merge arm folds the merge ritual (fishhawk_merge_run) first —
	// the run left the ci_failed dead end and is now merge-eligible.
	if len(na.Actions) == 0 || na.Actions[0].Action != "fishhawk_merge_run" {
		t.Fatalf("phase 3 next_actions.actions[0] = %+v, want the drive merge ritual folded first", na.Actions)
	}
}

// TestE2E_CancelledRequiredCheck_NeverParksCIFailed drives control 1 e2e: a
// single `cancelled` required check (the ci.yml cancel-in-progress outcome)
// carries no red verdict at the real DeriveState path, so the run never
// parks ci_failed — derived_status stays empty and next_actions is not a
// ci_failed_* arm.
func TestE2E_CancelledRequiredCheck_NeverParksCIFailed(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const requiredCheck = "ci/required"
	const prURL = "https://github.com/kuhlman-labs/fishhawk/pull/4547"
	srv, run, stage, stageCheckRepo, _ := recoverableRun(t, ctx, fx, []string{requiredCheck}, prURL)

	appendCheckRow(t, ctx, stageCheckRepo, stage.ID, requiredCheck, "completed", "cancelled", "cafebabe", time.Now().UTC())
	observeTick(t, ctx, srv, fx.runRepo, stage.ID, prURL)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, mountServer(t, srv))
	if got := getDerivedStatus(t, ctx, session, run.ID); got != "" {
		t.Fatalf("derived_status on a cancelled check = %q, want empty (never parks ci_failed)", got)
	}
	// The next_actions surface must be PRESENT and carry a routed, non-empty
	// state that is not a ci_failed_* arm — a nil block (the surface vanishing)
	// no longer satisfies this, so it cannot vacuously pass (#3414 fix-up: the
	// prior `na != nil &&` short-circuit permitted disappearance).
	na := getNextActions(t, ctx, session, run.ID)
	if na == nil {
		t.Fatal("next_actions absent on a cancelled check — recovery must EXPOSE a routed state, not remove the surface")
	}
	if na.State == "" {
		t.Fatal("next_actions.state empty on a cancelled check — want a present, routed state")
	}
	if strings.HasPrefix(na.State, "ci_failed") {
		t.Fatalf("next_actions.state = %q, want a non-ci_failed_* arm on a cancelled check", na.State)
	}
}

// TestE2E_CIRecovered_PersistsAcrossRealRepositories is binding approval
// CONDITION 2: the ci_recovered rule value must cross the real persistence
// boundary. A run whose latest auto-advance is ci_failed, with one required
// check superseded (cancelled) and another still pending, observed through
// the real postgres repositories, must PERSIST a ci_recovered entry, drop
// derived_status off ci_failed, and leave the ci_failed_unroutable arm.
func TestE2E_CIRecovered_PersistsAcrossRealRepositories(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	const checkA = "ci/required"
	const checkB = "fishhawk_audit_complete"
	const prURL = "https://github.com/kuhlman-labs/fishhawk/pull/4548"
	srv, run, stage, stageCheckRepo, auditRepo := recoverableRun(t, ctx, fx, []string{checkA, checkB}, prURL)

	t0 := time.Now().UTC().Add(-3 * time.Minute)
	// 1. checkA fails on the first head → the run parks ci_failed. checkB is
	// pending (in_progress) so the checks are not green.
	appendCheckRow(t, ctx, stageCheckRepo, stage.ID, checkA, "completed", "failure", "cafebabe", t0)
	appendCheckRow(t, ctx, stageCheckRepo, stage.ID, checkB, "in_progress", "", "cafebabe", t0)
	observeTick(t, ctx, srv, fx.runRepo, stage.ID, prURL)

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, mountServer(t, srv))
	if got := getDerivedStatus(t, ctx, session, run.ID); got != "ci_failed" {
		t.Fatalf("derived_status after failure = %q, want ci_failed", got)
	}

	// 2. A newer head supersedes checkA's red (cancelled); checkB is still
	// pending → no red verdict, latest entry is ci_failed → ci_recovered.
	appendCheckRow(t, ctx, stageCheckRepo, stage.ID, checkA, "completed", "cancelled", "deadbee2", t0.Add(1*time.Minute))
	appendCheckRow(t, ctx, stageCheckRepo, stage.ID, checkB, "in_progress", "", "deadbee2", t0.Add(1*time.Minute))
	observeTick(t, ctx, srv, fx.runRepo, stage.ID, prURL)

	// The ci_recovered entry is PERSISTED and read back through the real repo.
	entries, err := auditRepo.ListForRunByCategory(ctx, run.ID, drive.Category)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	recovered := 0
	for _, e := range entries {
		var adv drive.Advance
		if err := json.Unmarshal(e.Payload, &adv); err != nil {
			continue
		}
		if adv.Rule == drive.RuleCIRecovered {
			recovered++
			if adv.From != "ci_failed" || adv.NextAction == nil || adv.NextAction.Action != "await_checks" {
				t.Errorf("persisted ci_recovered = %+v, want From ci_failed / await_checks", adv)
			}
		}
	}
	if recovered != 1 {
		t.Fatalf("persisted ci_recovered entries = %d, want exactly 1", recovered)
	}

	// derived_status is no longer ci_failed, and next_actions leaves the dead end.
	if got := getDerivedStatus(t, ctx, session, run.ID); got == "ci_failed" {
		t.Fatalf("derived_status after recovery = %q, want NOT ci_failed", got)
	}
	// Recovery must EXPOSE a routed, non-empty next_actions state off the
	// ci_failed arm — a vanished surface (nil) no longer satisfies this
	// (#3414 fix-up: the prior `na != nil &&` short-circuit permitted it).
	na := getNextActions(t, ctx, session, run.ID)
	if na == nil {
		t.Fatal("next_actions absent after recovery — recovery must EXPOSE a routed state, not remove the surface")
	}
	if na.State == "" {
		t.Fatal("next_actions.state empty after recovery — want a present, routed state")
	}
	if strings.HasPrefix(na.State, "ci_failed") {
		t.Fatalf("next_actions.state after recovery = %q, want a non-ci_failed_* arm", na.State)
	}
}

// ciCancelledMergeVerbs is the #4706 no-merge invariant's closed set: no
// action on the ci_cancelled arm may carry one of these.
var ciCancelledMergeVerbs = map[string]bool{"fishhawk_merge_run": true, "merge_pr": true, "merge_and_file_follow_up": true}

// appendRunAudit appends one chained audit entry for the run through the REAL
// postgres audit repository (the head ledger and conflict-resolution rows the
// observer and the MCP classifier read).
func appendRunAudit(t *testing.T, ctx context.Context, repo audit.Repository, runID uuid.UUID, stageID *uuid.UUID, category string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", category, err)
	}
	if _, err := repo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		Payload:   raw,
	}); err != nil {
		t.Fatalf("AppendChained %s: %v", category, err)
	}
}

// persistedCIParks counts the run's persisted run_auto_advanced ci_failed and
// ci_cancelled entries, read back through the real audit repository.
func persistedCIParks(t *testing.T, ctx context.Context, repo audit.Repository, runID uuid.UUID) (ciFailed, ciCancelled int) {
	t.Helper()
	entries, err := repo.ListForRunByCategory(ctx, runID, drive.Category)
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	for _, e := range entries {
		var adv drive.Advance
		if json.Unmarshal(e.Payload, &adv) != nil {
			continue
		}
		switch adv.Rule {
		case drive.RuleCIFailed:
			ciFailed++
		case drive.RuleCICancelled:
			ciCancelled++
		}
	}
	return ciFailed, ciCancelled
}

// ciCancelledStatusView is the slice of ONE fishhawk_get_run_status call the
// #4706 walks assert on: the drive derived_status, whether the run read carries
// a review_head_mismatch, and the FINAL next_actions block (after
// getRunStatus's full post-fold chain).
type ciCancelledStatusView struct {
	DerivedStatus string
	Mismatch      bool
	NextActions   *nextActionsView
}

func getCICancelledStatusView(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) ciCancelledStatusView {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_run_status",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if result.IsError {
		t.Fatalf("get_run_status tool returned error: %s", toolContentString(t, result))
	}
	var out struct {
		Run struct {
			ReviewHeadMismatch json.RawMessage `json:"review_head_mismatch"`
		} `json:"run"`
		DriveStatus *struct {
			DerivedStatus string `json:"derived_status"`
		} `json:"drive_status"`
		NextActions *nextActionsView `json:"next_actions"`
	}
	decodeStructured(t, result, &out)
	v := ciCancelledStatusView{NextActions: out.NextActions}
	if out.DriveStatus != nil {
		v.DerivedStatus = out.DriveStatus.DerivedStatus
	}
	if m := strings.TrimSpace(string(out.Run.ReviewHeadMismatch)); m != "" && m != "null" {
		v.Mismatch = true
	}
	return v
}

// assertNoMergeVerb fails when any action on the block is a merge verb.
func assertNoMergeVerb(t *testing.T, na *nextActionsView) {
	t.Helper()
	for i, a := range na.Actions {
		if ciCancelledMergeVerbs[a.Action] {
			t.Errorf("next_actions.actions[%d] = %s, a merge verb on the ci_cancelled arm (%+v)", i, a.Action, na.Actions)
		}
	}
}

// TestE2E_CICancelled_ObserverToDerivedStatusToNextActions (E1, #4706) crosses
// the boundaries the per-layer units cannot exercise together: Postgres
// stage_checks + audit (the head ledger), the drive observer, GET
// /v0/runs/{id}'s derived_status read model, the MCP Run mirror and
// getRunStatus's full post-fold chain, through the real fishhawk-mcp binary.
//
//   - current_head: the ledger's recorded head is cafebabe and the required
//     check concluded cancelled AT cafebabe → the observer parks ci_cancelled,
//     derived_status ci_cancelled, next_actions ci_cancelled leading with
//     fishhawk_retrigger_ci, and no merge verb anywhere.
//   - superseded_head: the ledger's recorded head moved to f00dface (a
//     fix-up push) and the check was cancelled at the OLDER cafebabe → nothing
//     parks (#3414 unchanged): derived_status empty, no ci_failed or
//     ci_cancelled persisted.
//
// The pre-existing e2e walks above record no head ledger, so the head is
// undetermined there and their behavior is unchanged.
func TestE2E_CICancelled_ObserverToDerivedStatusToNextActions(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const requiredCheck = "ci/required"

	t.Run("current_head", func(t *testing.T) {
		const prURL = "https://github.com/kuhlman-labs/fishhawk/pull/4706"
		srv, run, stage, stageCheckRepo, auditRepo := recoverableRun(t, ctx, fx, []string{requiredCheck}, prURL)
		appendRunAudit(t, ctx, auditRepo, run.ID, nil, "pull_request_opened", map[string]any{"head_sha": "cafebabe"})
		appendCheckRow(t, ctx, stageCheckRepo, stage.ID, requiredCheck, "completed", "cancelled", "cafebabe", time.Now().UTC())
		observeTick(t, ctx, srv, fx.runRepo, stage.ID, prURL)

		if f, c := persistedCIParks(t, ctx, auditRepo, run.ID); f != 0 || c != 1 {
			t.Fatalf("persisted parks = ci_failed %d / ci_cancelled %d, want 0 / 1", f, c)
		}
		session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, mountServer(t, srv))
		v := getCICancelledStatusView(t, ctx, session, run.ID)
		if v.DerivedStatus != "ci_cancelled" {
			t.Fatalf("drive_status.derived_status = %q, want ci_cancelled", v.DerivedStatus)
		}
		if v.Mismatch {
			t.Fatal("run.review_head_mismatch present; the walk stages no review round, so it must be absent")
		}
		na := v.NextActions
		if na == nil {
			t.Fatal("next_actions absent on the ci_cancelled run")
		}
		if na.State != "ci_cancelled" {
			t.Fatalf("next_actions.state = %q, want ci_cancelled (actions %+v)", na.State, na.Actions)
		}
		if len(na.Actions) == 0 || na.Actions[0].Action != "fishhawk_retrigger_ci" {
			t.Fatalf("next_actions.actions[0] = %+v, want fishhawk_retrigger_ci leading", na.Actions)
		}
		if na.Actions[0].Params["run_id"] != run.ID.String() {
			t.Errorf("fishhawk_retrigger_ci params = %v, want run_id %s", na.Actions[0].Params, run.ID)
		}
		assertNoMergeVerb(t, na)
	})

	t.Run("superseded_head", func(t *testing.T) {
		const prURL = "https://github.com/kuhlman-labs/fishhawk/pull/4707"
		srv, run, stage, stageCheckRepo, auditRepo := recoverableRun(t, ctx, fx, []string{requiredCheck}, prURL)
		appendRunAudit(t, ctx, auditRepo, run.ID, nil, "pull_request_opened", map[string]any{"head_sha": "cafebabe"})
		appendRunAudit(t, ctx, auditRepo, run.ID, nil, "fixup_pushed", map[string]any{"head_sha": "f00dface"})
		appendCheckRow(t, ctx, stageCheckRepo, stage.ID, requiredCheck, "completed", "cancelled", "cafebabe", time.Now().UTC())
		observeTick(t, ctx, srv, fx.runRepo, stage.ID, prURL)

		if f, c := persistedCIParks(t, ctx, auditRepo, run.ID); f != 0 || c != 0 {
			t.Fatalf("persisted parks = ci_failed %d / ci_cancelled %d, want 0 / 0 (a cancellation at a superseded recorded head never parks)", f, c)
		}
		session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, mountServer(t, srv))
		v := getCICancelledStatusView(t, ctx, session, run.ID)
		if v.DerivedStatus != "" {
			t.Fatalf("drive_status.derived_status = %q, want empty on a superseded cancellation", v.DerivedStatus)
		}
		if v.NextActions == nil || v.NextActions.State == "" {
			t.Fatal("next_actions absent or state empty; want a present, routed state")
		}
		if strings.HasPrefix(v.NextActions.State, "ci_") {
			t.Fatalf("next_actions.state = %q, want neither a ci_failed_* nor the ci_cancelled arm", v.NextActions.State)
		}
	})
}

// TestE2E_CICancelled_ConflictSignalKeepsFinalState (E2, #4706) pins the
// conflict-fold composition on real Postgres through the real MCP binary: the
// ledger's recorded head is cafebabe, a conflict-resolution entry lands, and
// THEN the required check is cancelled at cafebabe and the observer stamps
// ci_cancelled — so the ci_cancelled stamp is LATER than the conflict entry
// while conflictResolutionSignalIn still reads the conflict entry as Present.
// The FINAL next_actions state must stay ci_cancelled (never relabeled
// conflict_resolution_pass_*), fishhawk_retrigger_ci must lead, the signal's
// conflict action must still be APPENDED, and no merge verb may appear.
//
// The pushed subtest deliberately pins the documented #3673 limitation:
// conflict_resolution_pushed is not a head-report category, so cafebabe stays
// the current recorded head and the cancellation of it parks ci_cancelled. A
// later #3673 fix must update that subtest. Mismatch-present is covered at the
// C1/C2 route surfaces (staging a real review_head_mismatch needs a full review
// round), so every subtest asserts the mismatch absent.
func TestE2E_CICancelled_ConflictSignalKeepsFinalState(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	const requiredCheck = "ci/required"
	const refusal = "conflict_resolution_outside_hunk"
	for i, tc := range []struct {
		signal   string
		category string
		payload  map[string]any
	}{
		{"triggered", server.CategoryStageConflictResolutionTriggered, map[string]any{}},
		{"failed", server.CategoryStageConflictResolutionFailed, map[string]any{"reason": refusal}},
		{"pushed", server.CategoryConflictResolutionPushed, map[string]any{"head_sha": "deadbeef"}},
	} {
		t.Run(tc.signal, func(t *testing.T) {
			prURL := "https://github.com/kuhlman-labs/fishhawk/pull/" + strconv.Itoa(4710+i)
			srv, run, stage, stageCheckRepo, auditRepo := recoverableRun(t, ctx, fx, []string{requiredCheck}, prURL)
			appendRunAudit(t, ctx, auditRepo, run.ID, nil, "pull_request_opened", map[string]any{"head_sha": "cafebabe"})
			stageID := stage.ID
			appendRunAudit(t, ctx, auditRepo, run.ID, &stageID, tc.category, tc.payload)
			appendCheckRow(t, ctx, stageCheckRepo, stage.ID, requiredCheck, "completed", "cancelled", "cafebabe", time.Now().UTC())
			observeTick(t, ctx, srv, fx.runRepo, stage.ID, prURL)

			session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, mountServer(t, srv))
			v := getCICancelledStatusView(t, ctx, session, run.ID)
			if v.DerivedStatus != "ci_cancelled" {
				t.Fatalf("drive_status.derived_status = %q, want ci_cancelled", v.DerivedStatus)
			}
			if v.Mismatch {
				t.Fatal("run.review_head_mismatch present; the walk stages no review round, so it must be absent")
			}
			na := v.NextActions
			if na == nil {
				t.Fatal("next_actions absent on the ci_cancelled run")
			}
			if na.State != "ci_cancelled" {
				t.Errorf("final next_actions.state = %q, want ci_cancelled (the conflict fold must not relabel it; actions %+v)", na.State, na.Actions)
			}
			if len(na.Actions) == 0 || na.Actions[0].Action != "fishhawk_retrigger_ci" {
				t.Fatalf("next_actions.actions[0] = %+v, want fishhawk_retrigger_ci leading", na.Actions)
			}
			assertNoMergeVerb(t, na)
			conflict := -1
			for i, a := range na.Actions {
				if strings.Contains(a.Precondition, "conflict-resolution") {
					if conflict >= 0 {
						t.Fatalf("more than one conflict-resolution action: %+v", na.Actions)
					}
					conflict = i
				}
			}
			if conflict <= 0 {
				t.Fatalf("conflict-resolution action at %d, want exactly one appended after the lead retrigger (actions %+v)", conflict, na.Actions)
			}
			a := na.Actions[conflict]
			switch tc.signal {
			case "triggered":
				if a.Action != "fishhawk_await_stage" || a.Params["run_id"] != run.ID.String() || a.Params["stage_id"] != stage.ID.String() {
					t.Errorf("triggered: conflict action = %s %v, want fishhawk_await_stage {run_id, stage_id %s}", a.Action, a.Params, stage.ID)
				}
			case "failed":
				if a.Action != "fishhawk_rebase_run_branch" || a.Params["run_id"] != run.ID.String() || !strings.Contains(a.Reason, refusal) {
					t.Errorf("failed: conflict action = %s %v reason %q, want fishhawk_rebase_run_branch naming %s", a.Action, a.Params, a.Reason, refusal)
				}
			case "pushed":
				if a.Action != "fishhawk_rebase_run_branch" || a.Params["run_id"] != run.ID.String() || !strings.Contains(a.Reason, "SUCCEEDED") {
					t.Errorf("pushed: conflict action = %s %v reason %q, want fishhawk_rebase_run_branch naming SUCCEEDED", a.Action, a.Params, a.Reason)
				}
			}
		})
	}
}

// mountServer mounts the server on a throwaway httptest server and
// returns its URL, registering teardown.
func mountServer(t *testing.T, srv *server.Server) string {
	t.Helper()
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	return httpSrv.URL
}

// getDerivedStatus calls fishhawk_get_run_status and returns the decoded
// drive_status.derived_status ("" when absent).
func getDerivedStatus(t *testing.T, ctx context.Context, session *mcp.ClientSession, runID uuid.UUID) string {
	t.Helper()
	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "fishhawk_get_run_status",
		Arguments: map[string]any{"run_id": runID.String()},
	})
	if err != nil {
		t.Fatalf("CallTool fishhawk_get_run_status: %v", err)
	}
	if result.IsError {
		t.Fatalf("get_run_status tool returned error: %s", toolContentString(t, result))
	}
	var out struct {
		DriveStatus *struct {
			DerivedStatus string `json:"derived_status"`
		} `json:"drive_status"`
	}
	decodeStructured(t, result, &out)
	if out.DriveStatus == nil {
		return ""
	}
	return out.DriveStatus.DerivedStatus
}
