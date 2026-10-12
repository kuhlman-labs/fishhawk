package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// review_coverage_test.go pins reviewCoverageRefusal (#4219): one test per
// coverage mode named in the approved plan, plus the fail-closed read errors.
// The consumers (merge endpoint, gate view, drive observer, delegated arm, run
// derived_status) are pinned in their own files against this harness.

// coverageHarness is a decomposed parent (fanInVerifySpec: one implement agent
// reviewer) with a wired reviewer backend, on the autoDriveRepo backing store
// whose ListRuns honours the DecomposedFrom filter.
type coverageHarness struct {
	s        *Server
	repo     *autoDriveRepo
	au       *auditFake
	merger   *fakeMerger
	parentID uuid.UUID
	parent   *run.Run
}

func newCoverageHarness(t *testing.T) *coverageHarness {
	t.Helper()
	repo := &autoDriveRepo{driveE2ERepo: &driveE2ERepo{fakeRepo: newFakeRepo()}}
	au := newAuditFake()
	merger := &fakeMerger{}
	s := New(Config{
		Addr:          "127.0.0.1:0",
		RunRepo:       repo,
		AuditRepo:     au,
		ConcernRepo:   newFakeConcernRepo(),
		ApprovalRepo:  newFakeApprovalRepo(),
		Orchestrator:  &orchestrator.Orchestrator{Runs: repo},
		GateMerger:    merger,
		PlanReviewers: singleReviewerSet{&fakePlanReviewer{}},
	})
	parentID := uuid.New()
	parent := seedMergeRun(t, repo, parentID, run.StateRunning, mergePR, fanInVerifySpec, nil)
	return &coverageHarness{s: s, repo: repo, au: au, merger: merger, parentID: parentID, parent: parent}
}

// addChild seeds a decomposed child of the parent at slice index idx.
func (h *coverageHarness) addChild(t *testing.T, idx int, state run.State) uuid.UUID {
	t.Helper()
	id := uuid.New()
	parent := h.parentID
	i := idx
	h.repo.mu.Lock()
	h.repo.runs[id] = &run.Run{
		ID: id, State: state, WorkflowID: "feature_change", WorkflowSpec: fanInVerifySpec,
		DecomposedFrom: &parent, SliceIndex: &i,
		CreatedAt: time.Now().Add(time.Duration(idx) * time.Second),
	}
	h.repo.mu.Unlock()
	return id
}

// round seeds one implement-review round on runID: an implement_review_started
// at seq (configured agents) followed by one terminal entry per category in
// terminals, sequenced after it. Seeded BY CONSTRUCTION — the predicate under
// test, not the fixture, decides coverage.
func (h *coverageHarness) round(runID uuid.UUID, seq int64, configured int, terminals ...string) {
	started, _ := json.Marshal(planreview.ReviewStartedPayload{ConfiguredAgents: configured})
	rid := runID
	h.au.mu.Lock()
	defer h.au.mu.Unlock()
	h.au.seeded = append(h.au.seeded, &audit.Entry{RunID: &rid, Sequence: seq, Category: "implement_review_started", Payload: started})
	for i, cat := range terminals {
		payload := []byte(`{}`)
		if cat == "implement_reviewed" {
			payload, _ = json.Marshal(planreview.ImplementReviewedPayload{Verdict: planreview.VerdictApprove})
		}
		h.au.seeded = append(h.au.seeded, &audit.Entry{RunID: &rid, Sequence: seq + 1 + int64(i), Category: cat, Payload: payload})
	}
}

// d3b1d915 seeds the incident shape: the parent's consolidated round settled
// with its only reviewer failed; slice 0 carries its own verdict; slice 1 was
// never agent-reviewed. Returns slice 1's run id.
func (h *coverageHarness) d3b1d915(t *testing.T) uuid.UUID {
	t.Helper()
	c0 := h.addChild(t, 0, run.StateSucceeded)
	c1 := h.addChild(t, 1, run.StateSucceeded)
	h.round(c0, 5, 1, "implement_reviewed")
	h.round(h.parentID, 20, 1, "implement_review_failed")
	return c1
}

func (h *coverageHarness) refusal(t *testing.T) *mergeReadinessRefusal {
	t.Helper()
	ref, err := h.s.reviewCoverageRefusal(context.Background(), h.parent)
	if err != nil {
		t.Fatalf("reviewCoverageRefusal: unexpected error %v", err)
	}
	return ref
}

func uncoveredIDs(t *testing.T, ref *mergeReadinessRefusal) []string {
	t.Helper()
	list, ok := ref.Details["uncovered_slices"].([]map[string]any)
	if !ok {
		t.Fatalf("uncovered_slices = %T, want []map[string]any", ref.Details["uncovered_slices"])
	}
	var out []string
	for _, e := range list {
		out = append(out, e["child_run_id"].(string))
	}
	return out
}

