package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// campaign_grooming_currency_pg_test.go is the END-TO-END concurrency proof for
// the grooming-currency guard (E54.17 / #2817). It drives POST /v0/campaigns
// through the REAL handler backed by REAL Postgres run/artifact/approval/campaign
// repositories, and commits the SUPERSEDING run's approval from INSIDE the
// work-management provider's ResolveDependencies — i.e. strictly AFTER the
// handler's supersession scan and strictly BEFORE campaign.Persist, which IS the
// check-to-persist window. The assertions read COMMITTED state (zero campaign
// rows), not merely a non-2xx.
//
// HONEST GUARANTEE (asserted by the window case): a superseding approval
// COMMITTED BEFORE THE GUARDED INSERT STATEMENT BEGAN prevents creation and
// leaves no campaign row. The injection commits the approval from another
// connection strictly before Persist, so it is committed before the guard
// statement begins. This is NOT whole-request serializability.
//
// The fixture idiom is COPIED from grooming_window_pg_test.go (operator
// condition 4 — pg fixture idiom is copied, not shared), not extended from it.

// gcWindowFixture is a real source grooming run (approved report ranking the
// given issues) plus a real candidate grooming run carrying an UNAPPROVED report,
// so the handler's pre-Persist scan sees no superseding run. The provider's
// ResolveDependencies commits the candidate's approval inside the window.
type gcWindowFixture struct {
	s          *Server
	runRepo    run.Repository
	artRepo    artifact.Repository
	apprRepo   approval.Repository
	campRepo   campaign.Repository
	sourceRun  uuid.UUID
	candStage  uuid.UUID
	provider   *fakeIssueSetProvider
	campRepoID string
}

const gcRepo = "kuhlman-labs/fishhawk"

// newGCWindowFixture seeds the source + candidate runs. candidateNewer decides
// whether the candidate strictly follows the source in (created_at, id) order —
// the window (refuse) case — or precedes it — the control (201) case.
func newGCWindowFixture(t *testing.T, candidateNewer bool) *gcWindowFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	apprRepo := approval.NewPostgresRepository(pool)
	campRepo := campaign.NewPostgresRepository(pool)

	// runs.account_id carries an FK to accounts (migration 0055). The caller
	// (withAuth) is testOperatorAccountID, and the tenancy match is EXACT, so both
	// the source and candidate runs must carry that same account.
	acct := uuid.MustParse(testOperatorAccountID)
	if _, err := pool.Exec(ctx, "INSERT INTO accounts (id, account_key) VALUES ($1, $2)", acct, "op-account"); err != nil {
		t.Fatalf("seed account: %v", err)
	}

	// The SOURCE run: an APPROVED grooming report ranking issues 10, 20.
	sourceStage := seedGroomingRun(t, ctx, runRepo, artRepo, apprRepo, gcRepo, groomingSourceReportJSON(t, "kuhlman-labs", "fishhawk", 10, 20), true)
	sourceRunID := sourceStage.RunID
	base := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := pool.Exec(ctx, "UPDATE runs SET created_at=$1, account_id=$2 WHERE id=$3", base, acct, sourceRunID); err != nil {
		t.Fatalf("stamp source run: %v", err)
	}

	// The CANDIDATE run: a grooming report with NO approval yet, so the pre-Persist
	// scan sees no superseding run. Its approval is committed inside the window.
	candStage := seedGroomingRun(t, ctx, runRepo, artRepo, apprRepo, gcRepo, []byte(`{"kind":"grooming_report"}`), false)
	candCreated := base.Add(time.Hour)
	if !candidateNewer {
		candCreated = base.Add(-time.Hour)
	}
	if _, err := pool.Exec(ctx, "UPDATE runs SET created_at=$1, account_id=$2 WHERE id=$3", candCreated, acct, candStage.RunID); err != nil {
		t.Fatalf("stamp candidate run: %v", err)
	}

	provider := &fakeIssueSetProvider{result: &workmgmt.EpicChildrenResult{
		Children: []workmgmt.EpicChild{{Number: 10}, {Number: 20}},
	}}
	// THE WINDOW INJECTION: commit the candidate's approval strictly AFTER the
	// handler's supersession scan and strictly BEFORE campaign.Persist.
	provider.beforeResolve = func() {
		if _, err := apprRepo.Submit(ctx, approval.SubmitParams{
			StageID: candStage.ID, ApproverSubject: "op-window", Decision: approval.DecisionApprove, Surface: "cli",
		}); err != nil {
			t.Errorf("commit candidate approval in window: %v", err)
		}
	}
	registerIssueSetProvider(t, provider)

	s := New(Config{
		CampaignRepo: campRepo, RunRepo: runRepo,
		ArtifactRepo: artRepo, ApprovalRepo: apprRepo,
		AuditRepo: audit.NewPostgresRepository(pool),
	})
	return &gcWindowFixture{
		s: s, runRepo: runRepo, artRepo: artRepo, apprRepo: apprRepo, campRepo: campRepo,
		sourceRun: sourceRunID, candStage: candStage.ID, provider: provider, campRepoID: gcRepo,
	}
}

