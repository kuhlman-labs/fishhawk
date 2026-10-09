package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
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
	f.s.cfg.Orchestrator = &orchestrator.Orchestrator{Runs: f.rr, ConsolidatedReview: f.s}
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
