package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- ADR-090 D5 fan-in hold fixtures (#4018) ---------------------------------

// fanInHead is the consolidated head cannedCompareOneFile resolves (its last
// commit), the head every hold decision is bound to.
const fanInHead = "headsha1"

// fanInTailSentinel rides a recorded passed verdict's output tail, so a test
// can tell the injected parent-level run reached the reviewer.
const fanInTailSentinel = "FANIN_MERGE_CANDIDATE_TAIL_SENTINEL"

// fanInVerifySpec declares a verify command AND a gating implement reviewer,
// so the consolidated review runs synchronously inside its goroutine.
var fanInVerifySpec = []byte(`version: "0.5"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
          verify:
            command: "scripts/test verify"
            timeout: "5m"
        reviewers:
          agent: 1
          human: 0
`)

// fanInRepo is orchestratorRepo plus the fix-up re-open and recovery edges
// (succeeded/awaiting_approval → pending, …) the production repository admits,
// so run.FixupStage re-opens the parent implement stage here exactly as it
// does against Postgres.
type fanInRepo struct {
	*orchestratorRepo
}

func (r *fanInRepo) TransitionStage(ctx context.Context, id uuid.UUID, to run.StageState, c *run.StageCompletion) (*run.Stage, error) {
	r.mu.Lock()
	if st, ok := r.stagesByID[id]; ok && st.State != to &&
		(run.ValidStageFixupTransition(st.State, to) || run.ValidStageFixupRecoveryTransition(st.State, to)) {
		st.State = to
		st.FailureCategory, st.FailureReason = nil, nil
		r.mu.Unlock()
		return st, nil
	}
	r.mu.Unlock()
	return r.orchestratorRepo.TransitionStage(ctx, id, to, c)
}

type fanInFixture struct {
	t        *testing.T
	s        *Server
	rr       *fanInRepo
	au       *auditFake
	reviewer *fakePlanReviewer
	parent   *run.Run
	impl     *run.Stage
	review   *run.Stage
	branch   string
}

// newFanInFixture seeds a decomposed parent (plan + plan artifact, a succeeded
// implement stage, a review stage parked at awaiting_approval — the shape
// dispatchStage leaves before it calls DispatchConsolidatedReview — and a
// linked child) and records one fan-in integration row so the consolidated
// head classifies as fan_in. The audit fake stamps append-order sequences so
// trigger consumption behaves like the chain.
func newFanInFixture(t *testing.T, spec []byte) *fanInFixture {
	t.Helper()
	rr := &fanInRepo{orchestratorRepo: newOrchestratorRepo()}
	art := newFakeArtifactRepo()
	au := newAuditFake()
	au.stampSequence = true
	parent, impl := seedConsolidatedParent(t, rr.orchestratorRepo, art, spec)
	review := rr.seedStage(parent.ID, 2, run.StageStateAwaitingApproval)
	review.Type = run.StageTypeReview
	review.ExecutorKind = run.ExecutorHuman

	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-8",
	}
	s := New(Config{
		Addr:          "127.0.0.1:0",
		RunRepo:       rr,
		ArtifactRepo:  art,
		AuditRepo:     au,
		ConcernRepo:   newFakeConcernRepo(),
		PlanReviewers: singleReviewerSet{reviewer},
		GitHub:        cannedComparePatchClient(t, cannedCompareOneFile),
		// A wired trace store makes the per-slice rollup run (the child has no
		// implement stage, so it renders one named-absence row), which is what
		// stamps decomposed_parent_no_parent_level_verify when the parent
		// carries no verify evidence of its own.
		TraceStore: &hashKeyedTraceStore{bodies: map[string][]byte{}},
	})
	f := &fanInFixture{t: t, s: s, rr: rr, au: au, reviewer: reviewer, parent: parent,
		impl: impl, review: review, branch: "fishhawk/run-" + parent.ID.String()[:8]}
	f.appendRow(nil, auditcomplete.CategoryIntegrationCommitRecorded, map[string]any{
		"integration_commit_shas": []string{"integ1"},
	})
	return f
}

func (f *fanInFixture) appendRow(stageID *uuid.UUID, category string, payload any) {
	raw, _ := json.Marshal(payload)
	f.au.mu.Lock()
	defer f.au.mu.Unlock()
	f.au.appended = append(f.au.appended, audit.ChainAppendParams{
		RunID: f.parent.ID, StageID: stageID, Category: category, Payload: raw,
	})
}

// seedTrigger seeds a fan-in trigger for head on the parent implement stage.
func (f *fanInFixture) seedTrigger(head string) {
	id := f.impl.ID
	f.appendRow(&id, CategoryStageMergeCandidateVerifyTriggered, mergeCandidateVerifyTrigger{
		Branch: f.branch, BaseRef: "main", ExpectedHeadSHA: head,
		Cause: mergeCandidateCauseFanIn, VerifyCommand: "scripts/test verify",
		PriorState: string(run.StageStateSucceeded),
	})
}

// seedVerdict seeds a trigger for head followed by its verdict (consuming it).
func (f *fanInFixture) seedVerdict(head, result string) {
	f.seedTrigger(head)
	id := f.impl.ID
	f.appendRow(&id, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
		HeadSHA: head, Cause: mergeCandidateCauseFanIn, VerifyCommand: "scripts/test verify",
		Result: result, OutputTail: fanInTailSentinel, OutputUntrusted: true,
	})
}

