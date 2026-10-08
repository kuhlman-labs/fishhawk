package plan_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// --- comms_report schema (#4015, slice 0) ---
//
// The schema is not embedded yet (the embed and ParseCommsReport land with the
// ingest handler), so these tests compile the BACKEND MIRROR directly from
// disk: what slice 2 will embed is exactly what is tested here, and the
// canonical/mirror byte-equality row keeps scripts/sync-schemas honest.

const (
	commsCanonicalSchemaPath = "../../../docs/spec/comms-report-v1.schema.json"
	commsMirrorSchemaPath    = "schemas/comms-report-v1.schema.json"
	commsExamplePath         = "../../../docs/spec/examples/comms-report-v1-example.json"
)

func commsSchemaDoc(t *testing.T) map[string]any {
	t.Helper()
	b, err := os.ReadFile(commsMirrorSchemaPath)
	if err != nil {
		t.Fatalf("read mirror schema: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse mirror schema: %v", err)
	}
	return doc
}

func commsSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	c := jsonschema.NewCompiler()
	if err := c.AddResource("comms-report-v1.schema.json", commsSchemaDoc(t)); err != nil {
		t.Fatalf("register schema: %v", err)
	}
	s, err := c.Compile("comms-report-v1.schema.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return s
}

// commsSchemaValidate validates a JSON document against the mirror schema.
func commsSchemaValidate(t *testing.T, body []byte) error {
	t.Helper()
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("decode document: %v", err)
	}
	return commsSchema(t).Validate(doc)
}

func commsExampleBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(commsExamplePath)
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	return b
}

// commsExampleMutated decodes the example, applies mutate and re-encodes.
func commsExampleMutated(t *testing.T, mutate func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(commsExampleBytes(t), &m); err != nil {
		t.Fatalf("decode example: %v", err)
	}
	mutate(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return b
}

func commsDraftAt(m map[string]any, i int) map[string]any {
	return m["drafts"].([]any)[i].(map[string]any)
}

func commsIssueAt(m map[string]any, i int) map[string]any {
	return commsDraftAt(m, i)["proposed_issue"].(map[string]any)
}

func commsNDriftAt(m map[string]any, i int) map[string]any {
	return m["n_drift"].([]any)[i].(map[string]any)
}

// commsDecode strictly decodes a comms report, the way ParseCommsReport will.
func commsDecode(t *testing.T, body []byte) *plan.CommsReport {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	var r plan.CommsReport
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	return &r
}

func TestCommsReportSchema_ShippedExampleValid(t *testing.T) {
	body := commsExampleBytes(t)
	if err := commsSchemaValidate(t, body); err != nil {
		t.Fatalf("example fails the mirror schema: %v", err)
	}
	r := commsDecode(t, body)
	if err := plan.CheckCommsReportSemantics(r); err != nil {
		t.Fatalf("example fails the semantic rules: %v", err)
	}
	if len(r.Drafts) != 1 || len(r.NDrift) != 1 || len(r.NotDrafted) != 1 {
		t.Errorf("example drafts=%d n_drift=%d not_drafted=%d, want 1/1/1", len(r.Drafts), len(r.NDrift), len(r.NotDrafted))
	}
	want := []string{"UR-comment-12-7", "UR-issue-12", "UR-issue-40", "UR-issue-41"}
	if got := r.CitedReportIDs(); !reflect.DeepEqual(got, want) {
		t.Errorf("CitedReportIDs = %v, want %v", got, want)
	}
}

func TestCommsReportSchema_CanonicalMatchesMirror(t *testing.T) {
	canonical, err := os.ReadFile(commsCanonicalSchemaPath)
	if err != nil {
		t.Fatalf("read canonical: %v", err)
	}
	mirror, err := os.ReadFile(commsMirrorSchemaPath)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if !bytes.Equal(canonical, mirror) {
		t.Fatalf("%s differs from %s — run scripts/sync-schemas", commsMirrorSchemaPath, commsCanonicalSchemaPath)
	}
}

// commsReportIDs returns n distinct canonical report ids, sorted ascending.
func commsReportIDs(n int) []any {
	out := make([]any, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("UR-issue-%d", 100+i))
	}
	return out
}