// TestReviewCoverage_D3b1d915_NamesOnlyUncoveredSlice is the issue's shape. The
// counterfactual the issue names — swap the verdicts>=1 test for "the round
// settled" — turns this RED, because the failed parent round IS settled.
func TestReviewCoverage_D3b1d915_NamesOnlyUncoveredSlice(t *testing.T) {
	h := newCoverageHarness(t)
	c1 := h.d3b1d915(t)

	ref := h.refusal(t)
	if ref == nil {
		t.Fatal("refusal = nil, want review_coverage_incomplete for the never-reviewed slice 1")
	}
	if ref.Code != mergeCodeReviewCoverageIncomplete {
		t.Errorf("code = %q, want %q", ref.Code, mergeCodeReviewCoverageIncomplete)
	}
	if ids := uncoveredIDs(t, ref); len(ids) != 1 || ids[0] != c1.String() {
		t.Errorf("uncovered = %v, want only slice 1 %s", ids, c1)
	}
	entry := ref.Details["uncovered_slices"].([]map[string]any)[0]
	if entry["slice_index"] != 1 || entry["reason"] != reviewCoverageReasonNoRound {
		t.Errorf("entry = %v, want slice_index 1, reason %s", entry, reviewCoverageReasonNoRound)
	}
	if ref.Details["next_step"] != reviewCoverageNextRerun {
		t.Errorf("next_step = %v, want %s", ref.Details["next_step"], reviewCoverageNextRerun)
	}
	pr, _ := ref.Details["parent_round"].(map[string]any)
	if pr == nil || pr["sequence"] != int64(20) || pr["verdicts"] != 0 || pr["in_flight"] != false {
		t.Errorf("parent_round = %v, want sequence 20, 0 verdicts, settled", ref.Details["parent_round"])
	}
	for _, want := range []string{"slice 1", c1.String(), "fishhawk_rerun_review", "No merge verdict was recorded"} {
		if !strings.Contains(ref.Message, want) {
			t.Errorf("message missing %q:\n%s", want, ref.Message)
		}
	}
}

func TestReviewCoverage_ParentVerdictCoversEverySlice(t *testing.T) {
	h := newCoverageHarness(t)
	h.addChild(t, 0, run.StateSucceeded)
	h.addChild(t, 1, run.StateSucceeded)
	// A reject is still coverage: the review happened.
	h.round(h.parentID, 20, 2, "implement_review_failed", "implement_reviewed")
	if ref := h.refusal(t); ref != nil {
		t.Fatalf("refusal = %+v, want nil: a parent verdict covers every slice", ref)
	}
}

func TestReviewCoverage_AllSlicesOwnReviewed(t *testing.T) {
	h := newCoverageHarness(t)
	c0 := h.addChild(t, 0, run.StateSucceeded)
	c1 := h.addChild(t, 1, run.StateSucceeded)
	h.round(c0, 5, 1, "implement_reviewed")
	h.round(c1, 7, 1, "implement_reviewed")
	h.round(h.parentID, 20, 1, "implement_review_failed")
	if ref := h.refusal(t); ref != nil {
		t.Fatalf("refusal = %+v, want nil: every slice carries its own verdict", ref)
	}
}

func TestReviewCoverage_ParentRoundInFlight_AwaitNotRerun(t *testing.T) {
	h := newCoverageHarness(t)
	h.addChild(t, 0, run.StateSucceeded)
	h.round(h.parentID, 20, 2, "implement_review_failed") // 1 of 2 terminal
	ref := h.refusal(t)
	if ref == nil {
		t.Fatal("refusal = nil, want a hold while the parent round has no verdict yet")
	}
	if ref.Details["next_step"] != reviewCoverageNextAwait {
		t.Errorf("next_step = %v, want %s (a round in flight is awaited, not re-run)", ref.Details["next_step"], reviewCoverageNextAwait)
	}
	if pr, _ := ref.Details["parent_round"].(map[string]any); pr == nil || pr["in_flight"] != true {
		t.Errorf("parent_round = %v, want in_flight true", ref.Details["parent_round"])
	}
}

