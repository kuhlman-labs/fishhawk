package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// --- fixtures -------------------------------------------------------------

const (
	amendCoupledPath  = "backend/internal/server/prompt.go"
	amendHeldSHA      = "1111111111111111111111111111111111111111"
	amendRunBranch    = "fishhawk/run-parent/slice-0"
	amendVerifiedTree = "2222222222222222222222222222222222222222"
	amendConsolidated = "fishhawk/run-parent"
)

// amendFixture is the state an `amend` decision acts on: a DECOMPOSED CHILD
// whose implement stage is parked on a #2548 BUILD-REQUIRED shortfall — the
// class whose `exempt` is refused outright, so before #2591 its only resolution
// was `fail` — under a REAL approved decomposition on its parent (#4798). The
// child is locked local, and every other slice has a minted sibling run.
type amendFixture struct {
	s         *Server
	rr        *orchestratorRepo
	au        *auditFake
	sa        *fakeScopeAmendmentRepo
	art       *fakeArtifactRepo
	parent    *run.Run
	planStage *run.Stage
	child     *run.Run
	stage     *run.Stage
	// sibling and sibImpl are slice 1's run and implement stage.
	sibling  *run.Run
	sibImpl  *run.Stage
	siblings map[int]*run.Run
	sibImpls map[int]*run.Stage
}

// newAmendFixture is the amend mechanics shape: slice 0 depends_on [1], slice 1
// declares the coupled path and is a DELIVERED ancestor (run and implement
// succeeded, named by the parent's newest slices_integrated), so the mechanics
// tests keep their 200 on production shape.
func newAmendFixture(t *testing.T) *amendFixture {
	t.Helper()
	f := newAmendFixtureWithPlan(t, 0, ownershipSlices(
		ownSpec{files: []string{"backend/internal/server/own.go"}, deps: []int{1}},
		ownSpec{files: []string{amendCoupledPath}},
	))
	f.deliver(1)
	f.integrate(amendConsolidated, 1)
	return f
}

// newLegacyAmendFixture is the only shape left to the #2591 not-started and
// acknowledgement branch: a plan with no depends_on edge whose slice 1 does NOT
// declare the coupled path, while the park still attributes it there.
func newLegacyAmendFixture(t *testing.T) *amendFixture {
	t.Helper()
	return newAmendFixtureWithPlan(t, 0, ownershipSlices(
		ownSpec{files: []string{"backend/internal/server/own.go"}},
		ownSpec{files: []string{"backend/internal/server/other.go"}},
	))
}

// newAmendFixtureWithPlan seeds subPlans as the parent's approved decomposition
// and parks the child at childSlice. No sibling has started.
func newAmendFixtureWithPlan(t *testing.T, childSlice int, subPlans []plan.SubPlanSummary) *amendFixture {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newAuditFake()
	sa := newFakeScopeAmendmentRepo()
	art := newFakeArtifactRepo()

	parent := rr.seedRun()
	planStage := rr.seedStage(parent.ID, 0, run.StageStateSucceeded)
	f := &amendFixture{rr: rr, au: au, sa: sa, art: art, parent: parent, planStage: planStage,
		siblings: map[int]*run.Run{}, sibImpls: map[int]*run.Stage{}}
	f.seedParentPlan(t, &plan.Plan{
		PlanVersion:   "standard_v1",
		Summary:       "decomposed plan",
		Verification:  plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Decomposition: &plan.Decomposition{Rationale: "split", SubPlans: subPlans},
	})

	parentID := parent.ID
	child := rr.seedRun()
	idx := childSlice
	child.DecomposedFrom = &parentID
	child.ParentRunID = &parentID
	child.SliceIndex = &idx
	child.RunnerKind = run.RunnerKindLocal
	child.RunnerKindResolved = true

	slice1 := 1
	stage := rr.seedStage(child.ID, 1, run.StageStateAwaitingScopeDecision)
	stage.Type = run.StageTypeImplement
	stage.ScopeCompletenessPark = &run.ScopeCompletenessPark{
		HeldCommitSHA:      amendHeldSHA,
		RunBranch:          amendRunBranch,
		VerifiedTreeSHA:    amendVerifiedTree,
		BaseSHA:            "3333333333333333333333333333333333333333",
		BuildRequiredPaths: []string{amendCoupledPath},
		OwningSlices: []run.BuildRequiredOwner{
			{Path: amendCoupledPath, SliceIndex: &slice1, SliceTitle: "prompt plumbing"},
		},
	}
	f.child, f.stage = child, stage

	for i := range subPlans {
		if i == childSlice {
			continue
		}
		sib := rr.seedRun()
		sliceIdx := i
		sib.DecomposedFrom = &parentID
		sib.ParentRunID = &parentID
		sib.SliceIndex = &sliceIdx
		impl := rr.seedStage(sib.ID, 1, run.StageStatePending)
		impl.Type = run.StageTypeImplement // StartedAt nil: the sibling has not begun
		f.siblings[i], f.sibImpls[i] = sib, impl
	}
	f.sibling, f.sibImpl = f.siblings[1], f.sibImpls[1]

	f.s = New(Config{Addr: "127.0.0.1:0", RunRepo: newScopeParkDeciderRepo(rr, au), AuditRepo: au,
		ScopeAmendmentRepo: sa, ArtifactRepo: art})
	return f
}