// TestCommsReportSchema_NegativeRows: each row breaks exactly one schema
// constraint and must be refused by the schema.
func TestCommsReportSchema_NegativeRows(t *testing.T) {
	draft := func(m map[string]any) map[string]any { return commsDraftAt(m, 0) }
	cases := []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"26 drafts", func(m map[string]any) {
			var ds []any
			for i := 0; i < plan.CommsMaxDrafts+1; i++ {
				ds = append(ds, draft(m))
			}
			m["drafts"] = ds
		}},
		{"51 n_drift", func(m map[string]any) {
			var ns []any
			for i := 0; i < plan.CommsMaxNDrift+1; i++ {
				ns = append(ns, commsNDriftAt(m, 0))
			}
			m["n_drift"] = ns
		}},
		{"501 not_drafted", func(m map[string]any) {
			var ns []any
			for i := 0; i < plan.CommsMaxNotDrafted+1; i++ {
				ns = append(ns, map[string]any{"report_id": fmt.Sprintf("UR-issue-%d", i+1), "reason": "noise"})
			}
			m["not_drafted"] = ns
		}},
		{"21 source report ids", func(m map[string]any) {
			draft(m)["source_report_ids"] = commsReportIDs(plan.CommsMaxSourceReportIDs + 1)
		}},
		{"empty source report ids", func(m map[string]any) { draft(m)["source_report_ids"] = []any{} }},
		{"duplicate source report ids", func(m map[string]any) {
			draft(m)["source_report_ids"] = []any{"UR-issue-12", "UR-issue-12"}
		}},
		{"9 rubric citations", func(m map[string]any) {
			var cs []any
			for i := 0; i < plan.CommsMaxRubricCitations+1; i++ {
				cs = append(cs, map[string]any{"rubric_id": fmt.Sprintf("U%d", i+1)})
			}
			draft(m)["rubric_citations"] = cs
		}},
		{"no rubric citation", func(m map[string]any) { draft(m)["rubric_citations"] = []any{} }},
		{"lowercase rubric id", func(m map[string]any) {
			draft(m)["rubric_citations"] = []any{map[string]any{"rubric_id": "u1"}}
		}},
		{"UR-unknown report id", func(m map[string]any) {
			m["not_drafted"].([]any)[0].(map[string]any)["report_id"] = "UR-unknown-12-7"
		}},
		{"leading-zero report id", func(m map[string]any) {
			m["not_drafted"].([]any)[0].(map[string]any)["report_id"] = "UR-issue-012"
		}},
		{"zero report id", func(m map[string]any) {
			m["not_drafted"].([]any)[0].(map[string]any)["report_id"] = "UR-issue-0"
		}},
		{"bad draft id shape", func(m map[string]any) { draft(m)["id"] = "d-1" }},
		{"bad n_drift id shape", func(m map[string]any) { commsNDriftAt(m, 0)["id"] = "drift:N2" }},
		{"rubric-shaped non_goal_id", func(m map[string]any) { commsNDriftAt(m, 0)["non_goal_id"] = "U2" }},
		{"empty n_drift note", func(m map[string]any) { commsNDriftAt(m, 0)["note"] = "" }},
		{"unknown not_drafted reason", func(m map[string]any) {
			m["not_drafted"].([]any)[0].(map[string]any)["reason"] = "spam"
		}},
		{"title over 200 characters", func(m map[string]any) {
			commsIssueAt(m, 0)["title"] = strings.Repeat("a", plan.CommsMaxTitleBytes+1)
		}},
		{"body over 20000 characters", func(m map[string]any) {
			commsIssueAt(m, 0)["body"] = strings.Repeat("a", plan.CommsMaxBodyBytes+1)
		}},
		{"empty title", func(m map[string]any) { commsIssueAt(m, 0)["title"] = "" }},
		{"duplicate labels", func(m map[string]any) { commsIssueAt(m, 0)["labels"] = []any{"area:x", "area:x"} }},
		{"extra top-level property", func(m map[string]any) { m["extra"] = true }},
		{"extra draft property", func(m map[string]any) { draft(m)["score"] = 1 }},
		{"missing not_drafted", func(m map[string]any) { delete(m, "not_drafted") }},
		{"wrong kind", func(m map[string]any) { m["kind"] = "upkeep_report" }},
		{"wrong report_version", func(m map[string]any) { m["report_version"] = "comms_report_v2" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := commsSchemaValidate(t, commsExampleMutated(t, tc.mutate)); err == nil {
				t.Fatal("schema accepted the document, want a violation")
			}
		})
	}
}

