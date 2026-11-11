package run_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func infraStage(typ run.StageType, seq int, state run.StageState, cat *run.FailureCategory) *run.Stage {
	return &run.Stage{ID: uuid.New(), Type: typ, Sequence: seq, State: state, FailureCategory: cat}
}

func catPtr(c run.FailureCategory) *run.FailureCategory { return &c }

// infraFixture is the feature_change shape: plan, implement, acceptance
// (sequence 3, failed with the given category), review (sequence 4).
func infraFixture(accCat *run.FailureCategory, reviewState run.StageState) ([]*run.Stage, *run.Stage) {
	acc := infraStage(run.StageTypeAcceptance, 3, run.StageStateFailed, accCat)
	stages := []*run.Stage{
		infraStage(run.StageTypePlan, 1, run.StageStateSucceeded, nil),
		infraStage(run.StageTypeImplement, 2, run.StageStateSucceeded, nil),
		acc,
		infraStage(run.StageTypeReview, 4, reviewState, nil),
	}
	return stages, acc
}

// TestClassifyInfraFailurePark pins every guard of the #4356 predicate. Each
// non-Retryable row differs from a Retryable row in exactly the one field its
// guard reads, so the guard is the only thing in the path.
func TestClassifyInfraFailurePark(t *testing.T) {
	t.Parallel()

	c := catPtr(run.FailureC)

	t.Run("acceptance C, review pending, no prior parks -> Retryable", func(t *testing.T) {
		stages, acc := infraFixture(c, run.StageStatePending)
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{}); got != run.InfraParkRetryable {
			t.Fatalf("got %v, want Retryable", got)
		}
	})
	t.Run("two prior parks -> Retryable", func(t *testing.T) {
		stages, acc := infraFixture(c, run.StageStatePending)
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{PriorParks: 2}); got != run.InfraParkRetryable {
			t.Fatalf("got %v, want Retryable", got)
		}
	})
	t.Run("budget: three prior parks -> BudgetExhausted", func(t *testing.T) {
		stages, acc := infraFixture(c, run.StageStatePending)
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{PriorParks: run.InfraFailureParkRetryBudget}); got != run.InfraParkBudgetExhausted {
			t.Fatalf("got %v, want BudgetExhausted", got)
		}
	})
	t.Run("C1: three prior category-A retries (SelfRetryCount 3), no parks -> Retryable", func(t *testing.T) {
		stages, acc := infraFixture(c, run.StageStatePending)
		acc.SelfRetryCount = 3
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{}); got != run.InfraParkRetryable {
			t.Fatalf("got %v, want Retryable (self_retry_count must not count against the park budget)", got)
		}
	})
	// The allow-list stays closed: only acceptance and implement park.
	for _, typ := range []run.StageType{run.StageTypePlan, run.StageTypeReview, run.StageTypeDeploy} {
		t.Run("type guard: "+string(typ)+" C -> NotApplicable", func(t *testing.T) {
			stages, acc := infraFixture(c, run.StageStatePending)
			acc.Type = typ
			if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{}); got != run.InfraParkNotApplicable {
				t.Fatalf("got %v, want NotApplicable", got)
			}
		})
	}
	t.Run("exclusions are implement-only: acceptance C with DecomposedParent and ReopenedPass -> Retryable", func(t *testing.T) {
		stages, acc := infraFixture(c, run.StageStatePending)
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{DecomposedParent: true, ReopenedPass: true, StagePushed: true}); got != run.InfraParkRetryable {
			t.Fatalf("got %v, want Retryable (#4356 acceptance behaviour unchanged)", got)
		}
	})
	for _, cat := range []run.FailureCategory{run.FailureA, run.FailureB, run.FailureD} {
		t.Run("category guard: acceptance "+string(cat)+" -> NotApplicable", func(t *testing.T) {
			stages, acc := infraFixture(catPtr(cat), run.StageStatePending)
			if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{}); got != run.InfraParkNotApplicable {
				t.Fatalf("got %v, want NotApplicable", got)
			}
		})
	}
	t.Run("category guard: nil category -> NotApplicable", func(t *testing.T) {
		stages, acc := infraFixture(nil, run.StageStatePending)
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{}); got != run.InfraParkNotApplicable {
			t.Fatalf("got %v, want NotApplicable", got)
		}
	})
	t.Run("C2 carve-out: review resolved succeeded by the merge -> NotApplicable", func(t *testing.T) {
		stages, acc := infraFixture(c, run.StageStateSucceeded)
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{}); got != run.InfraParkNotApplicable {
			t.Fatalf("got %v, want NotApplicable", got)
		}
	})
	t.Run("C2 carve-out: PR merge observed -> NotApplicable", func(t *testing.T) {
		stages, acc := infraFixture(c, run.StageStatePending)
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{MergeObserved: true}); got != run.InfraParkNotApplicable {
			t.Fatalf("got %v, want NotApplicable", got)
		}
	})
	t.Run("C2: final acceptance with no other open stage and no merge -> Retryable", func(t *testing.T) {
		// Every other stage terminal (review cancelled is not a merge
		// resolution), no pr_merged: the old other-stage-open carve-out would
		// have failed the run; C2 parks it.
		stages, acc := infraFixture(c, run.StageStateCancelled)
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{}); got != run.InfraParkRetryable {
			t.Fatalf("got %v, want Retryable", got)
		}
	})
	t.Run("C2: post-deploy final acceptance after the merge -> Retryable", func(t *testing.T) {
		acc := infraStage(run.StageTypeAcceptance, 5, run.StageStateFailed, c)
		stages := []*run.Stage{
			infraStage(run.StageTypePlan, 1, run.StageStateSucceeded, nil),
			infraStage(run.StageTypeImplement, 2, run.StageStateSucceeded, nil),
			infraStage(run.StageTypeReview, 3, run.StageStateSucceeded, nil),
			infraStage(run.StageTypeDeploy, 4, run.StageStateSucceeded, nil),
			acc,
		}
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{MergeObserved: true}); got != run.InfraParkRetryable {
			t.Fatalf("got %v, want Retryable", got)
		}
	})
	t.Run("non-failed stage and nil -> NotApplicable", func(t *testing.T) {
		stages, acc := infraFixture(c, run.StageStatePending)
		acc.State = run.StageStateSucceeded
		if got := run.ClassifyInfraFailurePark(stages, acc, run.InfraParkHistory{}); got != run.InfraParkNotApplicable {
			t.Fatalf("succeeded: got %v, want NotApplicable", got)
		}
		if got := run.ClassifyInfraFailurePark(stages, nil, run.InfraParkHistory{}); got != run.InfraParkNotApplicable {
			t.Fatalf("nil: got %v, want NotApplicable", got)
		}
	})
}

