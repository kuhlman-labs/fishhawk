package plan_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// --- comms_report semantic rules (#4015, slice 0) ---
//
// Every row starts from the SHIPPED example, mutates it, PROVES the result is
// schema-valid against the backend mirror (so no schema constraint can mask
// the rule under test), strictly decodes it and asserts a *SemanticError
// opening with the rule's JSON pointer. Rule (j) is the exception: the
// schema's const already refuses a wrong kind or report_version, so no
// schema-valid document can violate it — it is tested on a decoded struct.

func commsSemanticErr(t *testing.T, r *plan.CommsReport, pointer, contains string) {
	t.Helper()
	err := plan.CheckCommsReportSemantics(r)
	var sem *plan.SemanticError
	if !errors.As(err, &sem) {
		t.Fatalf("CheckCommsReportSemantics = %v, want *SemanticError", err)
	}
	if !strings.HasPrefix(sem.Message, pointer+":") {
		t.Errorf("semantic error %q does not open with pointer %s", sem.Message, pointer)
	}
	if contains != "" && !strings.Contains(sem.Message, contains) {
		t.Errorf("semantic error %q does not contain %q", sem.Message, contains)
	}
}

func TestCheckCommsReportSemantics_Rows(t *testing.T) {
	cases := []struct {
		name     string
		mutate   func(m map[string]any)
		pointer  string
		contains string
	}{
		{"a source ids unsorted", func(m map[string]any) {
			d := commsDraftAt(m, 0)
			d["source_report_ids"] = []any{"UR-issue-40", "UR-issue-12"}
			// The id stays the DERIVED one (derivation sorts), so only (a) fires.
		}, "/drafts/0/source_report_ids/1", "sort strictly after"},
		{"b draft id not derived", func(m map[string]any) {
			commsDraftAt(m, 0)["id"] = "draft:UR-issue-12"
		}, "/drafts/0/id", "derived id"},
		{"b draft id joined unsorted", func(m map[string]any) {
			commsDraftAt(m, 0)["id"] = "draft:UR-issue-40+UR-issue-12"
		}, "/drafts/0/id", "derived id"},
		{"c rubric citations unsorted", func(m map[string]any) {
			commsDraftAt(m, 0)["rubric_citations"] = []any{
				map[string]any{"rubric_id": "U2"}, map[string]any{"rubric_id": "U1"},
			}
		}, "/drafts/0/rubric_citations/1", "sort strictly after"},
		{"c rubric citation repeated with a different note", func(m map[string]any) {
			commsDraftAt(m, 0)["rubric_citations"] = []any{
				map[string]any{"rubric_id": "U1", "note": "one"}, map[string]any{"rubric_id": "U1", "note": "two"},
			}
		}, "/drafts/0/rubric_citations/1", "sort strictly after"},
		{"d rubric id of the non-goal shape", func(m map[string]any) {
			commsDraftAt(m, 0)["rubric_citations"] = []any{map[string]any{"rubric_id": "N2"}}
		}, "/drafts/0/rubric_citations/0/rubric_id", "non-goal shape"},
		{"e n_drift id not derived", func(m map[string]any) {
			commsNDriftAt(m, 0)["id"] = "ndrift:N3:UR-issue-41"
		}, "/n_drift/0/id", "derived id"},
		{"e n_drift source ids unsorted", func(m map[string]any) {
			n := commsNDriftAt(m, 0)
			n["source_report_ids"] = []any{"UR-issue-41", "UR-issue-39"}
			n["id"] = "ndrift:N2:UR-issue-39+UR-issue-41"
		}, "/n_drift/0/source_report_ids/1", "sort strictly after"},
		{"f id in a draft and in not_drafted", func(m map[string]any) {
			m["not_drafted"].([]any)[0].(map[string]any)["report_id"] = "UR-issue-40"
		}, "/not_drafted/0/report_id", "already accounted for at /drafts/0/source_report_ids/1"},
		{"f id in a draft and in n_drift", func(m map[string]any) {
			n := commsNDriftAt(m, 0)
			n["source_report_ids"] = []any{"UR-issue-12"}
			n["id"] = "ndrift:N2:UR-issue-12"
		}, "/n_drift/0/source_report_ids/0", "already accounted for at /drafts/0/source_report_ids/0"},
		{"f id in two drafts", func(m map[string]any) {
			second := map[string]any{
				"id": "draft:UR-issue-12", "source_report_ids": []any{"UR-issue-12"},
				"rubric_citations": []any{map[string]any{"rubric_id": "U1"}},
				"proposed_issue":   map[string]any{"type": "bug", "title": "t", "body": "b", "labels": []any{}},
			}
			m["drafts"] = append(m["drafts"].([]any), second)
		}, "/drafts/1/source_report_ids/0", "already accounted for"},
		{"f id twice in not_drafted", func(m map[string]any) {
			nd := m["not_drafted"].([]any)
			m["not_drafted"] = append(nd, map[string]any{"report_id": "UR-comment-12-7", "reason": "noise"})
		}, "/not_drafted/1/report_id", "already accounted for at /not_drafted/0/report_id"},
		{"g title with a line feed", func(m map[string]any) {
			commsIssueAt(m, 0)["title"] = "first line\nsecond line"
		}, "/drafts/0/proposed_issue/title", "one line"},
		{"g title with a line separator rune", func(m map[string]any) {
			commsIssueAt(m, 0)["title"] = "first line\u2028second line"
		}, "/drafts/0/proposed_issue/title", "one line"},
		{"g title with a control rune", func(m map[string]any) {
			commsIssueAt(m, 0)["title"] = "bell\u0007"
		}, "/drafts/0/proposed_issue/title", "one line"},
		{"g title over 200 bytes within 200 characters", func(m map[string]any) {
			commsIssueAt(m, 0)["title"] = strings.Repeat("é", plan.CommsMaxTitleBytes/2+1)
		}, "/drafts/0/proposed_issue/title", "byte cap"},
		{"g body over 20000 bytes within 20000 characters", func(m map[string]any) {
			commsIssueAt(m, 0)["body"] = strings.Repeat("é", plan.CommsMaxBodyBytes/2+1)
		}, "/drafts/0/proposed_issue/body", "byte cap"},
		{"h autonomy label", func(m map[string]any) {
			commsIssueAt(m, 0)["labels"] = []any{"area:backend", "autonomy:high"}
		}, "/drafts/0/proposed_issue/labels/1", "sets an autonomy tier"},
		{"h autonomy label in another case", func(m map[string]any) {
			commsIssueAt(m, 0)["labels"] = []any{"Autonomy:low"}
		}, "/drafts/0/proposed_issue/labels/0", "sets an autonomy tier"},
		{"h foreign namespace", func(m map[string]any) {
			commsIssueAt(m, 0)["labels"] = []any{"priority:p1"}
		}, "/drafts/0/proposed_issue/labels/0", "outside the allowed namespaces"},
		{"h bare label", func(m map[string]any) {
			commsIssueAt(m, 0)["labels"] = []any{"good-first-issue"}
		}, "/drafts/0/proposed_issue/labels/0", "outside the allowed namespaces"},
		{"h allowed namespace with bad syntax", func(m map[string]any) {
			commsIssueAt(m, 0)["labels"] = []any{"area:back end"}
		}, "/drafts/0/proposed_issue/labels/0", "not a valid label name"},
		{"h allowed namespace with no value", func(m map[string]any) {
			commsIssueAt(m, 0)["labels"] = []any{"type:"}
		}, "/drafts/0/proposed_issue/labels/0", "not a valid label name"},
		{"i parent_epic zero", func(m map[string]any) {
			commsIssueAt(m, 0)["parent_epic"] = "#0"
		}, "/drafts/0/proposed_issue/parent_epic", "positive issue number"},
		{"i parent_epic not a number", func(m map[string]any) {
			commsIssueAt(m, 0)["parent_epic"] = "E35"
		}, "/drafts/0/proposed_issue/parent_epic", "positive issue number"},
		{"i parent_epic signed", func(m map[string]any) {
			commsIssueAt(m, 0)["parent_epic"] = "+35"
		}, "/drafts/0/proposed_issue/parent_epic", "positive issue number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := commsExampleMutated(t, tc.mutate)
			if err := commsSchemaValidate(t, body); err != nil {
				t.Fatalf("fixture is not schema-valid, so the schema could mask the rule: %v", err)
			}
			commsSemanticErr(t, commsDecode(t, body), tc.pointer, tc.contains)
		})
	}
}