// commsSchemaAt walks the schema document by property names.
func commsSchemaAt(t *testing.T, doc map[string]any, path ...string) map[string]any {
	t.Helper()
	cur := doc
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			t.Fatalf("schema has no object at %v (stopped at %q)", path, p)
		}
		cur = next
	}
	return cur
}

func commsSchemaNumber(t *testing.T, node map[string]any, key string) int {
	t.Helper()
	f, ok := node[key].(float64)
	if !ok {
		t.Fatalf("schema node has no numeric %q: %v", key, node)
	}
	return int(f)
}

// TestCommsReport_BoundsMatchSchema pins every Go-side bound and the reason
// enum to the schema (the single Go-side owner of each value).
func TestCommsReport_BoundsMatchSchema(t *testing.T) {
	doc := commsSchemaDoc(t)
	props := commsSchemaAt(t, doc, "properties")
	defs := commsSchemaAt(t, doc, "$defs")
	cases := []struct {
		name string
		node map[string]any
		key  string
		want int
	}{
		{"drafts maxItems", commsSchemaAt(t, props, "drafts"), "maxItems", plan.CommsMaxDrafts},
		{"n_drift maxItems", commsSchemaAt(t, props, "n_drift"), "maxItems", plan.CommsMaxNDrift},
		{"not_drafted maxItems", commsSchemaAt(t, props, "not_drafted"), "maxItems", plan.CommsMaxNotDrafted},
		{"source_report_ids maxItems", commsSchemaAt(t, defs, "source-report-ids"), "maxItems", plan.CommsMaxSourceReportIDs},
		{"rubric_citations maxItems", commsSchemaAt(t, defs, "draft", "properties", "rubric_citations"), "maxItems", plan.CommsMaxRubricCitations},
		{"title maxLength", commsSchemaAt(t, defs, "proposed-issue", "properties", "title"), "maxLength", plan.CommsMaxTitleBytes},
		{"body maxLength", commsSchemaAt(t, defs, "proposed-issue", "properties", "body"), "maxLength", plan.CommsMaxBodyBytes},
	}
	for _, tc := range cases {
		if got := commsSchemaNumber(t, tc.node, tc.key); got != tc.want {
			t.Errorf("%s = %d in the schema, %d in plan", tc.name, got, tc.want)
		}
	}
	var enum []string
	for _, v := range commsSchemaAt(t, defs, "not-drafted", "properties", "reason")["enum"].([]any) {
		enum = append(enum, v.(string))
	}
	if !reflect.DeepEqual(enum, plan.CommsNotDraftedReasons()) {
		t.Errorf("schema reason enum %v != plan.CommsNotDraftedReasons() %v", enum, plan.CommsNotDraftedReasons())
	}
	if got := commsSchemaAt(t, props, "kind")["const"]; got != plan.KindCommsReport {
		t.Errorf("schema kind const %v != plan.KindCommsReport %q", got, plan.KindCommsReport)
	}
	if got := commsSchemaAt(t, props, "report_version")["const"]; got != plan.CommsReportVersion {
		t.Errorf("schema report_version const %v != plan.CommsReportVersion %q", got, plan.CommsReportVersion)
	}
}

// commsPromptTrigger is a minimal comms-scan trigger: two shown reports and
// one conforming rubric line, enough for buildCommsScan to render the
// contract.
func commsPromptTrigger() prompt.Trigger {
	report := func(n int) prompt.UserReport {
		return prompt.UserReport{
			Kind: "issue", IssueNumber: n, ReportTitle: "Export broken", ReportBody: "It fails.",
			AuthorLogin: "someone", Association: "NONE", AssociationResolved: true,
			Classification: "external", ClassificationBasis: "association:NONE",
		}
	}
	return prompt.Trigger{
		Source:           "issue",
		IssueNumber:      4015,
		IssueTitle:       "Weekly comms scan",
		IssueURL:         "https://github.com/kuhlman-labs/fishhawk/issues/4015",
		Repo:             "kuhlman-labs/fishhawk",
		PlanStageTimeout: 30 * time.Minute,
		Comms: &prompt.CommsScanContext{
			Repo:        "kuhlman-labs/fishhawk",
			UserReports: []prompt.UserReport{report(12), report(40)},
			Rubric:      []prompt.CommsCharterLine{{ID: "U1", Text: "Correctness first."}},
			NonGoals:    []prompt.CommsCharterLine{{ID: "N2", Text: "No hosted offering."}},
		},
	}
}