func (f *fanInFixture) runRow() *run.Run {
	f.t.Helper()
	r, err := f.rr.GetRun(context.Background(), f.parent.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fanInFixture) dispatch() {
	f.t.Helper()
	f.s.DispatchConsolidatedReview(context.Background(), f.parent.ID, "main", f.branch)
	f.s.waitBackgroundReviews()
}

func (f *fanInFixture) count(category string) int {
	return len(auditEntries(f.au, category))
}

func (f *fanInFixture) state(id uuid.UUID) run.StageState {
	f.t.Helper()
	st, err := f.rr.GetStage(context.Background(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return st.State
}

func (f *fanInFixture) reviewerPrompts() []string {
	f.reviewer.mu.Lock()
	defer f.reviewer.mu.Unlock()
	return append([]string(nil), f.reviewer.calls...)
}

// captureReviewEvidence wraps the buildImplementReviewPrompt seam so a test can
// read the GateEvidence each review round was BUILT with. The
// decomposed_parent_no_parent_level_verify literal lives on that struct and is
// deliberately not rendered when the per-slice rows are (#3132), so the prompt
// text alone cannot tell whether a parent-level verify replaced it.
func (f *fanInFixture) captureReviewEvidence() func() []*prompt.GateEvidence {
	f.t.Helper()
	var mu sync.Mutex
	var got []*prompt.GateEvidence
	orig := buildImplementReviewPrompt
	f.t.Cleanup(func() { buildImplementReviewPrompt = orig })
	buildImplementReviewPrompt = func(kind string, trig prompt.Trigger) (string, error) {
		mu.Lock()
		got = append(got, trig.GateEvidence)
		mu.Unlock()
		return orig(kind, trig)
	}
	return func() []*prompt.GateEvidence {
		mu.Lock()
		defer mu.Unlock()
		return append([]*prompt.GateEvidence(nil), got...)
	}
}

// assertParentLevelVerify pins the round's evidence: exactly one verify run, the
// fan-in pass for fanInHead, and no structural-absence marker.
func (f *fanInFixture) assertParentLevelVerify(evs []*prompt.GateEvidence) {
	f.t.Helper()
	if len(evs) != 1 || evs[0] == nil {
		f.t.Fatalf("review rounds built = %d (evidence %v), want 1 with evidence", len(evs), evs)
	}
	ev := evs[0]
	if ev.VerifyEvidenceUnavailableReason == decomposedParentNoParentLevelVerifyReason {
		f.t.Errorf("gate evidence carries %s although a parent-level verify passed for the head", decomposedParentNoParentLevelVerifyReason)
	}
	if len(ev.VerifyRuns) != 1 || ev.VerifyRuns[0].HeadSHA != fanInHead || ev.VerifyRuns[0].Outcome != "passed" ||
		ev.VerifyRuns[0].Command != "scripts/test verify" || ev.VerifyRuns[0].OutputTail != fanInTailSentinel {
		f.t.Errorf("verify runs = %+v, want the one fan-in pass for %s", ev.VerifyRuns, fanInHead)
	}
	if ev.VerifySummary == nil || ev.VerifySummary.Outcome != "passed" {
		f.t.Errorf("verify summary = %+v, want passed", ev.VerifySummary)
	}
	if len(ev.SliceVerify) == 0 {
		f.t.Error("per-slice rollup missing: the parent-level verify must sit BESIDE the slice rows, not replace them")
	}
}

// assertHeld pins a held round by COMMITTED state: no review round recorded
// and the reviewer never invoked.
func (f *fanInFixture) assertHeld() {
	f.t.Helper()
	if n := f.count("implement_review_started"); n != 0 {
		f.t.Fatalf("implement_review_started = %d, want 0 (the round must be held)", n)
	}
	if n := len(f.reviewerPrompts()); n != 0 {
		f.t.Fatalf("reviewer invoked %d times, want 0 (the round must be held)", n)
	}
}

// assertReviewed pins a dispatched round for fanInHead and returns its prompt.
func (f *fanInFixture) assertReviewed() string {
	f.t.Helper()
	started := auditEntries(f.au, "implement_review_started")
	if len(started) != 1 {
		f.t.Fatalf("implement_review_started = %d, want 1", len(started))
	}
	var p struct {
		HeadSHA string `json:"head_sha"`
	}
	_ = json.Unmarshal(started[0].Payload, &p)
	if p.HeadSHA != fanInHead {
		f.t.Errorf("review round head = %q, want %q", p.HeadSHA, fanInHead)
	}
	prompts := f.reviewerPrompts()
	if len(prompts) != 1 {
		f.t.Fatalf("reviewer invoked %d times, want 1", len(prompts))
	}
	return prompts[0]
}

// --- hold decisions ------------------------------------------------------------

// An unverified fan-in head triggers exactly one verify-only pass (system actor,
// cause fan_in, bound to the consolidated head) and dispatches NO review round.
func TestFanInHold_UnverifiedHead_TriggersPassAndHolds(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	f.dispatch()

	f.assertHeld()
	rows := mcvTriggerRows(f.au)
	if len(rows) != 1 {
		t.Fatalf("stage_merge_candidate_verify_triggered = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.ActorKind == nil || *row.ActorKind != audit.ActorSystem ||
		row.ActorSubject == nil || *row.ActorSubject != mergeCandidateSystemSubject {
		t.Errorf("trigger actor = %v/%v, want system/%s", row.ActorKind, row.ActorSubject, mergeCandidateSystemSubject)
	}
	if row.StageID == nil || *row.StageID != f.impl.ID {
		t.Errorf("trigger stage = %v, want the parent implement stage %s", row.StageID, f.impl.ID)
	}
	var trig mergeCandidateVerifyTrigger
	if err := json.Unmarshal(row.Payload, &trig); err != nil {
		t.Fatal(err)
	}
	want := mergeCandidateVerifyTrigger{
		Branch: f.branch, BaseRef: "main", ExpectedHeadSHA: fanInHead, Cause: mergeCandidateCauseFanIn,
		VerifyCommand: "scripts/test verify", PriorState: string(run.StageStateSucceeded),
		ReparkedReviewStageID: f.review.ID.String(),
	}
	if trig != want {
		t.Errorf("trigger = %+v, want %+v", trig, want)
	}
	// The pass re-opened the parent implement stage and re-parked its review.
	if got := f.state(f.impl.ID); got != run.StageStatePending {
		t.Errorf("implement state = %q, want pending (re-opened for the pass)", got)
	}
	if got := f.state(f.review.ID); got != run.StageStatePending {
		t.Errorf("review state = %q, want pending (re-parked by the pass)", got)
	}
}

// A passed verdict for exactly the consolidated head releases the hold: the
// review runs, no new trigger is appended, and the round's gate evidence
// carries the authoritative parent-level verify, so the
// decomposed_parent_no_parent_level_verify absence is not rendered.
func TestFanInHold_PassedHead_ReviewRunsWithParentLevelEvidence(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	evidence := f.captureReviewEvidence()
	f.seedVerdict(fanInHead, mergeCandidateResultPassed)
	f.dispatch()

	got := f.assertReviewed()
	if n := len(mcvTriggerRows(f.au)); n != 1 {
		t.Errorf("trigger rows = %d, want 1 (the seeded one; a passed head starts nothing)", n)
	}
	f.assertParentLevelVerify(evidence())
	if !strings.Contains(got, fanInTailSentinel) {
		t.Errorf("reviewer prompt lacks the injected parent-level verify tail")
	}
}

// A passed verdict for a DIFFERENT head neither releases the hold nor counts as
// evidence (D2 exact-head binding): the head is unverified and a pass starts.
func TestFanInHold_PassedForOtherHead_StillHolds(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	f.seedVerdict("otherhead", mergeCandidateResultPassed)
	f.dispatch()

	f.assertHeld()
	if n := len(mcvTriggerRows(f.au)); n != 2 {
		t.Errorf("trigger rows = %d, want 2 (the seeded one + a fresh pass for %s)", n, fanInHead)
	}
}

// A failed verdict holds the round and starts nothing: the stage states are
// untouched and no trigger is appended.
func TestFanInHold_FailedHead_HoldsWithoutRetrigger(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	f.seedVerdict(fanInHead, mergeCandidateResultFailed)
	f.dispatch()

	f.assertHeld()
	if n := len(mcvTriggerRows(f.au)); n != 1 {
		t.Errorf("trigger rows = %d, want 1 (the seeded one; a failed head is never re-triggered)", n)
	}
	if got := f.state(f.impl.ID); got != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded (nothing re-opened)", got)
	}
}

// not_executed does not settle the head: the hold re-triggers a fresh pass.
func TestFanInHold_NotExecutedHead_Retriggers(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	f.seedVerdict(fanInHead, mergeCandidateResultNotExecuted)
	f.dispatch()

	f.assertHeld()
	if n := len(mcvTriggerRows(f.au)); n != 2 {
		t.Errorf("trigger rows = %d, want 2 (the seeded one + the re-trigger)", n)
	}
	if got := f.state(f.impl.ID); got != run.StageStatePending {
		t.Errorf("implement state = %q, want pending (re-opened for the re-trigger)", got)
	}
}

// A live (unconsumed) trigger for the head holds and appends nothing.
func TestFanInHold_InFlightHead_Holds(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	f.seedTrigger(fanInHead)
	f.dispatch()

	f.assertHeld()
	if n := len(mcvTriggerRows(f.au)); n != 1 {
		t.Errorf("trigger rows = %d, want 1 (the live one)", n)
	}
}

// D4: no declared verify command means no hold, no trigger and no injected
// evidence — the round renders exactly the pre-ADR-090 named absence.
func TestFanInHold_NoDeclaredVerify_NoHoldNoEvidence(t *testing.T) {
	f := newFanInFixture(t, specImplementGatingReviewers)
	evidence := f.captureReviewEvidence()
	f.dispatch()

	f.assertReviewed()
	if n := len(mcvTriggerRows(f.au)); n != 0 {
		t.Errorf("trigger rows = %d, want 0 (D4: nothing required)", n)
	}
	evs := evidence()
	if len(evs) != 1 || evs[0] == nil {
		t.Fatalf("review rounds built = %d, want 1 with the per-slice evidence", len(evs))
	}
	if evs[0].VerifyEvidenceUnavailableReason != decomposedParentNoParentLevelVerifyReason || len(evs[0].VerifyRuns) != 0 {
		t.Errorf("evidence = reason %q / %d verify runs, want the pre-ADR-090 %s absence and no runs",
			evs[0].VerifyEvidenceUnavailableReason, len(evs[0].VerifyRuns), decomposedParentNoParentLevelVerifyReason)
	}
}

// FAIL-CLOSED: an unreadable merge-candidate chain holds the round and starts
// nothing.
func TestFanInHold_StateReadError_FailsClosed(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	f.au.listByCategoryErrCategory = CategoryMergeCandidateVerified
	f.dispatch()

	f.assertHeld()
	if n := len(mcvTriggerRows(f.au)); n != 0 {
		t.Errorf("trigger rows = %d, want 0", n)
	}
	if got := f.state(f.impl.ID); got != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded (nothing re-opened)", got)
	}
}

// A refused start (here: the trigger append fails) holds the round, and the
// append-then-reopen order leaves the implement stage untouched.
func TestFanInHold_StartRefused_Holds(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	f.au.appendErrCategory = CategoryStageMergeCandidateVerifyTriggered
	f.dispatch()

	f.assertHeld()
	if got := f.state(f.impl.ID); got != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded (a refused start re-opens nothing)", got)
	}
	if got := f.state(f.review.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval", got)
	}
}

// A start that answers not_required (the chain moved between the state read
// and the start: the verify command vanished) releases instead of holding.
func TestHoldConsolidatedReviewForFanInVerify_StartNotRequiredReleases(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	runRow := f.runRow()
	// The state read sees the verify command (unverified fan-in head); the run
	// row the start re-reads does not.
	f.rr.mu.Lock()
	f.rr.runs[f.parent.ID].WorkflowSpec = specImplementGatingReviewers
	f.rr.mu.Unlock()
	if f.s.holdConsolidatedReviewForFanInVerify(context.Background(), runRow, "main", f.branch, fanInHead) {
		t.Fatal("hold = true, want false: the start answered not_required")
	}
	if n := len(mcvTriggerRows(f.au)); n != 0 {
		t.Errorf("trigger rows = %d, want 0", n)
	}
}

// A head whose state cannot be decided (the compare resolved no head SHA) holds
// fail-closed rather than dispatching the gating review unbound.
func TestHoldConsolidatedReviewForFanInVerify_EmptyHeadHolds(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	runRow := f.runRow()
	if !f.s.holdConsolidatedReviewForFanInVerify(context.Background(), runRow, "main", f.branch, "") {
		t.Fatal("hold = false, want true: an undecidable head must not dispatch the review")
	}
	if n := len(mcvTriggerRows(f.au)); n != 0 {
		t.Errorf("trigger rows = %d, want 0", n)
	}
}

// --- evidence injection --------------------------------------------------------

// fanInVerifyGateRun answers ONLY for a consolidated round of a head carrying a
// passed verdict bound to exactly that head.
func TestFanInVerifyGateRun_Arms(t *testing.T) {
	consolidated := withReviewRoundSource(context.Background(),
		reviewRoundSource{Origin: reviewRoundOriginConsolidated, BaseSHA: "main"})
	t.Run("consolidated round with a passed verdict", func(t *testing.T) {
		f := newFanInFixture(t, fanInVerifySpec)
		f.seedVerdict(fanInHead, mergeCandidateResultPassed)
		vr, sum := f.s.fanInVerifyGateRun(consolidated, f.parent.ID, fanInHead)
		if vr == nil || sum == nil {
			t.Fatal("no evidence for a passed consolidated head")
		}
		if vr.Command != "scripts/test verify" || vr.HeadSHA != fanInHead || vr.Outcome != "passed" ||
			vr.ExitCode != 0 || vr.OutputTail != fanInTailSentinel {
			t.Errorf("run = %+v", *vr)
		}
		if sum.Outcome != "passed" {
			t.Errorf("summary = %+v", *sum)
		}
	})
	for name, tc := range map[string]struct {
		ctx     context.Context
		seed    func(f *fanInFixture)
		head    string
		listErr string
	}{
		"no round source":         {ctx: context.Background(), seed: func(f *fanInFixture) { f.seedVerdict(fanInHead, "passed") }, head: fanInHead},
		"fix-up push round":       {ctx: withReviewRoundSource(context.Background(), reviewRoundSource{Origin: reviewRoundOriginFixupPush, BaseSHA: "b"}), seed: func(f *fanInFixture) { f.seedVerdict(fanInHead, "passed") }, head: fanInHead},
		"passed for another head": {ctx: consolidated, seed: func(f *fanInFixture) { f.seedVerdict("otherhead", "passed") }, head: fanInHead},
		"failed verdict":          {ctx: consolidated, seed: func(f *fanInFixture) { f.seedVerdict(fanInHead, "failed") }, head: fanInHead},
		"no verdict":              {ctx: consolidated, seed: func(*fanInFixture) {}, head: fanInHead},
		"empty head":              {ctx: consolidated, seed: func(f *fanInFixture) { f.seedVerdict("", "passed") }, head: ""},
		"chain read error":        {ctx: consolidated, seed: func(f *fanInFixture) { f.seedVerdict(fanInHead, "passed") }, head: fanInHead, listErr: CategoryMergeCandidateVerified},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFanInFixture(t, fanInVerifySpec)
			tc.seed(f)
			f.au.listByCategoryErrCategory = tc.listErr
			if vr, sum := f.s.fanInVerifyGateRun(tc.ctx, f.parent.ID, tc.head); vr != nil || sum != nil {
				t.Fatalf("evidence = %+v / %+v, want none", vr, sum)
			}
		})
	}
}

