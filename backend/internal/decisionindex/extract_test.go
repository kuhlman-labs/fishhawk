package decisionindex

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

var (
	testRunID     = uuid.MustParse("11111111-1111-1111-1111-111111111111")
	testStageID   = uuid.MustParse("22222222-2222-2222-2222-222222222222")
	testAccountID = uuid.MustParse("33333333-3333-3333-3333-333333333333")
	testTS        = time.Date(2026, 9, 28, 12, 0, 0, 123456789, time.FixedZone("x", 3600))
)

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

func entry(t *testing.T, category string, seq int64, payload map[string]any) *audit.Entry {
	t.Helper()
	run, stage, acct := testRunID, testStageID, testAccountID
	kind := audit.ActorUser
	subject := "operator@example"
	return &audit.Entry{
		Sequence:     seq,
		RunID:        &run,
		StageID:      &stage,
		AccountID:    &acct,
		Timestamp:    testTS,
		Category:     category,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      mustJSON(t, payload),
		EntryHash:    "hash-" + category,
	}
}

func baseContext() RowContext {
	return RowContext{
		Repo:            "kuhlman-labs/fishhawk",
		WorkflowID:      "feature_change",
		DoctrineVersion: "wfsha",
		StageKind:       "plan",
		TouchedPaths:    []string{"b.go", "a.go"},
		EscalationKeys:  []string{"k2", "k1"},
	}
}

// TestExtract_EveryDecisionClass drives one realistic payload per
// decision-bearing category (shapes copied from the emit sites in
// backend/internal/server) through Extract and checks the class-specific
// columns. The shared columns are checked once for every case.
func TestExtract_EveryDecisionClass(t *testing.T) {
	cases := []struct {
		category string
		payload  map[string]any
		want     func(r *Row) // fills the class-specific expectations
	}{
		{"approval_submitted", map[string]any{"decision": "reject", "reject_class": "approach", "rejection_comment": "wrong fork", "delegated": "rule-x"},
			func(r *Row) {
				r.DecisionClass, r.Outcome, r.RejectClass, r.ReasonKey, r.Delegated = ClassPlanApproval, "reject", "approach", "rejection_comment", true
			}},
		{"concern_waived", map[string]any{"concern_id": "c", "reason": "accepted risk", "stage_kind": "implement", "severity": "high", "category": "Test_Coverage"},
			func(r *Row) {
				r.DecisionClass, r.Outcome, r.ReasonKey = ClassConcernWaive, "waived", "reason"
				r.ConcernCategoryRaw, r.ConcernCategory, r.Severity = "Test_Coverage", "testing", "high"
			}},
		{"concern_deferred", map[string]any{"concern_id": "c", "reason": "later", "severity": "low", "category": "bug", "issue_number": 7},
			func(r *Row) {
				r.DecisionClass, r.Outcome, r.ReasonKey = ClassConcernDefer, "deferred", "reason"
				r.ConcernCategoryRaw, r.ConcernCategory, r.Severity = "bug", "correctness", "low"
			}},
		{"concern_addressed_by_condition", map[string]any{"concern_id": "c", "verdict": "approve", "category": "perf", "severity": "medium"},
			func(r *Row) {
				r.DecisionClass, r.Outcome = ClassConcernAddressedByCondition, "addressed"
				r.ConcernCategoryRaw, r.ConcernCategory, r.Severity = "perf", "performance", "medium"
			}},
		{"scope_amendment_decided", map[string]any{"amendment_id": "a", "decision": "approve", "reason": "coupled test", "decided_by": "op"},
			func(r *Row) { r.DecisionClass, r.Outcome, r.ReasonKey = ClassScopeAmendment, "approve", "reason" }},
		{"acceptance_triage_arbitrated", map[string]any{"reason": "env flake", "verdict": "fail", "triage_class": "env", "delegated": false},
			func(r *Row) { r.DecisionClass, r.Outcome, r.ReasonKey = ClassAcceptanceArbitration, "fail", "reason" }},
		{"merge_verdict_recorded", map[string]any{"verdict": "merge", "pr_url": "u", "delegated": false},
			func(r *Row) { r.DecisionClass, r.Outcome = ClassMergeVerdict, "merge" }},
		{"clarification_answered", map[string]any{"answers": []string{"x"}, "comment": "see thread"},
			func(r *Row) { r.DecisionClass, r.Outcome, r.ReasonKey = ClassClarification, "answered", "comment" }},
		{"grooming_disposition_recorded", map[string]any{"entry_id": "e1", "verdict": "approved"},
			func(r *Row) { r.DecisionClass, r.Outcome = ClassGroomingDisposition, "approved" }},
	}
	if len(cases) != len(DecisionBearingCategories()) {
		t.Fatalf("table covers %d categories, want all %d", len(cases), len(DecisionBearingCategories()))
	}
	for _, tc := range cases {
		t.Run(tc.category, func(t *testing.T) {
			e := entry(t, tc.category, 42, tc.payload)
			got, err := Extract(e, baseContext())
			if err != nil {
				t.Fatalf("Extract: %v", err)
			}
			run, stage, acct := testRunID, testStageID, testAccountID
			want := &Row{
				SourceSequence:  42,
				SourceEntryHash: "hash-" + tc.category,
				RunID:           run,
				StageID:         &stage,
				AccountID:       &acct,
				Repo:            "kuhlman-labs/fishhawk",
				WorkflowID:      "feature_change",
				DoctrineVersion: "wfsha",
				StageKind:       "plan",
				TouchedPaths:    []string{"a.go", "b.go"},
				EscalationKeys:  []string{"k1", "k2"},
				ActorKind:       "user",
				ActorSubject:    "operator@example",
				DecidedAt:       testTS.UTC().Truncate(time.Microsecond),
				ReasonSequence:  42,
			}
			tc.want(want)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("Extract(%s)\n got  %+v\n want %+v", tc.category, got, want)
			}
		})
	}
}

