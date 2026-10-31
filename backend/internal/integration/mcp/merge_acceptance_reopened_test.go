package mcpe2e_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// TestE2E_NextActions_AcceptanceReopenedBlocksMerge is the cross-boundary half
// of E64.63 / #3222. Its scope spans four layers the per-layer units cannot
// exercise together:
//
//  1. run/stage persistence — an acceptance stage driven to succeeded and then
//     re-opened to pending by run.ReopenAcceptanceStage, the real write path.
//  2. the audit chain — a CHAINED acceptance_reopened entry scoped to that
//     stage's id, written through audit.AppendChained exactly as the server's
//     fix-up push handler writes it.
//  3. the server's stage + audit HTTP surface — what the MCP tool actually
//     decodes, including the stage_id field whose json tag the correlation
//     depends on (the #371-class wire-mirror trap: a silently-nil StageID would
//     degrade the wording to generic and a pure-unit test would never see it).
//  4. the MCP tool's render — the real fishhawk-mcp binary over stdio.
//
// The defect it pins: a run whose acceptance stage was re-opened by a fix-up
// push sits succeeded with its PR open, so next_actions reported
// succeeded_pr_open and offered the merge — while the merge could not fire,
// because fishhawk_audit_complete was pending on that very stage. The operator,
// with nothing on this surface naming the cause, reached for an admin bypass
// twice.
//
// NO FORGE CLIENT is involved on this path: the run's PR url is set through the
// repository, and fishhawk_get_run_status performs no forge round-trip. That is
// what makes this criterion sandbox-decidable rather than a skip.
//
// Deliberately NOT attempted here: driving fishhawk_merge_run itself. newFixture
// wires no GateMerger, so the merge POST would 503 merge_seam_unconfigured
// before any advisory could render; the merge surface's own branches are pinned
// by mcpserver/merge_run_test.go against its httptest backend.
func TestE2E_NextActions_AcceptanceReopenedBlocksMerge(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)

	const prURL = "https://github.com/kuhlman-labs/fishhawk/pull/3222"

	run, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := fx.runRepo.TransitionRun(ctx, run.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}

	// An implement stage that succeeded, then an acceptance stage driven to
	// succeeded — the shape a fix-up push re-opens.
	impl, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        run.ID,
		Sequence:     1,
		Type:         runpkg.StageTypeImplement,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(implement): %v", err)
	}
	driveStageToSucceeded(t, ctx, fx.runRepo, impl.ID)

	acc, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
		RunID:        run.ID,
		Sequence:     2,
		Type:         runpkg.StageTypeAcceptance,
		ExecutorKind: runpkg.ExecutorAgent,
		ExecutorRef:  "fishhawk/runner@v1",
	})
	if err != nil {
		t.Fatalf("CreateStage(acceptance): %v", err)
	}
	driveStageToSucceeded(t, ctx, fx.runRepo, acc.ID)

	kind := audit.ActorKind("system")
	outcomePayload, err := json.Marshal(map[string]any{"verdict": "passed", "stage_id": acc.ID.String()})
	if err != nil {
		t.Fatalf("marshal acceptance_outcome_recorded payload: %v", err)
	}
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     run.ID,
		StageID:   &acc.ID,
		Timestamp: time.Now(),
		Category:  "acceptance_outcome_recorded",
		ActorKind: &kind,
		Payload:   outcomePayload,
	}); err != nil {
		t.Fatalf("AppendChained acceptance_outcome_recorded: %v", err)
	}

	// THE RE-OPEN — the real write path, not a hand-set state.
	if _, err := runpkg.ReopenAcceptanceStage(ctx, fx.runRepo, acc.ID); err != nil {
		t.Fatalf("ReopenAcceptanceStage: %v", err)
	}
	reopenPayload, err := json.Marshal(map[string]any{"prior_state": "succeeded", "head_sha": "f1xuphead"})
	if err != nil {
		t.Fatalf("marshal acceptance_reopened payload: %v", err)
	}
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     run.ID,
		StageID:   &acc.ID, // SCOPED to this stage — the correlation the render depends on
		Timestamp: time.Now(),
		Category:  server.CategoryAcceptanceReopened,
		ActorKind: &kind,
		Payload:   reopenPayload,
	}); err != nil {
		t.Fatalf("AppendChained acceptance_reopened: %v", err)
	}

	// The run itself is succeeded with its PR open — the shape that made the
	// classifier offer a merge that could not fire.
	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, run.ID, prURL); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	if _, err := fx.runRepo.TransitionRun(ctx, run.ID, runpkg.StateSucceeded); err != nil {
		t.Fatalf("TransitionRun → succeeded: %v", err)
	}

	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)
	na := getNextActions(t, ctx, session, run.ID)
	if na == nil {
		t.Fatal("next_actions absent on the succeeded run")
	}
	if na.State != "succeeded_acceptance_reopened" {
		t.Fatalf("next_actions.state = %q, want succeeded_acceptance_reopened — the merge is blocked and the block is unnamed", na.State)
	}

	var dispatch *struct {
		Action       string            `json:"action"`
		Params       map[string]string `json:"params"`
		Precondition string            `json:"precondition"`
		Consumes     string            `json:"consumes"`
		Reason       string            `json:"reason"`
	}
	var sawMerge bool
	for i := range na.Actions {
		switch na.Actions[i].Action {
		case "fishhawk_dispatch_stage":
			dispatch = &na.Actions[i]
		case "fishhawk_merge_run":
			sawMerge = true
		}
	}
	if dispatch == nil {
		t.Fatalf("next_actions.actions = %+v, want fishhawk_dispatch_stage offered", na.Actions)
	}
	if dispatch.Params["stage"] != "acceptance" {
		t.Errorf("dispatch stage param = %q, want acceptance", dispatch.Params["stage"])
	}
	if dispatch.Params["stage_id"] != acc.ID.String() {
		t.Errorf("dispatch stage_id = %q, want the re-opened acceptance stage %s", dispatch.Params["stage_id"], acc.ID)
	}
	if dispatch.Params["run_id"] != run.ID.String() {
		t.Errorf("dispatch run_id = %q, want %s", dispatch.Params["run_id"], run.ID)
	}
	// The advisory NAMES the stage and reports the RE-OPENED cause — the
	// sharpened wording, which is only reachable when the stage_id survived
	// layers 2 and 3 intact.
	for _, want := range []string{
		acc.ID.String()[:8],
		"re-opened by a fix-up push",
		"the queued merge cannot fire",
	} {
		if !strings.Contains(dispatch.Reason, want) {
			t.Errorf("dispatch reason = %q, want it to contain %q", dispatch.Reason, want)
		}
	}
	// ADDITIVE: the merge stays offered — the server remains the authority on
	// whether it fires.
	if !sawMerge {
		t.Errorf("next_actions.actions = %+v, want the merge ritual retained", na.Actions)
	}
}

