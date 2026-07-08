package main

import (
	"reflect"
	"testing"
)

// TestStripReviewProse asserts the typed strip clears free_form + concern
// notes while leaving every gated key (verdict/authority/reviewer_*/
// reason and each concern's severity/category) intact.
func TestStripReviewProse(t *testing.T) {
	reviews := []PlanReview{
		{
			ReviewerKind:  "agent",
			ReviewerModel: "claude-opus-4-8",
			Authority:     "advisory",
			Verdict:       "approve_with_concerns",
			Reason:        "",
			FreeForm:      "the diff implements the plan cleanly, long prose here",
			Concerns: []PlanReviewConcern{
				{Severity: "low", Category: "scope", Note: "touched a file outside scope.files"},
				{Severity: "high", Category: "security", Note: "unvalidated input on the handler"},
			},
		},
	}

	stripReviewProse(reviews)

	rev := reviews[0]
	if rev.FreeForm != "" {
		t.Errorf("FreeForm = %q, want cleared", rev.FreeForm)
	}
	// Gated keys survive.
	if rev.Verdict != "approve_with_concerns" || rev.Authority != "advisory" || rev.ReviewerKind != "agent" || rev.ReviewerModel != "claude-opus-4-8" {
		t.Errorf("gated review keys altered: %+v", rev)
	}
	for i, c := range rev.Concerns {
		if c.Note != "" {
			t.Errorf("Concerns[%d].Note = %q, want cleared", i, c.Note)
		}
	}
	if rev.Concerns[0].Severity != "low" || rev.Concerns[0].Category != "scope" {
		t.Errorf("Concerns[0] keys altered: %+v", rev.Concerns[0])
	}
	if rev.Concerns[1].Severity != "high" || rev.Concerns[1].Category != "security" {
		t.Errorf("Concerns[1] keys altered: %+v", rev.Concerns[1])
	}
}

// TestStripReviewProse_EmptyAndNil confirms the strip is a no-op safe on
// nil and empty slices.
func TestStripReviewProse_EmptyAndNil(t *testing.T) {
	stripReviewProse(nil)
	stripReviewProse([]PlanReview{})
}

func TestCompactAuditPayload(t *testing.T) {
	// A representative review payload: top-level free_form + a verdict, and
	// nested prose under a slice of concern maps (the nested-strip case).
	reviewPayload := func() any {
		return map[string]any{
			"verdict":   "approve_with_concerns",
			"authority": "advisory",
			"free_form": "long reviewer prose",
			"concerns": []any{
				map[string]any{"severity": "low", "category": "scope", "free_form": "nested prose"},
			},
		}
	}
	// A representative issue-context payload: body + comments + a title.
	issuePayload := func() any {
		return map[string]any{
			"title":    "an issue",
			"number":   float64(1727),
			"body":     "the big issue body",
			"comments": []any{map[string]any{"body": "a comment body"}},
		}
	}

	tests := []struct {
		name             string
		payload          any
		dropIssueContext bool
		dropReviewProse  bool
		wantAbsentKeys   []string
		wantPresentKeys  []string
	}{
		{
			name:            "drop review prose removes free_form top and nested, keeps verdict",
			payload:         reviewPayload(),
			dropReviewProse: true,
			wantAbsentKeys:  []string{"free_form"},
			wantPresentKeys: []string{"verdict", "authority", "concerns"},
		},
		{
			name:             "drop issue context removes body+comments, keeps title",
			payload:          issuePayload(),
			dropIssueContext: true,
			wantAbsentKeys:   []string{"body", "comments"},
			wantPresentKeys:  []string{"title", "number"},
		},
		{
			name:             "both flags strip both denylists",
			payload:          map[string]any{"free_form": "x", "body": "y", "comments": []any{}, "verdict": "approve"},
			dropIssueContext: true,
			dropReviewProse:  true,
			wantAbsentKeys:   []string{"free_form", "body", "comments"},
			wantPresentKeys:  []string{"verdict"},
		},
		{
			name:            "neither flag is a pass-through (free_form retained)",
			payload:         map[string]any{"free_form": "x", "verdict": "approve"},
			wantPresentKeys: []string{"free_form", "verdict"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := compactAuditPayload(tc.payload, tc.dropIssueContext, tc.dropReviewProse)
			m, ok := got.(map[string]any)
			if !ok {
				t.Fatalf("result is not a map: %T", got)
			}
			for _, k := range tc.wantAbsentKeys {
				if _, present := m[k]; present {
					t.Errorf("key %q should be stripped, got %+v", k, m)
				}
			}
			for _, k := range tc.wantPresentKeys {
				if _, present := m[k]; !present {
					t.Errorf("key %q should survive, got %+v", k, m)
				}
			}
		})
	}
}

// TestCompactAuditPayload_NestedFreeForm pins that nested free_form under a
// slice of maps is stripped — a shallow top-level-only strip would leave it.
func TestCompactAuditPayload_NestedFreeForm(t *testing.T) {
	payload := map[string]any{
		"reviews": []any{
			map[string]any{"verdict": "approve", "free_form": "nested prose", "severity": "low"},
		},
	}
	got := compactAuditPayload(payload, false, true).(map[string]any)
	reviews := got["reviews"].([]any)
	nested := reviews[0].(map[string]any)
	if _, present := nested["free_form"]; present {
		t.Errorf("nested free_form should be stripped, got %+v", nested)
	}
	if nested["verdict"] != "approve" || nested["severity"] != "low" {
		t.Errorf("nested gated keys altered: %+v", nested)
	}
}

// TestCompactAuditPayload_NilAndNonMap confirms nil and non-map inputs pass
// through unchanged, and that a no-flag call returns the input untouched.
func TestCompactAuditPayload_NilAndNonMap(t *testing.T) {
	if got := compactAuditPayload(nil, true, true); got != nil {
		t.Errorf("nil payload should pass through as nil, got %+v", got)
	}
	if got := compactAuditPayload("a string", true, true); got != "a string" {
		t.Errorf("non-map payload should pass through unchanged, got %+v", got)
	}
	if got := compactAuditPayload(float64(42), true, true); got != float64(42) {
		t.Errorf("scalar payload should pass through unchanged, got %+v", got)
	}
	// No-flag call returns the exact same value (identity pass-through).
	in := map[string]any{"free_form": "x"}
	if got := compactAuditPayload(in, false, false); !reflect.DeepEqual(got, in) {
		t.Errorf("no-flag call should be a pass-through, got %+v", got)
	}
}
