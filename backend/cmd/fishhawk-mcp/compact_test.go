package main

import (
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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

const truncMarker = "; full value via fishhawk_list_audit)"

// TestTruncateAuditPayloadStrings pins the #1749 payload-string truncation:
// (a) an oversized string is truncated to the cap with a marker reporting the
// elided byte count; (b) a short string is unchanged; (c) recursion truncates
// strings nested in maps and slices while structured keys and short values
// survive; (d) non-string scalars, nil, and a bare non-map/slice value pass
// through untouched; (e) cap<=0 is a pass-through.
func TestTruncateAuditPayloadStrings(t *testing.T) {
	long := strings.Repeat("a", 500)

	t.Run("oversized string truncated with elided-byte marker", func(t *testing.T) {
		payload := map[string]any{"note": long}
		got := truncateAuditPayloadStrings(payload, 100).(map[string]any)
		s := got["note"].(string)
		if len(s) >= len(long) {
			t.Fatalf("string not truncated: len=%d", len(s))
		}
		if !strings.Contains(s, truncMarker) {
			t.Errorf("truncated string missing marker: %q", s)
		}
		// The marker reports the elided original-byte count (500 - kept prefix).
		if !strings.Contains(s, "…(+") {
			t.Errorf("truncated string missing elided-byte prefix: %q", s)
		}
	})

	t.Run("short string unchanged", func(t *testing.T) {
		payload := map[string]any{"note": "short value"}
		got := truncateAuditPayloadStrings(payload, 100).(map[string]any)
		if got["note"] != "short value" {
			t.Errorf("short string altered: %q", got["note"])
		}
	})

	t.Run("recurses into nested maps and slices, keeps short values and keys", func(t *testing.T) {
		payload := map[string]any{
			"verdict": "approve",
			"nested":  map[string]any{"deep": long, "keep": "ok"},
			"list":    []any{long, "small", map[string]any{"inner": long}},
		}
		got := truncateAuditPayloadStrings(payload, 100).(map[string]any)
		if got["verdict"] != "approve" {
			t.Errorf("structured key altered: %q", got["verdict"])
		}
		nested := got["nested"].(map[string]any)
		if !strings.Contains(nested["deep"].(string), truncMarker) {
			t.Errorf("nested map string not truncated: %q", nested["deep"])
		}
		if nested["keep"] != "ok" {
			t.Errorf("nested short value altered: %q", nested["keep"])
		}
		list := got["list"].([]any)
		if !strings.Contains(list[0].(string), truncMarker) {
			t.Errorf("slice string not truncated: %q", list[0])
		}
		if list[1] != "small" {
			t.Errorf("slice short value altered: %q", list[1])
		}
		if !strings.Contains(list[2].(map[string]any)["inner"].(string), truncMarker) {
			t.Errorf("map-in-slice string not truncated: %q", list[2])
		}
	})

	t.Run("non-string scalars, nil, and bare values pass through", func(t *testing.T) {
		if got := truncateAuditPayloadStrings(nil, 100); got != nil {
			t.Errorf("nil should pass through, got %+v", got)
		}
		if got := truncateAuditPayloadStrings(float64(42), 100); got != float64(42) {
			t.Errorf("scalar should pass through, got %+v", got)
		}
		// A number nested in a map survives; only strings are touched.
		payload := map[string]any{"n": float64(7), "b": true, "z": nil}
		got := truncateAuditPayloadStrings(payload, 100).(map[string]any)
		if got["n"] != float64(7) || got["b"] != true || got["z"] != nil {
			t.Errorf("non-string values altered: %+v", got)
		}
	})

	t.Run("cap<=0 is a pass-through", func(t *testing.T) {
		payload := map[string]any{"note": long}
		got := truncateAuditPayloadStrings(payload, 0).(map[string]any)
		if got["note"] != long {
			t.Errorf("cap<=0 should not truncate: got %d bytes", len(got["note"].(string)))
		}
	})
}

// TestTruncateAuditPayloadStrings_UTF8Boundary asserts truncation never splits
// a multi-byte rune: a string of multi-byte runes straddling the cap must stay
// valid UTF-8 after truncation (the utf8.DecodeLastRuneInString back-off).
func TestTruncateAuditPayloadStrings_UTF8Boundary(t *testing.T) {
	// "€" is 3 bytes; 200 of them = 600 bytes, straddling a cap that does not
	// land on a rune boundary (100 is not a multiple of 3).
	multi := strings.Repeat("€", 200)
	payload := map[string]any{"note": multi}
	got := truncateAuditPayloadStrings(payload, 100).(map[string]any)
	s := got["note"].(string)
	// The whole marker-bearing result must be valid UTF-8 (the back-off never
	// leaves a split rune before the marker).
	if !utf8.ValidString(s) {
		t.Errorf("truncated multi-byte string is not valid UTF-8: %q", s)
	}
	if !strings.Contains(s, truncMarker) {
		t.Errorf("truncated multi-byte string missing marker: %q", s)
	}
	// The kept prefix must be a whole number of 3-byte runes (<=100 bytes).
	before := strings.SplitN(s, "…(+", 2)[0]
	if len(before)%3 != 0 {
		t.Errorf("prefix length %d is not a rune boundary (multiple of 3)", len(before))
	}
}

// TestCollapseCacheEfficiencyStages pins the #1749 cache-stage collapse:
// Stages is nilled, the run-level rollup scalars are intact, and the helper is
// nil-safe.
func TestCollapseCacheEfficiencyStages(t *testing.T) {
	ce := &CacheEfficiency{
		CacheReadRatio: 0.5,
		NetSavingsUSD:  16.75,
		Stages: []CacheEfficiencyStage{
			{Source: "agent"},
			{Source: "plan_review"},
		},
	}
	collapseCacheEfficiencyStages(ce)
	if ce.Stages != nil {
		t.Errorf("Stages should be nilled, got %+v", ce.Stages)
	}
	if ce.CacheReadRatio != 0.5 || ce.NetSavingsUSD != 16.75 {
		t.Errorf("rollup scalars altered: %+v", ce)
	}
	// nil-safe.
	collapseCacheEfficiencyStages(nil)
}
