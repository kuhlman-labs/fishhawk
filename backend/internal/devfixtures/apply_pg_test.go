package devfixtures_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/devfixtures"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The committed-state half of the Apply contract: every scenario is
// materialized through the REAL Postgres repositories and read back
// through them, so a store refusing a shape (a transition the table
// forbids, a chain the hash linkage rejects, an FK the walk got wrong)
// fails here rather than on the first preview. The fake-driven tests in
// apply_test.go own the error branches; these own "the rows are what the
// scenario declared".

type pgRepos struct {
	runs      run.Repository
	artifacts artifact.Repository
	audit     audit.Repository
	approvals approval.Repository
}

func newPGRepos(pool *pgxpool.Pool) pgRepos {
	return pgRepos{
		runs:      run.NewPostgresRepository(pool),
		artifacts: artifact.NewPostgresRepository(pool),
		audit:     audit.NewPostgresRepository(pool),
		approvals: approval.NewPostgresRepository(pool),
	}
}

func (r pgRepos) deps(now func() time.Time) devfixtures.Deps {
	return devfixtures.Deps{Runs: r.runs, Artifacts: r.artifacts, Audit: r.audit, Approvals: r.approvals, Now: now}
}

// stagesByKey reads a run's stages back and keys them by the handle the
// Result mapped to each id.
func stagesByKey(t *testing.T, repos pgRepos, rr devfixtures.RunResult) map[string]*run.Stage {
	t.Helper()
	stages, err := repos.runs.ListStagesForRun(context.Background(), rr.ID)
	if err != nil {
		t.Fatalf("ListStagesForRun: %v", err)
	}
	if len(stages) != len(rr.Stages) {
		t.Fatalf("run %s carries %d stages, Result maps %d", rr.ID, len(stages), len(rr.Stages))
	}
	byID := make(map[uuid.UUID]*run.Stage, len(stages))
	for _, st := range stages {
		byID[st.ID] = st
	}
	out := make(map[string]*run.Stage, len(rr.Stages))
	for key, id := range rr.Stages {
		st, ok := byID[id]
		if !ok {
			t.Fatalf("Result stage %q id %s is not a stage of run %s", key, id, rr.ID)
		}
		out[key] = st
	}
	return out
}

