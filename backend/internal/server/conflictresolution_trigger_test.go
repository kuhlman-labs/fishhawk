package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// --- E64.62 / #3202: the rebase verb's budgeted conflict-resolution arm ---
//
// Every case here drives the REAL startConflictResolutionPass against the
// shared rebase seed, and each fail-closed branch asserts the OBSERVABLE
// consequence — no trigger entry appended, and the implement stage NOT
// re-opened — rather than only the returned refusal. A refusal string is
// identical whether or not the guard fired for the right reason; committed
// state is not.

// crOperatorRequest is a request carrying the operator identity the trigger
// arm records as the audit actor.
func crOperatorRequest() *http.Request {
	return withRebaseOperator(httptest.NewRequest(http.MethodPost, "/v0/runs/x/rebase-branch", nil))
}

// crTriggerEntries returns every appended stage_conflict_resolution_triggered
// entry.
func crTriggerEntries(au *auditFake) []audit.ChainAppendParams {
	return auditEntries(au, CategoryStageConflictResolutionTriggered)
}

// crImplementStage returns the seeded implement stage.
func crImplementStage(t *testing.T, sd *rebaseSeed) *run.Stage {
	t.Helper()
	for _, st := range sd.rr.stagesByRunID[sd.runID] {
		if st.Type == run.StageTypeImplement {
			return st
		}
	}
	t.Fatal("seed has no implement stage")
	return nil
}

// seedConflictFailure appends a stage_conflict_resolution_failed entry for the
// stage carrying a named refusal reason, as the runner-reported recovery does.
func seedConflictFailure(sd *rebaseSeed, stageID uuid.UUID, pass int, reason string) {
	payload, _ := json.Marshal(map[string]any{"pass": pass, "reason": reason})
	sd.au.mu.Lock()
	defer sd.au.mu.Unlock()
	sd.au.appended = append(sd.au.appended, audit.ChainAppendParams{
		RunID:    sd.runID,
		StageID:  &stageID,
		Category: CategoryStageConflictResolutionFailed,
		Payload:  payload,
	})
}

// seedConflictTrigger appends a spent stage_conflict_resolution_triggered
// entry, so the budget reads as already consumed.
func seedConflictTrigger(sd *rebaseSeed, stageID uuid.UUID, pass int) {
	payload, _ := json.Marshal(conflictResolutionTrigger{
		Branch: rebaseBranchName, BaseRef: rebaseBaseRef,
		ExpectedHeadSHA: rebasePriorHeadSHA, Pass: pass,
		PriorState: string(run.StageStateSucceeded),
	})
	sd.au.mu.Lock()
	defer sd.au.mu.Unlock()
	sd.au.appended = append(sd.au.appended, audit.ChainAppendParams{
		RunID:    sd.runID,
		StageID:  &stageID,
		Category: CategoryStageConflictResolutionTriggered,
		Payload:  payload,
	})
}

// TestStartConflictResolutionPass_AuthorizesUnderTheCeiling is the happy path:
// the trigger is recorded with every anchor the runner needs, the implement
// stage is re-opened to pending, and the review gate is re-parked with it.
func TestStartConflictResolutionPass_AuthorizesUnderTheCeiling(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
	impl := crImplementStage(t, sd)

	start, refusal := sd.s.startConflictResolutionPass(crOperatorRequest(), sd.runID,
		rebaseBranchName, rebaseBaseRef, rebasePriorHeadSHA)
	if refusal != nil {
		t.Fatalf("refusal = %+v, want a started pass", refusal)
	}
	if start.StageID != impl.ID {
		t.Errorf("started stage = %s, want the implement stage %s", start.StageID, impl.ID)
	}
	if start.Pass != 1 {
		t.Errorf("pass = %d, want 1", start.Pass)
	}

	entries := crTriggerEntries(sd.au)
	if len(entries) != 1 {
		t.Fatalf("stage_conflict_resolution_triggered entries = %d, want 1", len(entries))
	}
	if entries[0].StageID == nil || *entries[0].StageID != impl.ID {
		t.Errorf("trigger entry is not bound to the implement stage: %+v", entries[0].StageID)
	}
	var payload conflictResolutionTrigger
	if err := json.Unmarshal(entries[0].Payload, &payload); err != nil {
		t.Fatalf("decode trigger payload: %v", err)
	}
	if !payload.populated() {
		t.Errorf("trigger payload is half-populated (the runner would merge nothing and refuse naming the wrong cause): %+v", payload)
	}
	if payload.Branch != rebaseBranchName || payload.BaseRef != rebaseBaseRef ||
		payload.ExpectedHeadSHA != rebasePriorHeadSHA || payload.Pass != 1 {
		t.Errorf("trigger payload anchors = %+v", payload)
	}
	// The restore anchors are what the failure recovery reads back, and they
	// must be complete BEFORE the mutation — the payload is appended first.
	if payload.PriorState != string(run.StageStateSucceeded) {
		t.Errorf("prior_state = %q, want succeeded", payload.PriorState)
	}
	if payload.ReparkedReviewStageID != sd.review.ID.String() {
		t.Errorf("reparked_review_stage_id = %q, want %s", payload.ReparkedReviewStageID, sd.review.ID)
	}

	if impl.State != run.StageStatePending {
		t.Errorf("implement stage state = %q, want pending (re-opened for the pass)", impl.State)
	}
	if sd.review.State != run.StageStatePending {
		t.Errorf("review stage state = %q, want pending (re-parked with the re-open)", sd.review.State)
	}
}