// The approval-conditions branch reads a consolidated round exactly as it did
// before the injection: no bundle was uploaded, so no
// approval_conditions_unrecorded row is appended and the reviewer is not told
// to raise one, even though the round's gate evidence is now non-nil.
func TestFanInHold_ApprovalConditionsBranchUnchanged(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	evidence := f.captureReviewEvidence()
	f.au.seeded = append(f.au.seeded, makeApproveWithCommentEntry(f.parent.ID, "1. Keep the fan-in hold."))
	f.seedVerdict(fanInHead, mergeCandidateResultPassed)
	f.dispatch()

	got := f.assertReviewed()
	f.assertParentLevelVerify(evidence())
	if evs := evidence(); evs[0].ApprovalConditionsAttached {
		t.Error("ApprovalConditionsAttached = true on a bundle-less consolidated round")
	}
	if n := f.count(approvalConditionsUnrecordedCategory); n != 0 {
		t.Errorf("approval_conditions_unrecorded rows = %d, want 0 (no bundle on a consolidated round)", n)
	}
	if strings.Contains(got, "`approval_conditions_unrecorded`") {
		t.Error("reviewer prompt carries the unrecorded bullet on a bundle-less consolidated round")
	}
}

// --- end-to-end release (approval condition C1) ---------------------------------