// TestApply_GroomingConfirmGate_ParksHumanReviewAtAwaitingApproval reads
// the done-means shape back: run running; groom (plan, agent) succeeded
// with a grooming_report artifact and one approval; confirm (review,
// human) awaiting_approval; NO implement stage.
func TestApply_GroomingConfirmGate_ParksHumanReviewAtAwaitingApproval(t *testing.T) {
	pool := pgtest.NewPool(t)
	repos := newPGRepos(pool)
	ctx := context.Background()

	res, err := devfixtures.NewApplier(repos.deps(time.Now)).Apply(ctx, "grooming-confirm-gate")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rr, ok := res.Runs["groom-run"]
	if !ok {
		t.Fatalf("Result lacks groom-run: %+v", res)
	}

	r, err := repos.runs.GetRun(ctx, rr.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.State != run.StateRunning || r.WorkflowID != "backlog_grooming" || r.TriggerSource != run.TriggerGitHubIssue ||
		r.TriggerRef == nil || *r.TriggerRef != "issue:1" || len(r.WorkflowSpec) == 0 || r.AccountID != "" {
		t.Errorf("run = state %s workflow %s trigger %s/%v spec %d bytes account %q; want running/backlog_grooming/github_issue/issue:1/non-empty/untenanted",
			r.State, r.WorkflowID, r.TriggerSource, r.TriggerRef, len(r.WorkflowSpec), r.AccountID)
	}

	stages := stagesByKey(t, repos, rr)
	if len(stages) != 2 {
		t.Fatalf("stage set = %d stages, want exactly groom + confirm (no implement)", len(stages))
	}
	for _, st := range stages {
		if st.Type == run.StageTypeImplement {
			t.Errorf("scenario materialized an implement stage %s; the done-means shape has none", st.ID)
		}
	}
	groom := stages["groom"]
	if groom.State != run.StageStateSucceeded || groom.Type != run.StageTypePlan || groom.ExecutorKind != run.ExecutorAgent ||
		groom.Sequence != 1 || !groom.RequiresApproval || groom.EndedAt == nil {
		t.Errorf("groom = %+v, want succeeded plan/agent seq 1 requires_approval with ended_at", groom)
	}
	confirm := stages["confirm"]
	if confirm.State != run.StageStateAwaitingApproval || confirm.Type != run.StageTypeReview || confirm.ExecutorKind != run.ExecutorHuman ||
		confirm.Sequence != 2 || !confirm.RequiresApproval {
		t.Errorf("confirm = %+v, want awaiting_approval review/human seq 2 requires_approval", confirm)
	}

	arts, err := repos.artifacts.ListForStage(ctx, groom.ID)
	if err != nil {
		t.Fatalf("ListForStage(artifacts): %v", err)
	}
	if len(arts) != 1 || arts[0].Kind != artifact.KindGroomingReport ||
		arts[0].SchemaVersion == nil || *arts[0].SchemaVersion != "grooming_report_v1" {
		t.Fatalf("groom artifacts = %+v, want one grooming_report/grooming_report_v1", arts)
	}
	// ContentHash is sha256 over the bytes WRITTEN (the product's own
	// convention — server.sha256Hex over the upload body); the JSONB
	// column re-serializes on read, so the read-back bytes are not the
	// hash input.
	declared := mustLoad(t, "grooming-confirm-gate").Artifacts[0].Content
	sum := sha256.Sum256([]byte(declared))
	if arts[0].ContentHash != hex.EncodeToString(sum[:]) {
		t.Errorf("artifact content_hash = %s, want sha256 of declared content %s", arts[0].ContentHash, hex.EncodeToString(sum[:]))
	}
	if len(arts[0].Content) == 0 {
		t.Errorf("artifact content read back empty")
	}
	if confirmArts, _ := repos.artifacts.ListForStage(ctx, confirm.ID); len(confirmArts) != 0 {
		t.Errorf("confirm carries %d artifacts, want 0", len(confirmArts))
	}

	aps, err := repos.approvals.ListForStage(ctx, groom.ID)
	if err != nil {
		t.Fatalf("ListForStage(approvals): %v", err)
	}
	if len(aps) != 1 || aps[0].Decision != approval.DecisionApprove || aps[0].ApproverSubject != "fixture-operator" ||
		aps[0].Surface != approval.SurfaceAPI || aps[0].Comment == nil {
		t.Errorf("groom approvals = %+v, want one approve by fixture-operator via api with a comment", aps)
	}
	if confirmAps, _ := repos.approvals.ListForStage(ctx, confirm.ID); len(confirmAps) != 0 {
		t.Errorf("confirm carries %d approvals, want 0 (it is PARKED awaiting one)", len(confirmAps))
	}
}

// TestApply_TraceUploadTarget_StageDispatchedWithBackdatedSpendBaseline
// reads the four cost_recorded rows back through audit.Repository with
// the chain intact (prev_hash → entry_hash links AND every entry hash
// recomputing from the stored row), each Timestamp strictly before
// Now.Truncate(1h), and the ages ordered 1h5m < 2h < 3h < 4h.
func TestApply_TraceUploadTarget_StageDispatchedWithBackdatedSpendBaseline(t *testing.T) {
	pool := pgtest.NewPool(t)
	repos := newPGRepos(pool)
	ctx := context.Background()
	now := time.Now().UTC()

	res, err := devfixtures.NewApplier(repos.deps(func() time.Time { return now })).Apply(ctx, "trace-upload-target")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rr := res.Runs["target-run"]
	r, err := repos.runs.GetRun(ctx, rr.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.State != run.StateRunning || r.WorkflowID != "feature_change" {
		t.Errorf("run = %s/%s, want running/feature_change", r.State, r.WorkflowID)
	}
	stages := stagesByKey(t, repos, rr)
	plan := stages["plan"]
	if plan == nil || plan.State != run.StageStateDispatched || plan.Type != run.StageTypePlan || plan.DispatchedAt == nil {
		t.Fatalf("plan stage = %+v, want dispatched plan with dispatched_at", plan)
	}

	entries, err := repos.audit.ListForRun(ctx, rr.ID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	if len(entries) != 4 {
		t.Fatalf("run carries %d audit rows, want exactly the 4 baseline rows", len(entries))
	}
	byCat, err := repos.audit.ListForRunByCategory(ctx, rr.ID, "cost_recorded")
	if err != nil {
		t.Fatalf("ListForRunByCategory: %v", err)
	}
	if len(byCat) != 4 {
		t.Fatalf("cost_recorded rows = %d, want 4", len(byCat))
	}

	// Chain intact: links AND recomputed hashes.
	if entries[0].PrevHash != nil {
		t.Errorf("first entry prev_hash = %v, want nil (genesis)", *entries[0].PrevHash)
	}
	for i, e := range entries {
		if i > 0 && (e.PrevHash == nil || *e.PrevHash != entries[i-1].EntryHash) {
			t.Errorf("entry %d prev_hash = %v, want preceding hash %s", i, e.PrevHash, entries[i-1].EntryHash)
		}
		recomputed, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID: e.RunID, StageID: e.StageID, Timestamp: e.Timestamp, Category: e.Category,
			ActorKind: e.ActorKind, ActorSubject: e.ActorSubject, Payload: e.Payload, PrevHash: e.PrevHash,
		})
		if err != nil {
			t.Fatalf("ComputeEntryHash(entry %d): %v", i, err)
		}
		if recomputed != e.EntryHash {
			t.Errorf("entry %d stored hash %s != recomputed %s (backdated row broke the chain)", i, e.EntryHash, recomputed)
		}
		if e.StageID == nil || *e.StageID != plan.ID {
			t.Errorf("entry %d stage_id = %v, want plan stage %s", i, e.StageID, plan.ID)
		}
		if e.ActorKind == nil || *e.ActorKind != audit.ActorSystem || e.ActorSubject == nil || *e.ActorSubject != "devfixtures" {
			t.Errorf("entry %d actor = %v/%v, want system/devfixtures", i, e.ActorKind, e.ActorSubject)
		}
		if usd := costUSD(t, string(e.Payload)); usd <= 0 {
			t.Errorf("entry %d payload usd = %v, want > 0", i, usd)
		}
	}

	// Backdated: every row strictly before the top of the seed hour,
	// ordered by age 1h5m < 2h < 3h < 4h (sequence order is append order,
	// which is declaration order).
	hourTop := now.Truncate(time.Hour)
	wantAges := []time.Duration{65 * time.Minute, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour}
	for i, e := range entries {
		if !e.Timestamp.Before(hourTop) {
			t.Errorf("entry %d ts %s is not strictly before Now.Truncate(1h) %s", i, e.Timestamp, hourTop)
		}
		// Postgres stores microseconds; compare at that precision.
		want := now.Add(-wantAges[i]).Truncate(time.Microsecond)
		if !e.Timestamp.Equal(want) {
			t.Errorf("entry %d ts = %s, want Now−%s = %s", i, e.Timestamp, wantAges[i], want)
		}
		if i > 0 && !e.Timestamp.Before(entries[i-1].Timestamp) {
			t.Errorf("entry %d ts %s is not older than entry %d ts %s", i, e.Timestamp, i-1, entries[i-1].Timestamp)
		}
	}
}

// TestApply_PlanGateParked_PlanAwaitingApprovalWithPlanArtifact reads
// back a plan stage parked at awaiting_approval carrying one standard_v1
// plan artifact and no approvals.
func TestApply_PlanGateParked_PlanAwaitingApprovalWithPlanArtifact(t *testing.T) {
	pool := pgtest.NewPool(t)
	repos := newPGRepos(pool)
	ctx := context.Background()

	res, err := devfixtures.NewApplier(repos.deps(time.Now)).Apply(ctx, "plan-gate-parked")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	rr := res.Runs["parked-run"]
	r, err := repos.runs.GetRun(ctx, rr.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if r.State != run.StateRunning {
		t.Errorf("run state = %s, want running", r.State)
	}
	stages := stagesByKey(t, repos, rr)
	if len(stages) != 1 {
		t.Fatalf("stage set = %d, want 1", len(stages))
	}
	plan := stages["plan"]
	if plan.State != run.StageStateAwaitingApproval || plan.Type != run.StageTypePlan || !plan.RequiresApproval || plan.StartedAt == nil {
		t.Fatalf("plan stage = %+v, want awaiting_approval plan requires_approval with started_at", plan)
	}
	arts, err := repos.artifacts.ListForStage(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ListForStage(artifacts): %v", err)
	}
	if len(arts) != 1 || arts[0].Kind != artifact.KindPlan || arts[0].SchemaVersion == nil || *arts[0].SchemaVersion != "standard_v1" {
		t.Fatalf("plan artifacts = %+v, want one plan/standard_v1", arts)
	}
	aps, err := repos.approvals.ListForStage(ctx, plan.ID)
	if err != nil {
		t.Fatalf("ListForStage(approvals): %v", err)
	}
	if len(aps) != 0 {
		t.Errorf("parked plan carries %d approvals, want 0", len(aps))
	}
	if entries, _ := repos.audit.ListForRun(ctx, rr.ID); len(entries) != 0 {
		t.Errorf("plan-gate-parked wrote %d audit rows, want 0", len(entries))
	}
}

// TestApply_FreshRowsPerCall applies the same scenario twice and asserts
// disjoint run and stage ids, both runs readable, and each run's audit
// chain independent (the second run's genesis row has a nil prev_hash).
func TestApply_FreshRowsPerCall(t *testing.T) {
	pool := pgtest.NewPool(t)
	repos := newPGRepos(pool)
	ctx := context.Background()
	a := devfixtures.NewApplier(repos.deps(time.Now))

	first, err := a.Apply(ctx, "trace-upload-target")
	if err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	second, err := a.Apply(ctx, "trace-upload-target")
	if err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	r1, r2 := first.Runs["target-run"], second.Runs["target-run"]
	if r1.ID == r2.ID {
		t.Fatalf("both applies returned run id %s; every call must mint fresh rows", r1.ID)
	}
	if r1.Stages["plan"] == r2.Stages["plan"] {
		t.Fatalf("both applies returned stage id %s", r1.Stages["plan"])
	}
	for _, rr := range []devfixtures.RunResult{r1, r2} {
		if _, err := repos.runs.GetRun(ctx, rr.ID); err != nil {
			t.Errorf("GetRun(%s): %v", rr.ID, err)
		}
		entries, err := repos.audit.ListForRun(ctx, rr.ID)
		if err != nil {
			t.Fatalf("ListForRun(%s): %v", rr.ID, err)
		}
		if len(entries) != 4 || entries[0].PrevHash != nil {
			t.Errorf("run %s: %d entries, genesis prev_hash %v; want 4 rows chained from a nil genesis", rr.ID, len(entries), entries[0].PrevHash)
		}
	}
}