// TestStartConflictResolutionPass_NeverSpendsFixupBudget pins the STRUCTURAL
// budget separation the issue's second acceptance criterion claims: the pass
// writes its OWN category and the fix-up counter is unmoved.
func TestStartConflictResolutionPass_NeverSpendsFixupBudget(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})

	if _, refusal := sd.s.startConflictResolutionPass(crOperatorRequest(), sd.runID,
		rebaseBranchName, rebaseBaseRef, rebasePriorHeadSHA); refusal != nil {
		t.Fatalf("refusal = %+v, want a started pass", refusal)
	}
	if n := len(auditEntries(sd.au, CategoryStageFixupTriggered)); n != 0 {
		t.Errorf("stage_fixup_triggered entries = %d, want 0 — a conflict-resolution pass must never spend a fix-up slot", n)
	}
	if n := len(crTriggerEntries(sd.au)); n != 1 {
		t.Errorf("stage_conflict_resolution_triggered entries = %d, want 1", n)
	}
}

// TestStartConflictResolutionPass_BudgetSpentNamesTheFailedPass covers the
// ceiling arm and its wording: the refusal must name the FAILED pass, the
// runner's own refusal reason, and the resolve-push-vouch route.
func TestStartConflictResolutionPass_BudgetSpentNamesTheFailedPass(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
	impl := crImplementStage(t, sd)
	seedConflictTrigger(sd, impl.ID, 1)
	seedConflictFailure(sd, impl.ID, 1, "conflict_resolution_residual_marker")

	start, refusal := sd.s.startConflictResolutionPass(crOperatorRequest(), sd.runID,
		rebaseBranchName, rebaseBaseRef, rebasePriorHeadSHA)
	if start != nil {
		t.Fatalf("start = %+v, want a refusal at the ceiling", start)
	}
	if !refusal.BudgetSpent {
		t.Fatalf("refusal = %+v, want BudgetSpent", refusal)
	}
	if refusal.FailedPass != 1 {
		t.Errorf("failed pass = %d, want 1", refusal.FailedPass)
	}
	if refusal.FailedReason != "conflict_resolution_residual_marker" {
		t.Errorf("failed reason = %q, want the runner's named reason", refusal.FailedReason)
	}
	msg := refusal.message()
	for _, want := range []string{
		"conflict_resolution_residual_marker",
		"fishhawk_vouch_commit",
		"fishhawk_reset_run_branch",
		"Conflict-resolution pass 1 already ran and was REFUSED",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("budget-spent message missing %q:\n%s", want, msg)
		}
	}
	// No SECOND trigger may be appended, and the stage must stay at its gate.
	if n := len(crTriggerEntries(sd.au)); n != 1 {
		t.Errorf("trigger entries = %d, want 1 (the seeded one) — the ceiling must not append another", n)
	}
	if impl.State != run.StageStateSucceeded {
		t.Errorf("implement stage state = %q, want succeeded (untouched at the ceiling)", impl.State)
	}
}

