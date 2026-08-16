package server

import (
	"context"
	"encoding/json"
	"errors"
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
)

// amendFixture is the state an `amend` decision acts on: a DECOMPOSED CHILD
// (slice 0) whose implement stage is parked on a #2548 BUILD-REQUIRED shortfall
// — the class whose `exempt` is refused outright, so before #2591 its only
// resolution was `fail`. The coupled path is attributed to sibling slice 1,
// whose own implement stage exists and has NOT started.
type amendFixture struct {
	s       *Server
	rr      *orchestratorRepo
	au      *auditFake
	sa      *fakeScopeAmendmentRepo
	child   *run.Run
	stage   *run.Stage
	sibling *run.Run
	sibImpl *run.Stage
}

func newAmendFixture(t *testing.T) *amendFixture {
	t.Helper()
	rr := newOrchestratorRepo()
	parentID := uuid.New()

	child := rr.seedRun()
	slice0, slice1 := 0, 1
	child.DecomposedFrom = &parentID
	child.SliceIndex = &slice0

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

	sibling := rr.seedRun()
	sibling.DecomposedFrom = &parentID
	sibling.SliceIndex = &slice1
	sibImpl := rr.seedStage(sibling.ID, 1, run.StageStatePending)
	sibImpl.Type = run.StageTypeImplement // StartedAt nil: the sibling has not begun

	au := newAuditFake()
	sa := newFakeScopeAmendmentRepo()
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: au, ScopeAmendmentRepo: sa})

	return &amendFixture{s: s, rr: rr, au: au, sa: sa,
		child: child, stage: stage, sibling: sibling, sibImpl: sibImpl}
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
// sibling that OWNS the coupled path has already STARTED its implement pass, so
// from that moment two branches can carry divergent edits to one file. The
// started state is seeded BY CONSTRUCTION (StartedAt set at fixture time).
func TestAmend_OwnerSliceActive409(t *testing.T) {
	f := newAmendFixture(t)
	started := time.Now().UTC().Add(-time.Minute)
	f.sibImpl.StartedAt = &started
	f.sibImpl.State = run.StageStateRunning

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
	f.assertNoSideEffects(t, "an amend whose owning sibling slice is already running")
}

// TestAmend_OwnerSliceNotStartedAllowed is the OTHER side of the StartedAt
// predicate: an owner that has not begun cannot hold a divergent edit, so the
// amend proceeds with NO acknowledgement and the owning slice is named.
func TestAmend_OwnerSliceNotStartedAllowed(t *testing.T) {
	f := newAmendFixture(t) // sibling implement stage: StartedAt nil, state pending

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
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAmendFixture(t)
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

// TestAmend_OwnerListRunsErrorFailsClosed: an unreadable sibling list is not
// evidence that no sibling is active, so it takes the same fail-CLOSED path.
func TestAmend_OwnerListRunsErrorFailsClosed(t *testing.T) {
	f := newAmendFixture(t)
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newAmendFixture(t)
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
// covers UNRESOLVED ownership only. A RESOLVED-and-started owner is a known
// conflict and stays refused however loudly the operator acknowledges.
func TestAmend_AcknowledgementDoesNotRelaxTheActiveOwnerRefusal(t *testing.T) {
	f := newAmendFixture(t)
	started := time.Now().UTC().Add(-time.Minute)
	f.sibImpl.StartedAt = &started

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