// TestCheckCommsReportSemantics_KindAndVersion is rule (j). The schema's const
// is the FIRST line (asserted here: the mutated document is schema-invalid);
// (j) is the defence in depth for a caller holding a decoded struct that never
// went through the schema, so it is tested on the struct directly.
func TestCheckCommsReportSemantics_KindAndVersion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(r *plan.CommsReport)
		doc     func(m map[string]any)
		pointer string
	}{
		{"kind", func(r *plan.CommsReport) { r.Kind = "upkeep_report" }, func(m map[string]any) { m["kind"] = "upkeep_report" }, "/kind"},
		{"report_version", func(r *plan.CommsReport) { r.ReportVersion = "comms_report_v2" }, func(m map[string]any) { m["report_version"] = "comms_report_v2" }, "/report_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := commsSchemaValidate(t, commsExampleMutated(t, tc.doc)); err == nil {
				t.Fatal("schema accepted the wrong value; the const is meant to be the first line")
			}
			r := commsDecode(t, commsExampleBytes(t))
			tc.mutate(r)
			commsSemanticErr(t, r, tc.pointer, "is not")
		})
	}
}

func TestCheckCommsReportSemantics_NilAndEmpty(t *testing.T) {
	commsSemanticErr(t, nil, "/", "nil")
	r := commsDecode(t, commsExampleMutated(t, func(m map[string]any) {
		m["drafts"], m["n_drift"], m["not_drafted"] = []any{}, []any{}, []any{}
	}))
	if err := plan.CheckCommsReportSemantics(r); err != nil {
		t.Errorf("an empty report is valid (nothing shown), got %v", err)
	}
	if got := r.CitedReportIDs(); got == nil || len(got) != 0 {
		t.Errorf("CitedReportIDs on an empty report = %#v, want a non-nil empty slice", got)
	}
}

