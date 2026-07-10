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

// TestPageClassEvents_NoAdvisoryRejectArbitratedEvent covers proposal 3 of
// #1786: approving OVER a current-round reviewer reject no longer produces a
// page-class advisory_reject_arbitrated event — approving is the operator's
// own action and must not page them. The chain still projects the reviewer
// reject itself (that page is resolved-and-skipped at firePings time, not
// dropped from the projection). The arbitration line stays on the anchor
// timeline (asserted in anchor_template_test.go), not here.
func TestPageClassEvents_NoAdvisoryRejectArbitratedEvent(t *testing.T) {
	arbitrated := []*audit.Entry{
		startedEntry(10, "plan"),
		reviewerVerdictEntry(11, "plan_reviewed", "reject", "gpt-5.5"),
		approvalDecisionEntry(12, "approve"),
	}
	got := pageClassEvents(arbitrated, nil)
	// Only the reviewer-reject event (seq 11) — no advisory_reject_arbitrated.
	if len(got) != 1 {
		t.Fatalf("expected only the reviewer-reject event; got %+v", got)
	}
	if got[0].kind != "plan_review_rejected" || got[0].sequence != 11 {
		t.Errorf("got %+v, want plan_review_rejected at seq 11", got[0])
	}
	for _, ev := range got {
		if ev.kind == "advisory_reject_arbitrated" {
			t.Errorf("advisory_reject_arbitrated must no longer be a page class; got %+v", ev)
		}
	}

	// Clean approve (no preceding reject) likewise produces no page event.
	clean := []*audit.Entry{
		startedEntry(10, "plan"),
		reviewerVerdictEntry(11, "plan_reviewed", "approve", "claude-opus-4-8"),
		approvalDecisionEntry(12, "approve"),
	}
	if ev := pageClassEvents(clean, nil); len(ev) != 0 {
		t.Errorf("clean approve chain must produce no page-class events; got %+v", ev)
	}
}

// fixupEntry builds a stage_fixup_triggered audit entry (the implement-reject
// resolver).
func fixupEntry(seq int64) *audit.Entry {
	return &audit.Entry{Sequence: seq, Category: "stage_fixup_triggered"}
}