// seedParentPlan replaces the parent's approved plan artifact.
func (f *amendFixture) seedParentPlan(t *testing.T, p *plan.Plan) {
	t.Helper()
	f.art.all = nil
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	sv := "standard_v1"
	if _, err := f.art.Create(context.Background(), artifact.CreateParams{
		StageID: f.planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: body,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
}

// deliver marks a sibling's run and implement stage succeeded with a start stamp.
func (f *amendFixture) deliver(idx int) {
	started := time.Now().UTC().Add(-time.Hour)
	f.siblings[idx].State = run.StateSucceeded
	f.sibImpls[idx].State = run.StageStateSucceeded
	f.sibImpls[idx].StartedAt = &started
}

// setRunning marks a sibling's run and implement stage in flight.
func (f *amendFixture) setRunning(idx int) {
	started := time.Now().UTC().Add(-time.Minute)
	f.siblings[idx].State = run.StateRunning
	f.sibImpls[idx].State = run.StageStateRunning
	f.sibImpls[idx].StartedAt = &started
}

// integrate makes the parent's newest slices_integrated name branch and these
// slices' runs.
func (f *amendFixture) integrate(branch string, slices ...int) {
	ids := make([]string, 0, len(slices))
	for _, i := range slices {
		ids = append(ids, f.siblings[i].ID.String())
	}
	f.integrateIDs(branch, ids...)
}

// integrateIDs is integrate with literal child run ids.
func (f *amendFixture) integrateIDs(branch string, ids ...string) {
	payload, _ := json.Marshal(map[string]any{"consolidated_branch": branch, "child_run_ids": ids})
	f.seedIntegrationPayload(payload)
}

// seedIntegrationPayload seeds the raw newest slices_integrated payload.
func (f *amendFixture) seedIntegrationPayload(payload json.RawMessage) {
	rid := f.parent.ID
	f.au.seeded = []*audit.Entry{{RunID: &rid, Category: "slices_integrated", Payload: payload}}
}

// scopeAmendBody renders a decision body naming the coupled path.
func scopeAmendBody(reason string) string {
	return `{"decision":"amend","reason":"` + reason + `","paths":[{"path":"` + amendCoupledPath + `","operation":"modify"}]}`
}

// postAmend drives the REAL decision endpoint with an operator identity.
func postAmend(t *testing.T, f *amendFixture, body string) *httptest.ResponseRecorder {
	t.Helper()
	return postScopeCompletenessDecision(t, f.s, f.child.ID, body, operatorWriteStages)
}

// amendedEntries returns this stage's scope_completeness_amended audit entries.
func (f *amendFixture) amendedEntries() []audit.ChainAppendParams {
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, e := range f.au.appended {
		if e.Category == CategoryScopeCompletenessAmended {
			out = append(out, e)
		}
	}
	return out
}

// approvedRows returns the APPROVED scope-amendment rows on the parked stage —
// the durable effect the budget counts.
func (f *amendFixture) approvedRows(t *testing.T) []*scopeamendment.Amendment {
	t.Helper()
	all, err := f.sa.ListByRun(context.Background(), f.child.ID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	var out []*scopeamendment.Amendment
	for _, a := range all {
		if a.StageID == f.stage.ID && a.Status == scopeamendment.StatusApproved {
			out = append(out, a)
		}
	}
	return out
}

// pendingRows returns the PENDING scope-amendment rows on the parked stage —
// the orphan shape a failed auto-approve leaves behind.
func (f *amendFixture) pendingRows(t *testing.T) []*scopeamendment.Amendment {
	t.Helper()
	all, err := f.sa.ListByRun(context.Background(), f.child.ID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	var out []*scopeamendment.Amendment
	for _, a := range all {
		if a.StageID == f.stage.ID && a.Status == scopeamendment.StatusPending {
			out = append(out, a)
		}
	}
	return out
}

// repark puts the stage back in awaiting_scope_decision on a NEW park episode:
// the same shortfall, but the HELD COMMIT differs, which is what a re-run under
// the widened scope produces.
func (f *amendFixture) repark(t *testing.T, heldSHA string) {
	t.Helper()
	next := *f.stage.ScopeCompletenessPark
	next.HeldCommitSHA = heldSHA
	f.stage.ScopeCompletenessPark = &next
	f.stage.State = run.StageStateAwaitingScopeDecision
}

// refusedRows returns the scope_amendment_refused entries on the child.
func (f *amendFixture) refusedRows() []audit.ChainAppendParams {
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	var out []audit.ChainAppendParams
	for _, e := range f.au.appended {
		if e.Category == CategoryScopeAmendmentRefused {
			out = append(out, e)
		}
	}
	return out
}

// parkSnapshot captures the persisted park so assertAmendUntouched can compare it.
func (f *amendFixture) parkSnapshot(t *testing.T) string {
	t.Helper()
	got, err := f.rr.GetStage(context.Background(), f.stage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	raw, err := json.Marshal(got.ScopeCompletenessPark)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// assertAmendUntouched is what every #4798 refusal must leave: no amendment row
// of ANY status, the stage still parked, the park byte-equal to its pre-call
// snapshot and no scope_completeness_amended entry, so no cap slot was spent.
func (f *amendFixture) assertAmendUntouched(t *testing.T, parkBefore string) {
	t.Helper()
	got, err := f.rr.GetStage(context.Background(), f.stage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage state = %q, want it left parked", got.State)
	}
	all, err := f.sa.ListByRun(context.Background(), f.child.ID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(all) != 0 {
		t.Errorf("%d amendment row(s) of any status, want 0", len(all))
	}
	if after := f.parkSnapshot(t); after != parkBefore {
		t.Errorf("park changed:\n got: %s\nwant: %s", after, parkBefore)
	}
	if entries := f.amendedEntries(); len(entries) != 0 {
		t.Errorf("%d scope_completeness_amended entr(ies), want 0", len(entries))
	}
}

// assertNoSideEffects is the shape every amend REFUSAL must present: the stage
// is still parked, no amendment row exists, and no amended-kind audit entry was
// written. It reads COMMITTED STATE, not the error identity — a control that
// fires and is rolled back returns a byte-identical error.
func (f *amendFixture) assertNoSideEffects(t *testing.T, why string) {
	t.Helper()
	got, err := f.rr.GetStage(context.Background(), f.stage.ID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("%s: stage state = %q, want it left parked in awaiting_scope_decision", why, got.State)
	}
	if rows := f.approvedRows(t); len(rows) != 0 {
		t.Errorf("%s: %d approved amendment row(s) written, want 0", why, len(rows))
	}
	if entries := f.amendedEntries(); len(entries) != 0 {
		t.Errorf("%s: %d scope_completeness_amended audit entr(ies) written, want 0", why, len(entries))
	}
}

// --- done-means -----------------------------------------------------------

// TestAmendScopeCompleteness_BuildRequiredParkResumes is the issue's headline
// done-means: the BUILD-REQUIRED class — for which `exempt` is refused because
// the held tree is red by construction — now RESOLVES and continues instead of
// having `fail` as its only option. The stage returns to `pending` (not
// `failed`), the widening is on a pre-approved #961 row for this stage, and the
// amended audit entry carries the paths + amendment id.
//
// It ALSO carries binding condition 3's held-commit preservation assertion,
// read from the PERSISTED park before and after the amend rather than proxied
// through the absence of a prompt key: preserving the held commit on the
// slice's sole-writer branch (ADR-035) is a blocking criterion of its own.
func TestAmendScopeCompleteness_BuildRequiredParkResumes(t *testing.T) {
	f := newAmendFixture(t)
	notifier := &pageClassRecorder{}
	f.s.issueNotifier = notifier

	before, err := f.rr.GetStage(context.Background(), f.stage.ID)
	if err != nil {
		t.Fatalf("GetStage before: %v", err)
	}
	heldBefore, branchBefore := before.ScopeCompletenessPark.HeldCommitSHA, before.ScopeCompletenessPark.RunBranch

	w := postAmend(t, f, scopeAmendBody("the coupled prompt.go is build-required by this slice"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}

	// (1) RESOLVED AND CONTINUES: pending, not failed.
	got, err := f.rr.GetStage(context.Background(), f.stage.ID)
	if err != nil {
		t.Fatalf("GetStage after: %v", err)
	}
	if got.State != run.StageStatePending {
		t.Fatalf("stage state = %q, want pending — the whole point is that this class no longer has to fail", got.State)
	}
	if !dispatchAdmissible(got.State) {
		t.Errorf("stage state = %q, want a DISPATCH-ADMISSIBLE state", got.State)
	}
	if got.FailureCategory != nil {
		t.Errorf("stage failure category = %v, want nil (amend never fails the stage)", got.FailureCategory)
	}

	// (2) CONDITION 3 — the held commit is REUSED, not discarded. Both fields
	// byte-identical across the amend: the slice's work stays exactly where it
	// was pushed, on its own sole-writer run branch (ADR-035).
	if got.ScopeCompletenessPark == nil {
		t.Fatal("the park record must survive the amend — it is the durable record of what was parked")
	}
	if got.ScopeCompletenessPark.HeldCommitSHA != heldBefore {
		t.Errorf("park held_commit_sha = %q, want it byte-identical to the pre-amend %q",
			got.ScopeCompletenessPark.HeldCommitSHA, heldBefore)
	}
	if got.ScopeCompletenessPark.RunBranch != branchBefore {
		t.Errorf("park run_branch = %q, want it byte-identical to the pre-amend %q",
			got.ScopeCompletenessPark.RunBranch, branchBefore)
	}

	// (3) The widening is a pre-approved #961 row on THIS stage.
	rows := f.approvedRows(t)
	if len(rows) != 1 {
		t.Fatalf("%d approved amendment rows, want exactly 1", len(rows))
	}
	if len(rows[0].Paths) != 1 || rows[0].Paths[0].Path != amendCoupledPath ||
		rows[0].Paths[0].Operation != scopeamendment.OperationModify {
		t.Errorf("amendment row paths = %+v, want the coupled path with its declared operation", rows[0].Paths)
	}

	// (4) The amended audit entry carries the paths + the row id.
	entries := f.amendedEntries()
	if len(entries) != 1 {
		t.Fatalf("%d scope_completeness_amended entries, want exactly 1", len(entries))
	}
	var payload map[string]any
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["amendment_id"] != rows[0].ID.String() {
		t.Errorf("audit amendment_id = %v, want the created row %s", payload["amendment_id"], rows[0].ID)
	}
	if payload["decision"] != "amended" {
		t.Errorf("audit decision = %v, want amended", payload["decision"])
	}
	rawPaths, _ := json.Marshal(payload["paths"])
	if !strings.Contains(string(rawPaths), amendCoupledPath) {
		t.Errorf("audit paths = %s, want the amended path", rawPaths)
	}

	// (5) Response echoes the widening and the TRUE cost to the re-run agent.
	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Decision != "amend" || resp.State != string(run.StageStatePending) {
		t.Errorf("response = %+v, want amend/pending", resp)
	}
	if resp.AmendmentID != rows[0].ID.String() {
		t.Errorf("response amendment_id = %q, want %s", resp.AmendmentID, rows[0].ID)
	}
	if len(resp.AmendedPaths) != 1 || resp.AmendedPaths[0].Path != amendCoupledPath {
		t.Errorf("response amended_paths = %+v, want the coupled path", resp.AmendedPaths)
	}
	// The operator amend consumed ONE of the stage's TWO #961 slots.
	if resp.AgentAmendmentBudgetRemaining != maxScopeAmendmentsPerStage-1 {
		t.Errorf("agent_amendment_budget_remaining = %d, want %d — the operator amend consumes one row-counted slot",
			resp.AgentAmendmentBudgetRemaining, maxScopeAmendmentsPerStage-1)
	}
	// The RESOLVED owning slice is named, so the operator knows which sibling
	// will see the same file on another branch.
	if len(resp.OwningSlices) != 1 || resp.OwningSlices[0].Path != amendCoupledPath {
		t.Errorf("response owning_slices = %+v, want the resolved attribution echoed", resp.OwningSlices)
	}
	if resp.OwnerAttributionUnresolved || resp.AcknowledgedOwnerUnresolved {
		t.Errorf("a fully RESOLVED amend must need no acknowledgement; got %+v", resp)
	}

	// (6) CONDITION 5(b): the status-update channel is driven on the amend arm.
	// (`source` is a free-form log label, so the pinned fact is that the
	// notification FIRES for this run — not the string.)
	if len(notifier.status) != 1 || notifier.status[0] != f.child.ID {
		t.Errorf("status notifications = %v, want exactly one for the amended run %s", notifier.status, f.child.ID)
	}
}

// --- cross-boundary -------------------------------------------------------

// TestAmendScopeCompleteness_WidensPromptScopeEndToEnd is the CROSS-BOUNDARY
// test (#624/#627): the change spans the HTTP handler, the scopeamendment
// store, the stage row and the prompt builder, and per-layer units would each
// pass while the seam between them broke. It drives the REAL decision endpoint
// against pgtest-backed repositories, then fetches the re-dispatched implement
// stage's prompt through the REAL signed /prompt endpoint and asserts:
//
//	(a) scope_files carries the amended path WITH its declared operation, and
//	(b) open_pr_from_held_commit is ABSENT — an amend means "do NOT ship this
//	    tree", so the runner must take its ordinary agent path.
func TestAmendScopeCompleteness_WidensPromptScopeEndToEnd(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()

	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	signingRepo := signing.NewPostgresRepository(pool)
	amendRepo := scopeamendment.NewPostgresRepository(pool)
	art := newFakeArtifactRepo()

	realRun, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo:          "kuhlman-labs/fishhawk",
		WorkflowID:    "feature_change",
		WorkflowSHA:   "deadbeef",
		TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	planStage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: realRun.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create plan stage: %v", err)
	}
	implStage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: realRun.ID, Sequence: 1, Type: run.StageTypeImplement,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create implement stage: %v", err)
	}
	// The approved plan's scope: the amendment must widen it, not replace it.
	planBytes, err := json.Marshal(&plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "sliced plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "backend/internal/server/scope_completeness.go", Operation: plan.FileOpModify},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sv := "standard_v1"
	if _, err := art.Create(ctx, artifact.CreateParams{
		StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if _, err := runRepo.TransitionStage(ctx, implStage.ID, to, nil); err != nil {
			t.Fatalf("transition implement stage to %s: %v", to, err)
		}
	}
	key, err := signingRepo.Issue(ctx, realRun.ID, signing.DefaultTTL)
	if err != nil {
		t.Fatalf("issue signing key: %v", err)
	}

	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            runRepo,
		AuditRepo:          auditRepo,
		SigningRepo:        signingRepo,
		ArtifactRepo:       art,
		ScopeAmendmentRepo: amendRepo,
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}

	// Park the stage through the REAL runner report, so the park row this test
	// amends is the one the product writes.
	parkBytes := []byte(`{"outcome":"scope_park","branch":"` + amendRunBranch + `",` +
		`"head_sha":"` + amendHeldSHA + `","base_sha":"3333333333333333333333333333333333333333",` +
		`"verified_tree_sha":"` + amendVerifiedTree + `",` +
		`"missing_paths":["` + amendCoupledPath + `"]}`)
	pw := shipPRRequest(t, s, realRun.ID, implStage.ID, key.PrivateKey, parkBytes, "")
	if pw.Code != http.StatusOK {
		t.Fatalf("scope_park report status = %d, want 200:\n%s", pw.Code, pw.Body.String())
	}
	parked, err := runRepo.GetStage(ctx, implStage.ID)
	if err != nil {
		t.Fatalf("GetStage after park: %v", err)
	}
	heldBefore := parked.ScopeCompletenessPark.HeldCommitSHA
	branchBefore := parked.ScopeCompletenessPark.RunBranch

	// The operator amends. This run is NOT a decomposition child, so no sibling
	// slice can exist and ownership is resolved by construction — no
	// acknowledgement is required.
	dw := postScopeCompletenessDecision(t, s, realRun.ID,
		`{"decision":"amend","reason":"the declared path needs the coupled file to compile","paths":[{"path":"`+
			amendCoupledPath+`","operation":"modify"}]}`,
		operatorWriteStages)
	if dw.Code != http.StatusOK {
		t.Fatalf("amend status = %d, want 200; body = %s", dw.Code, dw.Body.String())
	}

	// CONDITION 3 through REAL Postgres: the persisted park's held commit and
	// run branch are byte-identical across the amend.
	resolved, err := runRepo.GetStage(ctx, implStage.ID)
	if err != nil {
		t.Fatalf("GetStage after amend: %v", err)
	}
	if resolved.ScopeCompletenessPark == nil ||
		resolved.ScopeCompletenessPark.HeldCommitSHA != heldBefore ||
		resolved.ScopeCompletenessPark.RunBranch != branchBefore {
		t.Fatalf("persisted park after amend = %+v, want held commit %q on branch %q unchanged",
			resolved.ScopeCompletenessPark, heldBefore, branchBefore)
	}
	if !dispatchAdmissible(resolved.State) {
		t.Fatalf("resolved stage state = %q, want dispatch-admissible", resolved.State)
	}

	// The re-dispatched runner's prompt.
	prw := promptRequest(t, s, realRun.ID, implStage.ID, key.PrivateKey, "")
	if prw.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", prw.Code, prw.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(prw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode prompt: %v", err)
	}
	var amended *scopeFile
	for i := range resp.ScopeFiles {
		if resp.ScopeFiles[i].Path == amendCoupledPath {
			amended = &resp.ScopeFiles[i]
		}
	}
	if amended == nil {
		t.Fatalf("amended path missing from the re-dispatched prompt's scope_files: %+v", resp.ScopeFiles)
	}
	if amended.Operation != string(plan.FileOpModify) {
		t.Errorf("amended scope file operation = %q, want the declared %q preserved across the seam",
			amended.Operation, plan.FileOpModify)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(prw.Body.Bytes(), &keys); err != nil {
		t.Fatal(err)
	}
	if raw, ok := keys["open_pr_from_held_commit"]; ok {
		t.Errorf("open_pr_from_held_commit present (%s) after an amend: the runner must RE-RUN the agent "+
			"against the widened scope, not short-circuit to a PR from the held commit", raw)
	}
}

// --- per-failure-mode: the refusals ---------------------------------------

// TestAmend_IncompleteCoverage409 (counterfactual: the coverage check). A
// build-required park amended with paths that omit one build_required_path
// would re-park identically and burn a full agent pass. Bad state is seeded BY
// CONSTRUCTION — the park's BuildRequiredPaths literally exceed the submitted
// set — so the RED lands on the behavioral assertion, not on fixture setup.
func TestAmend_IncompleteCoverage409(t *testing.T) {
	f := newAmendFixture(t)
	slice1 := 1
	f.stage.ScopeCompletenessPark.BuildRequiredPaths = []string{amendCoupledPath, "backend/internal/server/reads.go"}
	f.stage.ScopeCompletenessPark.OwningSlices = append(f.stage.ScopeCompletenessPark.OwningSlices,
		run.BuildRequiredOwner{Path: "backend/internal/server/reads.go", SliceIndex: &slice1, SliceTitle: "prompt plumbing"})

	w := postAmend(t, f, scopeAmendBody("widen only the first path"))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amend_incomplete_coverage") {
		t.Errorf("body must name amend_incomplete_coverage: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "reads.go") {
		t.Errorf("body must NAME the uncovered path: %s", w.Body.String())
	}
	f.assertNoSideEffects(t, "an incompletely-covering amend")
}

// TestAmend_OwnerSliceActive409 (counterfactual: the owner-slice guard). The
// sibling the PARK attributes the coupled path to, on a path the plan does not
// declare, has already begun its implement pass, so from that moment two
// branches can carry divergent edits to one file.
//
// One arm per DISJUNCT of the refusal predicate — `StartedAt != nil ||
// State == succeeded` — each seeded BY CONSTRUCTION at fixture time. The
// succeeded-with-nil-StartedAt arm is not hypothetical bookkeeping: it is the
// exact combination (a legacy or manually-transitioned row) the second disjunct
// exists for, and without it deleting that disjunct leaves the suite green.
func TestAmend_OwnerSliceActive409(t *testing.T) {
	started := time.Now().UTC().Add(-time.Minute)
	for _, tc := range []struct {
		name  string
		setup func(*amendFixture)
		why   string
	}{
		{
			name: "StartedAtSet",
			setup: func(f *amendFixture) {
				f.sibImpl.StartedAt = &started
				f.sibImpl.State = run.StageStateRunning
			},
			why: "an amend whose owning sibling slice is already running",
		},
		{
			name: "SucceededWithNilStartedAt",
			setup: func(f *amendFixture) {
				f.sibImpl.StartedAt = nil // the row carries no start stamp...
				f.sibImpl.State = run.StageStateSucceeded
			},
			why: "an amend whose owning sibling slice has already SUCCEEDED with no start stamp",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLegacyAmendFixture(t)
			tc.setup(f)

			w := postAmend(t, f, scopeAmendBody("widen despite the active sibling"))
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, "amend_refused_owner_slice_active") {
				t.Errorf("body must name amend_refused_owner_slice_active: %s", body)
			}
			for _, want := range []string{`"slice_index":1`, "prompt plumbing", f.sibling.ID.String()} {
				if !strings.Contains(body, want) {
					t.Errorf("details must carry %q so the operator knows WHICH boundary is live: %s", want, body)
				}
			}
			f.assertNoSideEffects(t, tc.why)
		})
	}
}

// TestAmend_UndeclaredParkAttributionNotStartedResolved is the OTHER side of the
// StartedAt predicate, on the only path left to it (#4798): a path the plan does
// not declare whose park-attributed owner has not begun cannot hold a divergent
// edit, so the amend proceeds with NO acknowledgement and the owning slice is
// named. A plan-declared non-ancestor never gets here: TestAmend_SliceOwnership.
func TestAmend_UndeclaredParkAttributionNotStartedResolved(t *testing.T) {
	f := newLegacyAmendFixture(t) // sibling implement stage: StartedAt nil, state pending

	w := postAmend(t, f, scopeAmendBody("the owning slice has not started"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OwnerAttributionUnresolved || resp.AcknowledgedOwnerUnresolved {
		t.Errorf("a resolved-and-not-started owner must need no acknowledgement; got %+v", resp)
	}
	if len(resp.OwningSlices) != 1 || resp.OwningSlices[0].SliceIndex == nil || *resp.OwningSlices[0].SliceIndex != 1 {
		t.Errorf("response must NAME the owning slice so the operator knows to drop the file from it: %+v", resp.OwningSlices)
	}
	entries := f.amendedEntries()
	if len(entries) != 1 {
		t.Fatalf("%d amended entries, want 1", len(entries))
	}
	var payload map[string]any
	_ = json.Unmarshal(entries[0].Payload, &payload)
	if _, ok := payload["owning_slices"]; !ok {
		t.Errorf("the amended audit entry must carry owning_slices: %v", payload)
	}
	if _, ok := payload["acknowledged_owner_unresolved"]; ok {
		t.Errorf("a fully resolved amend must record NO acknowledgement: %v", payload)
	}
}

// TestAmend_OwnerAttributionUnresolvedFailsClosed (binding condition 1,
// counterfactual: the unresolved-refusal branch). Unknown ownership is exactly
// the case in which a sibling might already hold divergent edits, so recording
// the risk is not preventing it: the amend is REFUSED with no side effects.
//
// Each arm seeds a DISTINCT unresolvable state by construction.
func TestAmend_OwnerAttributionUnresolvedFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*amendFixture)
		wantReason string
	}{
		{
			name: "NoOwningSlicesEntry",
			setup: func(f *amendFixture) {
				f.stage.ScopeCompletenessPark.OwningSlices = nil
			},
			wantReason: amendOwnerUnresolvedNoEntry,
		},
		{
			name: "NilSliceIndex",
			setup: func(f *amendFixture) {
				f.stage.ScopeCompletenessPark.OwningSlices = []run.BuildRequiredOwner{{Path: amendCoupledPath}}
			},
			wantReason: amendOwnerUnresolvedNoSliceIndex,
		},
		{
			name: "OwningSliceRunAbsent",
			setup: func(f *amendFixture) {
				f.sibling.SliceIndex = nil // the slice-1 child cannot be identified
			},
			wantReason: amendOwnerUnresolvedSiblingMissing,
		},
		{
			name: "OwningSliceHasNoImplementStage",
			setup: func(f *amendFixture) {
				f.sibImpl.Type = run.StageTypeReview
			},
			wantReason: amendOwnerUnresolvedNoImplement,
		},
		{
			// The sibling's stage list is unreadable, so whether its implement
			// pass has begun is unknown — the same uncertainty, reached through a
			// repository seam rather than a fixture field.
			name: "OwningSliceStagesUnreadable",
			setup: func(f *amendFixture) {
				f.s.cfg.RunRepo = &listStagesErrRepo{orchestratorRepo: f.rr, failFor: f.sibling.ID}
			},
			wantReason: amendOwnerUnresolvedStagesUnread,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLegacyAmendFixture(t)
			tc.setup(f)

			w := postAmend(t, f, scopeAmendBody("widen with unresolvable ownership"))
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409 — unknown ownership must FAIL CLOSED, not proceed with a recorded risk; body = %s",
					w.Code, w.Body.String())
			}
			body := w.Body.String()
			if !strings.Contains(body, "amend_owner_attribution_unresolved") {
				t.Errorf("body must name amend_owner_attribution_unresolved: %s", body)
			}
			if !strings.Contains(body, amendCoupledPath) {
				t.Errorf("details must NAME each unresolved path: %s", body)
			}
			if !strings.Contains(body, tc.wantReason) {
				t.Errorf("details must name the REASON %q so the operator knows what they would be acknowledging: %s",
					tc.wantReason, body)
			}
			f.assertNoSideEffects(t, "an amend with unresolvable ownership")
		})
	}
}