// TestExtract_RejectsNonDecisionCategory: an unindexed category returns
// ErrNotDecisionBearing and no row.
// COUNTERFACTUAL: make ClassFor return (ClassPlanApproval, true) for every
// category — RED.
func TestExtract_RejectsNonDecisionCategory(t *testing.T) {
	for _, c := range []string{"run_started", "escalation_fired"} {
		row, err := Extract(entry(t, c, 1, map[string]any{}), baseContext())
		if !errors.Is(err, ErrNotDecisionBearing) || row != nil {
			t.Errorf("Extract(%s) = %v, %v; want nil, ErrNotDecisionBearing", c, row, err)
		}
	}
}

// TestExtract_RejectsNilRunEntry: a decision-bearing entry with no run id is
// refused with ErrNoRun (counted by callers) rather than producing a row the
// run_id NOT NULL column would reject — or panicking on the dereference.
// COUNTERFACTUAL: delete the RunID == nil guard's body — RED (panic).
func TestExtract_RejectsNilRunEntry(t *testing.T) {
	e := entry(t, "approval_submitted", 1, map[string]any{"decision": "approve"})
	e.RunID = nil
	row, err := Extract(e, baseContext())
	if !errors.Is(err, ErrNoRun) || row != nil {
		t.Fatalf("Extract(nil run) = %v, %v; want nil, ErrNoRun", row, err)
	}
}

// TestExtract_RejectsNilEntry: a nil entry is an error, not a panic.
func TestExtract_RejectsNilEntry(t *testing.T) {
	if row, err := Extract(nil, baseContext()); !errors.Is(err, ErrNilEntry) || row != nil {
		t.Fatalf("Extract(nil) = %v, %v; want nil, ErrNilEntry", row, err)
	}
}