// TestStartConflictResolutionPass_BudgetSpentWithoutADecodableFailure is the
// degrade: the ceiling still refuses, naming the count, when the failure entry
// is absent or its payload is undecodable. A missing explanation must never
// suppress the refusal.
func TestStartConflictResolutionPass_BudgetSpentWithoutADecodableFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(sd *rebaseSeed, stageID uuid.UUID)
	}{
		{"no failure entry at all", func(*rebaseSeed, uuid.UUID) {}},
		{"undecodable failure payload", func(sd *rebaseSeed, stageID uuid.UUID) {
			sd.au.mu.Lock()
			defer sd.au.mu.Unlock()
			sd.au.appended = append(sd.au.appended, audit.ChainAppendParams{
				RunID: sd.runID, StageID: &stageID,
				Category: CategoryStageConflictResolutionFailed,
				Payload:  []byte("{not json"),
			})
		}},
		{"unreadable failure category", func(sd *rebaseSeed, _ uuid.UUID) {
			sd.au.listByCategoryErrCategory = CategoryStageConflictResolutionFailed
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
			impl := crImplementStage(t, sd)
			seedConflictTrigger(sd, impl.ID, 1)
			tc.seed(sd, impl.ID)

			start, refusal := sd.s.startConflictResolutionPass(crOperatorRequest(), sd.runID,
				rebaseBranchName, rebaseBaseRef, rebasePriorHeadSHA)
			if start != nil || refusal == nil || !refusal.BudgetSpent {
				t.Fatalf("start=%+v refusal=%+v, want a budget-spent refusal", start, refusal)
			}
			if !strings.Contains(refusal.message(), "1 of 1 passes used") {
				t.Errorf("message must still name the spent count:\n%s", refusal.message())
			}
			if !strings.Contains(refusal.message(), "fishhawk_vouch_commit") {
				t.Errorf("message must still name the fallback route:\n%s", refusal.message())
			}
		})
	}
}

// TestStartConflictResolutionPass_FailClosedBranches drives EVERY guard that
// refuses to start a pass, and asserts the OBSERVABLE consequence of each: no
// trigger appended and the implement stage left at its gate. Error identity
// alone would not discriminate — most of these produce the same 422.
func TestStartConflictResolutionPass_FailClosedBranches(t *testing.T) {
	for _, tc := range []struct {
		name    string
		opts    rebaseOpts
		mutate  func(sd *rebaseSeed)
		branch  string
		baseRef string
		headSHA string
		// wantTriggerAppended is true ONLY for the re-open-failure branch,
		// where the trigger is deliberately recorded FIRST and the budget slot
		// is therefore spent.
		wantTriggerAppended bool
		wantReason          string
	}{
		{
			name:   "no audit repository wired",
			mutate: func(sd *rebaseSeed) { sd.s.cfg.AuditRepo = nil },
			branch: rebaseBranchName, baseRef: rebaseBaseRef, headSHA: rebasePriorHeadSHA,
			wantReason: "no run/audit repository is wired",
		},
		{
			name:   "empty branch anchor",
			branch: "", baseRef: rebaseBaseRef, headSHA: rebasePriorHeadSHA,
			wantReason: "could not be resolved",
		},
		{
			name:   "empty base ref anchor",
			branch: rebaseBranchName, baseRef: "", headSHA: rebasePriorHeadSHA,
			wantReason: "could not be resolved",
		},
		{
			name:   "empty head sha anchor",
			branch: rebaseBranchName, baseRef: rebaseBaseRef, headSHA: "",
			wantReason: "could not be resolved",
		},
		{
			name: "no implement stage",
			mutate: func(sd *rebaseSeed) {
				sd.rr.stagesByRunID[sd.runID] = []*run.Stage{sd.review}
			},
			branch: rebaseBranchName, baseRef: rebaseBaseRef, headSHA: rebasePriorHeadSHA,
			wantReason: "no implement stage",
		},
		{
			name: "budget count unreadable",
			mutate: func(sd *rebaseSeed) {
				sd.au.listByCategoryErrCategory = CategoryStageConflictResolutionTriggered
			},
			branch: rebaseBranchName, baseRef: rebaseBaseRef, headSHA: rebasePriorHeadSHA,
			wantReason: "could not be read from the audit chain",
		},
		{
			name: "implement stage not at its review gate",
			mutate: func(sd *rebaseSeed) {
				crImplementStageOf(sd).State = run.StageStateRunning
			},
			branch: rebaseBranchName, baseRef: rebaseBaseRef, headSHA: rebasePriorHeadSHA,
			wantReason: "only a stage parked at its review gate",
		},
		{
			name: "trigger append fails",
			mutate: func(sd *rebaseSeed) {
				sd.au.appendErrCategory = CategoryStageConflictResolutionTriggered
			},
			branch: rebaseBranchName, baseRef: rebaseBaseRef, headSHA: rebasePriorHeadSHA,
			wantReason: "recording the conflict-resolution trigger failed",
		},
		{
			name: "stage re-open fails",
			mutate: func(sd *rebaseSeed) {
				sd.rr.transitionStageErr = errors.New("injected transition failure")
			},
			branch: rebaseBranchName, baseRef: rebaseBaseRef, headSHA: rebasePriorHeadSHA,
			wantTriggerAppended: true,
			wantReason:          "budget is now SPENT",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd := seedRebaseRun(t, cleanRebaseStub(), tc.opts)
			impl := crImplementStage(t, sd)
			priorState := impl.State
			if tc.mutate != nil {
				tc.mutate(sd)
			}

			start, refusal := sd.s.startConflictResolutionPass(crOperatorRequest(), sd.runID,
				tc.branch, tc.baseRef, tc.headSHA)
			if start != nil {
				t.Fatalf("start = %+v, want a refusal", start)
			}
			if refusal == nil {
				t.Fatal("refusal = nil, want a refusal")
			}
			if refusal.BudgetSpent {
				t.Errorf("refusal = %+v, want a cannot-start refusal, not the ceiling arm", refusal)
			}
			if !strings.Contains(refusal.Reason, tc.wantReason) {
				t.Errorf("reason = %q, want it to contain %q", refusal.Reason, tc.wantReason)
			}
			// The shipped 422 sentence must always name the fallback route.
			if !strings.Contains(refusal.message(), "fishhawk_vouch_commit") {
				t.Errorf("message must name the fallback route:\n%s", refusal.message())
			}

			// COMMITTED STATE, not error identity.
			if sd.s.cfg.AuditRepo != nil {
				got := len(crTriggerEntries(sd.au))
				want := 0
				if tc.wantTriggerAppended {
					want = 1
				}
				if got != want {
					t.Errorf("stage_conflict_resolution_triggered entries = %d, want %d", got, want)
				}
			}
			if tc.name != "implement stage not at its review gate" && impl.State != priorState {
				t.Errorf("implement stage state = %q, want %q (no pass was started)", impl.State, priorState)
			}
		})
	}
}

