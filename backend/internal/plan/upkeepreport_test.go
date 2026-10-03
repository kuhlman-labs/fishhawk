package plan_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// --- upkeep_report semantic rules (#3921, slice 1) ---
//
// Table-driven against STRUCT LITERALS, so no schema can mask a rule: each
// invalid row starts from upkeepFixture (valid) and introduces exactly ONE
// violation, so deleting that rule's branch makes CheckUpkeepReportSemantics
// return nil and the row goes RED.

const (
	upkeepRunA   = "6f1c2a3e-8b4d-4e5f-9a6b-7c8d9e0f1a2b"
	upkeepRunB   = "9b8a7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	upkeepStageA = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
)

func upkeepStrPtr(s string) *string { return &s }
func upkeepIntPtr(n int) *int       { return &n }

func upkeepRunRef(runID string) plan.UpkeepEvidenceRef {
	return plan.UpkeepEvidenceRef{Kind: plan.UpkeepEvidenceKindRun, RunID: runID}
}

func upkeepFileRef(path string, line *int) plan.UpkeepEvidenceRef {
	return plan.UpkeepEvidenceRef{Kind: plan.UpkeepEvidenceKindFile, Path: path, Line: line}
}

func upkeepFinding(source, subject string, ev ...plan.UpkeepEvidenceRef) plan.UpkeepFinding {
	return plan.UpkeepFinding{
		ID:       plan.UpkeepFindingID(source, subject),
		Source:   source,
		Subject:  subject,
		Evidence: ev,
		ProposedIssue: plan.UpkeepProposedIssue{
			Title:  "Fix " + subject,
			Body:   "body",
			Type:   "chore",
			Labels: []string{"area:backend", "type:bug"},
		},
	}
}

// upkeepFixture is a valid report with one finding per source: a flake citing
// a run (with a stage id), a drift naming two distinct lined paths, and a
// deprecation naming a file.
func upkeepFixture() *plan.UpkeepReport {
	flake := upkeepFinding(plan.UpkeepSourceFlake, "TestWidgetSync", upkeepRunRef(upkeepRunA))
	flake.Evidence[0].StageID = upkeepStrPtr(upkeepStageA)
	drift := upkeepFinding(plan.UpkeepSourceToolchainDrift, "go",
		upkeepFileRef("go.work", upkeepIntPtr(1)),
		upkeepFileRef("backend/Dockerfile", upkeepIntPtr(3)))
	drift.ProposedIssue.ParentEpic = upkeepStrPtr("#3726")
	dep := upkeepFinding(plan.UpkeepSourceDeprecation, "io/ioutil", upkeepFileRef("a/load.go", nil))
	dep.ProposedIssue.ParentEpic = upkeepStrPtr("3726")
	return &plan.UpkeepReport{
		Kind:           plan.KindUpkeepReport,
		ReportVersion:  plan.UpkeepReportVersion,
		Summary:        "scan",
		SourcesScanned: []string{plan.UpkeepSourceFlake, plan.UpkeepSourceToolchainDrift, plan.UpkeepSourceDeprecation},
		Findings:       []plan.UpkeepFinding{flake, drift, dep},
	}
}

// upkeepRunIDs returns n distinct, deterministic, non-nil run UUIDs.
func upkeepRunIDs(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = uuid.NewSHA1(uuid.NameSpaceOID, []byte{byte(i >> 8), byte(i)}).String()
	}
	return out
}

// upkeepFlakesCiting replaces the report's findings with flake findings that
// together cite ids (at most 50 refs per finding, as the schema would allow).
func upkeepFlakesCiting(r *plan.UpkeepReport, ids []string) {
	r.Findings = nil
	for chunk := 0; len(ids) > 0; chunk++ {
		n := min(50, len(ids))
		var ev []plan.UpkeepEvidenceRef
		for _, id := range ids[:n] {
			ev = append(ev, upkeepRunRef(id))
		}
		ids = ids[n:]
		r.Findings = append(r.Findings, upkeepFinding(plan.UpkeepSourceFlake, "TestChunk"+string(rune('A'+chunk)), ev...))
	}
}