// listRunsErrRepo makes the sibling lookup fail — the fourth unresolvable arm,
// which needs a repository seam rather than a fixture field.
type listRunsErrRepo struct {
	*orchestratorRepo
}

func (r *listRunsErrRepo) ListRuns(context.Context, run.ListRunsFilter) ([]*run.Run, error) {
	return nil, errors.New("sibling listing unavailable")
}

// listStagesErrRepo makes ListStagesForRun fail for ONE run — the OWNING
// SIBLING. Scoping it that way is load-bearing: the decision handler resolves
// the parked stage through the same call on the AMENDING child, so a repo that
// failed for every run would refuse upstream (500 internal_error) and the
// owner-guard branch under test would never be reached.
type listStagesErrRepo struct {
	*orchestratorRepo
	failFor uuid.UUID
}

func (r *listStagesErrRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if runID == r.failFor {
		return nil, errors.New("stage listing unavailable")
	}
	return r.orchestratorRepo.ListStagesForRun(ctx, runID)
}

// TestAmend_OwnerListRunsErrorFailsClosed: an unreadable sibling list is not
// evidence that no sibling is active, so it takes the same fail-CLOSED path.
func TestAmend_OwnerListRunsErrorFailsClosed(t *testing.T) {
	f := newLegacyAmendFixture(t)
	f.s.cfg.RunRepo = &listRunsErrRepo{f.rr}

	w := postAmend(t, f, scopeAmendBody("widen with an unreadable sibling list"))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "amend_owner_attribution_unresolved") {
		t.Errorf("body must name amend_owner_attribution_unresolved: %s", body)
	}
	if !strings.Contains(body, amendOwnerUnresolvedSiblingLookup) {
		t.Errorf("details must name the sibling-lookup failure as the reason: %s", body)
	}
	f.assertNoSideEffects(t, "an amend whose sibling lookup failed")
}

// TestAmend_UnresolvedAcknowledgementReadmitsAndIsRecorded is the ESCAPE HATCH
// (binding condition 1): the explicit flag re-admits the refused arm, and the
// acknowledgement is recorded on BOTH the response and the audit entry so it is
// an AUDITED operator decision rather than a silent default. Both a
// fixture-seeded unresolvable arm and the repository-seam arm are covered.
func TestAmend_UnresolvedAcknowledgementReadmitsAndIsRecorded(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setup      func(*amendFixture)
		wantReason string
	}{
		{"NoOwningSlicesEntry", func(f *amendFixture) { f.stage.ScopeCompletenessPark.OwningSlices = nil },
			amendOwnerUnresolvedNoEntry},
		{"SiblingLookupFailed", func(f *amendFixture) { f.s.cfg.RunRepo = &listRunsErrRepo{f.rr} },
			amendOwnerUnresolvedSiblingLookup},
		{"OwningSliceStagesUnreadable",
			func(f *amendFixture) {
				f.s.cfg.RunRepo = &listStagesErrRepo{orchestratorRepo: f.rr, failFor: f.sibling.ID}
			},
			amendOwnerUnresolvedStagesUnread},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLegacyAmendFixture(t)
			tc.setup(f)

			w := postAmend(t, f, `{"decision":"amend","reason":"accepting the sibling-conflict risk knowingly",`+
				`"acknowledge_owner_unresolved":true,"paths":[{"path":"`+amendCoupledPath+`","operation":"modify"}]}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 — the acknowledgement must re-admit the refused arm; body = %s",
					w.Code, w.Body.String())
			}
			var resp scopeCompletenessDecisionResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatal(err)
			}
			if !resp.OwnerAttributionUnresolved || !resp.AcknowledgedOwnerUnresolved {
				t.Errorf("response must record BOTH the unresolved attribution and the acknowledgement: %+v", resp)
			}
			if len(resp.OwnerUnresolvedPaths) != 1 || resp.OwnerUnresolvedPaths[0].Path != amendCoupledPath ||
				resp.OwnerUnresolvedPaths[0].Reason != tc.wantReason {
				t.Errorf("response owner_unresolved_paths = %+v, want the path + reason %q",
					resp.OwnerUnresolvedPaths, tc.wantReason)
			}

			entries := f.amendedEntries()
			if len(entries) != 1 {
				t.Fatalf("%d amended entries, want 1", len(entries))
			}
			var payload map[string]any
			if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
				t.Fatal(err)
			}
			if payload["owner_attribution_unresolved"] != true || payload["acknowledged_owner_unresolved"] != true {
				t.Errorf("the audit entry must carry BOTH flags — the relaxation is only defensible if it is audited: %v", payload)
			}
			raw, _ := json.Marshal(payload["owner_unresolved_paths"])
			if !strings.Contains(string(raw), amendCoupledPath) || !strings.Contains(string(raw), tc.wantReason) {
				t.Errorf("the audit entry must carry the unresolved path list with reasons: %s", raw)
			}
		})
	}
}

// TestAmend_AcknowledgementDoesNotRelaxTheActiveOwnerRefusal: the escape hatch
// covers UNRESOLVED ownership only. An ancestor still running is a known
// conflict and stays refused however loudly the operator acknowledges.
func TestAmend_AcknowledgementDoesNotRelaxTheActiveOwnerRefusal(t *testing.T) {
	f := newAmendFixture(t)
	f.setRunning(1)

	w := postAmend(t, f, `{"decision":"amend","reason":"try to acknowledge past a live sibling",`+
		`"acknowledge_owner_unresolved":true,"paths":[{"path":"`+amendCoupledPath+`","operation":"modify"}]}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amend_refused_owner_slice_active") {
		t.Errorf("the acknowledgement must not convert a KNOWN conflict into a permitted one: %s", w.Body.String())
	}
	f.assertNoSideEffects(t, "an acknowledged amend against a live owning slice")
}

// TestAmend_NonDecomposedRunResolvesByConstruction: a run that is not a
// decomposition child has NO sibling slice, so no sibling can hold a divergent
// edit — ownership is resolved by construction and the amend needs no
// acknowledgement. Without this carve-out the default refusal would make amend
// unusable on the two OLDER park classes, which are not slice-boundary problems
// at all.
func TestAmend_NonDecomposedRunResolvesByConstruction(t *testing.T) {
	f := newAmendFixture(t)
	f.child.DecomposedFrom = nil
	f.child.SliceIndex = nil
	f.stage.ScopeCompletenessPark.BuildRequiredPaths = nil
	f.stage.ScopeCompletenessPark.OwningSlices = nil
	f.stage.ScopeCompletenessPark.MissingPaths = []string{amendCoupledPath}

	w := postAmend(t, f, scopeAmendBody("no decomposition: nothing else can own this file"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OwnerAttributionUnresolved || resp.AcknowledgedOwnerUnresolved {
		t.Errorf("a non-decomposed run has no sibling to be uncertain about; got %+v", resp)
	}
}

// TestAmend_BudgetExhausted409 (counterfactual: the amend cap). A re-run under
// a widened scope can park again, so an unbounded amend loop burns a full agent
// pass per attempt. The prior amends are seeded BY CONSTRUCTION as audit
// entries, not by driving the endpoint twice.
func TestAmend_BudgetExhausted409(t *testing.T) {
	f := newAmendFixture(t)
	stageID := f.stage.ID
	for i := 0; i < maxScopeCompletenessAmendsPerStage; i++ {
		payload, _ := json.Marshal(map[string]any{"amendment_id": uuid.New().String()})
		if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: f.child.ID, StageID: &stageID, Category: CategoryScopeCompletenessAmended, Payload: payload,
		}); err != nil {
			t.Fatalf("seed prior amend entry: %v", err)
		}
	}

	w := postAmend(t, f, scopeAmendBody("a third amend"))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amend_budget_exhausted") {
		t.Errorf("body must name amend_budget_exhausted: %s", w.Body.String())
	}
	// The seeded entries are the PRIOR amends, so assert only the NEW effects.
	got, _ := f.rr.GetStage(context.Background(), stageID)
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage state = %q, want it left parked", got.State)
	}
	if rows := f.approvedRows(t); len(rows) != 0 {
		t.Errorf("%d approved amendment rows, want 0 — a refused amend writes nothing", len(rows))
	}
	if entries := f.amendedEntries(); len(entries) != maxScopeCompletenessAmendsPerStage {
		t.Errorf("%d amended entries, want the %d seeded ones and no more",
			len(entries), maxScopeCompletenessAmendsPerStage)
	}
}

// TestAmend_BudgetCountsDistinctAmendmentsNotEntries: a retry appends a SECOND
// entry for the SAME amendment, so a raw entry count would spend a slot the
// operator never used. Two entries keyed to one amendment_id must count as one.
func TestAmend_BudgetCountsDistinctAmendmentsNotEntries(t *testing.T) {
	f := newAmendFixture(t)
	stageID := f.stage.ID
	sameID := uuid.New().String()
	for i := 0; i < 2; i++ {
		payload, _ := json.Marshal(map[string]any{"amendment_id": sameID})
		if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: f.child.ID, StageID: &stageID, Category: CategoryScopeCompletenessAmended, Payload: payload,
		}); err != nil {
			t.Fatalf("seed entry: %v", err)
		}
	}

	got, err := f.s.countScopeCompletenessAmends(context.Background(), f.child.ID, stageID)
	if err != nil {
		t.Fatalf("countScopeCompletenessAmends: %v", err)
	}
	if got != 1 {
		t.Errorf("count = %d, want 1 — two entries for ONE amendment are one amend, not two", got)
	}
}

// --- per-failure-mode: retry-safe persistence (binding condition 2) --------

// transitionFailRepo fails the first N TransitionStage calls, so a test can
// drive the resume-failure arm and then a clean retry through the same server.
type transitionFailRepo struct {
	*orchestratorRepo
	failures int
}

func (r *transitionFailRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	if r.failures > 0 {
		r.failures--
		return nil, errors.New("transition unavailable")
	}
	return r.orchestratorRepo.TransitionStage(ctx, id, to, c)
}

// assertRetryLandsExactlyOneRow is binding condition 2's committed-state
// assertion, shared by all three mid-sequence failure arms: after a FAILED
// attempt followed by a successful retry there must be EXACTLY ONE approved
// amendment row for the stage, and agent_amendment_budget_remaining must report
// the same value as after a single clean amend. Asserting COMMITTED STATE
// matters because a control that fires and rolls back returns a byte-identical
// error.
func assertRetryLandsExactlyOneRow(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	rows := f.approvedRows(t)
	if len(rows) != 1 {
		t.Fatalf("%d approved amendment rows after failure+retry, want EXACTLY 1 — a duplicate row is inert for "+
			"the effective scope but silently eats one of the re-run agent's two #961 slots", len(rows))
	}
	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.AgentAmendmentBudgetRemaining != maxScopeAmendmentsPerStage-1 {
		t.Errorf("agent_amendment_budget_remaining = %d after failure+retry, want %d — the same as a single clean amend",
			resp.AgentAmendmentBudgetRemaining, maxScopeAmendmentsPerStage-1)
	}
	if resp.AmendmentID != rows[0].ID.String() {
		t.Errorf("response amendment_id = %q, want the REUSED row %s", resp.AmendmentID, rows[0].ID)
	}
	got, _ := f.rr.GetStage(context.Background(), f.stage.ID)
	if got.State != run.StageStatePending {
		t.Errorf("stage state after the retry = %q, want pending", got.State)
	}
	// The cap must still see ONE amend, not one per attempt.
	used, err := f.s.countScopeCompletenessAmends(context.Background(), f.child.ID, f.stage.ID)
	if err != nil {
		t.Fatalf("countScopeCompletenessAmends: %v", err)
	}
	if used != 1 {
		t.Errorf("amend budget used = %d after failure+retry, want 1", used)
	}
}

// TestAmend_AmendmentRowCreateFailureLeavesStageParked (counterfactual: the
// row-create refusal) + the retry arm.
func TestAmend_AmendmentRowCreateFailureLeavesStageParked(t *testing.T) {
	f := newAmendFixture(t)
	f.sa.createErr = errors.New("amendment store unavailable")

	w := postAmend(t, f, scopeAmendBody("widen the slice"))
	// COMMITTED STATE FIRST.
	f.assertNoSideEffects(t, "an amend whose row could not be created")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amend_unrecorded") {
		t.Errorf("body must name amend_unrecorded: %s", w.Body.String())
	}

	f.sa.createErr = nil
	assertRetryLandsExactlyOneRow(t, f, postAmend(t, f, scopeAmendBody("widen the slice")))
}