// crImplementStageOf is the non-fatal sibling of crImplementStage, usable
// inside a table's mutate closure (which has no *testing.T in scope).
func crImplementStageOf(sd *rebaseSeed) *run.Stage {
	for _, st := range sd.rr.stagesByRunID[sd.runID] {
		if st.Type == run.StageTypeImplement {
			return st
		}
	}
	return nil
}

// TestConflictResolutionRefusalDetails pins the structured half of the 422 so
// a machine reader does not have to parse the sentence.
func TestConflictResolutionRefusalDetails(t *testing.T) {
	spent := &conflictResolutionRefusal{
		BudgetSpent: true, FailedPass: 1, FailedReason: "conflict_resolution_no_conflict",
		Reason: "spent",
	}
	d := spent.details(rebaseBranchName, rebaseBaseRef, errors.New("Merge conflict"))
	if d["conflict_resolution_budget_spent"] != true {
		t.Errorf("details = %+v, want conflict_resolution_budget_spent", d)
	}
	if d["conflict_resolution_failure_reason"] != "conflict_resolution_no_conflict" {
		t.Errorf("details = %+v, want the named failure reason", d)
	}
	if d["error"] != "Merge conflict" {
		t.Errorf("details = %+v, want the underlying merge error", d)
	}

	cannot := &conflictResolutionRefusal{Reason: "no implement stage"}
	d2 := cannot.details(rebaseBranchName, rebaseBaseRef, nil)
	if _, ok := d2["conflict_resolution_budget_spent"]; ok {
		t.Errorf("details = %+v, want NO budget-spent key on a cannot-start refusal", d2)
	}
	if d2["conflict_resolution_available"] != false {
		t.Errorf("details = %+v, want conflict_resolution_available=false", d2)
	}
	if _, ok := d2["error"]; ok {
		t.Errorf("details = %+v, want no error key when no merge error was supplied", d2)
	}
}