// TestExtract_UndecodablePayload: a payload that is not JSON fails loud; an
// empty or null payload is tolerated as "no keys".
func TestExtract_UndecodablePayload(t *testing.T) {
	e := entry(t, "approval_submitted", 1, nil)
	e.Payload = json.RawMessage(`{not json`)
	if row, err := Extract(e, baseContext()); err == nil || row != nil {
		t.Fatalf("Extract(bad payload) = %v, %v; want nil, error", row, err)
	}
	for _, p := range []string{"", "null", "{}"} {
		e.Payload = json.RawMessage(p)
		row, err := Extract(e, baseContext())
		if err != nil {
			t.Fatalf("Extract(payload %q): %v", p, err)
		}
		if row.Outcome != "" || row.ReasonKey != "" || row.RejectClass != "" || row.Delegated {
			t.Errorf("Extract(payload %q) decoded fields from nothing: %+v", p, row)
		}
	}
}

// TestExtract_RejectClass (binding condition 2): an approval_submitted
// reject_class lands on the row; an entry predating E75.1 indexes it empty; a
// non-approval class never reads the key.
// COUNTERFACTUAL: delete the RejectClass assignment — RED.
func TestExtract_RejectClass(t *testing.T) {
	got, err := Extract(entry(t, "approval_submitted", 1, map[string]any{"decision": "reject", "reject_class": "verification"}), baseContext())
	if err != nil || got.RejectClass != "verification" {
		t.Fatalf("reject_class = %q, %v; want verification", got.RejectClass, err)
	}
	got, _ = Extract(entry(t, "approval_submitted", 2, map[string]any{"decision": "reject", "rejection_comment": "no"}), baseContext())
	if got.RejectClass != "" {
		t.Errorf("historical reject_class = %q, want empty", got.RejectClass)
	}
	got, _ = Extract(entry(t, "scope_amendment_decided", 3, map[string]any{"decision": "deny", "reject_class": "scope"}), baseContext())
	if got.RejectClass != "" {
		t.Errorf("scope_amendment_decided reject_class = %q, want empty (only approval_submitted carries it)", got.RejectClass)
	}
}

// TestExtract_ConcernCategoryFallback: a concern-class payload's own
// category/severity keys win — INCLUDING when present-but-empty, which an E75.1
// emitter records deliberately; only an ABSENT key (an entry predating E75.1)
// falls back to the resolver's review_concerns values, and with no resolver
// value the field indexes empty. Non-concern classes ignore both sources.
func TestExtract_ConcernCategoryFallback(t *testing.T) {
	rc := baseContext()
	rc.ConcernCategory, rc.ConcernSeverity = "security", "critical"

	got, _ := Extract(entry(t, "concern_addressed_by_condition", 1, map[string]any{"verdict": "approve"}), rc)
	if got.ConcernCategoryRaw != "security" || got.ConcernCategory != "correctness" || got.Severity != "critical" {
		t.Errorf("absent keys: raw=%q canonical=%q severity=%q; want resolver fallback security/correctness/critical",
			got.ConcernCategoryRaw, got.ConcernCategory, got.Severity)
	}
	got, _ = Extract(entry(t, "concern_addressed_by_condition", 2, map[string]any{"category": "", "severity": ""}), rc)
	if got.ConcernCategoryRaw != "" || got.ConcernCategory != "" || got.Severity != "" || got.ConcernCategoryUnmapped {
		t.Errorf("present-but-empty keys fell back to the resolver: %+v", got)
	}
	got, _ = Extract(entry(t, "concern_addressed_by_condition", 3, map[string]any{"verdict": "approve"}), baseContext())
	if got.ConcernCategoryRaw != "" || got.Severity != "" {
		t.Errorf("historical entry with no resolver value: raw=%q severity=%q, want empty", got.ConcernCategoryRaw, got.Severity)
	}
	got, _ = Extract(entry(t, "merge_verdict_recorded", 4, map[string]any{"verdict": "merge", "category": "bug", "severity": "high"}), rc)
	if got.ConcernCategoryRaw != "" || got.ConcernCategory != "" || got.Severity != "" {
		t.Errorf("non-concern class read concern fields: %+v", got)
	}
}