// TestAmend_AuditAppendFailureLeavesStageParked (counterfactual: the
// audit-append refusal) + the retry arm. The ordering guarantee: a stage whose
// widening the chain never recorded never becomes dispatchable — that entry is
// also what DEMOTES a stale `exempted` decision in prompt.go.
func TestAmend_AuditAppendFailureLeavesStageParked(t *testing.T) {
	f := newAmendFixture(t)
	f.au.appendErrCategory = CategoryScopeCompletenessAmended

	w := postAmend(t, f, scopeAmendBody("widen the slice"))
	// COMMITTED STATE FIRST: the stage must still be parked and no amended
	// entry may exist. The ROW is deliberately not asserted absent here — it is
	// created before the append and is REUSED by the retry below, which is the
	// whole point of binding condition 2.
	got, _ := f.rr.GetStage(context.Background(), f.stage.ID)
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Fatalf("stage state = %q, want it left parked — a widening the chain never recorded must never dispatch", got.State)
	}
	if entries := f.amendedEntries(); len(entries) != 0 {
		t.Errorf("%d amended audit entries written despite the append failure, want 0", len(entries))
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amend_unrecorded") {
		t.Errorf("body must name amend_unrecorded: %s", w.Body.String())
	}

	f.au.appendErrCategory = ""
	assertRetryLandsExactlyOneRow(t, f, postAmend(t, f, scopeAmendBody("widen the slice")))
}

// TestAmend_TransitionFailureLeavesStageParked (the arm the plan did not name,
// added by binding condition 2): a resume failure after the row AND the entry
// are committed must leave the stage parked, and the retry must reuse the row.
func TestAmend_TransitionFailureLeavesStageParked(t *testing.T) {
	f := newAmendFixture(t)
	f.s.cfg.RunRepo = &transitionFailRepo{orchestratorRepo: f.rr, failures: 1}

	w := postAmend(t, f, scopeAmendBody("widen the slice"))
	got, _ := f.rr.GetStage(context.Background(), f.stage.ID)
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Fatalf("stage state = %q, want it left parked when the resume failed", got.State)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amend_resume_failed") {
		t.Errorf("body must name amend_resume_failed: %s", w.Body.String())
	}
	// One row and one entry exist from the failed attempt; the retry must add
	// NEITHER a second row NOR a second amend against the cap.
	assertRetryLandsExactlyOneRow(t, f, postAmend(t, f, scopeAmendBody("widen the slice")))
}

// decideFailScopeAmendmentRepo fails the first N Decide calls while leaving
// Create intact — the shape that strands a PENDING orphan: the row exists but
// the operator's auto-approval never landed on it.
type decideFailScopeAmendmentRepo struct {
	*fakeScopeAmendmentRepo
	failures int
}

func (d *decideFailScopeAmendmentRepo) Decide(ctx context.Context, p scopeamendment.DecideParams) (*scopeamendment.Amendment, error) {
	if d.failures > 0 {
		d.failures--
		return nil, errors.New("amendment decision store unavailable")
	}
	return d.fakeScopeAmendmentRepo.Decide(ctx, p)
}

// TestAmend_PendingOrphanIsCompletedNotDuplicated is the Create-succeeded-but-
// auto-approve-Decide-failed arm of binding condition 2. It is the one
// mid-sequence failure that leaves a row in a NON-approved state, so it has its
// own reuse branch (StatusPending → complete it) that the two approved-orphan
// retry tests cannot exercise.
//
// A real Decide outage followed by a retry would otherwise either duplicate the
// row — draining one of the re-run agent's two #961 slots — or silently reuse an
// UNDECIDED row, which `resolveApprovedScopeAmendmentEntries` filters out, so the
// stage would resume with the widening not actually in its effective scope.
func TestAmend_PendingOrphanIsCompletedNotDuplicated(t *testing.T) {
	f := newAmendFixture(t)
	f.s.cfg.ScopeAmendmentRepo = &decideFailScopeAmendmentRepo{fakeScopeAmendmentRepo: f.sa, failures: 1}

	w := postAmend(t, f, scopeAmendBody("widen the slice"))
	// COMMITTED STATE FIRST: an undecided row is not a recorded widening, so the
	// stage must stay parked with no APPROVED row and no amended entry.
	f.assertNoSideEffects(t, "an amend whose auto-approve Decide failed")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amend_unrecorded") {
		t.Errorf("body must name amend_unrecorded: %s", w.Body.String())
	}

	// The orphan the retry must COMPLETE rather than duplicate.
	pending := f.pendingRows(t)
	if len(pending) != 1 {
		t.Fatalf("%d pending amendment rows after the Decide failure, want exactly 1", len(pending))
	}
	orphanID := pending[0].ID

	assertRetryLandsExactlyOneRow(t, f, postAmend(t, f, scopeAmendBody("widen the slice")))
	rows := f.approvedRows(t)
	if rows[0].ID != orphanID {
		t.Errorf("the retry landed on row %s, want the COMPLETED pending orphan %s — minting a second row "+
			"drains one of the re-run agent's two #961 slots", rows[0].ID, orphanID)
	}
	if left := f.pendingRows(t); len(left) != 0 {
		t.Errorf("%d pending amendment row(s) left after the retry, want 0 — an undecided row is not folded "+
			"into the effective scope, so the stage would resume without the widening", len(left))
	}
}

// TestAmend_RepeatedParkEpisodesEachSpendASlot (counterfactual: the
// park-EPISODE component of the reuse key). Reuse must recognize an INCOMPLETE
// RETRY of this park, and nothing else. A stage that is amended, re-runs and
// PARKS AGAIN presents a byte-identical (stage, reason, path set) on its second
// episode: keyed on that alone, the second amend would adopt the first's row,
// the cap would keep counting ONE distinct amendment_id, and the operator could
// amend without limit — an unbounded loop burning a full agent pass per attempt,
// which is exactly what maxScopeCompletenessAmendsPerStage exists to stop.
//
// Each episode is seeded BY CONSTRUCTION (a fresh park with the held commit the
// re-run would have pushed), so the RED lands on the behavioral assertion.
func TestAmend_RepeatedParkEpisodesEachSpendASlot(t *testing.T) {
	f := newAmendFixture(t)
	body := scopeAmendBody("the coupled prompt.go is build-required by this slice")

	// EPISODE 1 — the first park, amended.
	if w := postAmend(t, f, body); w.Code != http.StatusOK {
		t.Fatalf("episode 1 status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	first := f.approvedRows(t)
	if len(first) != 1 {
		t.Fatalf("%d approved rows after episode 1, want 1", len(first))
	}

	// EPISODE 2 — the re-run parked again on a NEW held commit and the operator
	// submits the IDENTICAL decision. This is a SECOND amend, not a retry.
	f.repark(t, "4444444444444444444444444444444444444444")
	w2 := postAmend(t, f, body)
	if w2.Code != http.StatusOK {
		t.Fatalf("episode 2 status = %d, want 200; body = %s", w2.Code, w2.Body.String())
	}
	if rows := f.approvedRows(t); len(rows) != 2 {
		t.Fatalf("%d approved rows after episode 2, want 2 — a NEW park episode must mint its OWN amendment, "+
			"not adopt the completed episode's row", len(rows))
	}
	var resp2 scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.AmendmentID == first[0].ID.String() {
		t.Errorf("episode 2 reused episode 1's amendment %s: the cap counts DISTINCT amendment_ids, so reusing "+
			"across park episodes lets the two-amend cap be bypassed indefinitely", resp2.AmendmentID)
	}
	used, err := f.s.countScopeCompletenessAmends(context.Background(), f.child.ID, f.stage.ID)
	if err != nil {
		t.Fatalf("countScopeCompletenessAmends: %v", err)
	}
	if used != 2 {
		t.Errorf("amend budget used = %d after two park episodes, want 2", used)
	}

	// EPISODE 3 — the cap now bites. Without the episode key it never would.
	f.repark(t, "5555555555555555555555555555555555555555")
	w3 := postAmend(t, f, body)
	if w3.Code != http.StatusConflict {
		t.Fatalf("episode 3 status = %d, want 409 — a third park episode amended with identical input must be "+
			"CAPPED, not admitted forever; body = %s", w3.Code, w3.Body.String())
	}
	if !strings.Contains(w3.Body.String(), "amend_budget_exhausted") {
		t.Errorf("body must name amend_budget_exhausted: %s", w3.Body.String())
	}
	got, _ := f.rr.GetStage(context.Background(), f.stage.ID)
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage state = %q, want it left parked by the capped third episode", got.State)
	}
	if rows := f.approvedRows(t); len(rows) != 2 {
		t.Errorf("%d approved rows after the capped third episode, want the 2 from episodes 1-2", len(rows))
	}
}

// TestAmend_UnidentifiableEpisodeMintsARowPerAttempt (counterfactual: the
// `episode != ""` guard on the in-flight row lookup). amendEpisodeKey returns
// "" for a park carrying NEITHER a held commit nor a verified tree SHA, and the
// handler then skips the lookup entirely — reuse is refused outright and every
// attempt mints its OWN row.
//
// That is the DOCUMENTED fail-closed direction, and this pins it as behaviour
// rather than prose: over-counting against the amend cap is safe (a third
// attempt is refused), while over-spending the re-run agent's two #961 slots is
// merely reported honestly by agent_amendment_budget_remaining. The unsafe
// alternative — reusing a row on an episode nothing can identify — would let a
// re-parking stage amend forever under one amendment_id.
//
// The SHA-less park is seeded BY CONSTRUCTION so the RED lands on the
// behavioral assertion, not on a fixture-setup failure.
func TestAmend_UnidentifiableEpisodeMintsARowPerAttempt(t *testing.T) {
	f := newAmendFixture(t)
	park := *f.stage.ScopeCompletenessPark
	park.HeldCommitSHA = ""
	park.VerifiedTreeSHA = ""
	f.stage.ScopeCompletenessPark = &park
	f.s.cfg.RunRepo = &transitionFailRepo{orchestratorRepo: f.rr, failures: 1}
	body := scopeAmendBody("widen the slice")

	// ATTEMPT 1 fails mid-sequence: the row and the entry are committed, the
	// resume is not — the exact shape the reuse path exists to fold.
	w := postAmend(t, f, body)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amend_resume_failed") {
		t.Errorf("body must name amend_resume_failed: %s", w.Body.String())
	}
	got, _ := f.rr.GetStage(context.Background(), f.stage.ID)
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Fatalf("stage state = %q, want it left parked when the resume failed", got.State)
	}
	first := f.approvedRows(t)
	if len(first) != 1 {
		t.Fatalf("%d approved rows after the failed attempt, want 1", len(first))
	}

	// THE RETRY. With no identifiable episode there is no reuse: it must MINT a
	// second row rather than adopt the orphan.
	w2 := postAmend(t, f, body)
	if w2.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200; body = %s", w2.Code, w2.Body.String())
	}
	rows := f.approvedRows(t)
	if len(rows) != 2 {
		t.Fatalf("%d approved rows after failure+retry on a SHA-less park, want 2 — an episode nothing can "+
			"identify must refuse reuse, because adopting a row across unidentifiable episodes uncaps the "+
			"amend loop", len(rows))
	}
	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w2.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.AmendmentID == first[0].ID.String() {
		t.Errorf("the retry reused amendment %s despite an unidentifiable episode", resp.AmendmentID)
	}
	if resp.AgentAmendmentBudgetRemaining != maxScopeAmendmentsPerStage-2 {
		t.Errorf("agent_amendment_budget_remaining = %d, want %d — the over-spend is REPORTED, not hidden",
			resp.AgentAmendmentBudgetRemaining, maxScopeAmendmentsPerStage-2)
	}

	// THE CAP COUNTED BOTH. Each attempt spent its own slot, so a third is
	// refused — the fail-CLOSED half of the trade-off.
	used, err := f.s.countScopeCompletenessAmends(context.Background(), f.child.ID, f.stage.ID)
	if err != nil {
		t.Fatalf("countScopeCompletenessAmends: %v", err)
	}
	if used != 2 {
		t.Fatalf("amend budget used = %d after two SHA-less attempts, want 2 — each attempt must count on its "+
			"own against the cap", used)
	}
	f.stage.State = run.StageStateAwaitingScopeDecision
	w3 := postAmend(t, f, body)
	if w3.Code != http.StatusConflict {
		t.Fatalf("third attempt status = %d, want 409 — an unidentifiable episode must over-count against the "+
			"cap, never under-count; body = %s", w3.Code, w3.Body.String())
	}
	if !strings.Contains(w3.Body.String(), "amend_budget_exhausted") {
		t.Errorf("body must name amend_budget_exhausted: %s", w3.Body.String())
	}
	if after := f.approvedRows(t); len(after) != 2 {
		t.Errorf("%d approved rows after the capped third attempt, want the 2 from the earlier attempts", len(after))
	}
}

// TestAmend_TransitionFailureOnTheFinalSlotStaysRetryable (counterfactual: the
// cap's already-counted-retry admission). A resume failure on the SECOND — and
// last — permitted amend has ALREADY appended that amendment's audit entry, so a
// plain `used >= max` check refuses the retry amend_budget_exhausted and strands
// the stage parked with no admissible decision left: the widening is recorded,
// the stage is not resumed, and no further amend can ever move it.
//
// TestAmend_TransitionFailureLeavesStageParked starts from zero prior amends and
// so never reaches this boundary. The prior amend here is seeded BY
// CONSTRUCTION as an audit entry, not by driving the endpoint twice.
func TestAmend_TransitionFailureOnTheFinalSlotStaysRetryable(t *testing.T) {
	f := newAmendFixture(t)
	stageID := f.stage.ID
	priorID := uuid.New().String()
	payload, _ := json.Marshal(map[string]any{"amendment_id": priorID})
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: f.child.ID, StageID: &stageID, Category: CategoryScopeCompletenessAmended, Payload: payload,
	}); err != nil {
		t.Fatalf("seed the prior amend entry: %v", err)
	}
	f.s.cfg.RunRepo = &transitionFailRepo{orchestratorRepo: f.rr, failures: 1}

	// The SECOND — and last — permitted amend, whose resume fails.
	w := postAmend(t, f, scopeAmendBody("widen the slice"))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amend_resume_failed") {
		t.Errorf("body must name amend_resume_failed: %s", w.Body.String())
	}
	got, _ := f.rr.GetStage(context.Background(), stageID)
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Fatalf("stage state = %q, want it left parked when the resume failed", got.State)
	}
	// The cap is now AT its limit and the in-flight amendment is one of the two.
	used, err := f.s.countScopeCompletenessAmends(context.Background(), f.child.ID, stageID)
	if err != nil {
		t.Fatalf("countScopeCompletenessAmends: %v", err)
	}
	if used != maxScopeCompletenessAmendsPerStage {
		t.Fatalf("amend budget used = %d after the failed second amend, want %d — the fixture must reach the "+
			"cap boundary for this test to mean anything", used, maxScopeCompletenessAmendsPerStage)
	}

	// THE RETRY. It re-POSTs an amendment the cap has already counted, so it adds
	// no new amendment and must stay admissible.
	w2 := postAmend(t, f, scopeAmendBody("widen the slice"))
	if w2.Code == http.StatusConflict {
		t.Fatalf("the retry was refused at the cap (%s): the widening is recorded but the stage is not resumed, "+
			"so the stage is stranded parked with no admissible decision left", w2.Body.String())
	}
	if w2.Code != http.StatusOK {
		t.Fatalf("retry status = %d, want 200; body = %s", w2.Code, w2.Body.String())
	}
	resumed, _ := f.rr.GetStage(context.Background(), stageID)
	if resumed.State != run.StageStatePending {
		t.Errorf("stage state after the retry = %q, want pending", resumed.State)
	}
	if rows := f.approvedRows(t); len(rows) != 1 {
		t.Errorf("%d approved amendment rows after failure+retry, want exactly 1 — the retry must REUSE the "+
			"in-flight row", len(rows))
	}
	after, err := f.s.countScopeCompletenessAmends(context.Background(), f.child.ID, stageID)
	if err != nil {
		t.Fatalf("countScopeCompletenessAmends: %v", err)
	}
	if after != maxScopeCompletenessAmendsPerStage {
		t.Errorf("amend budget used = %d after the retry, want %d — a retry re-appends an entry keyed to the "+
			"SAME amendment_id and must not spend a further slot", after, maxScopeCompletenessAmendsPerStage)
	}
}