func TestCommsReportIDDerivation(t *testing.T) {
	in := []string{"UR-issue-40", "UR-comment-12-7", "UR-issue-12"}
	if got, want := plan.CommsDraftID(in), "draft:UR-comment-12-7+UR-issue-12+UR-issue-40"; got != want {
		t.Errorf("CommsDraftID = %q, want %q", got, want)
	}
	if in[0] != "UR-issue-40" {
		t.Errorf("CommsDraftID reordered its input: %v", in)
	}
	// Byte order, not numeric: UR-issue-12 sorts before UR-issue-9.
	if got, want := plan.CommsNDriftID("N7", []string{"UR-issue-9", "UR-issue-12"}), "ndrift:N7:UR-issue-12+UR-issue-9"; got != want {
		t.Errorf("CommsNDriftID = %q, want %q", got, want)
	}
}

func TestCommsParentEpicNumber(t *testing.T) {
	for ref, want := range map[string]int{"35": 35, "#35": 35, "#1": 1} {
		if n, ok := plan.CommsParentEpicNumber(ref); !ok || n != want {
			t.Errorf("CommsParentEpicNumber(%q) = %d, %v; want %d, true", ref, n, ok, want)
		}
	}
	for _, ref := range []string{"", "#", "0", "#0", "##3", "+3", "-3", " 3", "E3", "#3a", "99999999999999999999"} {
		if n, ok := plan.CommsParentEpicNumber(ref); ok {
			t.Errorf("CommsParentEpicNumber(%q) = %d, true; want invalid", ref, n)
		}
	}
}

func TestCommsIDShapes(t *testing.T) {
	for id, want := range map[string]bool{"U1": true, "R12": true, "N1": false, "u1": false, "U": false, "UU1": false} {
		if got := plan.CommsValidRubricID(id); got != want {
			t.Errorf("CommsValidRubricID(%q) = %v, want %v", id, got, want)
		}
	}
	for id, want := range map[string]bool{"N1": true, "N42": true, "U1": false, "N": false, "n1": false} {
		if got := plan.CommsValidNonGoalID(id); got != want {
			t.Errorf("CommsValidNonGoalID(%q) = %v, want %v", id, got, want)
		}
	}
	if got, want := plan.CommsLabelNamespaces(), []string{"area:", "type:", "phase:"}; !reflect.DeepEqual(got, want) {
		t.Errorf("CommsLabelNamespaces = %v, want %v", got, want)
	}
}