// TestCountStageEntries covers the read-failure discrimination: an unreadable
// category must report NOT-OK rather than a count of zero, because zero is
// read by the caller as budget headroom.
func TestCountStageEntries(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
	impl := crImplementStage(t, sd)
	other := uuid.New()

	if n, ok := sd.s.countStageEntries(t.Context(), sd.runID, impl.ID, CategoryStageConflictResolutionTriggered); n != 0 || !ok {
		t.Errorf("count on an empty chain = (%d, %v), want (0, true)", n, ok)
	}
	seedConflictTrigger(sd, impl.ID, 1)
	seedConflictTrigger(sd, other, 1)
	if n, ok := sd.s.countStageEntries(t.Context(), sd.runID, impl.ID, CategoryStageConflictResolutionTriggered); n != 1 || !ok {
		t.Errorf("count = (%d, %v), want (1, true) — an entry bound to ANOTHER stage must not count", n, ok)
	}

	sd.au.listByCategoryErrCategory = CategoryStageConflictResolutionTriggered
	if n, ok := sd.s.countStageEntries(t.Context(), sd.runID, impl.ID, CategoryStageConflictResolutionTriggered); ok {
		t.Errorf("count on an unreadable chain = (%d, %v), want ok=false — a read failure must never read as zero", n, ok)
	}

	sd.s.cfg.AuditRepo = nil
	if _, ok := sd.s.countStageEntries(t.Context(), sd.runID, impl.ID, CategoryStageConflictResolutionTriggered); ok {
		t.Error("count with no audit repo, want ok=false")
	}
}

// TestOpenReviewStageFor covers the review-anchor selection the trigger payload
// records — including the two nil arms, which are what make the recovery
// restore nothing rather than the wrong thing.
func TestOpenReviewStageFor(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
	if got := sd.s.openReviewStageFor(t.Context(), sd.runID); got == nil || got.ID != sd.review.ID {
		t.Fatalf("openReviewStageFor = %+v, want the awaiting_approval review stage", got)
	}
	sd.review.State = run.StageStateSucceeded
	if got := sd.s.openReviewStageFor(t.Context(), sd.runID); got != nil {
		t.Errorf("openReviewStageFor = %+v, want nil once the review gate has closed", got)
	}
	sd.rr.stagesByRunID = nil
	if got := sd.s.openReviewStageFor(t.Context(), sd.runID); got != nil {
		t.Errorf("openReviewStageFor = %+v, want nil when the stage list is unreadable", got)
	}
}

// --- #4304: the re-open preconditions are PRE-FLIGHTED before the budget ---

// crSeedAcceptance turns the shared seed into the feature_change shape: the
// review stage is pending behind an acceptance stage (Sequence 3) in the given
// state, which is what holds the review gate closed.
func crSeedAcceptance(sd *rebaseSeed, state run.StageState) *run.Stage {
	sd.review.State = run.StageStatePending
	acc := &run.Stage{ID: uuid.New(), RunID: sd.runID, Type: run.StageTypeAcceptance,
		State: state, Sequence: 3}
	sd.rr.getStages[acc.ID] = acc
	sd.rr.stagesByRunID[sd.runID] = append(sd.rr.stagesByRunID[sd.runID], acc)
	return acc
}

// crFixupSentinelText lists the run fix-up sentinel prefixes and the fix-up
// budget wording a conflict-arm reason must never carry (C6): the reason is
// about the CONFLICT-RESOLUTION budget, and a spent fix-up budget is a
// different, untouched counter.
var crFixupSentinelText = []string{
	"fixup not applicable", "fixup ceiling reached", "fixup budget exhausted", "fix-up budget",
}

const crReinvoke = "re-invoke fishhawk_rebase_run_branch"