// TestFanInHold_EndToEndRelease_ApprovalGatedImplement drives the whole D5 loop
// on a parent whose implement stage carries an approval gate
// (RequiresApproval=true): DispatchConsolidatedReview holds and starts the
// fan-in pass; the runner's merge_candidate_verified{passed} report goes
// through the real HTTP report arm, which settles the re-opened implement
// stage to the trigger's PriorState (succeeded) and calls
// Orchestrator.Advance; Advance re-walks the re-parked review stage and
// re-enters DispatchConsolidatedReview, which now finds the head passed and
// dispatches the consolidated review for it. A settle by RequiresApproval
// (advanceImplementStageAfterPR) would park the implement stage at
// awaiting_approval, never call Advance, and the hold would never release.
func TestFanInHold_EndToEndRelease_ApprovalGatedImplement(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	evidence := f.captureReviewEvidence()
	f.impl.RequiresApproval = true
	prURL := "https://github.com/kuhlman-labs/example/pull/9"
	f.rr.mu.Lock()
	f.parent.PullRequestURL = &prURL
	f.rr.mu.Unlock()
	f.s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.rr, ConsolidatedReview: f.s, DefaultBranch: defaultbranch.Static("main")}
	sf := newSigningFake()
	f.s.cfg.SigningRepo = sf
	priv, _ := sf.issue(t, f.parent.ID)

	// 1. The orchestrator's dispatch of the parent review stage: held.
	f.s.DispatchConsolidatedReview(context.Background(), f.parent.ID, "main", f.branch)
	f.s.waitBackgroundReviews()
	f.assertHeld()
	if n := len(mcvTriggerRows(f.au)); n != 1 {
		t.Fatalf("trigger rows = %d, want 1", n)
	}

	// 2. The runner starts the verify-only pass on the re-opened stage.
	f.rr.mu.Lock()
	f.impl.State = run.StageStateRunning
	f.rr.mu.Unlock()

	// 3. It reports passed for exactly the consolidated head.
	body, _ := json.Marshal(map[string]any{
		"outcome": outcomeMergeCandidateVerified, "branch": f.branch, "head_sha": fanInHead,
		"merge_candidate_result": "passed", "merge_candidate_reason": "",
		"merge_candidate_output_tail": fanInTailSentinel,
	})
	w := shipFanInReport(t, f, priv, body)
	if w.Code != http.StatusOK {
		t.Fatalf("report status = %d:\n%s", w.Code, w.Body.String())
	}
	f.s.waitBackgroundReviews()

	// 4. The hold released: the consolidated round ran for the head with the
	// parent-level verify as its evidence, the implement stage is back at its
	// PRIOR gate, and the review stage was re-walked.
	if n := f.count("implement_review_started"); n != 1 {
		t.Fatalf("implement_review_started = %d, want 1: the D5 hold never released (implement %q, review %q)",
			n, f.state(f.impl.ID), f.state(f.review.ID))
	}
	f.assertReviewed()
	f.assertParentLevelVerify(evidence())
	if got := f.state(f.impl.ID); got != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded (the trigger's PriorState)", got)
	}
	if got := f.state(f.review.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval (re-walked by Advance)", got)
	}
	if n := len(mcvTriggerRows(f.au)); n != 1 {
		t.Errorf("trigger rows = %d, want 1 (the release must not re-trigger)", n)
	}
}

func shipFanInReport(t *testing.T, f *fanInFixture, priv ed25519.PrivateKey, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	return shipPRRequest(t, f.s, f.parent.ID, f.impl.ID, priv, body, "")
}

// --- #4785: acceptance ordered ahead of review ----------------------------------

// newFanInAcceptanceAheadFixture is the feature_change shape (#4785): the
// parent's implement stage succeeded, acceptance pending at seq 2 and review
// pending at seq 3, on the local runner. The base fixture's seq-2 stage becomes
// the acceptance stage; f.review is the new seq-3 review stage. acc returns the
// acceptance stage.
func newFanInAcceptanceAheadFixture(t *testing.T, spec []byte) (f *fanInFixture, acc *run.Stage) {
	t.Helper()
	f = newFanInFixture(t, spec)
	f.rr.mu.Lock()
	acc = f.review
	acc.Type = run.StageTypeAcceptance
	acc.ExecutorKind = run.ExecutorAgent
	acc.State = run.StageStatePending
	f.rr.runs[f.parent.ID].RunnerKind = run.RunnerKindLocal
	f.rr.runs[f.parent.ID].RunnerKindResolved = true
	f.rr.mu.Unlock()
	review := f.rr.seedStage(f.parent.ID, 3, run.StageStatePending)
	review.Type = run.StageTypeReview
	review.ExecutorKind = run.ExecutorHuman
	f.review = review
	return f, acc
}

func TestFanInVerifySettled_Arms(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		spec    []byte
		setup   func(f *fanInFixture)
		want    bool
		wantErr bool
	}{
		{name: "passed", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.seedVerdict(fanInHead, mergeCandidateResultPassed) }, want: true},
		{name: "not_required", spec: specImplementGatingReviewers, want: true},
		{name: "failed", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.seedVerdict(fanInHead, mergeCandidateResultFailed) }, want: true},
		{name: "unverified", spec: fanInVerifySpec, want: false},
		{name: "not_executed", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.seedVerdict(fanInHead, mergeCandidateResultNotExecuted) }, want: false},
		{name: "in_flight", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.seedTrigger(fanInHead) }, want: false},
		{name: "state_read_error", spec: fanInVerifySpec, setup: func(f *fanInFixture) {
			f.au.listByCategoryErrCategory = CategoryMergeCandidateVerified
		}, wantErr: true},
		{name: "non_parent_child_run", spec: fanInVerifySpec, setup: func(f *fanInFixture) {
			f.rr.mu.Lock()
			other := uuid.New()
			f.rr.runs[f.parent.ID].DecomposedFrom = &other
			f.rr.mu.Unlock()
		}, want: true},
		{name: "non_parent_no_children", spec: fanInVerifySpec, setup: func(f *fanInFixture) {
			f.rr.mu.Lock()
			for _, r := range f.rr.runs {
				if r.DecomposedFrom != nil && *r.DecomposedFrom == f.parent.ID {
					r.DecomposedFrom = nil
				}
			}
			f.rr.mu.Unlock()
		}, want: true},
		{name: "list_children_error", spec: fanInVerifySpec, setup: func(f *fanInFixture) {
			f.rr.listRunsErr = errors.New("db unavailable")
		}, wantErr: true},
		{name: "forge_not_wired", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.s.cfg.GitHub = nil }, want: true},
		{name: "compare_error", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.s.cfg.GitHub = erroringComparePatchClient(t) }, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newFanInAcceptanceAheadFixture(t, tc.spec)
			if tc.setup != nil {
				tc.setup(f)
			}
			got, err := f.s.FanInVerifySettled(ctx, f.parent.ID, "main", f.branch)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("FanInVerifySettled = %v, nil; want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("FanInVerifySettled: %v", err)
			}
			if got != tc.want {
				t.Errorf("FanInVerifySettled = %v, want %v", got, tc.want)
			}
			// The hook is READ-ONLY: it never starts a pass.
			if n := len(mcvTriggerRows(f.au)); tc.name == "unverified" && n != 0 {
				t.Errorf("trigger rows = %d, want 0 (the hook starts nothing)", n)
			}
		})
	}

	t.Run("get_run_error", func(t *testing.T) {
		f, _ := newFanInAcceptanceAheadFixture(t, fanInVerifySpec)
		if _, err := f.s.FanInVerifySettled(ctx, uuid.New(), "main", f.branch); err == nil {
			t.Fatal("FanInVerifySettled on an unknown run = nil error, want the read error")
		}
	})
	t.Run("no_run_repo", func(t *testing.T) {
		s := &Server{}
		if got, err := s.FanInVerifySettled(ctx, uuid.New(), "main", "b"); err != nil || !got {
			t.Errorf("FanInVerifySettled with no run repo = %v, %v; want true, nil", got, err)
		}
	})
}