// TestExtract_UnmappedConcernCategory: an unknown category is indexed AS
// ITSELF and flagged, never dropped.
// COUNTERFACTUAL: set ConcernCategoryUnmapped to false unconditionally — RED.
func TestExtract_UnmappedConcernCategory(t *testing.T) {
	got, err := Extract(entry(t, "concern_waived", 1, map[string]any{"category": "Flakiness Under Race", "reason": "r"}), baseContext())
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if got.ConcernCategoryRaw != "Flakiness Under Race" || got.ConcernCategory != "flakiness-under-race" || !got.ConcernCategoryUnmapped {
		t.Errorf("unmapped category: raw=%q canonical=%q unmapped=%v; want verbatim raw, itself, true",
			got.ConcernCategoryRaw, got.ConcernCategory, got.ConcernCategoryUnmapped)
	}
	got, _ = Extract(entry(t, "concern_waived", 2, map[string]any{"category": "bug"}), baseContext())
	if got.ConcernCategoryUnmapped {
		t.Error("mapped category flagged unmapped")
	}
}

// TestExtract_DelegatedShapes: `delegated` is recorded as a rule-name string
// (approval, waive) or a bool (merge verdict, arbitration).
func TestExtract_DelegatedShapes(t *testing.T) {
	for _, tc := range []struct {
		v    any
		want bool
	}{{"operator-default", true}, {"", false}, {true, true}, {false, false}, {42, false}, {nil, false}} {
		got, _ := Extract(entry(t, "approval_submitted", 1, map[string]any{"delegated": tc.v}), baseContext())
		if got.Delegated != tc.want {
			t.Errorf("delegated=%#v -> %v, want %v", tc.v, got.Delegated, tc.want)
		}
	}
	got, _ := Extract(entry(t, "approval_submitted", 1, map[string]any{"decision": "approve"}), baseContext())
	if got.Delegated {
		t.Error("absent delegated key -> true, want false")
	}
}

// TestExtract_ReasonIsPointerNotCopy (ADR-082 rule 1): the reason is recorded
// as the source sequence plus the payload key; the prose appears in NO string
// field of the row. The ladder prefers `reason` over the comment keys.
func TestExtract_ReasonIsPointerNotCopy(t *testing.T) {
	const prose = "UNIQUE-REASON-PROSE"
	got, _ := Extract(entry(t, "concern_waived", 77, map[string]any{"reason": prose, "comment": "other"}), baseContext())
	if got.ReasonSequence != 77 || got.ReasonKey != "reason" {
		t.Errorf("reason pointer = (%d, %q), want (77, reason)", got.ReasonSequence, got.ReasonKey)
	}
	v := reflect.ValueOf(*got)
	for i := 0; i < v.NumField(); i++ {
		if s, ok := v.Field(i).Interface().(string); ok && strings.Contains(s, prose) {
			t.Errorf("Row.%s copies the reason prose", v.Type().Field(i).Name)
		}
	}
	got, _ = Extract(entry(t, "approval_submitted", 78, map[string]any{"decision": "approve", "comment": "ok"}), baseContext())
	if got.ReasonKey != "comment" {
		t.Errorf("approve reason key = %q, want comment", got.ReasonKey)
	}
	got, _ = Extract(entry(t, "scope_amendment_decided", 79, map[string]any{"decision": "approve", "reason": nil}), baseContext())
	if got.ReasonKey != "" {
		t.Errorf("null reason key = %q, want empty", got.ReasonKey)
	}
}