// TestPageEventResolved covers proposal 2 of #1786: a reviewer-reject page is
// "resolved" (→ firePings records the dedup row but skips the stale post) once
// a later arbitration lands, and never for a non-reviewer-reject kind or a
// resolver at an earlier/equal sequence.
func TestPageEventResolved(t *testing.T) {
	planReject := pageEvent{sequence: 11, kind: "plan_review_rejected"}
	implReject := pageEvent{sequence: 11, kind: "implement_review_rejected"}

	cases := []struct {
		name    string
		ev      pageEvent
		entries []*audit.Entry
		want    bool
	}{
		{
			name:    "plan reject resolved by later approval_submitted",
			ev:      planReject,
			entries: []*audit.Entry{approvalDecisionEntry(12, "approve")},
			want:    true,
		},
		{
			name:    "plan reject resolved by a later reject decision too (gate arbitrated → replan)",
			ev:      planReject,
			entries: []*audit.Entry{approvalDecisionEntry(12, "reject")},
			want:    true,
		},
		{
			name:    "plan reject NOT resolved by a fixup (implement-only resolver)",
			ev:      planReject,
			entries: []*audit.Entry{fixupEntry(12)},
			want:    false,
		},
		{
			name:    "plan reject NOT resolved with no later approval",
			ev:      planReject,
			entries: nil,
			want:    false,
		},
		{
			name:    "plan reject NOT resolved by an EARLIER approval (seq <= ev)",
			ev:      planReject,
			entries: []*audit.Entry{approvalDecisionEntry(10, "approve"), approvalDecisionEntry(11, "approve")},
			want:    false,
		},
		{
			name:    "implement reject resolved by later stage_fixup_triggered",
			ev:      implReject,
			entries: []*audit.Entry{fixupEntry(12)},
			want:    true,
		},
		{
			name:    "implement reject resolved by later approval_submitted",
			ev:      implReject,
			entries: []*audit.Entry{approvalDecisionEntry(12, "approve")},
			want:    true,
		},
		{
			name:    "implement reject NOT resolved with no later resolver",
			ev:      implReject,
			entries: []*audit.Entry{fixupEntry(11)}, // equal sequence, not strictly greater
			want:    false,
		},
		{
			name:    "non-reviewer-reject kind never resolvable (always pages)",
			ev:      pageEvent{sequence: 11, kind: "scope_amendment"},
			entries: []*audit.Entry{approvalDecisionEntry(12, "approve"), fixupEntry(13)},
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := pageEventResolved(tc.ev, tc.entries); got != tc.want {
				t.Errorf("pageEventResolved = %v, want %v", got, tc.want)
			}
		})
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

// clarificationEntry builds a clarification_requested audit entry whose
// payload nests the parked document under `clarification_request`, mirroring
// what server/plan.go::handleClarificationRequest writes.
func clarificationEntry(seq int64, questionIDs ...string) *audit.Entry {
	questions := make([]map[string]any, 0, len(questionIDs))
	for _, id := range questionIDs {
		questions = append(questions, map[string]any{
			"id":                  id,
			"question":            "which?",
			"recommended_default": "the first",
			"tradeoffs":           "trade",
		})
	}
	doc := map[string]any{
		"kind":      "clarification_request",
		"summary":   "not yet plannable",
		"questions": questions,
	}
	payload, _ := json.Marshal(map[string]any{"clarification_request": doc})
	return &audit.Entry{Sequence: seq, Category: "clarification_requested", Payload: payload}
}

// TestPageClassEvents_ClarificationRequestPagesHuman covers the #1057
// awaiting_input park: a clarification_requested event is a must_page_human
// page class (the planner parked for operator direction) and must surface a
// NEW ping naming how many questions need an answer.
func TestPageClassEvents_ClarificationRequestPagesHuman(t *testing.T) {
	entries := []*audit.Entry{
		{Sequence: 1, Category: "run_dispatched"},
		clarificationEntry(2, "auth-backend", "rate-limit"),
	}
	got := pageClassEvents(entries, nil)
	if len(got) != 1 {
		t.Fatalf("expected one clarification page event; got %+v", got)
	}
	if got[0].kind != "clarification_request" || got[0].sequence != 2 {
		t.Errorf("got %+v, want clarification_request at seq 2", got[0])
	}
	if !strings.Contains(got[0].message, "2 questions need") {
		t.Errorf("message = %q, want it to name the 2-question count", got[0].message)
	}
	if !strings.Contains(got[0].message, "❓") {
		t.Errorf("message = %q, want the clarification glyph", got[0].message)
	}
}

// TestClarificationQuestionPhrase covers the singular/plural/zero arms of
// the count-aware phrase so a malformed payload never renders "0 questions".
func TestClarificationQuestionPhrase(t *testing.T) {
	cases := map[int]string{
		0: "your parked questions need",
		1: "1 question needs",
		2: "2 questions need",
		5: "5 questions need",
	}
	for n, want := range cases {
		if got := clarificationQuestionPhrase(n); got != want {
			t.Errorf("clarificationQuestionPhrase(%d) = %q, want %q", n, got, want)
		}
	}
}

// campaignGatePagedEntry builds a campaign_gate_paged audit entry matching
// what server.emitCampaignGatePaged writes on the run chain (E25.7).
func campaignGatePagedEntry(seq int64, pageEvent string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{
		"page_event": pageEvent,
		"run_id":     "00000000-0000-0000-0000-000000000000",
		"reason":     "campaign auto-driver refused a must_page_human condition; handing the gate off to a human (E25.7)",
	})
	return &audit.Entry{Sequence: seq, Category: "campaign_gate_paged", Payload: payload}
}

// TestPageClassEvents_CampaignGatePagedPagesHuman covers the E25.7 hand-off: a
// campaign_gate_paged entry is a must_page_human page class (the auto-driver
// paused the issue and a human must own the gate) and surfaces exactly one NEW
// ping naming the gate/decision needed, keyed on the source Sequence so a
// re-render dedups it.
func TestPageClassEvents_CampaignGatePagedPagesHuman(t *testing.T) {
	entries := []*audit.Entry{
		{Sequence: 1, Category: "run_dispatched"},
		campaignGatePagedEntry(2, "reviewer_reject"),
	}
	got := pageClassEvents(entries, nil)
	if len(got) != 1 {
		t.Fatalf("expected one campaign_gate_paged page event; got %+v", got)
	}
	if got[0].kind != "campaign_gate_paged" || got[0].sequence != 2 {
		t.Errorf("got %+v, want campaign_gate_paged at seq 2 (the dedup key)", got[0])
	}
	if !strings.Contains(got[0].message, "🛑") {
		t.Errorf("message = %q, want the campaign-paused glyph", got[0].message)
	}
	if !strings.Contains(got[0].message, "reviewer flagged a blocking concern") {
		t.Errorf("message = %q, want it to name the reviewer-reject gate", got[0].message)
	}

	// Dedup-on-re-render: the event keys on its source Sequence, so once the
	// notifier records that sequence on CategoryAnchorPingPosted, a re-projection
	// of the SAME chain yields the same sequence and firePings skips it. Assert
	// the projection is stable (idempotent) across a re-render.
	again := pageClassEvents(entries, nil)
	if len(again) != 1 || again[0].sequence != 2 {
		t.Errorf("re-render projection = %+v, want a stable single event at seq 2", again)
	}
}

