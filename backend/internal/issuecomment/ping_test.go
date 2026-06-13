package issuecomment

import (
	"encoding/json"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func verdictEntry(seq int64, category, verdict string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"verdict": verdict})
	return &audit.Entry{Sequence: seq, Category: category, Payload: payload}
}

// planAwaitingStages is a stage set with the plan stage parked at the
// approval gate — the precondition for the plan_awaiting_approval ping.
func planAwaitingStages() []*run.Stage {
	return []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateAwaitingApproval}}
}

func TestPageClassEvents_OnePerPageClassEvent(t *testing.T) {
	entries := []*audit.Entry{
		{Sequence: 1, Category: "run_dispatched"},              // not page-class
		{Sequence: 2, Category: "plan_generated"},              // page: awaiting approval
		{Sequence: 3, Category: "plan_scope_precheck"},         // not page-class
		verdictEntry(4, "plan_reviewed", "approve"),            // not page (approve)
		verdictEntry(5, "implement_reviewed", "reject"),        // page: reject
		{Sequence: 6, Category: "cost_recorded"},               // not page-class
		{Sequence: 7, Category: "ci_failure_retry_dispatched"}, // page: CI failure
		{Sequence: 8, Category: "ci_retry_exhausted"},          // page: CI exhausted
	}
	got := pageClassEvents(entries, planAwaitingStages())
	if len(got) != 4 {
		t.Fatalf("expected 4 page-class events; got %d: %+v", len(got), got)
	}
	wantKinds := []string{"plan_awaiting_approval", "implement_review_rejected", "ci_failure", "ci_retry_exhausted"}
	for i, w := range wantKinds {
		if got[i].kind != w {
			t.Errorf("event[%d].kind = %q, want %q", i, got[i].kind, w)
		}
	}
	wantSeqs := []int64{2, 5, 7, 8}
	for i, w := range wantSeqs {
		if got[i].sequence != w {
			t.Errorf("event[%d].sequence = %d, want %d", i, got[i].sequence, w)
		}
	}
}

func TestPageClassEvents_ApproveVerdictIsNotPageClass(t *testing.T) {
	entries := []*audit.Entry{
		verdictEntry(1, "plan_reviewed", "approve"),
		verdictEntry(2, "implement_reviewed", "approve_with_concerns"),
	}
	if got := pageClassEvents(entries, planAwaitingStages()); len(got) != 0 {
		t.Errorf("non-reject verdicts must not be page-class; got %+v", got)
	}
}

func TestPageClassEvents_NonPageTransitionsProduceNothing(t *testing.T) {
	entries := []*audit.Entry{
		{Sequence: 1, Category: "status_comment_posted"},
		{Sequence: 2, Category: "trace_uploaded"},
		{Sequence: 3, Category: "cost_recorded"},
		{Sequence: 4, Category: "fixup_pushed"},
	}
	if got := pageClassEvents(entries, planAwaitingStages()); len(got) != 0 {
		t.Errorf("expected no page-class events; got %+v", got)
	}
}

// TestPageClassEvents_GatelessPlanDoesNotPing covers concerns #3/#4: a
// plan_generated entry must NOT yield a plan_awaiting_approval ping when
// no plan stage is actually parked at an approval gate (a gateless /
// routine plan flow), otherwise it fires a spurious "awaiting your
// review" notification.
func TestPageClassEvents_GatelessPlanDoesNotPing(t *testing.T) {
	entries := []*audit.Entry{
		{Sequence: 1, Category: "plan_generated"},
	}
	// Plan stage already succeeded (gateless: never parked awaiting).
	gateless := []*run.Stage{{Type: run.StageTypePlan, State: run.StageStateSucceeded}}
	if got := pageClassEvents(entries, gateless); len(got) != 0 {
		t.Errorf("a gateless plan_generated must not be page-class; got %+v", got)
	}
	// nil stages (no awaiting gate) likewise produces nothing.
	if got := pageClassEvents(entries, nil); len(got) != 0 {
		t.Errorf("plan_generated with no awaiting plan stage must not page; got %+v", got)
	}
	// With the plan stage parked at the gate, the SAME chain pages once.
	got := pageClassEvents(entries, planAwaitingStages())
	if len(got) != 1 || got[0].kind != "plan_awaiting_approval" {
		t.Fatalf("expected one plan_awaiting_approval event; got %+v", got)
	}
}

// TestPageClassEvents_PlanAwaitingKeysLatestGenerated proves the
// plan-awaiting ping keys on the LATEST plan_generated sequence (replan
// round), so a second round pages once on its own dedup key.
func TestPageClassEvents_PlanAwaitingKeysLatestGenerated(t *testing.T) {
	entries := []*audit.Entry{
		{Sequence: 2, Category: "plan_generated"}, // round 1
		{Sequence: 9, Category: "plan_generated"}, // round 2 (replan)
	}
	got := pageClassEvents(entries, planAwaitingStages())
	if len(got) != 1 {
		t.Fatalf("expected one plan-awaiting event; got %+v", got)
	}
	if got[0].sequence != 9 {
		t.Errorf("plan-awaiting must key on the latest plan_generated (9); got seq %d", got[0].sequence)
	}
}

// TestPageClassEvents_ScopeAmendmentPagesHuman covers concern #1: a
// scope_amendment_requested event is a must_page_human (ADR-040) page
// class — it must surface a NEW ping (it has no other issue-comment
// surface), so a parking-human case is never silent on edits.
func TestPageClassEvents_ScopeAmendmentPagesHuman(t *testing.T) {
	entries := []*audit.Entry{
		{Sequence: 1, Category: "run_dispatched"},
		{Sequence: 2, Category: "scope_amendment_requested"},
	}
	got := pageClassEvents(entries, nil)
	if len(got) != 1 {
		t.Fatalf("expected one must_page_human event; got %+v", got)
	}
	if got[0].kind != "scope_amendment" || got[0].sequence != 2 {
		t.Errorf("got %+v, want scope_amendment at seq 2", got[0])
	}
}