// TestReviewCoverage_OlderVerdictNewerFailedRound pins latest-round semantics
// on BOTH sides: a verdict a newer no-verdict round superseded covers nothing.
func TestReviewCoverage_OlderVerdictNewerFailedRound(t *testing.T) {
	h := newCoverageHarness(t)
	c0 := h.addChild(t, 0, run.StateSucceeded)
	h.round(c0, 5, 1, "implement_reviewed")
	h.round(c0, 8, 1, "implement_review_failed")
	h.round(h.parentID, 20, 1, "implement_reviewed")
	h.round(h.parentID, 30, 1, "implement_review_failed")
	ref := h.refusal(t)
	if ref == nil {
		t.Fatal("refusal = nil, want a hold: both latest rounds recorded zero verdicts")
	}
	entry := ref.Details["uncovered_slices"].([]map[string]any)[0]
	if entry["reason"] != reviewCoverageReasonNoVerdict || entry["round_sequence"] != int64(8) {
		t.Errorf("entry = %v, want reason %s at round 8", entry, reviewCoverageReasonNoVerdict)
	}
}

func TestReviewCoverage_SkippedOnlyRoundCoversNothing(t *testing.T) {
	h := newCoverageHarness(t)
	h.addChild(t, 0, run.StateSucceeded)
	h.round(h.parentID, 20, 1, "implement_review_skipped")
	if ref := h.refusal(t); ref == nil {
		t.Fatal("refusal = nil, want a hold: a skipped-only round covers nothing")
	}
}

func TestReviewCoverage_UncoveredChildRoundInFlightReason(t *testing.T) {
	h := newCoverageHarness(t)
	c0 := h.addChild(t, 0, run.StateSucceeded)
	h.round(c0, 5, 2, "implement_review_failed")
	h.round(h.parentID, 20, 1, "implement_review_failed")
	ref := h.refusal(t)
	if ref == nil {
		t.Fatal("refusal = nil, want a hold")
	}
	if got := ref.Details["uncovered_slices"].([]map[string]any)[0]["reason"]; got != reviewCoverageReasonInFlight {
		t.Errorf("reason = %v, want %s", got, reviewCoverageReasonInFlight)
	}
}

func TestReviewCoverage_NotApplicable(t *testing.T) {
	t.Run("non-parent", func(t *testing.T) {
		h := newCoverageHarness(t)
		h.round(h.parentID, 20, 1, "implement_review_failed")
		if ref := h.refusal(t); ref != nil {
			t.Fatalf("refusal = %+v, want nil for a run with no decomposed children", ref)
		}
	})
	t.Run("child run", func(t *testing.T) {
		h := newCoverageHarness(t)
		c0 := h.addChild(t, 0, run.StateSucceeded)
		h.round(c0, 5, 1, "implement_review_failed")
		h.repo.mu.Lock()
		child := h.repo.runs[c0]
		h.repo.mu.Unlock()
		// Give the CHILD a child of its own so only the DecomposedFrom guard
		// keeps it out of the predicate.
		gc := uuid.New()
		h.repo.mu.Lock()
		h.repo.runs[gc] = &run.Run{ID: gc, State: run.StateSucceeded, DecomposedFrom: &c0}
		h.repo.mu.Unlock()
		ref, err := h.s.reviewCoverageRefusal(context.Background(), child)
		if err != nil || ref != nil {
			t.Fatalf("child: refusal = %+v, err = %v, want nil, nil", ref, err)
		}
	})
	t.Run("zero reviewers configured", func(t *testing.T) {
		h := newCoverageHarness(t)
		h.d3b1d915(t)
		h.parent.WorkflowSpec = nil
		if ref := h.refusal(t); ref != nil {
			t.Fatalf("refusal = %+v, want nil when no implement reviewer is configured", ref)
		}
	})
	t.Run("no reviewer wired", func(t *testing.T) {
		h := newCoverageHarness(t)
		h.d3b1d915(t)
		h.s.cfg.PlanReviewers = nil
		if ref := h.refusal(t); ref != nil {
			t.Fatalf("refusal = %+v, want nil when no reviewer backend is wired", ref)
		}
	})
}

func TestReviewCoverage_CancelledChildExcluded(t *testing.T) {
	h := newCoverageHarness(t)
	c0 := h.addChild(t, 0, run.StateSucceeded)
	h.addChild(t, 1, run.StateCancelled)
	h.addChild(t, 2, run.StateFailed)
	h.round(c0, 5, 1, "implement_reviewed")
	h.round(h.parentID, 20, 1, "implement_review_failed")
	if ref := h.refusal(t); ref != nil {
		t.Fatalf("refusal = %+v, want nil: cancelled / failed children contributed no code", ref)
	}
}