// A fan_in pass start re-opens the parent implement stage past the
// never-dispatched acceptance stage ahead of review (TolerateUndispatchedAcceptance),
// re-parking no review; an acceptance already parked at awaiting_host_dispatch
// keeps the #3116 refusal and the trigger settles not_executed.
func TestBeginMergeCandidateVerifyPass_PendingAcceptanceAhead_ReopensImplement(t *testing.T) {
	params := func(f *fanInFixture) mergeCandidateVerifyParams {
		return mergeCandidateVerifyParams{Branch: f.branch, BaseRef: "main", HeadSHA: fanInHead, Cause: mergeCandidateCauseFanIn}
	}
	t.Run("pending_acceptance", func(t *testing.T) {
		f, acc := newFanInAcceptanceAheadFixture(t, fanInVerifySpec)
		start, refusal := f.s.beginMergeCandidateVerifyPass(context.Background(), f.parent.ID, params(f), mergeCandidateSystemActor())
		if refusal != nil {
			t.Fatalf("refusal = %q, want a started pass", refusal.Reason)
		}
		if start.State != mergeCandidateStateInFlight || !start.Triggered {
			t.Errorf("start = %+v, want in_flight + triggered", start)
		}
		rows := mcvTriggerRows(f.au)
		if len(rows) != 1 {
			t.Fatalf("trigger rows = %d, want 1", len(rows))
		}
		var trig mergeCandidateVerifyTrigger
		if err := json.Unmarshal(rows[0].Payload, &trig); err != nil {
			t.Fatal(err)
		}
		if trig.ReparkedReviewStageID != "" {
			t.Errorf("reparked_review_stage_id = %q, want empty (the review is still pending)", trig.ReparkedReviewStageID)
		}
		if got := f.state(f.impl.ID); got != run.StageStatePending {
			t.Errorf("implement state = %q, want pending (re-opened)", got)
		}
		if got := f.state(acc.ID); got != run.StageStatePending {
			t.Errorf("acceptance state = %q, want pending", got)
		}
		if got := f.state(f.review.ID); got != run.StageStatePending {
			t.Errorf("review state = %q, want pending", got)
		}
		if n := f.count(CategoryMergeCandidateVerified); n != 0 {
			t.Errorf("merge_candidate_verified rows = %d, want 0 (nothing settled)", n)
		}
	})
	t.Run("awaiting_host_dispatch_acceptance_refused", func(t *testing.T) {
		f, acc := newFanInAcceptanceAheadFixture(t, fanInVerifySpec)
		f.rr.mu.Lock()
		acc.State = run.StageStateAwaitingHostDispatch
		f.rr.mu.Unlock()
		_, refusal := f.s.beginMergeCandidateVerifyPass(context.Background(), f.parent.ID, params(f), mergeCandidateSystemActor())
		if refusal == nil {
			t.Fatal("refusal = nil, want the re-open refused")
		}
		if got := f.state(f.impl.ID); got != run.StageStateSucceeded {
			t.Errorf("implement state = %q, want succeeded (not re-opened)", got)
		}
		verdicts := auditEntries(f.au, CategoryMergeCandidateVerified)
		if len(verdicts) != 1 {
			t.Fatalf("merge_candidate_verified rows = %d, want 1 (the un-reopened trigger settled)", len(verdicts))
		}
		var v mergeCandidateVerifiedPayload
		_ = json.Unmarshal(verdicts[0].Payload, &v)
		if v.Result != mergeCandidateResultNotExecuted {
			t.Errorf("verdict = %q, want not_executed", v.Result)
		}
	})
}

func TestServerNew_WiresFanInVerifyHook(t *testing.T) {
	o := &orchestrator.Orchestrator{}
	s := New(Config{Addr: "127.0.0.1:0", Orchestrator: o})
	if o.FanInVerify != s {
		t.Errorf("Orchestrator.FanInVerify = %v, want the *Server", o.FanInVerify)
	}
}