func TestCheckUpkeepReportSemantics_Rules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(r *plan.UpkeepReport)
		pointer string // the SemanticError's leading JSON pointer; "" = valid
		substr  string
	}{
		{name: "fixture valid", mutate: func(*plan.UpkeepReport) {}},
		{
			name: "drift naming two distinct lined paths plus an unlined ref passes",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[1].Evidence = append(r.Findings[1].Evidence, upkeepFileRef("x.yaml", nil))
			},
		},
		{
			name:   "empty findings reads none found",
			mutate: func(r *plan.UpkeepReport) { r.Findings = []plan.UpkeepFinding{} },
		},
		{
			name: "(a) id not derived from source and subject",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[0].ID = "flake:TestA"
				r.Findings[0].Subject = "TestB"
			},
			pointer: "/findings/0/id", substr: `expected "flake:TestB"`,
		},
		{
			name: "(b) duplicate finding id",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings = append(r.Findings, r.Findings[0])
			},
			pointer: "/findings/3/id", substr: "already used by /findings/0",
		},
		{
			name: "(c) source absent from sources_scanned",
			mutate: func(r *plan.UpkeepReport) {
				r.SourcesScanned = []string{plan.UpkeepSourceFlake, plan.UpkeepSourceToolchainDrift}
			},
			pointer: "/findings/2/source", substr: "does not appear in sources_scanned",
		},
		{
			name: "(d) subject carries a control rune",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[0].Subject = "Test\u0007A"
				r.Findings[0].ID = plan.UpkeepFindingID(plan.UpkeepSourceFlake, "Test\u0007A")
			},
			pointer: "/findings/0/subject", substr: "control character",
		},
		{
			name:    "(e) run_id not a uuid",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].Evidence[0].RunID = "not-a-uuid" },
			pointer: "/findings/0/evidence/0/run_id", substr: "is not a UUID",
		},
		{
			name:    "(e) run_id is the nil uuid",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].Evidence[0].RunID = uuid.Nil.String() },
			pointer: "/findings/0/evidence/0/run_id", substr: "nil UUID",
		},
		{
			name:    "(e) stage_id malformed",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].Evidence[0].StageID = upkeepStrPtr("stage-1") },
			pointer: "/findings/0/evidence/0/stage_id", substr: "is not a UUID",
		},
		{
			name:    "(e) stage_id present but empty",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].Evidence[0].StageID = upkeepStrPtr("") },
			pointer: "/findings/0/evidence/0/stage_id", substr: "is not a UUID",
		},
		{
			name:    "(e) stage_id is the nil uuid",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].Evidence[0].StageID = upkeepStrPtr(uuid.Nil.String()) },
			pointer: "/findings/0/evidence/0/stage_id", substr: "nil UUID",
		},
		{
			name: "(f) flake citing only a file ref",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[0].Evidence = []plan.UpkeepEvidenceRef{upkeepFileRef("a_test.go", upkeepIntPtr(4))}
			},
			pointer: "/findings/0/evidence", substr: "at least one run ref",
		},
		{
			name: "(g) drift with two lined refs to the SAME path",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[1].Evidence = []plan.UpkeepEvidenceRef{
					upkeepFileRef("go.work", upkeepIntPtr(1)), upkeepFileRef("go.work", upkeepIntPtr(9)),
				}
			},
			pointer: "/findings/1/evidence", substr: "at least 2 DISTINCT paths",
		},
		{
			name: "(g) drift with two paths where one ref has no line",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[1].Evidence = []plan.UpkeepEvidenceRef{
					upkeepFileRef("go.work", upkeepIntPtr(1)), upkeepFileRef("backend/Dockerfile", nil),
				}
			},
			pointer: "/findings/1/evidence", substr: "(got 1)",
		},
		{
			name: "(h) deprecation citing only a run ref",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[2].Evidence = []plan.UpkeepEvidenceRef{upkeepRunRef(upkeepRunB)}
			},
			pointer: "/findings/2/evidence", substr: "at least one file ref",
		},
		{
			name:    "(i) label with whitespace",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].ProposedIssue.Labels = []string{"type:bug", "area: x"} },
			pointer: "/findings/0/proposed_issue/labels/1", substr: `"area: x"`,
		},
		{
			name:    "(i) label with leading punctuation",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].ProposedIssue.Labels = []string{":area"} },
			pointer: "/findings/0/proposed_issue/labels/0", substr: "not a valid label",
		},
		{
			name:    "(i) label with trailing symbol",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].ProposedIssue.Labels = []string{"area+"} },
			pointer: "/findings/0/proposed_issue/labels/0", substr: "not a valid label",
		},
		{
			name:    "(i) 51-rune label",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].ProposedIssue.Labels = []string{strings.Repeat("é", 51)} },
			pointer: "/findings/0/proposed_issue/labels/0", substr: "not a valid label",
		},
		{
			name:    "(i) empty label",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].ProposedIssue.Labels = []string{""} },
			pointer: "/findings/0/proposed_issue/labels/0", substr: "not a valid label",
		},
		{
			name:    "(j) parent_epic #0",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[1].ProposedIssue.ParentEpic = upkeepStrPtr("#0") },
			pointer: "/findings/1/proposed_issue/parent_epic", substr: "not a positive issue number",
		},
		{
			name:    "(j) parent_epic not numeric",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[1].ProposedIssue.ParentEpic = upkeepStrPtr("epic") },
			pointer: "/findings/1/proposed_issue/parent_epic", substr: "not a positive issue number",
		},
		{
			name:    "(j) parent_epic with two hashes",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[1].ProposedIssue.ParentEpic = upkeepStrPtr("##12") },
			pointer: "/findings/1/proposed_issue/parent_epic", substr: "not a positive issue number",
		},
		{
			name:    "(j) parent_epic with a sign",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[1].ProposedIssue.ParentEpic = upkeepStrPtr("+12") },
			pointer: "/findings/1/proposed_issue/parent_epic", substr: "not a positive issue number",
		},
		{
			name:    "(j) parent_epic bare hash",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[1].ProposedIssue.ParentEpic = upkeepStrPtr("#") },
			pointer: "/findings/1/proposed_issue/parent_epic", substr: "not a positive issue number",
		},
		{
			name:    "(k) 201 distinct run ids",
			mutate:  func(r *plan.UpkeepReport) { upkeepFlakesCiting(r, upkeepRunIDs(201)) },
			pointer: "/findings", substr: "cites 201 distinct run ids; at most 200",
		},
		{
			name:   "(k) exactly 200 distinct run ids passes",
			mutate: func(r *plan.UpkeepReport) { upkeepFlakesCiting(r, upkeepRunIDs(200)) },
		},
		{
			name: "(k) repeated ids count once",
			mutate: func(r *plan.UpkeepReport) {
				ids := upkeepRunIDs(200)
				upkeepFlakesCiting(r, append(ids, strings.ToUpper(ids[0]), ids[1]))
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := upkeepFixture()
			tc.mutate(r)
			err := plan.CheckUpkeepReportSemantics(r)
			if tc.pointer == "" {
				if err != nil {
					t.Fatalf("CheckUpkeepReportSemantics = %v, want nil", err)
				}
				return
			}
			var se *plan.SemanticError
			if !errors.As(err, &se) {
				t.Fatalf("CheckUpkeepReportSemantics = %v (%T), want *SemanticError at %s", err, err, tc.pointer)
			}
			if !strings.HasPrefix(se.Message, tc.pointer+":") {
				t.Errorf("SemanticError = %q, want pointer %q", se.Message, tc.pointer)
			}
			if !strings.Contains(se.Message, tc.substr) {
				t.Errorf("SemanticError = %q, want it to contain %q", se.Message, tc.substr)
			}
		})
	}
}