// TestAmend_ListByRunFailureRefusesRatherThanDuplicating: without the prior
// rows the handler cannot tell a fresh amend from a retry, and guessing wrong
// drains the agent's #961 budget silently — so it refuses instead.
func TestAmend_ListByRunFailureRefusesRatherThanDuplicating(t *testing.T) {
	f := newAmendFixture(t)
	f.sa.failListOn = 1 // the handler's idempotence read is the first ListByRun

	w := postAmend(t, f, scopeAmendBody("widen the slice"))
	f.assertNoSideEffects(t, "an amend whose prior-row read failed")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "amend_unrecorded") {
		t.Errorf("body must name amend_unrecorded: %s", w.Body.String())
	}
}

// --- per-failure-mode: the read-side degrades -----------------------------

// listFailAuditRepo fails the cap's ListForRunByCategory read while leaving
// AppendChained intact, so a test can drive the audit-READ branch alone.
type listFailAuditRepo struct {
	*auditFake
	err error
}

func (a *listFailAuditRepo) ListForRunByCategory(context.Context, uuid.UUID, string) ([]*audit.Entry, error) {
	return nil, a.err
}

// TestAmend_CapReadFailureRefuses: the per-stage cap is read from the audit
// chain, so a read failure means the cap CANNOT be evaluated. Proceeding would
// let an unbounded number of amends through exactly when the budget is
// unknowable, so the amend is refused with nothing written.
func TestAmend_CapReadFailureRefuses(t *testing.T) {
	f := newAmendFixture(t)
	f.s.cfg.AuditRepo = &listFailAuditRepo{auditFake: f.au, err: errors.New("audit store outage")}

	w := postAmend(t, f, scopeAmendBody("widen the slice"))
	f.assertNoSideEffects(t, "an amend whose cap read failed")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "internal_error") {
		t.Errorf("body must name internal_error: %s", w.Body.String())
	}
}

// getRunFailRepo fails GetRun, so no run row can be resolved for the decision.
type getRunFailRepo struct {
	*orchestratorRepo
	err error
}

func (r *getRunFailRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, r.err
}

// TestAmend_RunLookupFailureRefuses: the run row the owner-slice guard needs
// (DecomposedFrom / SliceIndex) is resolved ONCE by the shared
// resolveParkedScopeStage, which refuses a lookup failure before any arm runs —
// so the amend arm never sees a nil run row and mistake it for the
// safe-by-construction standalone case.
//
// This is the vehicle for that shared refusal on the AMEND arm. The amend
// handler deliberately has no second GetRun of its own: a duplicate fetch's
// error branch would be unreachable, and an unreachable branch cannot be
// counterfactually shown to do anything (verified empirically — neutralizing a
// duplicate branch left this test GREEN, because the upstream control fires
// first; the duplicate was removed rather than shipped untestable).
func TestAmend_RunLookupFailureRefuses(t *testing.T) {
	f := newAmendFixture(t)
	f.s.cfg.RunRepo = &getRunFailRepo{orchestratorRepo: f.rr, err: errors.New("run store outage")}

	w := postAmend(t, f, scopeAmendBody("widen the slice"))
	f.assertNoSideEffects(t, "an amend whose run lookup failed")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "internal_error") {
		t.Errorf("body must name internal_error: %s", w.Body.String())
	}
}

// countFailScopeAmendmentRepo fails only CountByStage, leaving every write path
// working — the shape that isolates the budget-reporting degrade.
type countFailScopeAmendmentRepo struct {
	*fakeScopeAmendmentRepo
}

func (f *countFailScopeAmendmentRepo) CountByStage(context.Context, uuid.UUID) (int, error) {
	return 0, errors.New("count unavailable")
}

// TestAmend_BudgetRemainingDegradesToZero: agent_amendment_budget_remaining is
// OBSERVABILITY, not a control, so a count failure must degrade to a
// conservative 0 rather than fail an otherwise-valid amend. The amend itself
// still lands — that is the point of the degrade.
func TestAmend_BudgetRemainingDegradesToZero(t *testing.T) {
	f := newAmendFixture(t)
	f.s.cfg.ScopeAmendmentRepo = &countFailScopeAmendmentRepo{fakeScopeAmendmentRepo: f.sa}

	w := postAmend(t, f, scopeAmendBody("widen the slice"))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a count failure must not fail the amend; body = %s", w.Code, w.Body.String())
	}
	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AgentAmendmentBudgetRemaining != 0 {
		t.Errorf("agent_amendment_budget_remaining = %d, want 0 when the row count is unavailable",
			resp.AgentAmendmentBudgetRemaining)
	}
	got, _ := f.rr.GetStage(context.Background(), f.stage.ID)
	if got.State != run.StageStatePending {
		t.Errorf("stage state = %q, want pending — the degrade is observability-only", got.State)
	}
}

// TestAmend_UnkeyableEntryCountsOnItsOwn: an amended entry whose payload
// carries no decodable amendment_id cannot be folded into another amendment's
// slot, so it counts on its own — the cap fails CLOSED rather than letting an
// unkeyable entry be free.
func TestAmend_UnkeyableEntryCountsOnItsOwn(t *testing.T) {
	f := newAmendFixture(t)
	stageID := f.stage.ID
	for _, payload := range []string{`{"amendment_id":""}`, `{"no_id":true}`} {
		if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
			RunID: f.child.ID, StageID: &stageID, Category: CategoryScopeCompletenessAmended,
			Payload: json.RawMessage(payload),
		}); err != nil {
			t.Fatalf("seed entry: %v", err)
		}
	}

	got, err := f.s.countScopeCompletenessAmends(context.Background(), f.child.ID, stageID)
	if err != nil {
		t.Fatalf("countScopeCompletenessAmends: %v", err)
	}
	if got != 2 {
		t.Errorf("count = %d, want 2 — two unkeyable entries are two amends, not one", got)
	}
}

// --- per-failure-mode: auth + input ---------------------------------------

// TestAmend_RunBoundToken403: the agent whose stage parked may not widen its
// own scope — that is an operator decision, on the amend arm as on exempt.
func TestAmend_RunBoundToken403(t *testing.T) {
	f := newAmendFixture(t)
	w := postScopeCompletenessDecision(t, f.s, f.child.ID, scopeAmendBody("self-widen"),
		func(r *http.Request) *http.Request {
			return withRunBoundIdentity(r, f.child.ID, "mcp:read", "write:scope-amendments")
		})
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "self_decision") {
		t.Errorf("body must name self_decision: %s", w.Body.String())
	}
	f.assertNoSideEffects(t, "an amend attempted with a run-bound agent token")
}

// TestAmend_MissingScope403 pins the token-scope gate on the amend arm.
func TestAmend_MissingScope403(t *testing.T) {
	f := newAmendFixture(t)
	w := postScopeCompletenessDecision(t, f.s, f.child.ID, scopeAmendBody("widen"),
		func(r *http.Request) *http.Request { return withOperatorIdentity(r, "read:runs") })
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "insufficient_scope") {
		t.Errorf("body must name insufficient_scope: %s", w.Body.String())
	}
	f.assertNoSideEffects(t, "an amend attempted without write:stages")
}