// Cross-layer (#4785): the real orchestrator.Advance → FanInVerifySettled hook →
// DispatchConsolidatedReview D5 hold → beginMergeCandidateVerifyPass →
// run.FixupStage(tolerant) → signed merge_candidate_verified report →
// settleMergeCandidateVerifyStage → Advance release → consolidated
// runImplementReviews, then the review-gate Advance with no duplicate round or
// trigger. Every step reads persisted stage states and audit rows.
func TestFanInAcceptanceHold_EndToEnd(t *testing.T) {
	f, acc := newFanInAcceptanceAheadFixture(t, fanInVerifySpec)
	evidence := f.captureReviewEvidence()
	prURL := "https://github.com/kuhlman-labs/example/pull/9"
	f.rr.mu.Lock()
	f.rr.runs[f.parent.ID].PullRequestURL = &prURL
	f.rr.mu.Unlock()
	o := &orchestrator.Orchestrator{Runs: f.rr, ConsolidatedReview: f.s, FanInVerify: f.s, DefaultBranch: defaultbranch.Static("main")}
	f.s.cfg.Orchestrator = o
	sf := newSigningFake()
	f.s.cfg.SigningRepo = sf
	priv, _ := sf.issue(t, f.parent.ID)
	ctx := context.Background()

	// 1. Advance reaches the pending acceptance: held; the verify-only pass is
	// started and re-opens implement; no review round runs.
	if _, err := o.Advance(ctx, f.parent.ID); err != nil {
		t.Fatalf("Advance #1: %v", err)
	}
	f.s.waitBackgroundReviews()
	if n := len(mcvTriggerRows(f.au)); n != 1 {
		t.Fatalf("trigger rows = %d, want 1", n)
	}
	if got := f.state(f.impl.ID); got != run.StageStatePending {
		t.Fatalf("implement state = %q, want pending (re-opened for the pass)", got)
	}
	if got := f.state(acc.ID); got != run.StageStatePending {
		t.Fatalf("acceptance state = %q, want pending (held)", got)
	}
	f.assertHeld()

	// 2. The runner runs the pass and reports passed for the consolidated head:
	// the release dispatches acceptance and the consolidated round runs.
	f.rr.mu.Lock()
	f.impl.State = run.StageStateRunning
	f.rr.mu.Unlock()
	body, _ := json.Marshal(map[string]any{
		"outcome": outcomeMergeCandidateVerified, "branch": f.branch, "head_sha": fanInHead,
		"merge_candidate_result": "passed", "merge_candidate_reason": "",
		"merge_candidate_output_tail": fanInTailSentinel,
	})
	w := shipFanInReport(t, f, priv, body)
	if w.Code != http.StatusOK {
		t.Fatalf("report status = %d:\n%s", w.Code, w.Body.String())
	}
	f.s.waitBackgroundReviews()
	if got := f.state(f.impl.ID); got != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded", got)
	}
	if got := f.state(acc.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("acceptance state = %q, want awaiting_host_dispatch (released)", got)
	}
	if n := f.count("implement_review_started"); n != 1 {
		t.Fatalf("implement_review_started = %d, want 1 after the release", n)
	}
	f.assertParentLevelVerify(evidence())
	if got := f.state(f.review.ID); got != run.StageStatePending {
		t.Errorf("review state = %q, want pending (acceptance runs first)", got)
	}

	// 3. Acceptance settles; the review gate opens with no duplicate round or
	// trigger.
	f.rr.mu.Lock()
	acc.State = run.StageStateSucceeded
	f.rr.mu.Unlock()
	if _, err := o.Advance(ctx, f.parent.ID); err != nil {
		t.Fatalf("Advance #3: %v", err)
	}
	f.s.waitBackgroundReviews()
	if got := f.state(f.review.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval", got)
	}
	if n := f.count("implement_review_started"); n != 1 {
		t.Errorf("implement_review_started = %d, want still 1 (the review-gate re-fire dedups)", n)
	}
	if n := len(mcvTriggerRows(f.au)); n != 1 {
		t.Errorf("trigger rows = %d, want still 1", n)
	}
}

// --- #4812: a base-advanced consolidated head ------------------------------------

// seedBaseAdvanceHead records a branch_rebased row whose new head (and merge
// commit) is fanInHead, so the consolidated head classifies base_advance —
// classifier rule 1, ahead of the fixture's fan-in integration row (rule 5).
func (f *fanInFixture) seedBaseAdvanceHead() {
	f.appendRow(nil, CategoryBranchRebased, map[string]any{
		"new_head_sha": fanInHead, "merge_commit_sha": fanInHead,
	})
}

// wireQueue wires a pure mcqFaultStore whose ActiveForRun answers active.
// Every other store call answers errMCQNilStore, so an unexpected write is a
// recorded call AND an error, never a silent success.
func (f *fanInFixture) wireQueue(active func(uuid.UUID) (mergequeue.Status, bool, error)) *mcqFaultStore {
	q := &mcqFaultStore{activeForRun: active}
	f.s.cfg.MergeQueue = q
	return q
}

// wireNotEligibleQueue wires a queue with NO active entry for the run: a
// serialized start then consults admission eligibility, which the parent (its
// review and acceptance not settled) fails.
func (f *fanInFixture) wireNotEligibleQueue() *mcqFaultStore {
	return f.wireQueue(func(uuid.UUID) (mergequeue.Status, bool, error) { return mergequeue.Status{}, false, nil })
}

// assertBaseAdvanceHead pins the PRECONDITIONS a #4812 test stands on, so a
// fixture that silently classifies fan_in (or never routes through the queue)
// cannot pass vacuously: the head classifies base_advance with merge-candidate
// state wantState, and a base_advance start for it routes through the queue.
func (f *fanInFixture) assertBaseAdvanceHead(wantState string) {
	f.t.Helper()
	ctx := context.Background()
	cause, err := f.s.classifyMergeCandidateHead(ctx, f.runRow(), fanInHead)
	if err != nil || cause != mergeCandidateCauseBaseAdvance {
		f.t.Fatalf("precondition: head classifies %q (err %v), want %s", cause, err, mergeCandidateCauseBaseAdvance)
	}
	state, err := f.s.mergeCandidateVerifyState(ctx, f.runRow(), fanInHead)
	if err != nil || state.State != wantState {
		f.t.Fatalf("precondition: merge-candidate state = %+v (err %v), want %s", state, err, wantState)
	}
	if wantState == mergeCandidateStateUnverified && state.Cause != mergeCandidateCauseBaseAdvance {
		f.t.Fatalf("precondition: unverified state cause = %q, want %s", state.Cause, mergeCandidateCauseBaseAdvance)
	}
	if !f.s.mergeCandidateQueueRoutes(mergeCandidateVerifyParams{Branch: f.branch, BaseRef: "main", HeadSHA: fanInHead,
		Cause: mergeCandidateCauseBaseAdvance}, mergeCandidateSystemActor()) {
		f.t.Fatal("precondition: a base_advance start does not route through the merge-candidate queue")
	}
}

// fanInLogRecord is one decoded JSON slog record.
type fanInLogRecord struct {
	level string
	msg   string
	attrs map[string]any
}

// captureLogs points the server's logger at a mutex-guarded JSON buffer
// (#3226: review goroutines write it while the test reads) and returns a
// decoder over the records written so far.
func (f *fanInFixture) captureLogs() func() []fanInLogRecord {
	f.t.Helper()
	buf := &syncBuffer{}
	f.s.cfg.Logger = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return func() []fanInLogRecord {
		f.t.Helper()
		var out []fanInLogRecord
		for _, line := range strings.Split(buf.String(), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				f.t.Fatalf("log line is not JSON: %v\n%s", err, line)
			}
			lvl, _ := m["level"].(string)
			msg, _ := m["msg"].(string)
			out = append(out, fanInLogRecord{level: lvl, msg: msg, attrs: m})
		}
		return out
	}
}

// fanInStartedRecords returns the records reporting a started pass.
func fanInStartedRecords(recs []fanInLogRecord) []fanInLogRecord {
	var out []fanInLogRecord
	for _, r := range recs {
		if strings.Contains(r.msg, "pass started") {
			out = append(out, r)
		}
	}
	return out
}

// assertOneFanInTrigger pins exactly one trigger row on the parent implement
// stage, written by the system actor, with cause fan_in for fanInHead.
func (f *fanInFixture) assertOneFanInTrigger() {
	f.t.Helper()
	rows := mcvTriggerRows(f.au)
	if len(rows) != 1 {
		f.t.Fatalf("stage_merge_candidate_verify_triggered = %d, want 1", len(rows))
	}
	row := rows[0]
	if row.ActorKind == nil || *row.ActorKind != audit.ActorSystem {
		f.t.Errorf("trigger actor = %v, want system", row.ActorKind)
	}
	if row.StageID == nil || *row.StageID != f.impl.ID {
		f.t.Errorf("trigger stage = %v, want the parent implement stage %s", row.StageID, f.impl.ID)
	}
	var trig mergeCandidateVerifyTrigger
	if err := json.Unmarshal(row.Payload, &trig); err != nil {
		f.t.Fatal(err)
	}
	if trig.Cause != mergeCandidateCauseFanIn || trig.ExpectedHeadSHA != fanInHead {
		f.t.Errorf("trigger cause/head = %q/%q, want %s/%s", trig.Cause, trig.ExpectedHeadSHA, mergeCandidateCauseFanIn, fanInHead)
	}
}