// TestExtract_DeterministicArrays: input order and duplicates in the context
// arrays never reach the row, and nil arrays become empty (never nil) so the
// stored TEXT[] is '{}' on both the writer and the backfill paths.
// COUNTERFACTUAL: replace sortedUnique's body with `return in` — RED.
func TestExtract_DeterministicArrays(t *testing.T) {
	a, b := baseContext(), baseContext()
	a.TouchedPaths, a.EscalationKeys = []string{"z", "a", "z", ""}, []string{"k", "j", "k"}
	b.TouchedPaths, b.EscalationKeys = []string{"a", "z"}, []string{"j", "k"}
	e := entry(t, "merge_verdict_recorded", 5, map[string]any{"verdict": "merge"})
	ra, _ := Extract(e, a)
	rb, _ := Extract(e, b)
	if !reflect.DeepEqual(ra, rb) {
		t.Errorf("permuted context produced different rows:\n %+v\n %+v", ra, rb)
	}
	c := baseContext()
	c.TouchedPaths, c.EscalationKeys = nil, nil
	rc, _ := Extract(e, c)
	if rc.TouchedPaths == nil || rc.EscalationKeys == nil || len(rc.TouchedPaths)+len(rc.EscalationKeys) != 0 {
		t.Errorf("nil context arrays -> %#v / %#v, want non-nil empty", rc.TouchedPaths, rc.EscalationKeys)
	}
}

// TestExtract_CopiesPointers: the row does not alias the entry's id pointers,
// so a caller mutating the entry afterwards cannot change an indexed row.
func TestExtract_CopiesPointers(t *testing.T) {
	e := entry(t, "merge_verdict_recorded", 5, map[string]any{"verdict": "merge"})
	e.AccountID = nil
	got, _ := Extract(e, baseContext())
	*e.StageID = uuid.New()
	if *got.StageID != testStageID {
		t.Error("Row.StageID aliases the entry's pointer")
	}
	if got.AccountID != nil {
		t.Errorf("nil account -> %v, want nil (the untenanted window)", got.AccountID)
	}
}

// TestExtract_DecidedAtIsUTCMicroseconds: decided_at is normalized to UTC at
// Postgres TIMESTAMPTZ precision, so a row built from an in-memory entry equals
// one rebuilt from the stored entry.
func TestExtract_DecidedAtIsUTCMicroseconds(t *testing.T) {
	got, _ := Extract(entry(t, "merge_verdict_recorded", 5, map[string]any{}), baseContext())
	if got.DecidedAt.Location() != time.UTC || got.DecidedAt.Nanosecond()%1000 != 0 || !got.DecidedAt.Equal(testTS.Truncate(time.Microsecond)) {
		t.Errorf("DecidedAt = %v, want %v in UTC", got.DecidedAt, testTS.UTC().Truncate(time.Microsecond))
	}
}

// TestTouchedPathsFromPlan covers the standard_v1 scope.files shape, a plan
// with no scope, empty content, and content that is not JSON.
func TestTouchedPathsFromPlan(t *testing.T) {
	got, err := TouchedPathsFromPlan([]byte(`{"scope":{"files":[{"path":"b.go","operation":"modify"},{"path":"a.go","operation":"create"},{"path":"b.go","operation":"modify"}]}}`))
	if err != nil || !reflect.DeepEqual(got, []string{"a.go", "b.go"}) {
		t.Errorf("TouchedPathsFromPlan = %v, %v; want [a.go b.go]", got, err)
	}
	for _, c := range []string{`{}`, `{"scope":null}`, ``} {
		got, err := TouchedPathsFromPlan([]byte(c))
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("TouchedPathsFromPlan(%q) = %#v, %v; want empty non-nil", c, got, err)
		}
	}
	if _, err := TouchedPathsFromPlan([]byte(`{bad`)); err == nil {
		t.Error("TouchedPathsFromPlan(bad json) = nil error, want error")
	}
}

func escalation(t *testing.T, seq int64, stage *uuid.UUID, keys []string) *audit.Entry {
	t.Helper()
	e := entry(t, "escalation_fired", seq, map[string]any{"fired_keys": keys})
	e.StageID = stage
	return e
}

