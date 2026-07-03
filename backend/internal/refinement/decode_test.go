package refinement

import (
	"strings"
	"testing"
)

const wellFormedDraftJSON = `{
  "epic": {"summary": "stand up X", "scope": "X wiring", "out_of_scope": ""},
  "children": [
    {"summary": "child one", "proposal": "do it", "done_means": "done",
     "acceptance_criteria": ["works"], "labels": ["area:backend"], "depends_on": []}
  ]
}`

func TestDecodeDraft_Plain(t *testing.T) {
	d, err := DecodeDraft([]byte(wellFormedDraftJSON))
	if err != nil {
		t.Fatalf("DecodeDraft: %v", err)
	}
	if d.Epic.Summary != "stand up X" || len(d.Children) != 1 {
		t.Fatalf("decoded draft = %+v, want epic 'stand up X' with 1 child", d)
	}
}

func TestDecodeDraft_ProsePrefixAndFenced(t *testing.T) {
	// The model both narrates AND wraps the JSON in a ```json fence — the two
	// classes DecodeDraft must tolerate, together.
	raw := "Here is the draft you asked for:\n\n```json\n" + wellFormedDraftJSON + "\n```\n"
	d, err := DecodeDraft([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeDraft on prose-prefixed fenced input: %v", err)
	}
	if d.Children[0].Summary != "child one" {
		t.Fatalf("decoded child summary = %q, want 'child one'", d.Children[0].Summary)
	}
}

func TestDecodeDraft_ProsePrefixAndSuffixUnfenced(t *testing.T) {
	// Prose on both sides, no fence — the balanced-brace scan must extract the
	// object and ignore the trailing narration.
	raw := "Sure! " + wellFormedDraftJSON + "\n\nLet me know if you want changes."
	d, err := DecodeDraft([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeDraft on prose-surrounded input: %v", err)
	}
	if d.Epic.Scope != "X wiring" {
		t.Fatalf("decoded epic scope = %q, want 'X wiring'", d.Epic.Scope)
	}
}

func TestDecodeDraft_BraceInStringValue(t *testing.T) {
	// A '}' inside a string value must not terminate the object early.
	raw := `prefix {"epic": {"summary": "use {curly} braces", "scope": "s", "out_of_scope": ""},
	 "children": [{"summary": "c", "proposal": "p", "done_means": "d",
	  "acceptance_criteria": ["a"], "labels": [], "depends_on": []}]} suffix`
	d, err := DecodeDraft([]byte(raw))
	if err != nil {
		t.Fatalf("DecodeDraft with brace-in-string: %v", err)
	}
	if d.Epic.Summary != "use {curly} braces" {
		t.Fatalf("decoded summary = %q, want 'use {curly} braces'", d.Epic.Summary)
	}
}

func TestDecodeDraft_UnknownFieldRejected(t *testing.T) {
	// An extra top-level field violates the closed field set (#1543/#1567).
	raw := `{"epic": {"summary": "s", "scope": "s", "out_of_scope": ""},
	 "children": [{"summary": "c", "proposal": "p", "done_means": "d",
	  "acceptance_criteria": ["a"], "labels": [], "depends_on": []}],
	 "extra_field": "not allowed"}`
	if _, err := DecodeDraft([]byte(raw)); err == nil {
		t.Fatal("DecodeDraft accepted an unknown top-level field")
	}
}

func TestDecodeDraft_UnknownFieldInChildRejected(t *testing.T) {
	// DisallowUnknownFields must apply to nested children[] elements too.
	raw := `{"epic": {"summary": "s", "scope": "s", "out_of_scope": ""},
	 "children": [{"summary": "c", "proposal": "p", "done_means": "d",
	  "acceptance_criteria": ["a"], "labels": [], "depends_on": [], "priority": "high"}]}`
	if _, err := DecodeDraft([]byte(raw)); err == nil {
		t.Fatal("DecodeDraft accepted an unknown field inside a child element")
	}
}

func TestDecodeDraft_MalformedPreservesStrictError(t *testing.T) {
	// Genuinely malformed JSON (unterminated) returns the strict-decode error,
	// not a nil or a masked one.
	raw := `{"epic": {"summary": "s"`
	_, err := DecodeDraft([]byte(raw))
	if err == nil {
		t.Fatal("DecodeDraft accepted truncated JSON")
	}
	// A json decode error, not some wrapped/replaced diagnostic.
	if !strings.Contains(err.Error(), "unexpected") && !strings.Contains(err.Error(), "EOF") {
		t.Errorf("error %q is not a strict json-decode diagnostic", err.Error())
	}
}

func TestDecodeDraft_NoObjectPreservesStrictError(t *testing.T) {
	// Input with no JSON object at all: firstJSONObject returns nil, and the
	// strict decode of the raw body produces the error.
	if _, err := DecodeDraft([]byte("no json here")); err == nil {
		t.Fatal("DecodeDraft accepted input with no JSON object")
	}
}