// The D5 hold on a base-advanced consolidated head (no queue entry, a run not
// yet merge-queue eligible) STARTS a fan_in pass: one trigger, the implement
// stage re-opened, nothing enqueued, and the "started" record names a real
// stage id. A forwarded base_advance cause answers not_queued and starts
// nothing (#4812).
func TestFanInHold_BaseAdvanceHead_StartsFanInPass(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	f.seedBaseAdvanceHead()
	q := f.wireNotEligibleQueue()
	f.assertBaseAdvanceHead(mergeCandidateStateUnverified)
	logs := f.captureLogs()
	f.dispatch()

	f.assertHeld()
	f.assertOneFanInTrigger()
	if got := f.state(f.impl.ID); got != run.StageStatePending {
		t.Errorf("implement state = %q, want pending (re-opened for the pass)", got)
	}
	if n := q.called("Enqueue"); n != 0 {
		t.Errorf("queue Enqueue calls = %d, want 0 (fan_in takes the direct start); calls %v", n, q.names())
	}
	started := fanInStartedRecords(logs())
	if len(started) != 1 {
		t.Fatalf("'pass started' records = %d, want 1: %+v", len(started), logs())
	}
	r := started[0]
	if r.level != "INFO" || r.attrs["triggered"] != true || r.attrs["cause"] != mergeCandidateCauseFanIn ||
		r.attrs["head_cause"] != mergeCandidateCauseBaseAdvance {
		t.Errorf("started record = %+v, want INFO triggered=true cause=fan_in head_cause=base_advance", r)
	}
	if id, _ := r.attrs["stage_id"].(string); id == "" || id == uuid.Nil.String() {
		t.Errorf("started record stage_id = %q, want the re-opened stage", id)
	}
}

// C4 residual: a parent holding an ACTIVE HELD queue entry still gets a DIRECT
// fan_in pass from the hold — the queue is not consulted for the start and no
// queue state is written. Per-head idempotence makes a later admission read the
// pass as in_flight/passed for the same head.
func TestFanInHold_BaseAdvanceHead_ActiveHeldEntry_StartsDirectPass(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	f.seedBaseAdvanceHead()
	liveRun := uuid.New()
	q := f.wireQueue(func(runID uuid.UUID) (mergequeue.Status, bool, error) {
		return mergequeue.Status{
			Entry: mergequeue.Entry{ID: uuid.New(), RunID: runID, BaseRef: "main", State: mergequeue.StateHeld,
				AnchoredHeadSHA: "olderhead"},
			Position: 2, LiveRunID: &liveRun,
		}, true, nil
	})
	f.assertBaseAdvanceHead(mergeCandidateStateUnverified)
	f.dispatch()

	f.assertHeld()
	f.assertOneFanInTrigger()
	if n := q.called("Enqueue"); n != 0 {
		t.Errorf("queue Enqueue calls = %d, want 0; calls %v", n, q.names())
	}
	readOnly := map[string]bool{"ActiveForRun": true, "ActiveBases": true, "ListForBase": true, "MergeDispatch": true, "RecentLineTurns": true}
	for _, name := range q.names() {
		if !readOnly[name] {
			t.Errorf("queue store call %q writes queue state; want none (calls %v)", name, q.names())
		}
	}
}

// Cross-layer (#4812 on the #4785 shape): a base-advanced consolidated head on
// a parent with acceptance ahead of review. Advance holds acceptance and the D5
// hold starts a fan_in pass; the passed report settles the head, so
// FanInVerifySettled flips true, acceptance is released and the consolidated
// round runs once with the parent-level evidence.
func TestFanInAcceptanceHold_BaseAdvanceHead_EndToEnd(t *testing.T) {
	f, acc := newFanInAcceptanceAheadFixture(t, fanInVerifySpec)
	f.seedBaseAdvanceHead()
	f.wireNotEligibleQueue()
	f.assertBaseAdvanceHead(mergeCandidateStateUnverified)
	evidence := f.captureReviewEvidence()
	prURL := "https://github.com/kuhlman-labs/example/pull/9"
	f.rr.mu.Lock()
	f.rr.runs[f.parent.ID].PullRequestURL = &prURL
	f.rr.mu.Unlock()
	o := &orchestrator.Orchestrator{Runs: f.rr, ConsolidatedReview: f.s, FanInVerify: f.s, DefaultBranch: defaultbranch.Static("main")}
	f.s.cfg.Orchestrator = o
	sf := newSigningFake()
	f.s.cfg.SigningRepo = sf
	priv, _ := sf.issue(t, f.parent.ID)
	ctx := context.Background()

	if _, err := o.Advance(ctx, f.parent.ID); err != nil {
		t.Fatalf("Advance #1: %v", err)
	}
	f.s.waitBackgroundReviews()
	f.assertOneFanInTrigger()
	if got := f.state(acc.ID); got != run.StageStatePending {
		t.Fatalf("acceptance state = %q, want pending (held)", got)
	}
	f.assertHeld()
	if settled, err := f.s.FanInVerifySettled(ctx, f.parent.ID, "main", f.branch); err != nil || settled {
		t.Fatalf("FanInVerifySettled before the pass = %v, %v; want false", settled, err)
	}

	f.rr.mu.Lock()
	f.impl.State = run.StageStateRunning
	f.rr.mu.Unlock()
	body, _ := json.Marshal(map[string]any{
		"outcome": outcomeMergeCandidateVerified, "branch": f.branch, "head_sha": fanInHead,
		"merge_candidate_result": "passed", "merge_candidate_reason": "",
		"merge_candidate_output_tail": fanInTailSentinel,
	})
	w := shipFanInReport(t, f, priv, body)
	if w.Code != http.StatusOK {
		t.Fatalf("report status = %d:\n%s", w.Code, w.Body.String())
	}
	f.s.waitBackgroundReviews()

	if settled, err := f.s.FanInVerifySettled(ctx, f.parent.ID, "main", f.branch); err != nil || !settled {
		t.Errorf("FanInVerifySettled after the pass = %v, %v; want true", settled, err)
	}
	verdicts := auditEntries(f.au, CategoryMergeCandidateVerified)
	if len(verdicts) != 1 {
		t.Fatalf("merge_candidate_verified rows = %d, want 1", len(verdicts))
	}
	var v mergeCandidateVerifiedPayload
	if err := json.Unmarshal(verdicts[0].Payload, &v); err != nil {
		t.Fatal(err)
	}
	if v.HeadSHA != fanInHead || v.Cause != mergeCandidateCauseFanIn || v.Result != mergeCandidateResultPassed {
		t.Errorf("verdict = %+v, want passed fan_in for %s", v, fanInHead)
	}
	if got := f.state(f.impl.ID); got != run.StageStateSucceeded {
		t.Errorf("implement state = %q, want succeeded", got)
	}
	if got := f.state(acc.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("acceptance state = %q, want awaiting_host_dispatch (released)", got)
	}
	f.assertReviewed()
	f.assertParentLevelVerify(evidence())
}

