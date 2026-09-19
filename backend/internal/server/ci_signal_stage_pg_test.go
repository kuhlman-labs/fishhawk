package server

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// ci_signal_stage_pg_test.go is the CROSS-BOUNDARY proof for #3489: a REAL
// GitHub `check_run` payload driven through ingestCheckRun against the REAL
// Postgres stagecheck, run and artifact repositories — so
// stagecheck/queries.sql's FindRunStagesForCheckRun SQL filter
// (`s.stage_type = 'review'`, #254), not a fake's matchedStages, decides
// WHERE the row lands — followed by the two readers of that row:
// deployCIGreenVerdict and reevaluateCIPolicyForPR. Both resolve their
// stage through findCISignalStage. Reverting either read to
// findImplementStage reads a stage the ingester never wrote to and turns
// the corresponding test RED on its behavioural assertion; the fixture seeds
// BOTH stages by construction so the RED cannot land on setup.

// ciSignalPGFixture is one run with an implement stage (carrying the
// pull_request artifact the ingester's SQL walks) and a review stage (the
// CI-signal stage), a required-checks snapshot naming `ci/build`, the PR URL
// the re-eval resolves the run by, and a prior deferred policy_evaluated row
// anchored on the implement stage.
type ciSignalPGFixture struct {
	s           *Server
	runRepo     run.Repository
	checkRepo   stagecheck.Repository
	audit       *reevalAuditRepo
	runID       uuid.UUID
	implStageID uuid.UUID
	revStageID  uuid.UUID
}

func newCISignalPGFixture(t *testing.T) *ciSignalPGFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	checkRepo := stagecheck.NewPostgresRepository(pool)

	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "x/y", WorkflowID: "feature_change", WorkflowSHA: "deadbeef", TriggerSource: run.TriggerCLI,
		RequiredChecksSnapshot: &run.RequiredChecksSnapshot{
			Contexts: []string{"ci/build"},
			Sources:  []string{"branch_protection"},
		},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	implStage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 0, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("CreateStage implement: %v", err)
	}
	reviewStage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: r.ID, Sequence: 1, Type: run.StageTypeReview,
		ExecutorKind: run.ExecutorHuman, ExecutorRef: "human",
		Gate: &run.Gate{Kind: run.GateKindApproval},
	})
	if err != nil {
		t.Fatalf("CreateStage review: %v", err)
	}
	if _, err := runRepo.SetRunPullRequestURL(ctx, r.ID, "https://github.com/x/y/pull/42"); err != nil {
		t.Fatalf("SetRunPullRequestURL: %v", err)
	}
	prBody, _ := json.Marshal(map[string]any{
		"pr_number":           42,
		"head_sha":            "abc123",
		"branch":              "feat",
		"base_sha":            "def456",
		"title":               "x",
		"files_changed_count": 1,
	})
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID:     implStage.ID,
		Kind:        artifact.KindPullRequest,
		Content:     prBody,
		ContentHash: "hashpr",
	}); err != nil {
		t.Fatalf("artifact Create: %v", err)
	}

	aud := newReevalAuditRepo()
	aud.seedPolicyEvaluated(r.ID, implStage.ID, policy.EvaluationPayload{
		StageType: "implement",
		Diff:      []policy.DiffEntry{{Path: "x.go", Status: policy.Status("modified")}},
		Applied: policy.Constraints{
			RequiredOutcomes: []string{"ci_green"},
		},
		Passed:           true,
		DeferredOutcomes: []string{"ci_green"},
	})

	s := New(Config{
		Addr:           "127.0.0.1:0",
		RunRepo:        runRepo,
		AuditRepo:      aud,
		StageCheckRepo: checkRepo,
	})
	return &ciSignalPGFixture{
		s: s, runRepo: runRepo, checkRepo: checkRepo, audit: aud,
		runID: r.ID, implStageID: implStage.ID, revStageID: reviewStage.ID,
	}
}

// ingest drives one completed/success `ci/build` check_run for PR 42 at
// headSHA through the real ingester.
func (f *ciSignalPGFixture) ingest(t *testing.T, headSHA string) {
	t.Helper()
	f.s.ingestCheckRun(context.Background(),
		makeCheckRunPayload("completed", "ci/build", headSHA, "completed", ptrStr("success"), []int{42}, 999))
}