// TestCommsReport_PromptParity is the #4013 obligation: the comms scan
// prompt (backend/internal/prompt, deliberately not edited here) states the
// same contract this validator enforces. Its literals and its rendered
// bounds, id derivations, reason order and label namespaces must equal
// plan's. It lives in the EXTERNAL plan_test package because prompt imports
// plan.
func TestCommsReport_PromptParity(t *testing.T) {
	if prompt.CommsReportKind != plan.KindCommsReport {
		t.Errorf("prompt.CommsReportKind %q != plan.KindCommsReport %q", prompt.CommsReportKind, plan.KindCommsReport)
	}
	if prompt.CommsReportVersion != plan.CommsReportVersion {
		t.Errorf("prompt.CommsReportVersion %q != plan.CommsReportVersion %q", prompt.CommsReportVersion, plan.CommsReportVersion)
	}
	if !reflect.DeepEqual(prompt.CommsNotDraftedReasons, plan.CommsNotDraftedReasons()) {
		t.Errorf("prompt.CommsNotDraftedReasons %v != plan.CommsNotDraftedReasons() %v", prompt.CommsNotDraftedReasons, plan.CommsNotDraftedReasons())
	}

	got, err := prompt.Build("plan", commsPromptTrigger())
	if err != nil {
		t.Fatalf("prompt.Build: %v", err)
	}
	var labelNS []string
	for _, ns := range plan.CommsLabelNamespaces() {
		labelNS = append(labelNS, "`"+ns+"*`")
	}
	want := []string{
		fmt.Sprintf("`kind` MUST be `%s` and `report_version` MUST be `%s`", plan.KindCommsReport, plan.CommsReportVersion),
		fmt.Sprintf("`drafts` (at most %d), `n_drift` (at most %d) and `not_drafted` (at most %d)",
			plan.CommsMaxDrafts, plan.CommsMaxNDrift, plan.CommsMaxNotDrafted),
		fmt.Sprintf("holds 1 to %d report ids, sorted and unique", plan.CommsMaxSourceReportIDs),
		fmt.Sprintf("`rubric_citations` holds 1 to %d `{rubric_id, note?}` entries, sorted by rubric_id and unique", plan.CommsMaxRubricCitations),
		fmt.Sprintf("`title` is ONE line of at most %d bytes", plan.CommsMaxTitleBytes),
		fmt.Sprintf("`body` at most %d bytes", plan.CommsMaxBodyBytes),
		// The prompt's worked draft id is plan's derivation of its ids.
		"(e.g. `" + plan.CommsDraftID([]string{"UR-issue-40", "UR-issue-12"}) + "`)",
		"DERIVED as `ndrift:` + non_goal_id + `:` + the sorted source_report_ids joined with `+`",
		"`reason` one of `" + strings.Join(plan.CommsNotDraftedReasons(), "`, `") + "`",
		"may carry ONLY " + labelNS[0] + ", " + labelNS[1] + " and " + labelNS[2] + " labels — NEVER `autonomy:*`",
	}
	for _, w := range want {
		if !strings.Contains(got, w) {
			t.Errorf("comms scan prompt does not state %q — the prompt contract and plan's validator have drifted", w)
		}
	}
	if len(plan.CommsLabelNamespaces()) != 3 {
		t.Errorf("plan.CommsLabelNamespaces() = %v; the prompt states exactly three namespaces", plan.CommsLabelNamespaces())
	}
	// The ndrift derivation the prompt states, worked through plan.
	if id := plan.CommsNDriftID("N2", []string{"UR-issue-41", "UR-comment-12-7"}); id != "ndrift:N2:UR-comment-12-7+UR-issue-41" {
		t.Errorf("CommsNDriftID = %q", id)
	}
}