// seedGroomingRun creates a run + plan stage + grooming_report artifact and,
// when approve is set, an approve row. Returns the created stage (carrying RunID).
func seedGroomingRun(t *testing.T, ctx context.Context, runRepo run.Repository, artRepo artifact.Repository, apprRepo approval.Repository, repo string, report []byte, approve bool) *run.Stage {
	t.Helper()
	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: repo, WorkflowID: "backlog_grooming", WorkflowSHA: "sha", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create grooming run: %v", err)
	}
	stage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: rn.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("create grooming stage: %v", err)
	}
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: stage.ID, Kind: artifact.KindGroomingReport, Content: report, ContentHash: "sha256:report",
	}); err != nil {
		t.Fatalf("create grooming report artifact: %v", err)
	}
	if approve {
		if _, err := apprRepo.Submit(ctx, approval.SubmitParams{
			StageID: stage.ID, ApproverSubject: "op-source", Decision: approval.DecisionApprove, Surface: "cli",
		}); err != nil {
			t.Fatalf("approve grooming report: %v", err)
		}
	}
	return stage
}

// TestCreateCampaign_GroomingCurrencyGuard_WindowRefusesAndCommitsNoRow is the
// window (refuse) case: the candidate becomes approved INSIDE the check-to-persist
// window, so the atomic guarded INSERT sees a strictly-newer approved grooming run
// and writes NOTHING. The request returns 422 grooming_order_superseded AND the
// campaigns table has zero rows for the repo.
func TestCreateCampaign_GroomingCurrencyGuard_WindowRefusesAndCommitsNoRow(t *testing.T) {
	f := newGCWindowFixture(t, true)

	w := postCampaign(t, f.s, groomingSourceBody(f.sourceRun, ""))
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 (body=%s)", w.Code, w.Body.String())
	}
	code, _ := decodeGroomingErr(t, w.Body.Bytes())
	if code != codeGroomingSuperseded {
		t.Fatalf("error code = %q, want %q", code, codeGroomingSuperseded)
	}
	if !f.provider.resolveCalled {
		t.Fatal("the provider seam never ran, so the window injection never fired")
	}
	// COMMITTED STATE: no campaign row exists for the repo.
	listed, err := f.campRepo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{Repo: f.campRepoID, Limit: 10})
	if err != nil {
		t.Fatalf("list campaigns: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("committed campaigns = %d, want 0 — the guarded INSERT must leave no row when superseded", len(listed))
	}
}

// TestCreateCampaign_GroomingCurrencyGuard_NotNewerControlPersists is the control
// that makes the window assertion DISCRIMINATE a real refusal from a blanket one:
// the resolver commits an approval on a run that is NOT newer than the source, so
// the guard does NOT refuse. The request returns 201 with a persisted campaign.
func TestCreateCampaign_GroomingCurrencyGuard_NotNewerControlPersists(t *testing.T) {
	f := newGCWindowFixture(t, false)

	w := postCampaign(t, f.s, groomingSourceBody(f.sourceRun, ""))
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201 (body=%s)", w.Code, w.Body.String())
	}
	if !f.provider.resolveCalled {
		t.Fatal("the provider seam never ran, so the control injection never fired")
	}
	listed, err := f.campRepo.ListCampaigns(context.Background(), campaign.ListCampaignsFilter{Repo: f.campRepoID, Limit: 10})
	if err != nil {
		t.Fatalf("list campaigns: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("committed campaigns = %d, want 1 — an approval on a not-newer run must not refuse creation", len(listed))
	}
}