// rowCounts returns the number of stage_checks rows on the implement and
// review stages respectively — the seam the readers depend on.
func (f *ciSignalPGFixture) rowCounts(t *testing.T) (impl, rev int) {
	t.Helper()
	ctx := context.Background()
	ic, err := f.checkRepo.LatestForStage(ctx, f.implStageID)
	if err != nil {
		t.Fatalf("LatestForStage implement: %v", err)
	}
	rc, err := f.checkRepo.LatestForStage(ctx, f.revStageID)
	if err != nil {
		t.Fatalf("LatestForStage review: %v", err)
	}
	return len(ic), len(rc)
}

// Test A (deploy): the ingested row lands on the REVIEW stage only, and
// deployCIGreenVerdict — reading the persisted run row, snapshot included —
// sees it green.
func TestCISignalStage_PG_DeployVerdictReadsIngestedCheck(t *testing.T) {
	fx := newCISignalPGFixture(t)
	ctx := context.Background()
	fx.ingest(t, "abc123")

	impl, rev := fx.rowCounts(t)
	if impl != 0 || rev != 1 {
		t.Fatalf("stage_checks rows: implement=%d review=%d, want implement=0 review=1 (FindRunStagesForCheckRun targets the review stage, #254)", impl, rev)
	}

	runRow, err := fx.runRepo.GetRun(ctx, fx.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if runRow.RequiredChecksSnapshot == nil {
		t.Fatal("persisted run carries no RequiredChecksSnapshot; fixture broken")
	}
	v := fx.s.deployCIGreenVerdict(ctx, runRow)
	if !v.Satisfied {
		t.Fatalf("deployCIGreenVerdict.Satisfied = false (branch=%v reason=%q), want true: the deploy read must resolve the review stage the ingester wrote to (#3489)", v.Details["branch"], v.Reason)
	}
}

// Test B (re-eval): after the same ingest, the post-CI re-eval flips
// ci_green to true, and the appended policy_evaluated row stays anchored on
// the IMPLEMENT stage — only the READ moved (#3489).
func TestCISignalStage_PG_ReevalFlipsFromIngestedCheck(t *testing.T) {
	fx := newCISignalPGFixture(t)
	ctx := context.Background()
	fx.ingest(t, "abc123")

	fx.s.reevaluateCIPolicyForPR(ctx, "x/y", 42, "ci/build")

	fx.audit.mu.Lock()
	defer fx.audit.mu.Unlock()
	var appended *policy.EvaluationPayload
	var appendedStage *uuid.UUID
	for i := len(fx.audit.appended) - 1; i >= 0; i-- {
		p := fx.audit.appended[i]
		if p.Category != policy.CategoryPolicyEvaluated {
			continue
		}
		var pl policy.EvaluationPayload
		if err := json.Unmarshal(p.Payload, &pl); err != nil {
			t.Fatalf("decode appended policy_evaluated: %v", err)
		}
		appended, appendedStage = &pl, p.StageID
		break
	}
	if appended == nil {
		t.Fatal("expected a new policy_evaluated row after the ingested check; got none (the re-eval read must resolve the review stage the ingester wrote to, #3489)")
	}
	if appended.Applied.CIGreen == nil || !*appended.Applied.CIGreen {
		t.Errorf("Applied.CIGreen = %v, want &true", appended.Applied.CIGreen)
	}
	if appendedStage == nil || *appendedStage != fx.implStageID {
		t.Errorf("appended policy_evaluated StageID = %v, want the implement stage %v", appendedStage, fx.implStageID)
	}
}

// Test C (negative control): a payload whose head_sha does not match the
// pull_request artifact matches no stage, so nothing is appended anywhere,
// the deploy verdict is contexts_pending and the re-eval appends no row.
func TestCISignalStage_PG_NonMatchingHeadSHA_NothingRecorded(t *testing.T) {
	fx := newCISignalPGFixture(t)
	ctx := context.Background()
	fx.ingest(t, "ffffff")

	if impl, rev := fx.rowCounts(t); impl != 0 || rev != 0 {
		t.Fatalf("stage_checks rows: implement=%d review=%d, want none for a non-matching head_sha", impl, rev)
	}
	runRow, err := fx.runRepo.GetRun(ctx, fx.runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	v := fx.s.deployCIGreenVerdict(ctx, runRow)
	if v.Satisfied {
		t.Fatal("deployCIGreenVerdict.Satisfied = true, want false with no recorded check")
	}
	if v.Details["branch"] != "contexts_pending" {
		t.Errorf("details.branch = %v, want contexts_pending", v.Details["branch"])
	}

	fx.s.reevaluateCIPolicyForPR(ctx, "x/y", 42, "ci/build")
	fx.audit.mu.Lock()
	defer fx.audit.mu.Unlock()
	if n := len(fx.audit.appended); n != 0 {
		t.Errorf("appended audit rows = %d, want 0 (no check landed)", n)
	}
}