// TestReviewCoverage_NoParentRound_RerunOncePROpen pins the no-round next_step
// (#4282). The harness parent is GATING (fanInVerifySpec: 1 agent, 0 human)
// with its consolidated PR open; the advisory arms swap in
// specRedispatchAdvisory (1 agent, 1 human). Before the PR opens the
// orchestrator has not dispatched the consolidated round yet, so the hold
// awaits; once it is open with no round, an advisory parent's next_step is the
// re-run verb, which starts the first consolidated round. A gating parent keeps
// await and names fishhawk_fixup_stage, because the verb refuses it (approval
// condition C5).
//
// COUNTERFACTUALS: revert the no-round case to always-await → the advisory
// PR-open arm RED; delete the gating case → the gating arm reads rerun → RED;
// delete the PR-open case → the advisory no-PR arm reads rerun → RED.
func TestReviewCoverage_NoParentRound_RerunOncePROpen(t *testing.T) {
	cases := []struct {
		name       string
		advisory   bool
		prOpen     bool
		wantNext   string
		wantAction string
		wantInMsg  []string
	}{
		{"advisory parent, PR open", true, true, reviewCoverageNextRerun, "rerun_review", []string{"fishhawk_rerun_review", "first consolidated round"}},
		{"advisory parent, PR not open", true, false, reviewCoverageNextAwait, "await_review", []string{"fishhawk_await_review", "not open"}},
		{"gating parent, PR open", false, true, reviewCoverageNextAwait, "await_review", []string{"fishhawk_await_review", "fishhawk_fixup_stage", "gating_authority"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newCoverageHarness(t)
			h.addChild(t, 0, run.StateSucceeded)
			if tc.advisory {
				h.parent.WorkflowSpec = specRedispatchAdvisory
			}
			if !tc.prOpen {
				h.parent.PullRequestURL = nil
			}
			ref := h.refusal(t)
			if ref == nil {
				t.Fatal("refusal = nil, want a hold: no round covers the slice")
			}
			if ref.Details["next_step"] != tc.wantNext {
				t.Errorf("next_step = %v, want %s", ref.Details["next_step"], tc.wantNext)
			}
			if ref.Details["parent_round"] != nil {
				t.Errorf("parent_round = %v, want nil", ref.Details["parent_round"])
			}
			for _, s := range tc.wantInMsg {
				if !strings.Contains(ref.Message, s) {
					t.Errorf("message does not name %q:\n%s", s, ref.Message)
				}
			}
			if got := reviewCoverageDriveAction(ref); got != tc.wantAction {
				t.Errorf("drive action = %q, want %s", got, tc.wantAction)
			}
		})
	}
}

func TestReviewCoverage_ReadErrorsFailClosed(t *testing.T) {
	for _, cat := range []string{"implement_review_started", "implement_reviewed", "implement_review_failed", "implement_review_skipped"} {
		t.Run(cat, func(t *testing.T) {
			h := newCoverageHarness(t)
			h.d3b1d915(t)
			h.au.listByCategoryErrCategory = cat
			ref, err := h.s.reviewCoverageRefusal(context.Background(), h.parent)
			if err == nil || ref != nil {
				t.Fatalf("refusal = %+v, err = %v, want a returned read error", ref, err)
			}
		})
	}
	t.Run("children list", func(t *testing.T) {
		h := newCoverageHarness(t)
		h.d3b1d915(t)
		h.repo.mu.Lock()
		h.repo.listErr = errors.New("list runs down")
		h.repo.mu.Unlock()
		if _, err := h.s.reviewCoverageRefusal(context.Background(), h.parent); err == nil {
			t.Fatal("err = nil, want the children-list error returned")
		}
	})
	t.Run("child round read", func(t *testing.T) {
		// Isolated: the parent's reads succeed (its round has no verdict, so
		// the walk reaches the children) and only slice 1's read fails.
		h := newCoverageHarness(t)
		c1 := h.d3b1d915(t)
		h.s.cfg.AuditRepo = runScopedErrAudit{auditFake: h.au, failRun: c1}
		_, err := h.s.reviewCoverageRefusal(context.Background(), h.parent)
		if err == nil || !strings.Contains(err.Error(), c1.String()) {
			t.Fatalf("err = %v, want the child's read error naming %s", err, c1)
		}
	})
}

// runScopedErrAudit fails ListForRunByCategory for ONE run id only.
type runScopedErrAudit struct {
	*auditFake
	failRun uuid.UUID
}

func (a runScopedErrAudit) ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	if runID == a.failRun {
		return nil, errors.New("audit read down for " + runID.String())
	}
	return a.auditFake.ListForRunByCategory(ctx, runID, category)
}

func TestReviewCoverageDriveAction(t *testing.T) {
	if got := reviewCoverageDriveAction(&mergeReadinessRefusal{Details: map[string]any{"next_step": reviewCoverageNextRerun}}); got != "rerun_review" {
		t.Errorf("rerun: action = %q, want rerun_review", got)
	}
	if got := reviewCoverageDriveAction(&mergeReadinessRefusal{Details: map[string]any{"next_step": reviewCoverageNextAwait}}); got != "await_review" {
		t.Errorf("await: action = %q, want await_review", got)
	}
}
