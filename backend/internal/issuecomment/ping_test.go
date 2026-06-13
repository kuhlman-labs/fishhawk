package issuecomment

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

func verdictEntry(seq int64, category, verdict string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"verdict": verdict})
	return &audit.Entry{Sequence: seq, Category: category, Payload: payload}
}

func reviewerVerdictEntry(seq int64, category, verdict, model string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"verdict": verdict, "reviewer_model": model})
	return &audit.Entry{Sequence: seq, Category: category, Payload: payload}
}

func approvalDecisionEntry(seq int64, decision string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"decision": decision})
	return &audit.Entry{Sequence: seq, Category: "approval_submitted", Payload: payload}
}

// TestPageClassEvents_ReviewerRejectWording covers the reworded advisory
// reject ping (#1070): it names the reviewer model, frames the reject as
// ADVISORY (awaiting operator arbitration), and never reads as a gate
// rejection.
func TestPageClassEvents_ReviewerRejectWording(t *testing.T) {
	entries := []*audit.Entry{
		reviewerVerdictEntry(5, "plan_reviewed", "reject", "gpt-5.5"),
	}
	got := pageClassEvents(entries, nil)
	if len(got) != 1 {
		t.Fatalf("expected one reviewer-reject event; got %+v", got)
	}
	msg := got[0].message
	if got[0].kind != "plan_review_rejected" {
		t.Errorf("kind token must stay plan_review_rejected (dedup parity); got %q", got[0].kind)
	}
	for _, want := range []string{"gpt-5.5", "advisory reject", "awaiting operator arbitration"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
	if strings.Contains(msg, "rejected the plan") {
		t.Errorf("advisory reject ping must not read as a gate rejection: %q", msg)
	}
}

// TestPageClassEvents_ReviewerRejectModelFallback covers the missing-model
// fallback to "A reviewer".
func TestPageClassEvents_ReviewerRejectModelFallback(t *testing.T) {
	entries := []*audit.Entry{
		verdictEntry(5, "implement_reviewed", "reject"), // no reviewer_model
	}
	got := pageClassEvents(entries, nil)
	if len(got) != 1 || !strings.HasPrefix(got[0].message, "🚫 A reviewer flagged") {
		t.Fatalf("expected 'A reviewer' fallback; got %+v", got)
	}
}

// TestPageClassEvents_AdvisoryRejectArbitrated covers the resolution ping
// (#1070): an approve OVER a current-round reviewer reject fires exactly
// one advisory_reject_arbitrated event keyed on the approval Sequence;
// a clean approve fires none.
func TestPageClassEvents_AdvisoryRejectArbitrated(t *testing.T) {
	arbitrated := []*audit.Entry{
		startedEntry(10, "plan"),
		reviewerVerdictEntry(11, "plan_reviewed", "reject", "gpt-5.5"),
		approvalDecisionEntry(12, "approve"),
	}
	got := pageClassEvents(arbitrated, nil)
	// Two events: the advisory-reject ping (seq 11) and the resolution
	// ping (seq 12), oldest-first.
	if len(got) != 2 {
		t.Fatalf("expected reject + resolution events; got %+v", got)
	}
	res := got[1]
	if res.kind != "advisory_reject_arbitrated" || res.sequence != 12 {
		t.Errorf("resolution event = %+v, want advisory_reject_arbitrated at seq 12", res)
	}
	if !strings.Contains(res.message, "over 1 advisory reject") {
		t.Errorf("resolution message missing override marker: %q", res.message)
	}

	// Clean approve (no preceding advisory reject) fires no resolution ping.
	clean := []*audit.Entry{
		startedEntry(10, "plan"),
		reviewerVerdictEntry(11, "plan_reviewed", "approve", "claude-opus-4-8"),
		approvalDecisionEntry(12, "approve"),
	}
	for _, ev := range pageClassEvents(clean, nil) {
		if ev.kind == "advisory_reject_arbitrated" {
			t.Errorf("clean approve must not fire a resolution ping; got %+v", ev)
		}
	}
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