// TestAmend_BadInput400 pins every input rejection the amend arm adds.
func TestAmend_BadInput400(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"NoPaths", `{"decision":"amend","reason":"r"}`},
		{"EmptyPathsArray", `{"decision":"amend","reason":"r","paths":[]}`},
		{"EmptyPathString", `{"decision":"amend","reason":"r","paths":[{"path":"  ","operation":"modify"}]}`},
		{"AbsolutePath", `{"decision":"amend","reason":"r","paths":[{"path":"/etc/passwd","operation":"modify"}]}`},
		{"TraversalPath", `{"decision":"amend","reason":"r","paths":[{"path":"../outside.go","operation":"modify"}]}`},
		{"BadOperation", `{"decision":"amend","reason":"r","paths":[{"path":"a/b.go","operation":"delete"}]}`},
		{"EmptyReason", `{"decision":"amend","reason":"  ","paths":[{"path":"a/b.go","operation":"modify"}]}`},
		{"PathsOnExempt", `{"decision":"exempt","reason":"r","paths":[{"path":"a/b.go","operation":"modify"}]}`},
		{"PathsOnFail", `{"decision":"fail","reason":"r","paths":[{"path":"a/b.go","operation":"modify"}]}`},
		// acknowledge_owner_unresolved is amend-only for the same reason paths
		// are: only the amend arm runs an owner-slice guard, so on exempt/fail
		// the flag lands nowhere and the operator would believe they had taken
		// an AUDITED risk that no audit entry records. The `fail` arm is the
		// sharp one — unrejected it FAILS the stage, so assertNoSideEffects
		// reads a state change, not merely a wrong status code.
		{"AcknowledgementOnExempt", `{"decision":"exempt","reason":"r","acknowledge_owner_unresolved":true}`},
		{"AcknowledgementOnFail", `{"decision":"fail","reason":"r","acknowledge_owner_unresolved":true}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAmendFixture(t)
			w := postAmend(t, f, tc.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "validation_failed") {
				t.Errorf("body must name validation_failed: %s", w.Body.String())
			}
			f.assertNoSideEffects(t, "an amend with invalid input")
		})
	}
}

// TestAmend_NotParked409: amend only applies to a stage currently parked.
func TestAmend_NotParked409(t *testing.T) {
	f := newAmendFixture(t)
	f.stage.State = run.StageStateRunning

	w := postAmend(t, f, scopeAmendBody("widen a running stage"))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "stage_not_parked") {
		t.Errorf("body must name stage_not_parked: %s", w.Body.String())
	}
	if rows := f.approvedRows(t); len(rows) != 0 {
		t.Errorf("%d approved amendment rows, want 0", len(rows))
	}
}

// TestAmend_UnconfiguredScopeAmendmentRepo503: without the amendment store the
// widening has nowhere to land, so the decision is refused rather than resuming
// a stage whose scope was never actually widened.
func TestAmend_UnconfiguredScopeAmendmentRepo503(t *testing.T) {
	f := newAmendFixture(t)
	f.s.cfg.ScopeAmendmentRepo = nil

	w := postAmend(t, f, scopeAmendBody("widen with no store"))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503; body = %s", w.Code, w.Body.String())
	}
	got, _ := f.rr.GetStage(context.Background(), f.stage.ID)
	if got.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage state = %q, want it left parked", got.State)
	}
	if entries := f.amendedEntries(); len(entries) != 0 {
		t.Errorf("%d amended entries, want 0", len(entries))
	}
}

// --- wire-shape preservation ----------------------------------------------

// TestAmend_ExemptedPayloadUnchanged is the BYTE-COMPARISON binding condition:
// #2591 adds keys to the AMENDED payload only, so the exempted and failed
// payload key SETS must stay identical to pre-change. A drift here is a
// wire-visible change to two surfaces this issue promised not to touch.
func TestAmend_ExemptedPayloadUnchanged(t *testing.T) {
	// The pre-#2591 key sets, transcribed from writeScopeCompletenessDecisionAudit.
	wantExempted := []string{
		"decided_by", "decision", "gate_evidence", "held_commit_sha", "missing_paths",
		"reason", "run_branch", "run_id", "stage_id", "unsatisfied_assertions", "verified_tree_sha",
	}
	wantFailed := []string{
		"decided_by", "decision", "missing_paths", "reason", "run_id", "stage_id",
		"unsatisfied_assertions",
	}
	for _, tc := range []struct {
		name, body, category string
		want                 []string
	}{
		{"exempt", `{"decision":"exempt","reason":"the coupled test file is genuinely already covered"}`,
			CategoryScopeCompletenessExempted, wantExempted},
		{"fail", `{"decision":"fail","reason":"the declared edit was dropped"}`,
			CategoryScopeCompletenessFailed, wantFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _, au, runRow, _ := scopeCompletenessServer(t) // park carries MissingPaths only
			w := postScopeCompletenessDecision(t, s, runRow.ID, tc.body, operatorWriteStages)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
			}
			au.mu.Lock()
			defer au.mu.Unlock()
			var found bool
			for _, e := range au.appended {
				if e.Category != tc.category {
					continue
				}
				found = true
				var payload map[string]any
				if err := json.Unmarshal(e.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				got := make([]string, 0, len(payload))
				for k := range payload {
					got = append(got, k)
				}
				sort.Strings(got)
				if strings.Join(got, ",") != strings.Join(tc.want, ",") {
					t.Errorf("%s payload key set drifted:\n got: %v\nwant: %v", tc.category, got, tc.want)
				}
			}
			if !found {
				t.Fatalf("want a %s entry, got %+v", tc.category, au.appended)
			}
		})
	}
}

// --- #4798: the slice-ownership guard and the ancestor-delivery proof -------

// amendDecisionBody renders an amend decision naming paths, with the operation modify.
func amendDecisionBody(reason string, acknowledge bool, paths ...string) string {
	entries := make([]string, 0, len(paths))
	for _, p := range paths {
		entries = append(entries, fmt.Sprintf(`{"path":%q,"operation":"modify"}`, p))
	}
	ack := ""
	if acknowledge {
		ack = `"acknowledge_owner_unresolved":true,`
	}
	return fmt.Sprintf(`{"decision":"amend","reason":%q,%s"paths":[%s]}`, reason, ack, strings.Join(entries, ","))
}

const (
	amendAncestorPath   = "docs/ancestor.md"
	amendTransitivePath = "docs/transitive.md"
	amendUndeclaredPath = "docs/nobody.md"
)

// newAmendOwnershipFixture is the contract's fixture: the child at slice 0
// depends_on [2] and owns own/; slice 1 declares the coupled path with no edge;
// slice 2 depends_on [3]; slices 2 and 3 are delivered and named by the parent's
// newest slices_integrated; the park attributes the coupled path to slice 1, as
// the real park does. Each mode changes ONE element, so that element is the only
// thing refusing or admitting. Coverage does not constrain the paths.
func newAmendOwnershipFixture(t *testing.T) *amendFixture {
	t.Helper()
	f := newAmendFixtureWithPlan(t, 0, ownershipSlices(
		ownSpec{files: []string{"own/"}, deps: []int{2}},
		ownSpec{files: []string{amendCoupledPath}},
		ownSpec{files: []string{amendAncestorPath}, deps: []int{3}},
		ownSpec{files: []string{amendTransitivePath}},
	))
	f.stage.ScopeCompletenessPark.BuildRequiredPaths = nil
	f.stage.ScopeCompletenessPark.MissingPaths = []string{amendCoupledPath}
	f.deliver(2)
	f.deliver(3)
	f.integrate(amendConsolidated, 2, 3)
	return f
}

// newAmendOverlapFixture is the M5 shape: the child depends_on [1, 2], slice 1
// declares shared/ and is delivered and covered, slice 2 declares shared/file.go.
// A single path therefore has two ancestors, the first of which is delivered.
func newAmendOverlapFixture(t *testing.T) *amendFixture {
	t.Helper()
	f := newAmendFixtureWithPlan(t, 0, ownershipSlices(
		ownSpec{files: []string{"own/"}, deps: []int{1, 2}},
		ownSpec{files: []string{"shared/"}},
		ownSpec{files: []string{"shared/file.go"}},
	))
	f.stage.ScopeCompletenessPark.BuildRequiredPaths = nil
	f.stage.ScopeCompletenessPark.OwningSlices = nil
	f.stage.ScopeCompletenessPark.MissingPaths = []string{"shared/file.go"}
	f.deliver(1)
	return f
}

// amendRefusal decodes the error body and pins status and code.
func amendRefusal(t *testing.T, w *httptest.ResponseRecorder, wantStatus int, wantCode string) errorBody {
	t.Helper()
	if w.Code != wantStatus {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, wantStatus, w.Body.String())
	}
	var env errorEnvelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if env.Error.Code != wantCode {
		t.Fatalf("code = %q, want %q; body = %s", env.Error.Code, wantCode, w.Body.String())
	}
	return env.Error
}

// assertOwnershipRefusal is the plan-declared non-ancestor contract: 409 naming
// owner and remedy, retryable false, exactly one scope_amendment_refused row with
// phase scope_completeness_amend and actor user, and nothing else touched.
func assertOwnershipRefusal(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, parkBefore string) {
	t.Helper()
	body := amendRefusal(t, w, http.StatusConflict, errCodeAmendmentPathOwnedBySibling)
	conflicts, _ := body.Details["conflicts"].([]any)
	if len(conflicts) == 0 {
		t.Fatalf("details.conflicts empty: %v", body.Details)
	}
	first, _ := conflicts[0].(map[string]any)
	if first["owning_slice"] != float64(1) || first["owning_child_run_id"] != f.sibling.ID.String() {
		t.Errorf("conflicts[0] = %v, want slice 1 and its child run %s", first, f.sibling.ID)
	}
	remedy, _ := body.Details["remedy"].(string)
	if !strings.Contains(remedy, "decide fail") || !strings.Contains(remedy, "corrected decomposition") {
		t.Errorf("remedy = %q, want it to name fail and a corrected decomposition", remedy)
	}
	if body.Details["retryable"] != false {
		t.Errorf("details.retryable = %v, want false", body.Details["retryable"])
	}
	rows := f.refusedRows()
	if len(rows) != 1 {
		t.Fatalf("%d scope_amendment_refused rows, want exactly 1", len(rows))
	}
	var payload map[string]any
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["phase"] != sliceOwnershipPhaseAmend {
		t.Errorf("refused row phase = %v, want %s", payload["phase"], sliceOwnershipPhaseAmend)
	}
	if rows[0].ActorKind == nil || *rows[0].ActorKind != audit.ActorUser {
		t.Errorf("refused row actor = %v, want user", rows[0].ActorKind)
	}
	if rows[0].RunID != f.child.ID {
		t.Errorf("refused row run = %s, want the child %s", rows[0].RunID, f.child.ID)
	}
	f.assertAmendUntouched(t, parkBefore)
}

// assertAmendAdmitted is the proven contract: 200, stage pending, an approved row,
// owning_slices naming exactly wantOwners, and no acknowledgement.
func assertAmendAdmitted(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, wantOwners ...int) {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	got, _ := f.rr.GetStage(context.Background(), f.stage.ID)
	if got.State != run.StageStatePending {
		t.Errorf("stage state = %q, want pending", got.State)
	}
	if rows := f.approvedRows(t); len(rows) != 1 {
		t.Errorf("%d approved rows, want 1", len(rows))
	}
	var resp scopeCompletenessDecisionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OwnerAttributionUnresolved || resp.AcknowledgedOwnerUnresolved {
		t.Errorf("a proven amend must need no acknowledgement: %+v", resp)
	}
	owners := make([]int, 0, len(resp.OwningSlices))
	for _, o := range resp.OwningSlices {
		if o.SliceIndex != nil {
			owners = append(owners, *o.SliceIndex)
		}
	}
	if fmt.Sprint(owners) != fmt.Sprint(wantOwners) {
		t.Errorf("owning_slices = %v, want %v", owners, wantOwners)
	}
	if rows := f.refusedRows(); len(rows) != 0 {
		t.Errorf("%d scope_amendment_refused rows on an admitted amend", len(rows))
	}
}

// assertProofRefusal pins an ancestor or plan refusal that writes NO audit row.
func assertProofRefusal(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, parkBefore string, status int, code, reason string, retryable bool, causes ...string) errorBody {
	t.Helper()
	body := amendRefusal(t, w, status, code)
	if body.Details["reason"] != reason {
		t.Errorf("details.reason = %v, want %s; body = %s", body.Details["reason"], reason, w.Body.String())
	}
	if body.Details["retryable"] != retryable {
		t.Errorf("details.retryable = %v, want %v", body.Details["retryable"], retryable)
	}
	if remedy, _ := body.Details["remedy"].(string); remedy == "" {
		t.Errorf("details.remedy is empty: %v", body.Details)
	}
	for _, cause := range causes {
		if strings.Contains(w.Body.String(), cause) {
			t.Errorf("raw cause %q in the body: %s", cause, w.Body.String())
		}
	}
	if strings.Contains(w.Body.String(), internalCauseKey) {
		t.Errorf("internal cause key in the body: %s", w.Body.String())
	}
	if rows := f.refusedRows(); len(rows) != 0 {
		t.Errorf("%d scope_amendment_refused rows, want 0", len(rows))
	}
	f.assertAmendUntouched(t, parkBefore)
	return body
}

// TestAmend_SliceOwnership is the amend half of the #4798 contract, one subtest
// per mode. A plan-declared non-ancestor is refused in every sibling state; own
// slice and a proven ancestor are allowed; a broken proof is refused by reason; a
// missing or unreadable plan fails closed with nothing written.
func TestAmend_SliceOwnership(t *testing.T) {
	const cause = "plan artifact store offline sentinel"
	for _, tc := range []struct {
		name    string
		fixture func(*testing.T) *amendFixture
		setup   func(*testing.T, *amendFixture)
		paths   []string
		ack     bool
		check   func(*testing.T, *amendFixture, *httptest.ResponseRecorder, string)
	}{
		{
			name:  "non_ancestor_not_started_refused",
			paths: []string{amendCoupledPath},
			check: assertOwnershipRefusal,
		},
		{
			name: "non_ancestor_started_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.setRunning(1)
			},
			paths: []string{amendCoupledPath},
			check: assertOwnershipRefusal,
		},
		{
			name: "non_ancestor_succeeded_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.deliver(1)
			},
			paths: []string{amendCoupledPath},
			check: assertOwnershipRefusal,
		},
		{
			name:  "non_ancestor_acknowledged_still_refused",
			paths: []string{amendCoupledPath},
			ack:   true,
			check: assertOwnershipRefusal,
		},
		{
			name: "non_ancestor_no_park_entry_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.stage.ScopeCompletenessPark.OwningSlices = nil
			},
			paths: []string{amendCoupledPath},
			check: assertOwnershipRefusal,
		},
		{
			name:  "mixed_set_refused_whole",
			paths: []string{amendAncestorPath, amendCoupledPath},
			check: assertOwnershipRefusal,
		},
		{
			name:  "ancestor_succeeded_allowed",
			paths: []string{amendAncestorPath},
			check: func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, _ string) {
				assertAmendAdmitted(t, f, w, 2)
			},
		},
		{
			name:  "transitive_ancestor_succeeded_allowed",
			paths: []string{amendTransitivePath},
			check: func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, _ string) {
				assertAmendAdmitted(t, f, w, 3)
			},
		},
		{
			name: "ancestor_no_park_entry_allowed",
			setup: func(_ *testing.T, f *amendFixture) {
				f.stage.ScopeCompletenessPark.OwningSlices = nil
			},
			paths: []string{amendAncestorPath},
			check: func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, _ string) {
				assertAmendAdmitted(t, f, w, 2)
			},
		},
		{
			name: "own_slice_no_park_entry_allowed",
			setup: func(_ *testing.T, f *amendFixture) {
				f.stage.ScopeCompletenessPark.OwningSlices = nil
			},
			paths: []string{"own/extra.go"},
			check: func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, _ string) {
				assertAmendAdmitted(t, f, w, 0)
			},
		},
		{
			name: "unresolved_child_local_parent_allowed",
			setup: func(_ *testing.T, f *amendFixture) {
				f.child.RunnerKindResolved = false
				f.parent.RunnerKind = run.RunnerKindLocal
				f.parent.RunnerKindResolved = true
			},
			paths: []string{amendAncestorPath},
			check: func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, _ string) {
				assertAmendAdmitted(t, f, w, 2)
			},
		},
		{
			name: "unresolved_child_unresolved_parent_allowed",
			setup: func(_ *testing.T, f *amendFixture) {
				f.child.RunnerKindResolved = false
			},
			paths: []string{amendAncestorPath},
			check: func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, _ string) {
				assertAmendAdmitted(t, f, w, 2)
			},
		},
		{
			name: "ancestor_running_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.setRunning(2)
			},
			paths: []string{amendAncestorPath},
			check: assertAncestorNotDelivered(string(run.StageStateRunning), string(run.StateRunning)),
		},
		{
			name: "ancestor_not_started_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.siblings[2].State = run.StatePending
				f.sibImpls[2].State = run.StageStatePending
				f.sibImpls[2].StartedAt = nil
			},
			paths: []string{amendAncestorPath},
			check: assertAncestorNotDelivered(string(run.StageStatePending), string(run.StatePending)),
		},
		{
			name: "ancestor_run_not_succeeded_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.siblings[2].State = run.StateRunning
			},
			paths: []string{amendAncestorPath},
			check: assertAncestorNotDelivered(string(run.StageStateSucceeded), string(run.StateRunning)),
		},
		{
			name: "ancestor_implement_not_succeeded_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.sibImpls[2].State = run.StageStateRunning
			},
			paths: []string{amendAncestorPath},
			check: assertAncestorNotDelivered(string(run.StageStateRunning), string(run.StateSucceeded)),
		},
		{
			name:  "ancestor_without_wave_gate_refused_github_actions",
			setup: setLockedRunnerKind(run.RunnerKindGitHubActions),
			paths: []string{amendAncestorPath},
			check: assertAncestorNoWaveGate,
		},
		{
			name:  "ancestor_without_wave_gate_refused_gitlab_ci",
			setup: setLockedRunnerKind(run.RunnerKindGitLabCI),
			paths: []string{amendAncestorPath},
			check: assertAncestorNoWaveGate,
		},
		{
			name:  "ancestor_without_wave_gate_refused_unknown_kind",
			setup: setLockedRunnerKind("banana"),
			paths: []string{amendAncestorPath},
			check: assertAncestorNoWaveGate,
		},
		{
			name:  "ancestor_without_wave_gate_refused_empty_kind",
			setup: setLockedRunnerKind(""),
			paths: []string{amendAncestorPath},
			check: assertAncestorNoWaveGate,
		},
		{
			// The Captain's rule: a remote ancestor's later success infers nothing.
			name: "ancestor_parent_locked_remote_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.child.RunnerKindResolved = false
				f.parent.RunnerKind = run.RunnerKindGitHubActions
				f.parent.RunnerKindResolved = true
			},
			paths: []string{amendAncestorPath},
			check: assertAncestorNoWaveGate,
		},
		{
			name:    "any_undelivered_ancestor_refused",
			fixture: newAmendOverlapFixture,
			setup: func(t *testing.T, f *amendFixture) {
				t.Helper()
				f.setRunning(2)
				f.integrate(amendConsolidated, 1)
				pinOverlapOwners(t)
			},
			paths: []string{"shared/file.go"},
			check: func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, parkBefore string) {
				body := assertProofRefusal(t, f, w, parkBefore, http.StatusConflict, "amend_refused_owner_slice_active", amendAncestorNotDelivered, true)
				if body.Details["slice_index"] != float64(2) {
					t.Errorf("slice_index = %v, want the LATER, undelivered owner 2", body.Details["slice_index"])
				}
			},
		},
		{
			name:    "all_overlapping_ancestors_delivered_allowed",
			fixture: newAmendOverlapFixture,
			setup: func(t *testing.T, f *amendFixture) {
				t.Helper()
				f.deliver(2)
				f.integrate(amendConsolidated, 1, 2)
				pinOverlapOwners(t)
			},
			paths: []string{"shared/file.go"},
			check: func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, _ string) {
				assertAmendAdmitted(t, f, w, 1, 2)
			},
		},
		{
			name:  "undeclared_path_unresolved",
			paths: []string{amendUndeclaredPath},
			check: func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, parkBefore string) {
				amendRefusal(t, w, http.StatusConflict, "amend_owner_attribution_unresolved")
				if !strings.Contains(w.Body.String(), amendOwnerUnresolvedNoEntry) {
					t.Errorf("body must name %s: %s", amendOwnerUnresolvedNoEntry, w.Body.String())
				}
				f.assertAmendUntouched(t, parkBefore)
			},
		},
		{
			name: "plan_read_error_fails_closed",
			setup: func(_ *testing.T, f *amendFixture) {
				f.art.listErr = errors.New(cause)
			},
			paths: []string{amendCoupledPath},
			check: assertPlanUnreadable(cause),
		},
		{
			name: "plan_read_error_acknowledged_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.art.listErr = errors.New(cause)
			},
			paths: []string{amendCoupledPath},
			ack:   true,
			check: assertPlanUnreadable(cause),
		},
		{
			name: "nil_artifact_repo_not_retryable",
			setup: func(_ *testing.T, f *amendFixture) {
				f.s.cfg.ArtifactRepo = nil
			},
			paths: []string{amendCoupledPath},
			check: assertPlanStoreUnconfigured,
		},
		{
			name: "nil_artifact_repo_acknowledged_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.s.cfg.ArtifactRepo = nil
			},
			paths: []string{amendCoupledPath},
			ack:   true,
			check: assertPlanStoreUnconfigured,
		},
		{
			name: "plan_absent_fails_closed_no_artifact",
			setup: func(_ *testing.T, f *amendFixture) {
				f.art.all = nil
			},
			paths: []string{amendCoupledPath},
			check: assertDecompositionAbsent,
		},
		{
			name: "plan_absent_fails_closed_no_decomposition",
			setup: func(t *testing.T, f *amendFixture) {
				f.seedParentPlan(t, &plan.Plan{PlanVersion: "standard_v1", Summary: "flat",
					Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"}})
			},
			paths: []string{amendCoupledPath},
			check: assertDecompositionAbsent,
		},
		{
			name: "plan_absent_fails_closed_slice_out_of_range",
			setup: func(_ *testing.T, f *amendFixture) {
				idx := 99
				f.child.SliceIndex = &idx
			},
			paths: []string{amendCoupledPath},
			check: assertDecompositionAbsent,
		},
		{
			name: "plan_absent_fails_closed_nil_slice_index",
			setup: func(_ *testing.T, f *amendFixture) {
				f.child.SliceIndex = nil
			},
			paths: []string{amendCoupledPath},
			check: assertDecompositionAbsent,
		},
		{
			name: "refusal_audit_append_failure_still_refused",
			setup: func(_ *testing.T, f *amendFixture) {
				f.au.appendErrCategory = CategoryScopeAmendmentRefused
			},
			paths: []string{amendCoupledPath},
			check: func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, parkBefore string) {
				amendRefusal(t, w, http.StatusConflict, errCodeAmendmentPathOwnedBySibling)
				if rows := f.refusedRows(); len(rows) != 0 {
					t.Errorf("%d refused rows despite the failed append", len(rows))
				}
				f.assertAmendUntouched(t, parkBefore)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := tc.fixture
			if fixture == nil {
				fixture = newAmendOwnershipFixture
			}
			f := fixture(t)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			parkBefore := f.parkSnapshot(t)
			w := postAmend(t, f, amendDecisionBody("widen for the slice-ownership contract", tc.ack, tc.paths...))
			tc.check(t, f, w, parkBefore)
		})
	}
}

// setLockedRunnerKind locks the child to kind, with a fully delivered ancestor, so
// the dispatch channel alone decides.
func setLockedRunnerKind(kind string) func(*testing.T, *amendFixture) {
	return func(_ *testing.T, f *amendFixture) {
		f.child.RunnerKind = kind
		f.child.RunnerKindResolved = true
	}
}

// pinOverlapOwners pins, as a precondition, that the first actual owner of
// shared/file.go is slice 1 and the later is slice 2: stopping after the first is
// what M5 mutates, so the order is what makes that fixture discriminating.
func pinOverlapOwners(t *testing.T) {
	t.Helper()
	subs := ownershipSlices(
		ownSpec{files: []string{"own/"}, deps: []int{1, 2}},
		ownSpec{files: []string{"shared/"}},
		ownSpec{files: []string{"shared/file.go"}},
	)
	c := classifySliceOwnership(subs, 0, []string{"shared/file.go"})
	got := []int{}
	for _, o := range c.AncestorOwned {
		got = append(got, o.OwningSlice)
	}
	if fmt.Sprint(got) != "[1 2]" {
		t.Fatalf("declared ancestor owners = %v, want [1 2]", got)
	}
}

func assertAncestorNotDelivered(wantImplement, wantRun string) func(*testing.T, *amendFixture, *httptest.ResponseRecorder, string) {
	return func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, parkBefore string) {
		t.Helper()
		body := assertProofRefusal(t, f, w, parkBefore, http.StatusConflict, "amend_refused_owner_slice_active", amendAncestorNotDelivered, true)
		if body.Details["sibling_implement_state"] != wantImplement {
			t.Errorf("sibling_implement_state = %v, want %s", body.Details["sibling_implement_state"], wantImplement)
		}
		if body.Details["sibling_run_state"] != wantRun {
			t.Errorf("sibling_run_state = %v, want %s", body.Details["sibling_run_state"], wantRun)
		}
		if body.Details["sibling_run_id"] != f.siblings[2].ID.String() {
			t.Errorf("sibling_run_id = %v, want the ancestor %s", body.Details["sibling_run_id"], f.siblings[2].ID)
		}
	}
}

func assertAncestorNoWaveGate(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, parkBefore string) {
	t.Helper()
	assertProofRefusal(t, f, w, parkBefore, http.StatusConflict, "amend_refused_owner_slice_active", amendAncestorNoWaveGate, false)
}

func assertPlanUnreadable(cause string) func(*testing.T, *amendFixture, *httptest.ResponseRecorder, string) {
	return func(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, parkBefore string) {
		t.Helper()
		assertProofRefusal(t, f, w, parkBefore, http.StatusInternalServerError, errCodeSliceOwnershipCheckFailed, "parent_plan_unreadable", true, cause)
	}
}

func assertPlanStoreUnconfigured(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, parkBefore string) {
	t.Helper()
	body := assertProofRefusal(t, f, w, parkBefore, http.StatusInternalServerError, errCodeSliceOwnershipCheckFailed, "plan_store_unconfigured", false)
	if remedy, _ := body.Details["remedy"].(string); !strings.Contains(remedy, "configure the artifact repository") {
		t.Errorf("remedy = %q, want it to name the configuration", remedy)
	}
}

func assertDecompositionAbsent(t *testing.T, f *amendFixture, w *httptest.ResponseRecorder, parkBefore string) {
	t.Helper()
	assertProofRefusal(t, f, w, parkBefore, http.StatusInternalServerError, errCodeSliceOwnershipCheckFailed, "decomposition_absent", false)
}

// getRunErrForRepo fails GetRun only for failFor: the parent lock read.
type getRunErrForRepo struct {
	*orchestratorRepo
	failFor uuid.UUID
	err     error
}

func (r *getRunErrForRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if id == r.failFor {
		return nil, r.err
	}
	return r.orchestratorRepo.GetRun(ctx, id)
}

// TestAmend_AncestorProof_AcknowledgeNeverAdmits resolves HIGH defect 53c9cd95:
// every failure of the proof is a hard refusal that acknowledge_owner_unresolved
// cannot admit. Each mode is seeded by construction against a transitive-ancestor
// path with every other proof element satisfied, under local and github_actions
// and with the flag true and false. On github_actions every mode answers
// ancestor_no_wave_gate, decided before any read's result is consulted.
func TestAmend_AncestorProof_AcknowledgeNeverAdmits(t *testing.T) {
	const (
		codeActive = "amend_refused_owner_slice_active"
		codeFailed = errCodeSliceOwnershipCheckFailed
	)
	for _, mode := range []struct {
		name      string
		setup     func(*amendFixture)
		paths     []string
		status    int
		code      string
		reason    string
		retryable bool
		cause     string
	}{
		{
			name:   "sibling_list_error",
			setup:  func(f *amendFixture) { f.s.cfg.RunRepo = &listRunsErrRepo{f.rr} },
			status: http.StatusInternalServerError, code: codeFailed, reason: amendAncestorSiblingsUnreadable, retryable: true,
			cause: "sibling listing unavailable",
		},
		{
			name: "sibling_run_missing",
			setup: func(f *amendFixture) {
				f.rr.mu.Lock()
				defer f.rr.mu.Unlock()
				delete(f.rr.runs, f.siblings[3].ID)
			},
			status: http.StatusConflict, code: codeActive, reason: amendAncestorRunNotFound,
		},
		{
			name:   "sibling_slice_index_nil",
			setup:  func(f *amendFixture) { f.siblings[3].SliceIndex = nil },
			status: http.StatusConflict, code: codeActive, reason: amendAncestorRunNotFound,
		},
		{
			name: "stages_unreadable",
			setup: func(f *amendFixture) {
				f.s.cfg.RunRepo = &listStagesErrRepo{orchestratorRepo: f.rr, failFor: f.siblings[3].ID}
			},
			status: http.StatusInternalServerError, code: codeFailed, reason: amendAncestorStagesUnreadable, retryable: true,
			cause: "stage listing unavailable",
		},
		{
			name:   "implement_absent",
			setup:  func(f *amendFixture) { f.sibImpls[3].Type = run.StageTypeReview },
			status: http.StatusConflict, code: codeActive, reason: amendAncestorNoImplement,
		},
		{
			name:   "integration_read_error",
			setup:  func(f *amendFixture) { f.au.listByCategoryErrCategory = "slices_integrated" },
			status: http.StatusInternalServerError, code: codeFailed, reason: amendAncestorIntegrationUnreadable, retryable: true,
			cause: "auditFake: injected ListForRunByCategory error",
		},
		{
			name:   "integration_absent",
			setup:  func(f *amendFixture) { f.au.seeded = nil },
			status: http.StatusConflict, code: codeActive, reason: amendAncestorNotIntegrated, retryable: true,
		},
		{
			name:   "integration_undecodable",
			setup:  func(f *amendFixture) { f.seedIntegrationPayload(json.RawMessage(`not json`)) },
			status: http.StatusConflict, code: codeActive, reason: amendAncestorNotIntegrated, retryable: true,
		},
		{
			name:   "integration_empty_branch",
			setup:  func(f *amendFixture) { f.integrate("", 2, 3) },
			status: http.StatusConflict, code: codeActive, reason: amendAncestorNotIntegrated, retryable: true,
		},
		{
			name:   "integration_stale",
			setup:  func(f *amendFixture) { f.integrateIDs(amendConsolidated, uuid.NewString()) },
			status: http.StatusConflict, code: codeActive, reason: amendAncestorNotIntegrated, retryable: true,
		},
		{
			name: "park_no_entry_undelivered",
			setup: func(f *amendFixture) {
				f.stage.ScopeCompletenessPark.OwningSlices = nil
				f.setRunning(3)
			},
			status: http.StatusConflict, code: codeActive, reason: amendAncestorNotDelivered, retryable: true,
		},
		{
			name: "park_nil_index_undelivered",
			setup: func(f *amendFixture) {
				f.stage.ScopeCompletenessPark.OwningSlices = []run.BuildRequiredOwner{{Path: amendTransitivePath}}
				f.setRunning(3)
			},
			status: http.StatusConflict, code: codeActive, reason: amendAncestorNotDelivered, retryable: true,
		},
		{
			// The unresolved path comes FIRST, so a refusal that merely joined the
			// unresolved set would be acknowledgeable.
			name: "mixed_legacy_and_undelivered",
			setup: func(f *amendFixture) {
				f.stage.ScopeCompletenessPark.OwningSlices = nil
				f.setRunning(3)
			},
			paths:  []string{amendUndeclaredPath, amendTransitivePath},
			status: http.StatusConflict, code: codeActive, reason: amendAncestorNotDelivered, retryable: true,
		},
	} {
		for _, kind := range []string{run.RunnerKindLocal, run.RunnerKindGitHubActions} {
			for _, ack := range []bool{true, false} {
				t.Run(fmt.Sprintf("%s/%s/ack_%v", mode.name, kind, ack), func(t *testing.T) {
					f := newAmendOwnershipFixture(t)
					f.child.RunnerKind = kind
					mode.setup(f)
					paths := mode.paths
					if paths == nil {
						paths = []string{amendTransitivePath}
					}
					parkBefore := f.parkSnapshot(t)
					w := postAmend(t, f, amendDecisionBody("widen against an unproven ancestor", ack, paths...))
					causes := []string{}
					if mode.cause != "" {
						causes = append(causes, mode.cause)
					}
					if kind == run.RunnerKindLocal {
						assertProofRefusal(t, f, w, parkBefore, mode.status, mode.code, mode.reason, mode.retryable, causes...)
						return
					}
					assertProofRefusal(t, f, w, parkBefore, http.StatusConflict, codeActive, amendAncestorNoWaveGate, false, causes...)
				})
			}
		}
	}

	for _, ack := range []bool{true, false} {
		t.Run(fmt.Sprintf("parent_lock_read_error/ack_%v", ack), func(t *testing.T) {
			f := newAmendOwnershipFixture(t)
			const cause = "parent lock read offline"
			f.child.RunnerKindResolved = false
			f.s.cfg.RunRepo = &getRunErrForRepo{orchestratorRepo: f.rr, failFor: f.parent.ID, err: errors.New(cause)}
			parkBefore := f.parkSnapshot(t)
			w := postAmend(t, f, amendDecisionBody("widen with an unreadable parent lock", ack, amendTransitivePath))
			assertProofRefusal(t, f, w, parkBefore, http.StatusInternalServerError, codeFailed, amendAncestorChannelUnreadable, true, cause)
		})
	}
}

// proofReadCounts counts the reads the ancestor proof makes.
type proofReadCounts struct {
	*orchestratorRepo
	stages map[uuid.UUID]int
	getRun map[uuid.UUID]int
}

func (r *proofReadCounts) ListStagesForRun(ctx context.Context, id uuid.UUID) ([]*run.Stage, error) {
	r.stages[id]++
	return r.orchestratorRepo.ListStagesForRun(ctx, id)
}

func (r *proofReadCounts) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	r.getRun[id]++
	return r.orchestratorRepo.GetRun(ctx, id)
}

// integrationReadCounts counts the slices_integrated reads.
type integrationReadCounts struct {
	*auditFake
	reads int
}

func (a *integrationReadCounts) ListForRunByCategory(ctx context.Context, id uuid.UUID, category string) ([]*audit.Entry, error) {
	if category == "slices_integrated" {
		a.reads++
	}
	return a.auditFake.ListForRunByCategory(ctx, id, category)
}

// TestAmend_AncestorProof_ReadsOncePerDecision pins the proof's memo: however many
// paths a decision names, each ancestor's stages, the parent's lock and the
// parent's integration record are read once.
func TestAmend_AncestorProof_ReadsOncePerDecision(t *testing.T) {
	f := newAmendFixtureWithPlan(t, 0, ownershipSlices(
		ownSpec{files: []string{"own/"}, deps: []int{1, 2}},
		ownSpec{files: []string{"docs/one/"}},
		ownSpec{files: []string{"docs/two/"}},
	))
	f.stage.ScopeCompletenessPark.BuildRequiredPaths = nil
	f.stage.ScopeCompletenessPark.OwningSlices = nil
	f.stage.ScopeCompletenessPark.MissingPaths = []string{"docs/one/a.md"}
	f.deliver(1)
	f.deliver(2)
	f.integrate(amendConsolidated, 1, 2)
	f.child.RunnerKindResolved = false // so the channel check reads the parent lock

	runs := &proofReadCounts{orchestratorRepo: f.rr, stages: map[uuid.UUID]int{}, getRun: map[uuid.UUID]int{}}
	au := &integrationReadCounts{auditFake: f.au}
	f.s.cfg.RunRepo = runs
	f.s.cfg.AuditRepo = au

	w := postAmend(t, f, amendDecisionBody("three paths, two ancestors", false, "docs/one/a.md", "docs/one/b.md", "docs/two/c.md"))
	assertAmendAdmitted(t, f, w, 1, 1, 2)
	if got := runs.stages[f.siblings[1].ID]; got != 1 {
		t.Errorf("ancestor 1 stages read %d times for two paths, want 1", got)
	}
	if got := runs.stages[f.siblings[2].ID]; got != 1 {
		t.Errorf("ancestor 2 stages read %d times, want 1", got)
	}
	if got := runs.getRun[f.parent.ID]; got != 1 {
		t.Errorf("parent lock read %d times, want 1", got)
	}
	if au.reads != 1 {
		t.Errorf("slices_integrated read %d times for two ancestors, want 1", au.reads)
	}
}

// amendE2E is one fresh real-Postgres decomposition for the end-to-end arms.
type amendE2E struct {
	ctx       context.Context
	runRepo   run.Repository
	auditRepo audit.Repository
	amendRepo scopeamendment.Repository
	s         *Server
	parent    *run.Run
	child     *run.Run
	stageID   uuid.UUID
	key       *signing.IssuedKey
	siblings  map[int]*run.Run
	impls     map[int]*run.Stage
}

// seedAmendE2E mints the real shape over real repositories: a parent locked local
// through ResolveRunnerKind, an approved decomposition, one child per slice minted
// un-resolved with the kind copied, and the requesting child parked through the
// REAL signed runner report carrying missing_paths only, so coverage does not
// constrain the amend.
func seedAmendE2E(t *testing.T, ctx context.Context, runRepo run.Repository, auditRepo audit.Repository, signingRepo signing.Repository, amendRepo scopeamendment.Repository, art *fakeArtifactRepo, childKind string, childSlice int, subPlans []plan.SubPlanSummary) *amendE2E {
	t.Helper()
	mint := func(kind string, parent *uuid.UUID, idx *int) *run.Run {
		t.Helper()
		r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
			Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change", WorkflowSHA: "deadbeef",
			TriggerSource: run.TriggerCLI, RunnerKind: kind, ParentRunID: parent, DecomposedFrom: parent, SliceIndex: idx,
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		return r
	}
	parent := mint(run.RunnerKindLocal, nil, nil)
	locker, ok := any(runRepo).(interface {
		ResolveRunnerKind(context.Context, uuid.UUID, string) (run.RunnerKindResolution, error)
	})
	if !ok {
		t.Fatal("run repository cannot lock runner_kind")
	}
	if _, err := locker.ResolveRunnerKind(ctx, parent.ID, run.RunnerKindLocal); err != nil {
		t.Fatalf("lock parent runner_kind: %v", err)
	}
	planStage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: parent.ID, Sequence: 0, Type: run.StageTypePlan, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create plan stage: %v", err)
	}
	planBytes, err := json.Marshal(&plan.Plan{
		PlanVersion: "standard_v1", Summary: "decomposed",
		Verification:  plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Decomposition: &plan.Decomposition{Rationale: "split", SubPlans: subPlans},
	})
	if err != nil {
		t.Fatal(err)
	}
	sv := "standard_v1"
	if _, err := art.Create(ctx, artifact.CreateParams{
		StageID: planStage.ID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	e := &amendE2E{ctx: ctx, runRepo: runRepo, auditRepo: auditRepo, amendRepo: amendRepo, parent: parent,
		siblings: map[int]*run.Run{}, impls: map[int]*run.Stage{}}
	e.s = New(Config{Addr: "127.0.0.1:0", RunRepo: runRepo, AuditRepo: auditRepo, SigningRepo: signingRepo,
		ArtifactRepo: art, ScopeAmendmentRepo: amendRepo})
	e.s.promptIssueGetterOverride = &stubIssueGetter{}

	for i := range subPlans {
		idx := i
		pid := parent.ID
		sib := mint(childKind, &pid, &idx)
		impl, err := runRepo.CreateStage(ctx, run.CreateStageParams{
			RunID: sib.ID, Sequence: 1, Type: run.StageTypeImplement, ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		})
		if err != nil {
			t.Fatalf("create implement stage: %v", err)
		}
		if i == childSlice {
			e.child, e.stageID = sib, impl.ID
			continue
		}
		e.siblings[i], e.impls[i] = sib, impl
	}
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning} {
		if _, err := runRepo.TransitionStage(ctx, e.stageID, to, nil); err != nil {
			t.Fatalf("transition requester stage to %s: %v", to, err)
		}
	}
	e.key, err = signingRepo.Issue(ctx, e.child.ID, signing.DefaultTTL)
	if err != nil {
		t.Fatalf("issue signing key: %v", err)
	}
	parkBytes := []byte(`{"outcome":"scope_park","branch":"` + amendRunBranch + `",` +
		`"head_sha":"` + amendHeldSHA + `","base_sha":"3333333333333333333333333333333333333333",` +
		`"verified_tree_sha":"` + amendVerifiedTree + `",` +
		`"missing_paths":["docs/never-edited.md"]}`)
	if pw := shipPRRequest(t, e.s, e.child.ID, e.stageID, e.key.PrivateKey, parkBytes, ""); pw.Code != http.StatusOK {
		t.Fatalf("scope_park report status = %d, want 200:\n%s", pw.Code, pw.Body.String())
	}
	return e
}

// deliver walks a sibling's run and implement stage to succeeded.
func (e *amendE2E) deliver(t *testing.T, idx int) {
	t.Helper()
	for _, to := range []run.StageState{run.StageStateDispatched, run.StageStateRunning, run.StageStateSucceeded} {
		if _, err := e.runRepo.TransitionStage(e.ctx, e.impls[idx].ID, to, nil); err != nil {
			t.Fatalf("transition implement to %s: %v", to, err)
		}
	}
	for _, to := range []run.State{run.StateRunning, run.StateSucceeded} {
		if _, err := e.runRepo.TransitionRun(e.ctx, e.siblings[idx].ID, to); err != nil {
			t.Fatalf("transition run to %s: %v", to, err)
		}
	}
}

// integrate appends a slices_integrated entry to the parent's REAL chain.
func (e *amendE2E) integrate(t *testing.T, slices ...int) {
	t.Helper()
	ids := make([]string, 0, len(slices))
	for _, i := range slices {
		ids = append(ids, e.siblings[i].ID.String())
	}
	payload, _ := json.Marshal(map[string]any{"consolidated_branch": amendConsolidated, "child_run_ids": ids})
	actor := audit.ActorSystem
	subject := "test"
	if _, err := e.auditRepo.AppendChained(e.ctx, audit.ChainAppendParams{
		RunID: e.parent.ID, Timestamp: time.Now().UTC(), Category: "slices_integrated",
		ActorKind: &actor, ActorSubject: &subject, Payload: payload,
	}); err != nil {
		t.Fatalf("append slices_integrated: %v", err)
	}
}

// assertUntouched pins the persisted refusal shape on real repositories.
func (e *amendE2E) assertUntouched(t *testing.T, parkBefore string) {
	t.Helper()
	rows, err := e.amendRepo.ListByRun(e.ctx, e.child.ID)
	if err != nil {
		t.Fatalf("ListByRun: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("%d amendment row(s), want 0", len(rows))
	}
	if got := e.parkSnapshot(t); got != parkBefore {
		t.Errorf("persisted park changed:\n got: %s\nwant: %s", got, parkBefore)
	}
	stage, err := e.runRepo.GetStage(e.ctx, e.stageID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	if stage.State != run.StageStateAwaitingScopeDecision {
		t.Errorf("stage state = %q, want parked", stage.State)
	}
}

func (e *amendE2E) parkSnapshot(t *testing.T) string {
	t.Helper()
	stage, err := e.runRepo.GetStage(e.ctx, e.stageID)
	if err != nil {
		t.Fatalf("GetStage: %v", err)
	}
	raw, err := json.Marshal(stage.ScopeCompletenessPark)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// assertScopeFile fetches the REAL signed /prompt and pins the amended entry.
func (e *amendE2E) assertScopeFile(t *testing.T, path, operation string) {
	t.Helper()
	prw := promptRequest(t, e.s, e.child.ID, e.stageID, e.key.PrivateKey, "")
	if prw.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want 200:\n%s", prw.Code, prw.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(prw.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode prompt: %v", err)
	}
	for _, f := range resp.ScopeFiles {
		if f.Path == path {
			if f.Operation != operation {
				t.Errorf("scope file %s operation = %q, want %q", path, f.Operation, operation)
			}
			return
		}
	}
	t.Errorf("%s missing from the re-dispatched prompt's scope_files: %+v", path, resp.ScopeFiles)
}

// TestAmend_SliceOwnership_EndToEnd crosses HTTP handler, plan read, sibling
// paging, audit chain, amendment store and the signed prompt on REAL Postgres.
// delivered_transitive_ancestor_path_folds also proves the paged sibling read,
// because postgresRepo.ListRuns rejects Limit <= 0 and the in-memory fake does not.
func TestAmend_SliceOwnership_EndToEnd(t *testing.T) {
	pool := pgtest.NewPool(t)
	ctx := context.Background()
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	signingRepo := signing.NewPostgresRepository(pool)
	amendRepo := scopeamendment.NewPostgresRepository(pool)

	const transitive = "docs/slice3-only.md"
	base := func() []plan.SubPlanSummary {
		return ownershipSlices(
			ownSpec{files: []string{"internal/own/"}, deps: []int{2}},
			ownSpec{files: []string{"docs/slice1.md"}},
			ownSpec{files: []string{"docs/slice2.md"}, deps: []int{3}},
			ownSpec{files: []string{transitive}},
		)
	}
	seed := func(t *testing.T, kind string) *amendE2E {
		t.Helper()
		return seedAmendE2E(t, ctx, runRepo, auditRepo, signingRepo, amendRepo, newFakeArtifactRepo(), kind, 0, base())
	}
	decide := func(t *testing.T, e *amendE2E, body string) *httptest.ResponseRecorder {
		t.Helper()
		return postScopeCompletenessDecision(t, e.s, e.child.ID, body, operatorWriteStages)
	}

	t.Run("delivered_transitive_ancestor_path_folds", func(t *testing.T) {
		e := seed(t, run.RunnerKindLocal)
		e.deliver(t, 2)
		e.deliver(t, 3)
		e.integrate(t, 2, 3)
		w := decide(t, e, `{"decision":"amend","reason":"needs the ancestor's file","paths":[{"path":"`+transitive+`","operation":"modify"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		e.assertScopeFile(t, transitive, string(plan.FileOpModify))
	})

	t.Run("own_slice_directory_path_folds", func(t *testing.T) {
		e := seed(t, run.RunnerKindLocal)
		w := decide(t, e, `{"decision":"amend","reason":"a new file in my own directory","paths":[{"path":"internal/own/extra.go","operation":"create"}]}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
		}
		e.assertScopeFile(t, "internal/own/extra.go", string(plan.FileOpCreate))
	})

	t.Run("non_ancestor_refused_with_chain_row", func(t *testing.T) {
		e := seed(t, run.RunnerKindLocal)
		before := e.parkSnapshot(t)
		w := decide(t, e, `{"decision":"amend","reason":"a sibling's file","paths":[{"path":"docs/slice1.md","operation":"modify"}]}`)
		amendRefusal(t, w, http.StatusConflict, errCodeAmendmentPathOwnedBySibling)
		entries, err := auditRepo.ListForRunByCategory(ctx, e.child.ID, CategoryScopeAmendmentRefused)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Fatalf("%d scope_amendment_refused chain rows, want 1", len(entries))
		}
		e.assertUntouched(t, before)
	})

	t.Run("not_integrated_ancestor_acknowledged_refused", func(t *testing.T) {
		e := seed(t, run.RunnerKindLocal)
		e.deliver(t, 2)
		e.deliver(t, 3)
		before := e.parkSnapshot(t)
		w := decide(t, e, `{"decision":"amend","reason":"no integration yet","acknowledge_owner_unresolved":true,"paths":[{"path":"`+transitive+`","operation":"modify"}]}`)
		body := amendRefusal(t, w, http.StatusConflict, "amend_refused_owner_slice_active")
		if body.Details["reason"] != amendAncestorNotIntegrated || body.Details["retryable"] != true {
			t.Errorf("details = %v, want %s retryable", body.Details, amendAncestorNotIntegrated)
		}
		e.assertUntouched(t, before)
	})

	t.Run("no_wave_gate_acknowledged_refused", func(t *testing.T) {
		e := seed(t, run.RunnerKindGitHubActions)
		e.deliver(t, 2)
		e.deliver(t, 3)
		e.integrate(t, 2, 3)
		before := e.parkSnapshot(t)
		w := decide(t, e, `{"decision":"amend","reason":"remote child","acknowledge_owner_unresolved":true,"paths":[{"path":"`+transitive+`","operation":"modify"}]}`)
		body := amendRefusal(t, w, http.StatusConflict, "amend_refused_owner_slice_active")
		if body.Details["reason"] != amendAncestorNoWaveGate || body.Details["retryable"] != false {
			t.Errorf("details = %v, want %s not retryable", body.Details, amendAncestorNoWaveGate)
		}
		e.assertUntouched(t, before)
	})
}