// TestE2E_NextActions_FixupReopenedAcceptanceConcernsOpenHoldsMerge is the
// cross-boundary half of #4801: the live shape where succeedFixupPushStage
// advanced implement (review parked at awaiting_approval, so the fix-up gate is
// OPEN) before the fix-up push re-opened the settled acceptance stage, with an
// implement concern still open. It crosses persistence
// (run.ReopenAcceptanceStage), the audit chain (a stage-scoped
// acceptance_reopened), the server stage/audit/checks HTTP surface and the real
// fishhawk-mcp render.
//
// (1) next_actions leads with the acceptance dispatch carrying stage_id and
// offers NO merge (the concerns arm used to lead with merge_and_file_follow_up).
// (2) fishhawk_audit_complete on the review stage is pending on the re-opened
// acceptance stage, naming it — the merge the dropped action named is refused.
func TestE2E_NextActions_FixupReopenedAcceptanceConcernsOpenHoldsMerge(t *testing.T) {
	t.Parallel()
	fx := newFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	auditRepo := audit.NewPostgresRepository(fx.pool)
	kind := audit.ActorKind("system")

	// A local run: the operator host dispatches acceptance (a github_actions
	// run would auto-dispatch and lead with a poll instead).
	run, err := fx.runRepo.CreateRun(ctx, runpkg.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: runpkg.TriggerCLI,
		RunnerKind:    runpkg.RunnerKindLocal,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if _, err := fx.runRepo.TransitionRun(ctx, run.ID, runpkg.StateRunning); err != nil {
		t.Fatalf("TransitionRun → running: %v", err)
	}
	mkStage := func(seq int, typ runpkg.StageType, approval bool) *runpkg.Stage {
		t.Helper()
		st, err := fx.runRepo.CreateStage(ctx, runpkg.CreateStageParams{
			RunID: run.ID, Sequence: seq, Type: typ,
			ExecutorKind: runpkg.ExecutorAgent, ExecutorRef: "fishhawk/runner@v1",
			RequiresApproval: approval,
		})
		if err != nil {
			t.Fatalf("CreateStage(%s): %v", typ, err)
		}
		return st
	}
	planStage := mkStage(0, runpkg.StageTypePlan, false)
	driveStageToSucceeded(t, ctx, fx.runRepo, planStage.ID)
	impl := mkStage(1, runpkg.StageTypeImplement, false)
	driveStageToSucceeded(t, ctx, fx.runRepo, impl.ID)
	acc := mkStage(2, runpkg.StageTypeAcceptance, false)
	driveStageToSucceeded(t, ctx, fx.runRepo, acc.ID)
	review := mkStage(3, runpkg.StageTypeReview, true)

	// One OPEN implement concern from an approve_with_concerns verdict.
	seedImplementReview(t, ctx, auditRepo, run.ID, impl.ID,
		planreview.Concern{Severity: planreview.SeverityMedium, Category: "scope", Note: "guard the nil stage"})

	outcomePayload, _ := json.Marshal(map[string]any{"verdict": "passed", "stage_id": acc.ID.String()})
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: run.ID, StageID: &acc.ID, Timestamp: time.Now(),
		Category: "acceptance_outcome_recorded", ActorKind: &kind, Payload: outcomePayload,
	}); err != nil {
		t.Fatalf("AppendChained acceptance_outcome_recorded: %v", err)
	}

	// The live ordering: review parks at awaiting_approval FIRST (the advance
	// inside succeedFixupPushStage), THEN the fix-up push re-opens acceptance.
	parkAtGate(t, ctx, fx.runRepo, review.ID)
	if _, err := runpkg.ReopenAcceptanceStage(ctx, fx.runRepo, acc.ID); err != nil {
		t.Fatalf("ReopenAcceptanceStage: %v", err)
	}
	reopenPayload, _ := json.Marshal(map[string]any{"prior_state": "succeeded", "head_sha": "f1xuphead"})
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: run.ID, StageID: &acc.ID, Timestamp: time.Now(),
		Category: server.CategoryAcceptanceReopened, ActorKind: &kind, Payload: reopenPayload,
	}); err != nil {
		t.Fatalf("AppendChained acceptance_reopened: %v", err)
	}
	if _, err := fx.runRepo.SetRunPullRequestURL(ctx, run.ID, "https://github.com/kuhlman-labs/fishhawk/pull/4801"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}

	// (1) next_actions through the real fishhawk-mcp binary.
	session := connectMCPClient(t, ctx, fx.mcpBinary, fx.operatorTok, fx.url)
	na := getNextActions(t, ctx, session, run.ID)
	if na == nil {
		t.Fatal("next_actions absent")
	}
	if na.State != "implement_concerns_open_acceptance_reopened" {
		t.Fatalf("next_actions.state = %q, want implement_concerns_open_acceptance_reopened (actions %+v)", na.State, na.Actions)
	}
	lead := -1
	for i, a := range na.Actions {
		if a.Action == "merge_and_file_follow_up" || a.Action == "fishhawk_merge_run" {
			t.Errorf("next_actions offers %s while acceptance is re-opened: %+v", a.Action, na.Actions)
		}
		if lead < 0 && strings.HasPrefix(a.Action, "fishhawk_") {
			lead = i
		}
	}
	// #3321: at most the single run_preview ritual step may precede the lead.
	if lead < 0 || lead > 1 {
		t.Fatalf("first fishhawk_* action at index %d, want 0 or 1 (actions %+v)", lead, na.Actions)
	}
	if lead == 1 && na.Actions[0].Action != "run_preview" {
		t.Errorf("action ahead of the lead = %q, want only the run_preview ritual", na.Actions[0].Action)
	}
	d := na.Actions[lead]
	if d.Action != "fishhawk_dispatch_stage" || d.Params["stage"] != "acceptance" || d.Params["stage_id"] != acc.ID.String() {
		t.Fatalf("lead = %s %v, want fishhawk_dispatch_stage(stage=acceptance, stage_id=%s)", d.Action, d.Params, acc.ID)
	}
	if !strings.Contains(d.Reason, "re-opened by a fix-up push") {
		t.Errorf("lead reason = %q, want the re-open wording", d.Reason)
	}

	// (2) the checks surface: a second backend over the SAME pool with the
	// stage-check + artifact repos the checks endpoint needs.
	srv := server.New(server.Config{
		Addr: "127.0.0.1:0", RunRepo: fx.runRepo, AuditRepo: auditRepo,
		ArtifactRepo:   artifact.NewPostgresRepository(fx.pool),
		StageCheckRepo: stagecheck.NewPostgresRepository(fx.pool),
		APITokenRepo:   fx.apitokenRepo,
	})
	httpSrv := httptest.NewServer(srv.Handler())
	t.Cleanup(httpSrv.Close)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, httpSrv.URL+"/v0/stages/"+review.ID.String()+"/checks", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+fx.operatorTok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET checks: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET checks status %d: %s", resp.StatusCode, raw)
	}
	var checks struct {
		Items []struct {
			Name    string `json:"name"`
			State   string `json:"state"`
			Missing []struct {
				Kind   string `json:"kind"`
				Detail string `json:"detail"`
			} `json:"missing"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &checks); err != nil {
		t.Fatalf("decode checks: %v", err)
	}
	found := false
	for _, it := range checks.Items {
		if it.Name != server.AuditCompleteCheckName {
			continue
		}
		found = true
		if it.State != "pending" || len(it.Missing) == 0 || it.Missing[0].Kind != "stage_not_terminal" {
			t.Fatalf("audit_complete = %+v, want pending with missing[0] stage_not_terminal", it)
		}
		for _, want := range []string{acc.ID.String()[:8], "re-opened by a fix-up push"} {
			if !strings.Contains(it.Missing[0].Detail, want) {
				t.Errorf("missing[0].detail = %q, want it to contain %q", it.Missing[0].Detail, want)
			}
		}
	}
	if !found {
		t.Fatalf("checks = %s, want a %s row", raw, server.AuditCompleteCheckName)
	}
	if strings.Contains(string(raw), "headline") {
		t.Errorf("checks wire carries a headline key: %s", raw)
	}
}

// driveStageToSucceeded walks a stage through the legal transition table to
// succeeded (the table refuses a pending → succeeded jump).
func driveStageToSucceeded(t *testing.T, ctx context.Context, repo runpkg.Repository, stageID uuid.UUID) {
	t.Helper()
	for _, to := range []runpkg.StageState{
		runpkg.StageStateDispatched,
		runpkg.StageStateRunning,
		runpkg.StageStateSucceeded,
	} {
		if _, err := repo.TransitionStage(ctx, stageID, to, nil); err != nil {
			t.Fatalf("TransitionStage → %s: %v", to, err)
		}
	}
}
