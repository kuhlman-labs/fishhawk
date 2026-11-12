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
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/defaultbranch"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
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
	gitlabRef := "gitlab:42"
	cases := []struct {
		name       string
		spec       []byte
		setup      func(f *fanInFixture)
		want       bool
		wantReason string
		wantDetail string
		// wantDetailPrefix replaces wantDetail when the detail embeds the run's
		// own repo string.
		wantDetailPrefix string
		wantErr          bool
		// wantHead is the consolidated head the answer judged (#4846): set on
		// every compare-derived answer, settled or not, and empty on every arm
		// that compared nothing (child, no children, fail-open).
		wantHead string
	}{
		{name: "passed", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.seedVerdict(fanInHead, mergeCandidateResultPassed) }, want: true, wantHead: fanInHead},
		{name: "not_required", spec: specImplementGatingReviewers, want: true, wantHead: fanInHead},
		{name: "failed", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.seedVerdict(fanInHead, mergeCandidateResultFailed) }, want: true, wantHead: fanInHead},
		{name: "unverified", spec: fanInVerifySpec, want: false, wantHead: fanInHead},
		{name: "not_executed", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.seedVerdict(fanInHead, mergeCandidateResultNotExecuted) }, want: false, wantHead: fanInHead},
		{name: "in_flight", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.seedTrigger(fanInHead) }, want: false, wantHead: fanInHead},
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
		// The fail-open arms (#4792): Settled stays true, and the answer names why
		// no verify was consulted.
		{name: "forge_not_wired", spec: fanInVerifySpec, setup: func(f *fanInFixture) { f.s.cfg.GitHub = nil },
			want: true, wantReason: orchestrator.FanInVerifyUnavailableForgeNotWired, wantDetail: "github client not wired"},
		{name: "forge_no_installation_id", spec: fanInVerifySpec, setup: func(f *fanInFixture) {
			f.rr.mu.Lock()
			f.rr.runs[f.parent.ID].InstallationID = nil
			f.rr.mu.Unlock()
		}, want: true, wantReason: orchestrator.FanInVerifyUnavailableForgeNotWired, wantDetail: "no installation id"},
		{name: "forge_parse_failure", spec: fanInVerifySpec, setup: func(f *fanInFixture) {
			f.rr.mu.Lock()
			f.rr.runs[f.parent.ID].Repo = "noslash"
			f.rr.mu.Unlock()
		}, want: true, wantReason: orchestrator.FanInVerifyUnavailableForgeNotWired, wantDetailPrefix: "parse repo"},
		{name: "forge_unresolved_family", spec: fanInVerifySpec, setup: func(f *fanInFixture) {
			f.rr.mu.Lock()
			f.rr.runs[f.parent.ID].InstallationRef = &gitlabRef
			f.rr.mu.Unlock()
			f.s.cfg.ForgeResolver = func(string) (forge.Forge, error) { return nil, errors.New("registry down") }
		}, want: true, wantReason: orchestrator.FanInVerifyUnavailableForgeNotWired, wantDetail: "forge gitlab unresolved"},
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
					t.Fatalf("FanInVerifySettled = %+v, nil; want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("FanInVerifySettled: %v", err)
			}
			if got.Settled != tc.want {
				t.Errorf("FanInVerifySettled.Settled = %v, want %v", got.Settled, tc.want)
			}
			if got.UnavailableReason != tc.wantReason {
				t.Errorf("FanInVerifySettled.UnavailableReason = %q, want %q", got.UnavailableReason, tc.wantReason)
			}
			if tc.wantDetailPrefix != "" {
				if !strings.HasPrefix(got.UnavailableDetail, tc.wantDetailPrefix) {
					t.Errorf("FanInVerifySettled.UnavailableDetail = %q, want prefix %q", got.UnavailableDetail, tc.wantDetailPrefix)
				}
			} else if got.UnavailableDetail != tc.wantDetail {
				t.Errorf("FanInVerifySettled.UnavailableDetail = %q, want %q", got.UnavailableDetail, tc.wantDetail)
			}
			if got.HeadSHA != tc.wantHead {
				t.Errorf("FanInVerifySettled.HeadSHA = %q, want %q", got.HeadSHA, tc.wantHead)
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
		got, err := s.FanInVerifySettled(ctx, uuid.New(), "main", "b")
		if err != nil || !got.Settled || got.UnavailableReason != orchestrator.FanInVerifyUnavailableNoRunRepo || got.HeadSHA != "" {
			t.Errorf("FanInVerifySettled with no run repo = %+v, %v; want settled with reason no_run_repo and no head", got, err)
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

// --- #4846: the hold runs BEFORE the acceptance short-circuit ------------------

// setFanInPlanBytes replaces the content of the fixture's approved plan
// artifact (seedConsolidatedParent's plan stage), so a test can choose the
// plan's verification block.
func setFanInPlanBytes(t *testing.T, f *fanInFixture, planBytes json.RawMessage) {
	t.Helper()
	art, ok := f.s.cfg.ArtifactRepo.(*fakeArtifactRepo)
	if !ok {
		t.Fatalf("ArtifactRepo = %T, want *fakeArtifactRepo", f.s.cfg.ArtifactRepo)
	}
	art.mu.Lock()
	defer art.mu.Unlock()
	for _, a := range art.all {
		if a.Kind == artifact.KindPlan {
			a.Content = planBytes
			return
		}
	}
	t.Fatal("no plan artifact seeded")
}

// auditFeedItem is the slice of a GET /v0/runs/{id}/audit item the #4846
// cross-layer test reads.
type auditFeedItem struct {
	Sequence int64           `json:"sequence"`
	Category string          `json:"category"`
	Payload  json.RawMessage `json:"payload"`
}

// getAuditFeed GETs the real audit feed filtered to category.
func getAuditFeed(t *testing.T, s *Server, runID uuid.UUID, category string) []auditFeedItem {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String()+"/audit?category="+category, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET audit feed (%s) status = %d:\n%s", category, w.Code, w.Body.String())
	}
	var page struct {
		Items []auditFeedItem `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode audit feed: %v", err)
	}
	return page.Items
}

// (f) TestFanInAcceptanceHold_AllSkipShortCircuit_EndToEnd is the #4846
// cross-layer test: the real *Server as FanInVerify and ConsolidatedReview,
// the real Orchestrator with an approved all-skip-with-basis plan, the audit
// chain fake, the real REST handlers and the real issuecomment renderer.
// Advance #1 holds the parent's acceptance with ZERO verdict rows on the feed;
// a signed merge_candidate_verified passed report for the consolidated head
// releases it through the real report handler; the feed then carries exactly
// one not_validated verdict bound to that head, sequenced after the verify; and
// the PR status comment renders it as covering, not validating, that head (R8).
// MECHANISM: the head is unverified at Advance #1, so under the old ordering
// (short-circuit before the hold) the verdict is already on the feed there.
func TestFanInAcceptanceHold_AllSkipShortCircuit_EndToEnd(t *testing.T) {
	f, acc := newFanInAcceptanceAheadFixture(t, fanInVerifySpec)
	setFanInPlanBytes(t, f, admissionPlanBytes(t, nil, allSkipWithBasisCriteria))
	prURL := "https://github.com/kuhlman-labs/example/pull/9"
	f.rr.mu.Lock()
	f.rr.runs[f.parent.ID].PullRequestURL = &prURL
	f.rr.mu.Unlock()
	o := &orchestrator.Orchestrator{Runs: f.rr, Audit: f.au, Artifacts: f.s.cfg.ArtifactRepo,
		ConsolidatedReview: f.s, FanInVerify: f.s, DefaultBranch: defaultbranch.Static("main")}
	f.s.cfg.Orchestrator = o
	sf := newSigningFake()
	f.s.cfg.SigningRepo = sf
	priv, _ := sf.issue(t, f.parent.ID)
	ctx := context.Background()

	// 1. Advance reaches the pending acceptance with the head unverified: held,
	// the verify-only pass started, and NO verdict on the feed.
	if _, err := o.Advance(ctx, f.parent.ID); err != nil {
		t.Fatalf("Advance #1: %v", err)
	}
	f.s.waitBackgroundReviews()
	if got := f.state(acc.ID); got != run.StageStatePending {
		t.Fatalf("acceptance state = %q, want pending (held until the fan-in verify settles)", got)
	}
	if n := len(mcvTriggerRows(f.au)); n != 1 {
		t.Fatalf("merge-candidate trigger rows = %d, want 1", n)
	}
	if items := getAuditFeed(t, f.s, f.parent.ID, CategoryAcceptanceOutcomeRecorded); len(items) != 0 {
		t.Fatalf("acceptance_outcome_recorded feed items = %d, want 0 before the consolidated head is verified: %+v", len(items), items)
	}

	// 2. The runner reports passed for exactly the consolidated head.
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

	// 3. The release short-circuited acceptance with ONE verdict bound to the
	// verified head, after the verify row, and the review gate opened.
	if got := f.state(acc.ID); got != run.StageStateSucceeded {
		t.Fatalf("acceptance state = %q, want succeeded (short-circuited on release)", got)
	}
	verdicts := getAuditFeed(t, f.s, f.parent.ID, CategoryAcceptanceOutcomeRecorded)
	if len(verdicts) != 1 {
		t.Fatalf("acceptance_outcome_recorded feed items = %d, want exactly 1: %+v", len(verdicts), verdicts)
	}
	var verdict struct {
		Verdict string `json:"verdict"`
		Basis   string `json:"basis"`
		HeadSHA string `json:"head_sha"`
	}
	if err := json.Unmarshal(verdicts[0].Payload, &verdict); err != nil {
		t.Fatalf("decode verdict payload %s: %v", verdicts[0].Payload, err)
	}
	if verdict.Verdict != "not_validated" || verdict.Basis != "all-skip-with-basis" {
		t.Errorf("verdict = %q basis %q, want not_validated / all-skip-with-basis", verdict.Verdict, verdict.Basis)
	}
	if verdict.HeadSHA != fanInHead {
		t.Errorf("verdict head_sha = %q, want the hook-judged consolidated head %q", verdict.HeadSHA, fanInHead)
	}
	verified := getAuditFeed(t, f.s, f.parent.ID, CategoryMergeCandidateVerified)
	if len(verified) != 1 {
		t.Fatalf("merge_candidate_verified feed items = %d, want 1", len(verified))
	}
	if verdicts[0].Sequence <= verified[0].Sequence {
		t.Errorf("verdict sequence %d <= merge_candidate_verified sequence %d, want the verdict AFTER the verify settled",
			verdicts[0].Sequence, verified[0].Sequence)
	}
	if got := f.state(f.review.ID); got != run.StageStateAwaitingApproval {
		t.Errorf("review state = %q, want awaiting_approval", got)
	}

	// 4. R8 consumer: the chain rows as written render the verdict as covering
	// the head, never as having validated it.
	entries, err := f.au.ListForRun(ctx, f.parent.ID)
	if err != nil {
		t.Fatalf("ListForRun: %v", err)
	}
	for i, e := range entries {
		e.Sequence = int64(i + 1)
	}
	comment := issuecomment.RenderPRStatusBody(issuecomment.PRStatusInput{Run: f.runRow(), Audit: entries, Now: time.Now().UTC()})
	if want := "Covers head: `" + fanInHead + "` (not validated)"; !strings.Contains(comment, want) {
		t.Errorf("PR status comment missing %q:\n%s", want, comment)
	}
	if strings.Contains(comment, "Validated head") {
		t.Errorf("PR status comment certifies the short-circuited head as validated:\n%s", comment)
	}
}

// --- #4792: the fail-open acceptance release is observable --------------------

// failOpenParent is the feature_change-shaped decomposed parent (#4785) with a
// stamped consolidated PR and NO forge wired (cfg.GitHub nil), driven by a REAL
// orchestrator whose Audit is the fixture's audit fake and whose FanInVerify and
// ConsolidatedReview are the real *Server: the producer of the
// fan_in_verify_unavailable signal, un-mocked end to end.
func failOpenParent(t *testing.T) (*fanInFixture, *run.Stage, *orchestrator.Orchestrator) {
	t.Helper()
	f, acc := newFanInAcceptanceAheadFixture(t, fanInVerifySpec)
	prURL := "https://github.com/kuhlman-labs/example/pull/9"
	f.rr.mu.Lock()
	f.rr.runs[f.parent.ID].PullRequestURL = &prURL
	f.rr.mu.Unlock()
	f.s.cfg.GitHub = nil
	o := &orchestrator.Orchestrator{Runs: f.rr, Audit: f.au, ConsolidatedReview: f.s, FanInVerify: f.s, DefaultBranch: defaultbranch.Static("main")}
	f.s.cfg.Orchestrator = o
	return f, acc, o
}

// getRunBody GETs /v0/runs/{id} through the real handler and decodes the body.
func getRunBody(t *testing.T, s *Server, runID uuid.UUID) (map[string]json.RawMessage, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/v0/runs/"+runID.String(), nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v0/runs/%s status = %d:\n%s", runID, w.Code, w.Body.String())
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode run body: %v", err)
	}
	return body, w.Body.String()
}

// fanInUnavailableFeedItem is the slice of a fan_in_verify_unavailable audit
// feed item the integration tests read.
type fanInUnavailableFeedItem struct {
	Category string          `json:"category"`
	StageID  string          `json:"stage_id"`
	Payload  json.RawMessage `json:"payload"`
}

// getUnavailableFeed GETs the real audit feed filtered to the category.
func getUnavailableFeed(t *testing.T, s *Server, runID uuid.UUID) ([]fanInUnavailableFeedItem, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet,
		"/v0/runs/"+runID.String()+"/audit?category="+orchestrator.CategoryFanInVerifyUnavailable, nil)
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET audit feed status = %d:\n%s", w.Code, w.Body.String())
	}
	var page struct {
		Items []fanInUnavailableFeedItem `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode audit feed: %v", err)
	}
	return page.Items, w.Body.String()
}

// TestFanInVerifyUnavailable_EndToEnd (#4792) crosses every layer of the signal:
// the real server hook, the real orchestrator, the audit chain and the REST
// handlers. A decomposed parent with a stamped PR and no forge wired is
// RELEASED (never held), exactly one fan_in_verify_unavailable row lands on the
// chain, and the real GET /v0/runs/{id} body and the real audit feed both carry
// it. The healthy arm (a passed verdict) records nothing and omits the key.
// COUNTERFACTUAL: delete the handleGetRun line → the body assertions go RED
// while the row stays (the wiring, not the emit); delete the orchestrator's
// hook-answer record → no row, RED.
func TestFanInVerifyUnavailable_EndToEnd(t *testing.T) {
	ctx := context.Background()
	t.Run("fail-open release is recorded and surfaced", func(t *testing.T) {
		f, acc, o := failOpenParent(t)
		if _, err := o.Advance(ctx, f.parent.ID); err != nil {
			t.Fatalf("Advance: %v", err)
		}
		f.s.waitBackgroundReviews()
		if got := f.state(acc.ID); got != run.StageStateAwaitingHostDispatch {
			t.Fatalf("acceptance state = %q, want awaiting_host_dispatch (released: a signal, never a hold)", got)
		}
		if n := f.count(orchestrator.CategoryFanInVerifyUnavailable); n != 1 {
			t.Fatalf("fan_in_verify_unavailable rows = %d, want exactly 1", n)
		}

		body, _ := getRunBody(t, f.s, f.parent.ID)
		raw, ok := body["fan_in_verify_unavailable"]
		if !ok {
			t.Fatalf("run body has no fan_in_verify_unavailable key; keys: %v", func() []string {
				var ks []string
				for k := range body {
					ks = append(ks, k)
				}
				return ks
			}())
		}
		var block struct {
			Reason            string `json:"reason"`
			Detail            string `json:"detail"`
			AcceptanceStageID string `json:"acceptance_stage_id"`
			Note              string `json:"note"`
		}
		if err := json.Unmarshal(raw, &block); err != nil {
			t.Fatalf("decode block %s: %v", raw, err)
		}
		if block.Reason != orchestrator.FanInVerifyUnavailableForgeNotWired || block.Detail != "github client not wired" ||
			block.AcceptanceStageID != acc.ID.String() {
			t.Errorf("block = %+v, want forge_not_wired / github client not wired / %s", block, acc.ID)
		}
		if !strings.Contains(block.Note, orchestrator.FanInVerifyUnavailableForgeNotWired) || !strings.Contains(block.Note, "merge gate") {
			t.Errorf("note %q must name the reason and say the merge gate still applies", block.Note)
		}

		// The list endpoint stays free of the extra audit read: it carries the
		// parent but never the block.
		listReq := httptest.NewRequest(http.MethodGet, "/v0/runs", nil)
		listW := httptest.NewRecorder()
		f.s.Handler().ServeHTTP(listW, listReq)
		if listW.Code != http.StatusOK {
			t.Fatalf("GET /v0/runs status = %d:\n%s", listW.Code, listW.Body.String())
		}
		var list struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(listW.Body.Bytes(), &list); err != nil {
			t.Fatalf("decode run list: %v", err)
		}
		listed := false
		for _, it := range list.Items {
			if strings.Contains(string(it["id"]), f.parent.ID.String()) {
				listed = true
				if _, has := it["fan_in_verify_unavailable"]; has {
					t.Errorf("the list endpoint carries fan_in_verify_unavailable: %s", it["fan_in_verify_unavailable"])
				}
			}
		}
		if !listed {
			t.Fatalf("the parent run is absent from the list response: %s", listW.Body.String())
		}

		items, _ := getUnavailableFeed(t, f.s, f.parent.ID)
		if len(items) != 1 {
			t.Fatalf("audit feed items = %d, want 1", len(items))
		}
		if items[0].Category != orchestrator.CategoryFanInVerifyUnavailable || items[0].StageID != acc.ID.String() {
			t.Errorf("feed item = %+v, want the category anchored on the acceptance stage %s", items[0], acc.ID)
		}
		var payload orchestrator.FanInVerifyUnavailablePayload
		if err := json.Unmarshal(items[0].Payload, &payload); err != nil {
			t.Fatalf("decode feed payload %s: %v", items[0].Payload, err)
		}
		if payload.Reason != orchestrator.FanInVerifyUnavailableForgeNotWired || payload.AcceptanceStageID != acc.ID.String() {
			t.Errorf("feed payload = %+v, want reason forge_not_wired on the acceptance stage", payload)
		}
	})

	t.Run("healthy release records nothing and omits the key", func(t *testing.T) {
		f, acc, o := failOpenParent(t)
		f.s.cfg.GitHub = cannedComparePatchClient(t, cannedCompareOneFile)
		f.seedVerdict(fanInHead, mergeCandidateResultPassed)
		if _, err := o.Advance(ctx, f.parent.ID); err != nil {
			t.Fatalf("Advance: %v", err)
		}
		f.s.waitBackgroundReviews()
		if got := f.state(acc.ID); got != run.StageStateAwaitingHostDispatch {
			t.Fatalf("acceptance state = %q, want awaiting_host_dispatch", got)
		}
		if n := f.count(orchestrator.CategoryFanInVerifyUnavailable); n != 0 {
			t.Errorf("fan_in_verify_unavailable rows = %d, want 0 on a healthy release", n)
		}
		if body, _ := getRunBody(t, f.s, f.parent.ID); body["fan_in_verify_unavailable"] != nil {
			t.Errorf("run body carries fan_in_verify_unavailable %s on a healthy release, want the key omitted", body["fan_in_verify_unavailable"])
		}
	})
}

// TestFanInVerifyUnavailable_DetailOmitsUpstreamErrorAndRefTail (#4792, approval
// condition C5) traces the persisted detail to its producer with a SYNTHETIC
// installation-ref tail and resolver error: the forge lookup detail is
// forgeCompareFor's constant "forge <family> unresolved", so neither the
// ref tail (after the colon) nor the wrapped upstream error reaches the
// run-status block, the audit row or the audit feed.
func TestFanInVerifyUnavailable_DetailOmitsUpstreamErrorAndRefTail(t *testing.T) {
	const (
		refTail  = "SECRET-INSTALLATION-REF-TAIL-7f3a"
		upstream = "UPSTREAM-ERROR-DSN-postgres://u:p@internal-host"
	)
	f, acc, o := failOpenParent(t)
	ref := "gitlab:" + refTail
	f.rr.mu.Lock()
	f.rr.runs[f.parent.ID].InstallationRef = &ref
	f.rr.mu.Unlock()
	f.s.cfg.ForgeResolver = func(string) (forge.Forge, error) { return nil, errors.New(upstream) }

	if _, err := o.Advance(context.Background(), f.parent.ID); err != nil {
		t.Fatalf("Advance: %v", err)
	}
	f.s.waitBackgroundReviews()
	if got := f.state(acc.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("acceptance state = %q, want awaiting_host_dispatch", got)
	}

	rows := auditEntries(f.au, orchestrator.CategoryFanInVerifyUnavailable)
	if len(rows) != 1 {
		t.Fatalf("fan_in_verify_unavailable rows = %d, want 1", len(rows))
	}
	var payload orchestrator.FanInVerifyUnavailablePayload
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Reason != orchestrator.FanInVerifyUnavailableForgeNotWired || payload.Detail != "forge gitlab unresolved" {
		t.Errorf("payload = %+v, want forge_not_wired / %q", payload, "forge gitlab unresolved")
	}
	body, rawBody := getRunBody(t, f.s, f.parent.ID)
	_, rawFeed := getUnavailableFeed(t, f.s, f.parent.ID)
	for label, surface := range map[string]string{
		"audit row payload": string(rows[0].Payload),
		"run-status block":  string(body["fan_in_verify_unavailable"]),
		"audit feed":        rawFeed,
		"run-status body":   rawBody,
	} {
		for _, leaked := range []string{refTail, upstream} {
			if strings.Contains(surface, leaked) {
				t.Errorf("%s leaks %q:\n%s", label, leaked, surface)
			}
		}
	}
	if !strings.Contains(string(body["fan_in_verify_unavailable"]), "forge gitlab unresolved") {
		t.Errorf("run-status block lacks the constant detail: %s", body["fan_in_verify_unavailable"])
	}
}

// appendUnavailableRow seeds one fan_in_verify_unavailable row on the audit fake.
func appendUnavailableRow(au *auditFake, runID, stageID uuid.UUID, reason string) {
	raw, _ := json.Marshal(orchestrator.FanInVerifyUnavailablePayload{
		Reason: reason, AcceptanceStageID: stageID.String(), ConsolidatedBranch: "fishhawk/run-x",
	})
	au.mu.Lock()
	defer au.mu.Unlock()
	au.appended = append(au.appended, audit.ChainAppendParams{
		RunID: runID, StageID: &stageID, Category: orchestrator.CategoryFanInVerifyUnavailable, Payload: raw,
	})
}

// The run-status distiller is best-effort and never fails the read: nil
// AuditRepo -> nil, list error -> WARN + nil, no row -> nil, undecodable newest
// row -> WARN + nil, and the NEWEST of several rows wins.
func TestFanInVerifyUnavailableForRun_Arms(t *testing.T) {
	ctx := context.Background()
	runID, stageID := uuid.New(), uuid.New()

	t.Run("nil AuditRepo", func(t *testing.T) {
		s := New(Config{})
		if got := s.fanInVerifyUnavailableForRun(ctx, runID); got != nil {
			t.Errorf("block = %+v, want nil", got)
		}
	})
	t.Run("list error WARNs", func(t *testing.T) {
		au := newAuditFake()
		au.listByCategoryErr = errors.New("audit unavailable")
		s := New(Config{AuditRepo: au})
		buf := captureLogger(s)
		if got := s.fanInVerifyUnavailableForRun(ctx, runID); got != nil {
			t.Errorf("block = %+v, want nil on a list error", got)
		}
		if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "list fan_in_verify_unavailable failed") {
			t.Errorf("no WARN for the list error; logs:\n%s", buf.String())
		}
	})
	t.Run("no row", func(t *testing.T) {
		s := New(Config{AuditRepo: newAuditFake()})
		if got := s.fanInVerifyUnavailableForRun(ctx, runID); got != nil {
			t.Errorf("block = %+v, want nil with no row", got)
		}
	})
	t.Run("undecodable newest row WARNs", func(t *testing.T) {
		au := newAuditFake()
		au.stampSequence = true
		appendUnavailableRow(au, runID, stageID, orchestrator.FanInVerifyUnavailableNoRunRepo)
		au.appended = append(au.appended, audit.ChainAppendParams{
			RunID: runID, StageID: &stageID, Category: orchestrator.CategoryFanInVerifyUnavailable, Payload: []byte("not json"),
		})
		s := New(Config{AuditRepo: au})
		buf := captureLogger(s)
		if got := s.fanInVerifyUnavailableForRun(ctx, runID); got != nil {
			t.Errorf("block = %+v, want nil for an undecodable newest row", got)
		}
		if !strings.Contains(buf.String(), "level=WARN") || !strings.Contains(buf.String(), "decode fan_in_verify_unavailable payload failed") {
			t.Errorf("no WARN for the undecodable row; logs:\n%s", buf.String())
		}
	})
	t.Run("newest of two rows wins", func(t *testing.T) {
		au := newAuditFake()
		au.stampSequence = true
		appendUnavailableRow(au, runID, stageID, orchestrator.FanInVerifyUnavailableNoConsolidatedPR)
		appendUnavailableRow(au, runID, stageID, orchestrator.FanInVerifyUnavailableHookNotWired)
		s := New(Config{AuditRepo: au})
		got := s.fanInVerifyUnavailableForRun(ctx, runID)
		if got == nil || got.Reason != orchestrator.FanInVerifyUnavailableHookNotWired || got.AcceptanceStageID != stageID.String() {
			t.Fatalf("block = %+v, want the newest row (hook_not_wired) on stage %s", got, stageID)
		}
		if !strings.Contains(got.Note, "hook_not_wired") || !strings.Contains(got.Note, "merge gate") {
			t.Errorf("note %q must name the reason and say the merge gate still applies", got.Note)
		}
	})
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
	if ans, err := f.s.FanInVerifySettled(ctx, f.parent.ID, "main", f.branch); err != nil || ans.Settled {
		t.Fatalf("FanInVerifySettled before the pass = %+v, %v; want unsettled", ans, err)
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

	if ans, err := f.s.FanInVerifySettled(ctx, f.parent.ID, "main", f.branch); err != nil || !ans.Settled {
		t.Errorf("FanInVerifySettled after the pass = %+v, %v; want settled", ans, err)
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

// --- #4874: the D5 hold binds the live head; acceptance reads the ledger ---

const (
	fanInLedgerF       = "integF00" // the recorded integration merge, != fanInHead
	fanInDivergenceMsg = "fan-in verify: the live consolidated head differs from the acceptance ledger head; the live-head answer stands"
)

// seedLedgerIntegration records integration_commit_recorded{merge_sha: F}.
func (f *fanInFixture) seedLedgerIntegration() {
	f.appendRow(nil, auditcomplete.CategoryIntegrationCommitRecorded, map[string]any{"merge_sha": fanInLedgerF})
}

// seedLedgerRebase records branch_rebased F -> fanInHead, clean or not.
func (f *fanInFixture) seedLedgerRebase(clean bool) {
	f.appendRow(nil, CategoryBranchRebased, map[string]any{
		"prior_head_sha": fanInLedgerF, "new_head_sha": fanInHead, "merge_commit_sha": fanInHead,
		"already_up_to_date": false, "clean_verified": clean, "base_tip_sha": "basetip1",
	})
}

// seedVerdictOnly appends a merge_candidate_verified row for head with no
// trigger, so it is exact-head evidence and nothing else.
func (f *fanInFixture) seedVerdictOnly(head, result string) {
	id := f.impl.ID
	f.appendRow(&id, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
		HeadSHA: head, Cause: mergeCandidateCauseFanIn, VerifyCommand: "scripts/test verify",
		Result: result, OutputTail: fanInTailSentinel, OutputUntrusted: true,
	})
}

// Criterion fan-in-and-acceptance-agree-after-clean-advance: on a clean
// recorded advance F -> fanInHead the D5 hold's live head and acceptance's
// ledger head agree. A passed verdict at F cannot settle the gate; a passed
// verdict at fanInHead settles it and releases acceptance, and the prompt's
// resolver names fanInHead. No divergence WARN is logged.
func TestFanInAcceptanceHold_RebasedHead_GatesAgree(t *testing.T) {
	f, acc := newFanInAcceptanceAheadFixture(t, fanInVerifySpec)
	if fanInLedgerF == fanInHead {
		t.Fatal("precondition: the recorded integration head must differ from the compare head")
	}
	f.seedLedgerIntegration()
	f.seedLedgerRebase(true)
	f.wireNotEligibleQueue()
	buf := captureLogger(f.s)
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

	if got, err := f.s.acceptanceHeadAsOf(ctx, f.parent.ID, 0, false); err != nil || got != fanInHead {
		t.Fatalf("acceptanceHeadAsOf = (%q, %v), want the compare head %q", got, err, fanInHead)
	}
	// A passed verdict at the OLD head F, recorded before the hold's pass so it
	// consumes no trigger: it must not settle the gate.
	f.seedVerdictOnly(fanInLedgerF, mergeCandidateResultPassed)
	if ans, err := f.s.FanInVerifySettled(ctx, f.parent.ID, "main", f.branch); err != nil || ans.Settled {
		t.Fatalf("FanInVerifySettled with a pass only at F = %+v, %v; want unsettled (the old head cannot settle)", ans, err)
	}
	if _, err := o.Advance(ctx, f.parent.ID); err != nil {
		t.Fatalf("Advance #1: %v", err)
	}
	f.s.waitBackgroundReviews()
	if got := f.state(acc.ID); got != run.StageStatePending {
		t.Fatalf("acceptance state = %q, want pending (held)", got)
	}
	f.assertOneFanInTrigger()

	f.rr.mu.Lock()
	f.impl.State = run.StageStateRunning
	f.rr.mu.Unlock()
	body, _ := json.Marshal(map[string]any{
		"outcome": outcomeMergeCandidateVerified, "branch": f.branch, "head_sha": fanInHead,
		"merge_candidate_result": "passed", "merge_candidate_reason": "",
		"merge_candidate_output_tail": fanInTailSentinel,
	})
	if w := shipFanInReport(t, f, priv, body); w.Code != http.StatusOK {
		t.Fatalf("report status = %d:\n%s", w.Code, w.Body.String())
	}
	f.s.waitBackgroundReviews()
	if ans, err := f.s.FanInVerifySettled(ctx, f.parent.ID, "main", f.branch); err != nil || !ans.Settled {
		t.Errorf("FanInVerifySettled after the pass at fanInHead = %+v, %v; want settled", ans, err)
	}
	if got := f.state(acc.ID); got != run.StageStateAwaitingHostDispatch {
		t.Fatalf("acceptance state = %q, want awaiting_host_dispatch (released)", got)
	}
	if got := f.s.resolveAcceptanceExpectedHeadSHA(ctx, f.parent.ID, acc.ID); got != fanInHead {
		t.Errorf("resolveAcceptanceExpectedHeadSHA = %q, want %q", got, fanInHead)
	}
	if strings.Contains(buf.String(), fanInDivergenceMsg) {
		t.Errorf("divergence WARN logged on the agreeing head:\n%s", buf.String())
	}
}

// TestFanInVerifySettled_DivergedHead_WarnsWithoutChangingAnswer: each named
// divergence logs exactly one WARN naming both heads, and the settled answer
// stays the LIVE head's; a ledger read error WARNs and changes nothing.
func TestFanInVerifySettled_DivergedHead_WarnsWithoutChangingAnswer(t *testing.T) {
	ctx := context.Background()
	divergences := []struct {
		name string
		seed func(f *fanInFixture)
	}{
		{"unrecorded", func(*fanInFixture) {}},
		{"unclean", func(f *fanInFixture) { f.seedLedgerRebase(false) }},
		// This fixture has no PR number, so rule 4 reads D8 Amendment 1's
		// cannot-probe answer: the row asserts the settled answer equals the
		// live-head state rather than implying a probe ran.
		{"vouched", func(f *fanInFixture) {
			f.appendRow(nil, CategoryOperatorCommitVouched, map[string]any{"vouched_sha": fanInHead})
		}},
	}
	for _, d := range divergences {
		for _, verdictAt := range []string{fanInHead, fanInLedgerF} {
			t.Run(d.name+"/passed_at_"+verdictAt, func(t *testing.T) {
				f := newFanInFixture(t, fanInVerifySpec)
				f.seedLedgerIntegration()
				d.seed(f)
				f.seedVerdictOnly(verdictAt, mergeCandidateResultPassed)
				if got, err := f.s.acceptanceHeadAsOf(ctx, f.parent.ID, 0, false); err != nil || got != fanInLedgerF {
					t.Fatalf("precondition: ledger head = (%q, %v), want %q", got, err, fanInLedgerF)
				}
				state, err := f.s.mergeCandidateVerifyState(ctx, f.runRow(), fanInHead)
				if err != nil {
					t.Fatal(err)
				}
				wantSettled := state.State == mergeCandidateStateNotRequired || state.State == mergeCandidateStatePassed || state.State == mergeCandidateStateFailed
				if d.name != "vouched" {
					// The live head's own answer: settled only by a verdict AT it.
					if want := verdictAt == fanInHead; wantSettled != want {
						t.Fatalf("precondition: live-head state %+v, want settled=%v", state, want)
					}
				}
				buf := captureLogger(f.s)
				ans, err := f.s.FanInVerifySettled(ctx, f.parent.ID, "main", f.branch)
				if err != nil || ans.Settled != wantSettled || ans.UnavailableReason != "" {
					t.Errorf("FanInVerifySettled = %+v, %v; want settled=%v with no reason (the live head's answer)", ans, err, wantSettled)
				}
				lines := linesContaining(buf.String(), fanInDivergenceMsg)
				if len(lines) != 1 {
					t.Fatalf("divergence WARN lines = %d, want exactly 1:\n%s", len(lines), buf.String())
				}
				for _, attr := range []string{"level=WARN", "live_head_sha=" + fanInHead, "ledger_head_sha=" + fanInLedgerF} {
					if !strings.Contains(lines[0], attr) {
						t.Errorf("divergence WARN missing %q: %s", attr, lines[0])
					}
				}
			})
		}
	}
	t.Run("a ledger read error WARNs and keeps the live answer", func(t *testing.T) {
		f := newFanInFixture(t, fanInVerifySpec)
		f.seedLedgerIntegration()
		f.seedVerdictOnly(fanInHead, mergeCandidateResultPassed)
		f.au.listByCategoryErrCategory = CategoryBranchReset // read by the ledger walk, not the classifier
		state, err := f.s.mergeCandidateVerifyState(ctx, f.runRow(), fanInHead)
		if err != nil || state.State != mergeCandidateStatePassed {
			t.Fatalf("precondition: live-head state = %+v (err %v), want passed", state, err)
		}
		buf := captureLogger(f.s)
		ans, err := f.s.FanInVerifySettled(ctx, f.parent.ID, "main", f.branch)
		if err != nil || !ans.Settled {
			t.Errorf("FanInVerifySettled = %+v, %v; want settled, nil (a ledger read error changes nothing)", ans, err)
		}
		lines := linesContaining(buf.String(), "fan-in verify: acceptance ledger head unreadable; the live-head answer stands")
		if len(lines) != 1 || !strings.Contains(lines[0], "level=WARN") {
			t.Errorf("want exactly one read-error WARN, got:\n%s", buf.String())
		}
		if strings.Contains(buf.String(), fanInDivergenceMsg) {
			t.Errorf("divergence WARN logged on a read error:\n%s", buf.String())
		}
	})
}