// TestCampaignGatePagedPhrase covers the known page-event tokens plus the
// generic fallback so an empty/unknown payload never renders a bare token.
func TestCampaignGatePagedPhrase(t *testing.T) {
	cases := map[string]string{
		"reviewer_reject":         "a reviewer flagged a blocking concern that needs your decision",
		"gating_reviewer_reject":  "a reviewer flagged a blocking concern that needs your decision",
		"requirement_arbitration": "a requirement needs your arbitration",
		"":                        "a gate decision",
		"something_unknown":       "a gate decision",
	}
	for token, want := range cases {
		if got := campaignGatePagedPhrase(token); got != want {
			t.Errorf("campaignGatePagedPhrase(%q) = %q, want %q", token, got, want)
		}
	}
}

// TestPagePageEvent reads the page_event off the payload and degrades to "" for
// an absent/garbled body (→ the generic phrase).
func TestPagePageEvent(t *testing.T) {
	if ev := pagePageEvent(campaignGatePagedEntry(1, "reviewer_reject").Payload); ev != "reviewer_reject" {
		t.Errorf("page_event = %q, want reviewer_reject", ev)
	}
	if ev := pagePageEvent(nil); ev != "" {
		t.Errorf("nil payload page_event = %q, want empty", ev)
	}
	if ev := pagePageEvent([]byte("{not json")); ev != "" {
		t.Errorf("garbled payload page_event = %q, want empty", ev)
	}
}

// TestClarificationQuestionCount reads the question count off the nested
// payload and degrades to 0 (→ count-free phrase) for an absent/garbled body.
func TestClarificationQuestionCount(t *testing.T) {
	if n := clarificationQuestionCount(clarificationEntry(1, "a", "b", "c").Payload); n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
	if n := clarificationQuestionCount(nil); n != 0 {
		t.Errorf("nil payload count = %d, want 0", n)
	}
	if n := clarificationQuestionCount([]byte("{not json")); n != 0 {
		t.Errorf("garbled payload count = %d, want 0", n)
	}
}

// acceptanceTriageEntry builds an acceptance_triage_decided audit entry with
// the given class + disposition.
func acceptanceTriageEntry(seq int64, class, disposition string) *audit.Entry {
	payload, _ := json.Marshal(map[string]any{"class": class, "disposition": disposition})
	return &audit.Entry{Sequence: seq, Category: "acceptance_triage_decided", Payload: payload}
}

// TestPageClassEvents_AcceptanceTriage covers the E31.8 (#1536) page-class
// ping: the human-needed dispositions each fire exactly one ping naming the
// class + disposition, while the auto-routed dispositions fire none.
func TestPageClassEvents_AcceptanceTriage(t *testing.T) {
	pagedDispositions := []string{
		"paged", "rerun_budget_exhausted",
		"fixup_unavailable_paged", "retry_unavailable_paged", "unsettled_paged",
		"externally_unvalidatable_paged", // #1671 class-5 terminal page
	}
	for _, disp := range pagedDispositions {
		entries := []*audit.Entry{acceptanceTriageEntry(9, "3", disp)}
		got := pageClassEvents(entries, nil)
		if len(got) != 1 {
			t.Fatalf("disposition %q: events = %d, want 1", disp, len(got))
		}
		if got[0].kind != "acceptance_triage" {
			t.Errorf("disposition %q: kind = %q, want acceptance_triage", disp, got[0].kind)
		}
		for _, want := range []string{"class-3", disp} {
			if !strings.Contains(got[0].message, want) {
				t.Errorf("disposition %q: message %q missing %q", disp, got[0].message, want)
			}
		}
	}

	// Auto-routed dispositions stay edit-only (the fixup/retry surfaces render).
	for _, disp := range []string{"fixup_dispatched", "retry_dispatched"} {
		entries := []*audit.Entry{acceptanceTriageEntry(9, "1", disp)}
		if got := pageClassEvents(entries, nil); len(got) != 0 {
			t.Errorf("disposition %q must not page; got %+v", disp, got)
		}
	}

	// A malformed / empty payload fires no ping.
	if got := pageClassEvents([]*audit.Entry{{Sequence: 9, Category: "acceptance_triage_decided"}}, nil); len(got) != 0 {
		t.Errorf("empty payload must not page; got %+v", got)
	}
}

// TestAcceptanceTriageNeedsHuman_Class5 pins the #1671 class-5 disposition: a
// class-5 externally_unvalidatable_paged payload needs a human (ok=true), and
// the exact literal is asserted per-package (binding condition 3) so a silent
// value drift from the server const is test-caught here.
func TestAcceptanceTriageNeedsHuman_Class5(t *testing.T) {
	const disposition = "externally_unvalidatable_paged"
	payload, _ := json.Marshal(map[string]any{"class": "5", "disposition": disposition})
	class, got, ok := acceptanceTriageNeedsHuman(payload)
	if !ok {
		t.Fatalf("acceptanceTriageNeedsHuman ok = false, want true for %q", disposition)
	}
	if class != "5" || got != disposition {
		t.Errorf("class/disposition = %q/%q, want 5/%q", class, got, disposition)
	}
}