// TestStartConflictResolutionPass_PreflightRefusalSpendsNoBudget is the #4304
// defect: a re-open precondition the arm never pre-checked used to be found by
// run.FixupStage AFTER the trigger was appended, spending the ceiling-of-one
// slot on a pass that could never run. Each row asserts COMMITTED STATE — no
// trigger entry, no TransitionStage call, stage states unchanged — and the
// named precondition and remedy in the reason.
func TestStartConflictResolutionPass_PreflightRefusalSpendsNoBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		opts   rebaseOpts
		mutate func(sd *rebaseSeed)
		want   []string
		reject []string
		// reinvokeOnce: the row is clearable, so the reason names the re-invoke
		// instruction EXACTLY once (C1); every other row must not name it.
		reinvokeOnce bool
		// readFailure: the row is the unknown-applicability arm, not a sentinel.
		readFailure bool
	}{
		{
			name:   "acceptance awaiting_host_dispatch",
			mutate: func(sd *rebaseSeed) { crSeedAcceptance(sd, run.StageStateAwaitingHostDispatch) },
			want:   []string{"must settle first", "fishhawk_dispatch_stage", "awaiting_host_dispatch"},
			reject: []string{"route the fix-up"}, reinvokeOnce: true,
		},
		{
			name:   "acceptance pending",
			mutate: func(sd *rebaseSeed) { crSeedAcceptance(sd, run.StageStatePending) },
			want:   []string{"must settle first", "fishhawk_dispatch_stage"},
			reject: []string{"route the fix-up"}, reinvokeOnce: true,
		},
		{
			name:   "acceptance dispatched (in flight)",
			mutate: func(sd *rebaseSeed) { crSeedAcceptance(sd, run.StageStateDispatched) },
			want:   []string{"already in flight", "Wait for acceptance to settle"},
			reject: []string{"route the fix-up", "fishhawk_dispatch_stage"}, reinvokeOnce: true,
		},
		{
			name: "terminal run",
			mutate: func(sd *rebaseSeed) {
				sd.rr.getRuns[sd.runID].State = run.StateCancelled
			},
			want: []string{"already terminal", "cancelled"},
		},
		{
			name:   "review already resolved",
			mutate: func(sd *rebaseSeed) { sd.review.State = run.StageStateSucceeded },
			want:   []string{"already resolved"},
		},
		{
			name:   "review pending without acceptance",
			mutate: func(sd *rebaseSeed) { sd.review.State = run.StageStatePending },
			want:   []string{"review gate has not opened yet", "review stage state \"pending\""},
		},
		{
			name: "no review stage",
			opts: rebaseOpts{noReviewStage: true},
			want: []string{"no review stage at all"},
		},
		{
			name: "pre-flight read failure",
			mutate: func(sd *rebaseSeed) {
				sd.rr.runErr = errors.New("injected pre-flight run read failure")
			},
			want:        []string{"could not be confirmed", "injected pre-flight run read failure", "BEFORE spending"},
			readFailure: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd := seedRebaseRun(t, cleanRebaseStub(), tc.opts)
			impl := crImplementStage(t, sd)
			if tc.mutate != nil {
				tc.mutate(sd)
			}
			implBefore := impl.State
			var reviewBefore run.StageState
			if sd.review != nil {
				reviewBefore = sd.review.State
			}

			start, refusal := sd.s.startConflictResolutionPass(crOperatorRequest(), sd.runID,
				rebaseBranchName, rebaseBaseRef, rebasePriorHeadSHA)
			if start != nil {
				t.Fatalf("start = %+v, want a pre-flight refusal", start)
			}
			if refusal == nil {
				t.Fatal("refusal = nil, want a pre-flight refusal")
			}
			if refusal.BudgetSpent {
				t.Errorf("refusal = %+v, want a cannot-start refusal, not the ceiling arm", refusal)
			}
			if !refusal.Preflight {
				t.Errorf("refusal = %+v, want it marked as a pre-flight refusal", refusal)
			}

			// COMMITTED STATE: nothing written, nothing moved.
			if n := len(crTriggerEntries(sd.au)); n != 0 {
				t.Errorf("stage_conflict_resolution_triggered entries = %d, want 0 — the pre-flight must refuse BEFORE the budget is spent", n)
			}
			if n := len(sd.rr.transitionStageCalls); n != 0 {
				t.Errorf("TransitionStage calls = %d, want 0: %+v", n, sd.rr.transitionStageCalls)
			}
			if impl.State != implBefore {
				t.Errorf("implement stage state = %q, want %q (unchanged)", impl.State, implBefore)
			}
			if sd.review != nil && sd.review.State != reviewBefore {
				t.Errorf("review stage state = %q, want %q (unchanged)", sd.review.State, reviewBefore)
			}

			// The reason names the precondition and its remedy.
			reason := refusal.Reason
			if !strings.Contains(reason, "NO conflict-resolution budget was spent") {
				t.Errorf("reason must state that no budget was spent:\n%s", reason)
			}
			for _, w := range tc.want {
				if !strings.Contains(reason, w) {
					t.Errorf("reason missing %q:\n%s", w, reason)
				}
			}
			for _, r := range tc.reject {
				if strings.Contains(reason, r) {
					t.Errorf("reason must not contain %q:\n%s", r, reason)
				}
			}
			// C6: never read as a fix-up refusal or a spent fix-up budget.
			for _, r := range crFixupSentinelText {
				if strings.Contains(reason, r) {
					t.Errorf("reason carries fix-up wording %q (C6):\n%s", r, reason)
				}
			}
			// C1: the re-invoke instruction appears ONLY on the clearable rows,
			// and exactly once there.
			n := strings.Count(reason, crReinvoke)
			if tc.reinvokeOnce && n != 1 {
				t.Errorf("clearable row names %q %d times, want exactly 1:\n%s", crReinvoke, n, reason)
			}
			if !tc.reinvokeOnce && n != 0 {
				t.Errorf("non-clearable row names %q %d times, want 0 (a remedy the operator cannot take):\n%s", crReinvoke, n, reason)
			}

			// C2: the shipped 422 still names the vouch route, as the FALLBACK.
			msg := refusal.message()
			if !strings.Contains(msg, "fishhawk_vouch_commit") {
				t.Errorf("message must still name the fallback route:\n%s", msg)
			}
			if !strings.Contains(msg, "If the precondition cannot be cleared: Resolve the conflict") {
				t.Errorf("message must word the vouch route as the FALLBACK (C2):\n%s", msg)
			}
			if tc.readFailure && strings.Contains(reason, "NO conflict-resolution budget was spent;") {
				t.Errorf("a read failure must not be framed as an unmet precondition:\n%s", reason)
			}
		})
	}
}