// implFixture is the feature_change shape with the IMPLEMENT stage failed
// (sequence 2, the given category) and acceptance + review pending.
func implFixture(implCat *run.FailureCategory) ([]*run.Stage, *run.Stage) {
	impl := infraStage(run.StageTypeImplement, 2, run.StageStateFailed, implCat)
	stages := []*run.Stage{
		infraStage(run.StageTypePlan, 1, run.StageStateSucceeded, nil),
		impl,
		infraStage(run.StageTypeAcceptance, 3, run.StageStatePending, nil),
		infraStage(run.StageTypeReview, 4, run.StageStatePending, nil),
	}
	return stages, impl
}

// TestClassifyInfraFailurePark_Implement pins the #4426 implement arm. Each
// non-Retryable row differs from the first row in exactly the one field its
// guard reads.
func TestClassifyInfraFailurePark_Implement(t *testing.T) {
	t.Parallel()

	c := catPtr(run.FailureC)
	cases := []struct {
		name string
		cat  *run.FailureCategory
		h    run.InfraParkHistory
		want run.InfraParkDecision
	}{
		{"implement C -> Retryable", c, run.InfraParkHistory{}, run.InfraParkRetryable},
		{"implement C, two prior parks -> Retryable", c, run.InfraParkHistory{PriorParks: 2}, run.InfraParkRetryable},
		{"implement C, DecomposedParent -> NotApplicable", c, run.InfraParkHistory{DecomposedParent: true}, run.InfraParkNotApplicable},
		{"implement C, ReopenedPass -> NotApplicable", c, run.InfraParkHistory{ReopenedPass: true}, run.InfraParkNotApplicable},
		{"implement A -> NotApplicable", catPtr(run.FailureA), run.InfraParkHistory{}, run.InfraParkNotApplicable},
		{"implement B -> NotApplicable", catPtr(run.FailureB), run.InfraParkHistory{}, run.InfraParkNotApplicable},
		{"implement D -> NotApplicable", catPtr(run.FailureD), run.InfraParkHistory{}, run.InfraParkNotApplicable},
		{"implement C, MergeObserved (C2) -> NotApplicable", c, run.InfraParkHistory{MergeObserved: true}, run.InfraParkNotApplicable},
		{"implement C, three prior parks -> BudgetExhausted", c, run.InfraParkHistory{PriorParks: run.InfraFailureParkRetryBudget}, run.InfraParkBudgetExhausted},
		// #4816: an operator-reaped live merge-candidate verify pass overrides
		// ReopenedPass and StagePushed, but never DecomposedParent.
		{"implement C, ReopenedPass + StagePushed + ReapedLiveMergeCandidatePass -> Retryable", c,
			run.InfraParkHistory{ReopenedPass: true, StagePushed: true, ReapedLiveMergeCandidatePass: true}, run.InfraParkRetryable},
		{"implement C, DecomposedParent + ReopenedPass + ReapedLiveMergeCandidatePass -> NotApplicable", c,
			run.InfraParkHistory{DecomposedParent: true, ReopenedPass: true, ReapedLiveMergeCandidatePass: true}, run.InfraParkNotApplicable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stages, impl := implFixture(tc.cat)
			if got := run.ClassifyInfraFailurePark(stages, impl, tc.h); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func parkEntry(t *testing.T, seq int64, stageID uuid.UUID, failedAt string, ordinal int) *audit.Entry {
	t.Helper()
	b, err := json.Marshal(map[string]any{"failed_at": failedAt, "retry_ordinal": ordinal})
	if err != nil {
		t.Fatal(err)
	}
	return &audit.Entry{Sequence: seq, StageID: &stageID, Category: run.CategoryStageInfraParked, Payload: b}
}

// TestReadInfraParkHistory pins the C1 budget count: only park rows of THIS
// stage for EARLIER episodes since its last recorded outcome count.
func TestReadInfraParkHistory(t *testing.T) {
	t.Parallel()

	ended := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	acc := &run.Stage{ID: uuid.New(), Type: run.StageTypeAcceptance, State: run.StageStateFailed, EndedAt: &ended, SelfRetryCount: 2}
	curAt, curOrd := run.InfraParkEpisode(acc)
	if curAt != ended.Format(time.RFC3339Nano) || curOrd != 2 {
		t.Fatalf("episode = (%q, %d)", curAt, curOrd)
	}
	other := uuid.New()

	t.Run("earlier episodes count, current episode does not", func(t *testing.T) {
		h := run.ReadInfraParkHistory([]*audit.Entry{
			parkEntry(t, 1, acc.ID, "2026-10-15T10:00:00Z", 0),
			parkEntry(t, 2, acc.ID, "2026-10-15T11:00:00Z", 1),
			parkEntry(t, 3, acc.ID, curAt, curOrd),
			parkEntry(t, 4, other, "2026-10-15T11:30:00Z", 0),
		}, acc)
		if h.PriorParks != 2 || !h.CurrentEpisodeParked || h.MergeObserved {
			t.Fatalf("history = %+v, want PriorParks 2, CurrentEpisodeParked, no merge", h)
		}
	})
	t.Run("a recorded acceptance outcome for the stage resets the count", func(t *testing.T) {
		stageID := acc.ID
		h := run.ReadInfraParkHistory([]*audit.Entry{
			parkEntry(t, 1, acc.ID, "2026-10-15T09:00:00Z", 0),
			parkEntry(t, 2, acc.ID, "2026-10-15T10:00:00Z", 0),
			{Sequence: 3, StageID: &stageID, Category: "acceptance_outcome_recorded"},
			parkEntry(t, 4, acc.ID, "2026-10-15T11:00:00Z", 1),
		}, acc)
		if h.PriorParks != 1 {
			t.Fatalf("PriorParks = %d, want 1 (rows before the outcome reset)", h.PriorParks)
		}
	})
	t.Run("pr_merged sets MergeObserved; malformed park payload counts", func(t *testing.T) {
		stageID := acc.ID
		h := run.ReadInfraParkHistory([]*audit.Entry{
			{Sequence: 1, Category: "pr_merged"},
			{Sequence: 2, StageID: &stageID, Category: run.CategoryStageInfraParked, Payload: json.RawMessage(`not json`)},
			nil,
		}, acc)
		if !h.MergeObserved || h.PriorParks != 1 || h.CurrentEpisodeParked {
			t.Fatalf("history = %+v", h)
		}
	})
	t.Run("nil stage", func(t *testing.T) {
		if h := run.ReadInfraParkHistory(nil, nil); h != (run.InfraParkHistory{}) {
			t.Fatalf("history = %+v", h)
		}
		if at, ord := run.InfraParkEpisode(nil); at != "" || ord != 0 {
			t.Fatalf("episode = (%q, %d)", at, ord)
		}
	})
}

func stageEntry(seq int64, stageID *uuid.UUID, category string) *audit.Entry {
	return &audit.Entry{Sequence: seq, StageID: stageID, Category: category}
}

// TestReadInfraParkHistory_Exclusions pins the #4426 implement-only
// exclusions as ReadInfraParkHistory derives them, end to end through
// ClassifyInfraFailurePark: every row that sets an exclusion answers
// NotApplicable, and every row that must NOT set one still parks.
func TestReadInfraParkHistory_Exclusions(t *testing.T) {
	t.Parallel()

	stages, impl := implFixture(catPtr(run.FailureC))
	implID := impl.ID
	otherID := stages[2].ID

	t.Run("plan_decomposed sets DecomposedParent", func(t *testing.T) {
		h := run.ReadInfraParkHistory([]*audit.Entry{stageEntry(1, &otherID, "plan_decomposed")}, impl)
		if !h.DecomposedParent || h.ReopenedPass {
			t.Fatalf("history = %+v, want DecomposedParent only", h)
		}
		if got := run.ClassifyInfraFailurePark(stages, impl, h); got != run.InfraParkNotApplicable {
			t.Fatalf("got %v, want NotApplicable", got)
		}
	})
	t.Run("no exclusion rows -> neither flag, parks", func(t *testing.T) {
		h := run.ReadInfraParkHistory([]*audit.Entry{stageEntry(1, &implID, "implement_dispatched")}, impl)
		if h.DecomposedParent || h.ReopenedPass || h.StagePushed {
			t.Fatalf("history = %+v, want no exclusion", h)
		}
		if got := run.ClassifyInfraFailurePark(stages, impl, h); got != run.InfraParkRetryable {
			t.Fatalf("got %v, want Retryable", got)
		}
	})
	for _, cat := range run.InfraParkReopenedPassCategories() {
		t.Run(cat+" scoped to the failed stage sets ReopenedPass", func(t *testing.T) {
			h := run.ReadInfraParkHistory([]*audit.Entry{stageEntry(1, &implID, cat)}, impl)
			if !h.ReopenedPass || h.StagePushed {
				t.Fatalf("history = %+v, want ReopenedPass from the trigger row", h)
			}
			if got := run.ClassifyInfraFailurePark(stages, impl, h); got != run.InfraParkNotApplicable {
				t.Fatalf("got %v, want NotApplicable", got)
			}
		})
		t.Run(cat+" scoped to another stage or unscoped leaves ReopenedPass false", func(t *testing.T) {
			h := run.ReadInfraParkHistory([]*audit.Entry{stageEntry(1, &otherID, cat), stageEntry(2, nil, cat)}, impl)
			if h.ReopenedPass {
				t.Fatalf("history = %+v, want no ReopenedPass", h)
			}
			if got := run.ClassifyInfraFailurePark(stages, impl, h); got != run.InfraParkRetryable {
				t.Fatalf("got %v, want Retryable", got)
			}
		})
	}
	// #4426 approval condition C1: a head-producing row scoped to the stage
	// is a re-opened pass with NO trigger row.
	for _, cat := range []string{"pull_request_opened", "fixup_pushed", "child_pushed"} {
		t.Run("C1 durable signal: "+cat+" with no trigger row -> NotApplicable", func(t *testing.T) {
			h := run.ReadInfraParkHistory([]*audit.Entry{stageEntry(1, &implID, cat)}, impl)
			if !h.StagePushed || !h.ReopenedPass {
				t.Fatalf("history = %+v, want StagePushed and ReopenedPass", h)
			}
			if got := run.ClassifyInfraFailurePark(stages, impl, h); got != run.InfraParkNotApplicable {
				t.Fatalf("got %v, want NotApplicable", got)
			}
		})
		t.Run("C1 durable signal: "+cat+" on another stage -> Retryable", func(t *testing.T) {
			h := run.ReadInfraParkHistory([]*audit.Entry{stageEntry(1, &otherID, cat), stageEntry(2, nil, cat)}, impl)
			if h.StagePushed || h.ReopenedPass {
				t.Fatalf("history = %+v, want no exclusion", h)
			}
			if got := run.ClassifyInfraFailurePark(stages, impl, h); got != run.InfraParkRetryable {
				t.Fatalf("got %v, want Retryable", got)
			}
		})
	}
	t.Run("implement budget counts parks over the stage lifetime", func(t *testing.T) {
		// An acceptance outcome scoped to a DIFFERENT stage never resets
		// the implement stage's count.
		h := run.ReadInfraParkHistory([]*audit.Entry{
			parkEntry(t, 1, implID, "2026-10-15T09:00:00Z", 0),
			parkEntry(t, 2, implID, "2026-10-15T10:00:00Z", 1),
			stageEntry(3, &otherID, "acceptance_outcome_recorded"),
			parkEntry(t, 4, implID, "2026-10-15T11:00:00Z", 2),
		}, impl)
		if h.PriorParks != 3 {
			t.Fatalf("PriorParks = %d, want 3", h.PriorParks)
		}
		if got := run.ClassifyInfraFailurePark(stages, impl, h); got != run.InfraParkBudgetExhausted {
			t.Fatalf("got %v, want BudgetExhausted", got)
		}
	})
}

// TestInfraParkHistoryOnReadError pins #4426 approval condition C1's
// read-error arm: an implement failure whose audit read failed classifies as
// a re-opened pass (NotApplicable: the run fails as today), while an
// acceptance failure answers ok=false so the caller keeps #4356's
// error-with-no-transition contract.
func TestInfraParkHistoryOnReadError(t *testing.T) {
	t.Parallel()

	t.Run("implement C read error -> ok, NotApplicable", func(t *testing.T) {
		stages, impl := implFixture(catPtr(run.FailureC))
		h, ok := run.InfraParkHistoryOnReadError(stages, impl)
		if !ok {
			t.Fatalf("ok = false, want true (the read error must fail the run, not error)")
		}
		if got := run.ClassifyInfraFailurePark(stages, impl, h); got != run.InfraParkNotApplicable {
			t.Fatalf("got %v, want NotApplicable", got)
		}
	})
	t.Run("acceptance C read error -> not ok", func(t *testing.T) {
		stages, acc := infraFixture(catPtr(run.FailureC), run.StageStatePending)
		if _, ok := run.InfraParkHistoryOnReadError(stages, acc); ok {
			t.Fatalf("ok = true, want false (an acceptance park cannot be bounded without its history)")
		}
	})
}

// TestInfraParkReopenedPassCategories_ReturnsCopy pins that a caller cannot
// mutate the predicate's category list through the returned slice.
func TestInfraParkReopenedPassCategories_ReturnsCopy(t *testing.T) {
	t.Parallel()
	got := run.InfraParkReopenedPassCategories()
	if len(got) != 3 {
		t.Fatalf("categories = %v, want 3", got)
	}
	got[0] = "mutated"
	if run.InfraParkReopenedPassCategories()[0] == "mutated" {
		t.Fatal("InfraParkReopenedPassCategories returned the backing slice")
	}
}

// mcTriggerPayload is a populated server stage_merge_candidate_verify_triggered
// payload with the given anchor overrides (an override of "" blanks it).
func mcTriggerPayload(t *testing.T, overrides map[string]any) json.RawMessage {
	t.Helper()
	p := map[string]any{
		"branch":                   "fishhawk/run-4816",
		"base_ref":                 "main",
		"expected_head_sha":        "a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1",
		"cause":                    "base_advance",
		"verify_command":           "scripts/test verify",
		"prior_state":              "succeeded",
		"reparked_review_stage_id": uuid.NewString(),
	}
	for k, v := range overrides {
		p[k] = v
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// reapPayload is an operator dispatch_reaper_failed payload: the shared reap
// key set plus, when stamp is true, the episode keys the operator arm adds.
func reapPayload(t *testing.T, reason string, stamp bool, failedAt string, ordinal int, extra map[string]any) json.RawMessage {
	t.Helper()
	p := map[string]any{
		"run_id":           uuid.NewString(),
		"stage_id":         uuid.NewString(),
		"failure_category": "C",
		"reason":           reason,
		"detail":           "",
		"exit_code":        0,
		"reported_at":      "2026-11-11T12:00:00Z",
		"auth_method":      "bearer",
	}
	if stamp {
		p["failed_at"] = failedAt
		p["retry_ordinal"] = ordinal
	}
	for k, v := range extra {
		p[k] = v
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func payloadEntry(seq int64, stageID *uuid.UUID, category string, payload json.RawMessage) *audit.Entry {
	return &audit.Entry{Sequence: seq, StageID: stageID, Category: category, Payload: payload}
}

// TestReadInfraParkHistory_ReapedLiveMergeCandidatePass pins the #4816 arm end
// to end through ClassifyInfraFailurePark. Unless a row says otherwise, the
// trigger is populated and the operator reap is a runner_lost reap stamped
// with the failed stage's own episode. Every NotApplicable row differs from a
// Retryable row in exactly the one fact its conjunct reads, so that conjunct
// is the only thing in the path.
func TestReadInfraParkHistory_ReapedLiveMergeCandidatePass(t *testing.T) {
	t.Parallel()

	ended := time.Date(2026, 11, 11, 12, 34, 56, 789000, time.UTC)
	newImpl := func(cat *run.FailureCategory, endedAt *time.Time, ordinal int) ([]*run.Stage, *run.Stage) {
		stages, impl := implFixture(cat)
		impl.EndedAt = endedAt
		impl.SelfRetryCount = ordinal
		return stages, impl
	}
	const (
		trig     = "stage_merge_candidate_verify_triggered"
		verified = "merge_candidate_verified"
		reap     = "dispatch_reaper_failed"
		watchdog = "dispatch_watchdog_elapsed"
	)

	type tc struct {
		name string
		// cat / endedAt / ordinal shape the failed stage (defaults: C, ended, 2).
		cat      *run.FailureCategory
		unended  bool
		ordinal  *int
		rows     func(t *testing.T, id, other uuid.UUID, at string, ord int) []*audit.Entry
		want     run.InfraParkDecision
		wantFlag bool
	}
	intp := func(i int) *int { return &i }
	stamped := func(t *testing.T, at string, ord int) json.RawMessage {
		return reapPayload(t, run.InfraParkRunnerLostReason, true, at, ord, nil)
	}
	cases := []tc{
		// --- Retryable ---
		{name: "trigger + stamped operator reap -> Retryable", want: run.InfraParkRetryable, wantFlag: true,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord))}
			}},
		{name: "conditional expected_state=dispatched operator reap -> Retryable", want: run.InfraParkRetryable, wantFlag: true,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{
					payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)),
					payloadEntry(6, &id, reap, reapPayload(t, run.InfraParkRunnerLostReason, true, at, ord, map[string]any{"expected_state": "dispatched"})),
				}
			}},
		{name: "pull_request_opened before the trigger (StagePushed override) -> Retryable", want: run.InfraParkRetryable, wantFlag: true,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{stageEntry(2, &id, "pull_request_opened"), payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord))}
			}},
		{name: "older stage_fixup_triggered then trigger (newestPass) -> Retryable", want: run.InfraParkRetryable, wantFlag: true,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{stageEntry(2, &id, "stage_fixup_triggered"), payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord))}
			}},
		{name: "earlier merge_candidate_verified then a newer trigger (live) -> Retryable", want: run.InfraParkRetryable, wantFlag: true,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{stageEntry(3, &id, verified), payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord))}
			}},
		{name: "a prior-episode park row counts (PriorParks 1) -> Retryable", want: run.InfraParkRetryable, wantFlag: true,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{parkEntry(t, 1, id, "2026-11-10T09:00:00Z", 0), payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord))}
			}},
		{name: "a current-episode park row is not double counted -> Retryable", want: run.InfraParkRetryable, wantFlag: true,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord)), parkEntry(t, 7, id, at, ord)}
			}},
		// --- BudgetExhausted ---
		{name: "three prior-episode parks + a live stamped reap -> BudgetExhausted", want: run.InfraParkBudgetExhausted, wantFlag: true,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{
					parkEntry(t, 1, id, "2026-11-10T09:00:00Z", 0), parkEntry(t, 2, id, "2026-11-10T10:00:00Z", 1), parkEntry(t, 3, id, "2026-11-10T11:00:00Z", 1),
					payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord)),
				}
			}},
		// --- NotApplicable ---
		{name: "trigger only, no reap -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, _ string, _ int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil))}
			}},
		{name: "trigger consumed by a later merge_candidate_verified -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), stageEntry(6, &id, verified), payloadEntry(7, &id, reap, stamped(t, at, ord))}
			}},
		{name: "trigger then a newer stage_fixup_triggered -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), stageEntry(6, &id, "stage_fixup_triggered"), payloadEntry(7, &id, reap, stamped(t, at, ord))}
			}},
		{name: "trigger then a newer stage_conflict_resolution_triggered -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), stageEntry(6, &id, "stage_conflict_resolution_triggered"), payloadEntry(7, &id, reap, stamped(t, at, ord))}
			}},
		{name: "reap stamped with an EARLIER episode (failed_at and ordinal differ) -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, _ string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, "2026-11-11T10:00:00Z", ord-1))}
			}},
		{name: "matching failed_at but a different retry_ordinal -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord+1))}
			}},
		{name: "LEGACY unstamped reap on an unended ordinal-0 stage (the (\"\",0) collision) -> NotApplicable", want: run.InfraParkNotApplicable,
			unended: true, ordinal: intp(0),
			rows: func(t *testing.T, id, _ uuid.UUID, _ string, _ int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, reapPayload(t, run.InfraParkRunnerLostReason, false, "", 0, nil))}
			}},
		{name: "current-episode stamp carrying reported_by runner (N1 provenance) -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{
					payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)),
					payloadEntry(6, &id, reap, reapPayload(t, run.InfraParkRunnerLostReason, true, at, ord, map[string]any{"reported_by": "runner", "reported_attempt": "attempt-1"})),
				}
			}},
		{name: "detached reaper non-zero-exit reason (C2) -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{
					payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)),
					payloadEntry(6, &id, reap, reapPayload(t, "runner exited 1 before reporting a terminal state", true, at, ord, map[string]any{"exit_code": 1})),
				}
			}},
		{name: "zero-exit strand reaper reason (C2) -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{
					payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)),
					payloadEntry(6, &id, reap, reapPayload(t, "runner exited 0 without settling the stage (state=running)", true, at, ord, nil)),
				}
			}},
		{name: "concurrency-slot spawn-failure compensation (C2, conditional dispatched) -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{
					payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)),
					payloadEntry(6, &id, reap, reapPayload(t, "slot waiter: spawn failed after admission: fork/exec: no such file", true, at, ord, map[string]any{"expected_state": "dispatched"})),
				}
			}},
		{name: "dispatch_watchdog_elapsed carrying a current-episode stamp (N1: never parks) -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, watchdog, stamped(t, at, ord))}
			}},
		{name: "half-populated trigger (no verify_command) -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, map[string]any{"verify_command": ""})), payloadEntry(6, &id, reap, stamped(t, at, ord))}
			}},
		{name: "invalid-cause trigger -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, map[string]any{"cause": "rebase"})), payloadEntry(6, &id, reap, stamped(t, at, ord))}
			}},
		{name: "malformed trigger payload -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, json.RawMessage(`{"branch":`)), payloadEntry(6, &id, reap, stamped(t, at, ord))}
			}},
		{name: "stamped reap scoped to another stage -> NotApplicable", want: run.InfraParkNotApplicable,
			rows: func(t *testing.T, id, other uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &other, reap, stamped(t, at, ord))}
			}},
		{name: "plan_decomposed (DecomposedParent precedence) -> NotApplicable", want: run.InfraParkNotApplicable, wantFlag: true,
			rows: func(t *testing.T, id, other uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{stageEntry(1, &other, "plan_decomposed"), payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord))}
			}},
		{name: "pr_merged (C2 carve-out) -> NotApplicable", want: run.InfraParkNotApplicable, wantFlag: true,
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord)), stageEntry(7, nil, "pr_merged")}
			}},
		{name: "failure category B -> NotApplicable", want: run.InfraParkNotApplicable, wantFlag: true, cat: catPtr(run.FailureB),
			rows: func(t *testing.T, id, _ uuid.UUID, at string, ord int) []*audit.Entry {
				return []*audit.Entry{payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)), payloadEntry(6, &id, reap, stamped(t, at, ord))}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cat := c.cat
			if cat == nil {
				cat = catPtr(run.FailureC)
			}
			endedAt := &ended
			if c.unended {
				endedAt = nil
			}
			ordinal := 2
			if c.ordinal != nil {
				ordinal = *c.ordinal
			}
			stages, impl := newImpl(cat, endedAt, ordinal)
			at, ord := run.InfraParkEpisode(impl)
			if !c.unended && (at == "" || ord != ordinal) {
				t.Fatalf("precondition: episode = (%q, %d), want a non-empty failed_at and ordinal %d", at, ord, ordinal)
			}
			rows := c.rows(t, impl.ID, stages[2].ID, at, ord)
			h := run.ReadInfraParkHistory(rows, impl)
			if h.ReapedLiveMergeCandidatePass != c.wantFlag {
				t.Fatalf("ReapedLiveMergeCandidatePass = %v, want %v (history %+v)", h.ReapedLiveMergeCandidatePass, c.wantFlag, h)
			}
			if got := run.ClassifyInfraFailurePark(stages, impl, h); got != c.want {
				t.Fatalf("got %v, want %v (history %+v)", got, c.want, h)
			}
		})
	}

	t.Run("the reaped pass is still a re-opened pass, and park counts are unchanged", func(t *testing.T) {
		stages, impl := newImpl(catPtr(run.FailureC), &ended, 2)
		at, ord := run.InfraParkEpisode(impl)
		id := impl.ID
		h := run.ReadInfraParkHistory([]*audit.Entry{
			parkEntry(t, 1, id, "2026-11-10T09:00:00Z", 0),
			stageEntry(2, &id, "pull_request_opened"),
			payloadEntry(5, &id, trig, mcTriggerPayload(t, nil)),
			payloadEntry(6, &id, reap, stamped(t, at, ord)),
			parkEntry(t, 7, id, at, ord),
		}, impl)
		if !h.ReopenedPass || !h.StagePushed || !h.ReapedLiveMergeCandidatePass || h.PriorParks != 1 || !h.CurrentEpisodeParked {
			t.Fatalf("history = %+v, want ReopenedPass, StagePushed, ReapedLiveMergeCandidatePass, PriorParks 1, CurrentEpisodeParked", h)
		}
		if got := run.ClassifyInfraFailurePark(stages, impl, h); got != run.InfraParkRetryable {
			t.Fatalf("got %v, want Retryable", got)
		}
	})
	t.Run("read error keeps failing the run", func(t *testing.T) {
		stages, impl := newImpl(catPtr(run.FailureC), &ended, 2)
		h, ok := run.InfraParkHistoryOnReadError(stages, impl)
		if !ok || h.ReapedLiveMergeCandidatePass {
			t.Fatalf("ok = %v, history = %+v, want ok and no ReapedLiveMergeCandidatePass", ok, h)
		}
	})
}

// TestInfraParkMergeCandidateAccessors pins the mirrored values the server's
// mirror test binds and the orchestrator's park-row marker writes.
func TestInfraParkMergeCandidateAccessors(t *testing.T) {
	t.Parallel()
	if got := run.InfraParkOperatorReapCategory(); got != "dispatch_reaper_failed" {
		t.Fatalf("InfraParkOperatorReapCategory = %q", got)
	}
	if got := run.InfraParkMergeCandidateVerifiedCategory(); got != "merge_candidate_verified" {
		t.Fatalf("InfraParkMergeCandidateVerifiedCategory = %q", got)
	}
	if run.InfraParkReopenedPassMergeCandidateVerify != "merge_candidate_verify" {
		t.Fatalf("InfraParkReopenedPassMergeCandidateVerify = %q", run.InfraParkReopenedPassMergeCandidateVerify)
	}
	if run.InfraParkRunnerLostReason != "runner_lost" {
		t.Fatalf("InfraParkRunnerLostReason = %q", run.InfraParkRunnerLostReason)
	}
}