func TestCheckUpkeepReportSemantics_NilReport(t *testing.T) {
	var se *plan.SemanticError
	if err := plan.CheckUpkeepReportSemantics(nil); !errors.As(err, &se) {
		t.Fatalf("CheckUpkeepReportSemantics(nil) = %v, want *SemanticError", err)
	}
}

// TestUpkeepReport_CanonicalExampleDecodesAndPasses proves the struct's json
// tags match the shipped example (strict decode) and that the example obeys
// every semantic rule.
func TestUpkeepReport_CanonicalExampleDecodesAndPasses(t *testing.T) {
	body, err := os.ReadFile("../../../docs/spec/examples/upkeep-report-v1-example.json")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	var r plan.UpkeepReport
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("strict decode: %v", err)
	}
	if err := plan.CheckUpkeepReportSemantics(&r); err != nil {
		t.Fatalf("CheckUpkeepReportSemantics(example) = %v, want nil", err)
	}
	if r.Kind != plan.KindUpkeepReport || r.ReportVersion != plan.UpkeepReportVersion {
		t.Errorf("kind/version = %q/%q, want %q/%q", r.Kind, r.ReportVersion, plan.KindUpkeepReport, plan.UpkeepReportVersion)
	}
	if got := len(r.RunRefIDs()); got != 2 {
		t.Errorf("RunRefIDs = %d ids, want 2", got)
	}
}

func TestUpkeepFindingID(t *testing.T) {
	if got := plan.UpkeepFindingID(plan.UpkeepSourceDeprecation, "io/ioutil"); got != "deprecation:io/ioutil" {
		t.Errorf("UpkeepFindingID = %q, want %q", got, "deprecation:io/ioutil")
	}
}

func TestUpkeepReport_RunRefIDs(t *testing.T) {
	r := &plan.UpkeepReport{Findings: []plan.UpkeepFinding{
		upkeepFinding(plan.UpkeepSourceFlake, "TestA",
			upkeepRunRef(upkeepRunB),
			// A file ref is never a cited run, even carrying a run_id (the
			// schema forbids that; a struct literal can still build it).
			plan.UpkeepEvidenceRef{Kind: plan.UpkeepEvidenceKindFile, Path: "x.go", RunID: upkeepStageA},
			upkeepRunRef("not-a-uuid"),
			upkeepRunRef(uuid.Nil.String())),
		upkeepFinding(plan.UpkeepSourceFlake, "TestB",
			upkeepRunRef(strings.ToUpper(upkeepRunB)), // same id, other spelling
			upkeepRunRef(upkeepRunA)),
	}}
	got := r.RunRefIDs()
	want := []uuid.UUID{uuid.MustParse(upkeepRunB), uuid.MustParse(upkeepRunA)}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("RunRefIDs = %v, want %v (distinct, first-seen order)", got, want)
	}
	if ids := (*plan.UpkeepReport)(nil).RunRefIDs(); ids != nil {
		t.Errorf("nil report RunRefIDs = %v, want nil", ids)
	}
	if ids := (&plan.UpkeepReport{}).RunRefIDs(); ids == nil || len(ids) != 0 {
		t.Errorf("empty report RunRefIDs = %#v, want empty non-nil", ids)
	}
}