// TestConflictResolutionPreflightRefusal_BoundSentinels covers the bound arm
// of the mapper, which the arm itself cannot reach (its budget-spent check uses
// the same count): it must still say no budget was spent and must not carry
// the fix-up wording of the embedded error (C6).
func TestConflictResolutionPreflightRefusal_BoundSentinels(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("%w: 1 of 1 fix-up passes already used (hard ceiling; 0 delivered-nothing refund credit)", run.ErrFixupCeilingReached),
		fmt.Errorf("%w: 1 of 1 fix-up passes already used", run.ErrFixupBudgetExhausted),
	} {
		ref := conflictResolutionPreflightRefusal(err)
		if ref.BudgetSpent || !ref.Preflight {
			t.Errorf("refusal = %+v, want a pre-flight cannot-start refusal", ref)
		}
		if !strings.Contains(ref.Reason, "NO conflict-resolution budget was spent") {
			t.Errorf("reason must state that no budget was spent:\n%s", ref.Reason)
		}
		for _, r := range append([]string{"fix-up passes"}, crFixupSentinelText...) {
			if strings.Contains(ref.Reason, r) {
				t.Errorf("reason carries fix-up wording %q (C6):\n%s", r, ref.Reason)
			}
		}
	}
}

// TestStripFixupSentinel pins the C6 prefix strip for each sentinel, and that
// an error with no sentinel prefix passes through unchanged.
func TestStripFixupSentinel(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: run x is already terminal", run.ErrFixupNotApplicable), "run x is already terminal"},
		{fmt.Errorf("%w: 1 of 1 used", run.ErrFixupCeilingReached), "1 of 1 used"},
		{fmt.Errorf("%w: 1 of 1 used", run.ErrFixupBudgetExhausted), "1 of 1 used"},
		{errors.New("FixupStage: get run: boom"), "FixupStage: get run: boom"},
	} {
		if got := stripFixupSentinel(tc.err); got != tc.want {
			t.Errorf("stripFixupSentinel(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// TestStartConflictResolutionPass_ReinvokeAfterAcceptanceSettlesStartsPassOne
// proves the refusal really did spend nothing: once acceptance settles and the
// review gate parks, the SAME stage gets pass 1 — not the budget-spent 422.
func TestStartConflictResolutionPass_ReinvokeAfterAcceptanceSettlesStartsPassOne(t *testing.T) {
	sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
	impl := crImplementStage(t, sd)
	acc := crSeedAcceptance(sd, run.StageStateAwaitingHostDispatch)

	if start, refusal := sd.s.startConflictResolutionPass(crOperatorRequest(), sd.runID,
		rebaseBranchName, rebaseBaseRef, rebasePriorHeadSHA); start != nil || refusal == nil {
		t.Fatalf("first call: start=%+v refusal=%+v, want a pre-flight refusal", start, refusal)
	}

	acc.State = run.StageStateSucceeded
	sd.review.State = run.StageStateAwaitingApproval

	start, refusal := sd.s.startConflictResolutionPass(crOperatorRequest(), sd.runID,
		rebaseBranchName, rebaseBaseRef, rebasePriorHeadSHA)
	if refusal != nil {
		t.Fatalf("re-invoke refusal = %+v, want pass 1", refusal)
	}
	if start.Pass != 1 {
		t.Errorf("pass = %d, want 1 — the refused call must not have spent the slot", start.Pass)
	}
	if n := len(crTriggerEntries(sd.au)); n != 1 {
		t.Errorf("stage_conflict_resolution_triggered entries = %d, want 1", n)
	}
	if impl.State != run.StageStatePending {
		t.Errorf("implement stage state = %q, want pending", impl.State)
	}
}

// crPostTriggerRunRepo wraps the seed's run repository and runs afterTrigger
// once, on the first GetRun after the conflict-resolution trigger has been
// appended. That is FixupStage's own admission read — the window between the
// pre-flight and the re-open the #4304 residual contract names.
type crPostTriggerRunRepo struct {
	*promptRunRepo
	au           *auditFake
	afterTrigger func()
	fired        bool
}

func (r *crPostTriggerRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	if !r.fired && len(crTriggerEntries(r.au)) > 0 {
		r.fired = true
		r.afterTrigger()
	}
	return r.promptRunRepo.GetRun(ctx, id)
}

// TestStartConflictResolutionPass_ResidualsAfterPreflightSpendTheSlot pins the
// HONEST residual (C3, C4): the pre-flight passes, then FixupStage's own
// admission — which re-reads AFTER the append — refuses or fails. The slot is
// spent loudly: exactly ONE trigger entry and the "budget is now SPENT" reason.
func TestStartConflictResolutionPass_ResidualsAfterPreflightSpendTheSlot(t *testing.T) {
	for _, tc := range []struct {
		name string
		flip func(sd *rebaseSeed, acc *run.Stage)
		want string
	}{
		{
			// C3: a concurrent writer re-opens acceptance between the
			// pre-flight and the re-open.
			name: "acceptance flips to awaiting_host_dispatch",
			flip: func(sd *rebaseSeed, acc *run.Stage) {
				acc.State = run.StageStateAwaitingHostDispatch
				sd.review.State = run.StageStatePending
			},
			want: "must settle first",
		},
		{
			// C4: a repository READ fails inside FixupStage's own admission.
			name: "FixupStage's own run read fails",
			flip: func(sd *rebaseSeed, _ *run.Stage) {
				sd.rr.runErr = errors.New("injected second-admission read failure")
			},
			want: "injected second-admission read failure",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sd := seedRebaseRun(t, cleanRebaseStub(), rebaseOpts{})
			impl := crImplementStage(t, sd)
			acc := crSeedAcceptance(sd, run.StageStateSucceeded)
			sd.review.State = run.StageStateAwaitingApproval
			sd.s.cfg.RunRepo = &crPostTriggerRunRepo{promptRunRepo: sd.rr, au: sd.au,
				afterTrigger: func() { tc.flip(sd, acc) }}

			start, refusal := sd.s.startConflictResolutionPass(crOperatorRequest(), sd.runID,
				rebaseBranchName, rebaseBaseRef, rebasePriorHeadSHA)
			if start != nil || refusal == nil {
				t.Fatalf("start=%+v refusal=%+v, want the spent-slot refusal", start, refusal)
			}
			if refusal.Preflight {
				t.Errorf("refusal = %+v, want the post-append residual, not a pre-flight refusal", refusal)
			}
			if !strings.Contains(refusal.Reason, "budget is now SPENT") {
				t.Errorf("reason must state the slot is spent:\n%s", refusal.Reason)
			}
			if !strings.Contains(refusal.Reason, tc.want) {
				t.Errorf("reason missing %q:\n%s", tc.want, refusal.Reason)
			}
			for _, r := range crFixupSentinelText[:3] {
				if strings.Contains(refusal.Reason, r) {
					t.Errorf("reason carries the fix-up sentinel %q (C6):\n%s", r, refusal.Reason)
				}
			}
			if n := len(crTriggerEntries(sd.au)); n != 1 {
				t.Errorf("stage_conflict_resolution_triggered entries = %d, want exactly 1 (the residual spends the slot)", n)
			}
			if impl.State != run.StageStateSucceeded {
				t.Errorf("implement stage state = %q, want succeeded (never re-opened)", impl.State)
			}
		})
	}
}