// A passed verdict recorded under cause fan_in satisfies the hold on a head the
// classifier calls base_advance: verdicts bind by head, not cause.
func TestFanInHold_BaseAdvanceHead_PassedVerdictReleases(t *testing.T) {
	f := newFanInFixture(t, fanInVerifySpec)
	f.seedBaseAdvanceHead()
	f.wireNotEligibleQueue()
	f.seedVerdict(fanInHead, mergeCandidateResultPassed)
	f.assertBaseAdvanceHead(mergeCandidateStatePassed)
	evidence := f.captureReviewEvidence()
	f.dispatch()

	f.assertReviewed()
	f.assertParentLevelVerify(evidence())
	if n := len(mcvTriggerRows(f.au)); n != 1 {
		t.Errorf("trigger rows = %d, want 1 (the seeded one; a passed head starts nothing)", n)
	}
}

// fanInHoldOnStart reads every start answer explicitly (#4812): only a
// triggered in_flight answer is logged as a started pass.
func TestFanInHoldOnStart_Answers(t *testing.T) {
	stageID := uuid.New()
	liveRun := uuid.New()
	cases := []struct {
		name string
		// answer builds the start answer on the row's fixture.
		answer      func(t *testing.T, f *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal)
		wantHold    bool
		wantLevel   string
		wantMsg     string
		wantAttrs   map[string]any
		wantStarted bool
	}{
		{name: "refusal", answer: func(*testing.T, *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
			return nil, &mergeCandidateVerifyRefusal{Reason: "append failed"}
		}, wantHold: true, wantLevel: "WARN", wantMsg: "could not be started", wantAttrs: map[string]any{"refusal": "append failed"}},
		{name: "nil_start", answer: func(*testing.T, *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
			return nil, nil
		}, wantHold: true, wantLevel: "WARN", wantMsg: "returned no answer"},
		{name: "not_required", answer: func(*testing.T, *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
			return &mergeCandidateVerifyStart{State: mergeCandidateStateNotRequired}, nil
		}},
		{name: "passed", answer: func(*testing.T, *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
			return &mergeCandidateVerifyStart{State: mergeCandidateStatePassed}, nil
		}},
		{name: "in_flight_triggered", answer: func(*testing.T, *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
			return &mergeCandidateVerifyStart{State: mergeCandidateStateInFlight, StageID: stageID, Triggered: true}, nil
		}, wantHold: true, wantLevel: "INFO", wantMsg: "pass started", wantStarted: true,
			wantAttrs: map[string]any{"triggered": true, "stage_id": stageID.String(), "cause": mergeCandidateCauseFanIn}},
		{name: "in_flight_untriggered", answer: func(*testing.T, *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
			return &mergeCandidateVerifyStart{State: mergeCandidateStateInFlight, StageID: stageID}, nil
		}, wantHold: true, wantLevel: "INFO", wantMsg: "already in flight",
			wantAttrs: map[string]any{"start_state": mergeCandidateStateInFlight, "stage_id": stageID.String()}},
		{name: "failed", answer: func(*testing.T, *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
			return &mergeCandidateVerifyStart{State: mergeCandidateStateFailed}, nil
		}, wantHold: true, wantLevel: "WARN", wantMsg: "FAILED", wantAttrs: map[string]any{"start_state": mergeCandidateStateFailed}},
		{name: "not_queued_real_start", answer: func(t *testing.T, f *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
			// The REAL answer the pre-#4812 forwarded cause produced: a
			// base_advance start on a base-advanced head with no queue entry
			// and an ineligible run.
			f.seedBaseAdvanceHead()
			q := f.wireNotEligibleQueue()
			f.assertBaseAdvanceHead(mergeCandidateStateUnverified)
			start, refusal := f.s.startMergeCandidateVerifyPass(context.Background(), f.parent.ID, mergeCandidateVerifyParams{
				Branch: f.branch, BaseRef: "main", HeadSHA: fanInHead, Cause: mergeCandidateCauseBaseAdvance,
			}, mergeCandidateSystemActor())
			if refusal != nil || start == nil || start.State != mergeCandidateStateNotQueued {
				t.Fatalf("precondition: forced base_advance start = %+v / %+v, want not_queued", start, refusal)
			}
			if n := q.called("Enqueue"); n != 0 {
				t.Fatalf("precondition: Enqueue calls = %d, want 0", n)
			}
			if n := len(mcvTriggerRows(f.au)); n != 0 {
				t.Fatalf("precondition: trigger rows = %d, want 0 (not_queued starts nothing)", n)
			}
			return start, refusal
		}, wantHold: true, wantLevel: "WARN", wantMsg: "answered not_queued", wantAttrs: map[string]any{"start_state": mergeCandidateStateNotQueued}},
		{name: "held", answer: func(*testing.T, *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
			return &mergeCandidateVerifyStart{State: mergeCandidateStateHeld, Position: 2, LiveRunID: &liveRun}, nil
		}, wantHold: true, wantLevel: "WARN", wantMsg: "answered held",
			wantAttrs: map[string]any{"start_state": mergeCandidateStateHeld, "position": float64(2), "live_run_id": liveRun.String()}},
		{name: "unknown", answer: func(*testing.T, *fanInFixture) (*mergeCandidateVerifyStart, *mergeCandidateVerifyRefusal) {
			return &mergeCandidateVerifyStart{State: "bogus"}, nil
		}, wantHold: true, wantLevel: "WARN", wantMsg: "unknown merge-candidate verify start answer", wantAttrs: map[string]any{"start_state": "bogus"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFanInFixture(t, fanInVerifySpec)
			start, refusal := tc.answer(t, f)
			logs := f.captureLogs()
			attrs := []slog.Attr{slog.String("run_id", f.parent.ID.String()), slog.String("head_sha", fanInHead)}
			got := f.s.fanInHoldOnStart(context.Background(), attrs, mergeCandidateCauseFanIn, start, refusal)
			if got != tc.wantHold {
				t.Fatalf("hold = %v, want %v", got, tc.wantHold)
			}
			recs := logs()
			started := fanInStartedRecords(recs)
			if tc.wantStarted != (len(started) == 1) || len(started) > 1 {
				t.Errorf("'pass started' records = %+v, want started=%v", started, tc.wantStarted)
			}
			if !tc.wantHold {
				if len(recs) != 0 {
					t.Errorf("a release logged %+v, want nothing", recs)
				}
				return
			}
			if len(recs) != 1 {
				t.Fatalf("records = %+v, want exactly 1", recs)
			}
			r := recs[0]
			if r.level != tc.wantLevel || !strings.Contains(r.msg, tc.wantMsg) {
				t.Errorf("record = %s %q, want %s containing %q", r.level, r.msg, tc.wantLevel, tc.wantMsg)
			}
			if r.attrs["run_id"] != f.parent.ID.String() || r.attrs["head_sha"] != fanInHead {
				t.Errorf("record lost the hold's attrs: %+v", r.attrs)
			}
			for k, want := range tc.wantAttrs {
				if r.attrs[k] != want {
					t.Errorf("record attr %s = %#v, want %#v", k, r.attrs[k], want)
				}
			}
		})
	}
}