// TestLatestEscalationKeys (binding condition 3): the resolver takes the
// fired_keys of the LATEST escalation_fired entry on the SAME stage with a
// sequence LOWER than the decision's, sorted and de-duplicated; an escalation on
// a DIFFERENT stage, or one at/after the decision, never lands.
// COUNTERFACTUAL: drop the `sid != *decisionStage` comparison — RED on the
// different-stage case; drop the `e.Sequence >= decisionSequence` bound — RED
// on the later-escalation case.
func TestLatestEscalationKeys(t *testing.T) {
	stage, other := testStageID, uuid.New()

	t.Run("same stage earlier lands", func(t *testing.T) {
		got := LatestEscalationKeys([]*audit.Entry{escalation(t, 10, &stage, []string{"rk-b", "rk-a", "rk-b"})}, &stage, 20)
		if !reflect.DeepEqual(got, []string{"rk-a", "rk-b"}) {
			t.Errorf("keys = %v, want [rk-a rk-b]", got)
		}
	})
	t.Run("different stage does not land", func(t *testing.T) {
		got := LatestEscalationKeys([]*audit.Entry{escalation(t, 10, &other, []string{"rk-x"})}, &stage, 20)
		if len(got) != 0 || got == nil {
			t.Errorf("keys = %#v, want empty non-nil", got)
		}
	})
	t.Run("later escalation does not land", func(t *testing.T) {
		got := LatestEscalationKeys([]*audit.Entry{escalation(t, 21, &stage, []string{"rk-late"}), escalation(t, 20, &stage, []string{"rk-same"})}, &stage, 20)
		if len(got) != 0 {
			t.Errorf("keys = %v, want empty (only strictly-lower sequences qualify)", got)
		}
	})
	t.Run("latest of several wins regardless of order", func(t *testing.T) {
		es := []*audit.Entry{
			escalation(t, 15, &stage, []string{"rk-new"}),
			escalation(t, 5, &stage, []string{"rk-old"}),
			escalation(t, 18, &other, []string{"rk-other"}),
			entry(t, "approval_submitted", 17, map[string]any{"fired_keys": []string{"rk-wrong-category"}}),
		}
		got := LatestEscalationKeys(es, &stage, 20)
		if !reflect.DeepEqual(got, []string{"rk-new"}) {
			t.Errorf("keys = %v, want [rk-new]", got)
		}
	})
	t.Run("payload stage_id fallback", func(t *testing.T) {
		e := entry(t, "escalation_fired", 10, map[string]any{"stage_id": stage.String(), "fired_keys": []string{"rk-p"}})
		e.StageID = nil
		if got := LatestEscalationKeys([]*audit.Entry{e}, &stage, 20); !reflect.DeepEqual(got, []string{"rk-p"}) {
			t.Errorf("keys = %v, want [rk-p]", got)
		}
		e.Payload = mustJSON(t, map[string]any{"fired_keys": []string{"rk-p"}})
		if got := LatestEscalationKeys([]*audit.Entry{e}, &stage, 20); len(got) != 0 {
			t.Errorf("stageless escalation keys = %v, want empty", got)
		}
	})
	t.Run("decision without a stage", func(t *testing.T) {
		got := LatestEscalationKeys([]*audit.Entry{escalation(t, 10, &stage, []string{"rk"})}, nil, 20)
		if got == nil || len(got) != 0 {
			t.Errorf("keys = %#v, want empty non-nil", got)
		}
	})
	t.Run("latest entry predates E75.1", func(t *testing.T) {
		old := entry(t, "escalation_fired", 12, map[string]any{"summary": "s", "fired": []int{0}})
		got := LatestEscalationKeys([]*audit.Entry{escalation(t, 10, &stage, []string{"rk"}), old}, &stage, 20)
		if len(got) != 0 {
			t.Errorf("keys = %v, want empty (the latest same-stage entry carries no fired_keys)", got)
		}
	})
}

// TestFiredKeys: decode, sort and de-duplicate; absent key or bad payload is
// empty, never nil.
func TestFiredKeys(t *testing.T) {
	if got := FiredKeys(mustJSON(t, map[string]any{"fired_keys": []string{"b", "a", "b"}})); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("FiredKeys = %v, want [a b]", got)
	}
	for _, p := range []string{`{}`, `{bad`, ``, `{"fired_keys":"x"}`} {
		if got := FiredKeys(json.RawMessage(p)); got == nil || len(got) != 0 {
			t.Errorf("FiredKeys(%q) = %#v, want empty non-nil", p, got)
		}
	}
}
