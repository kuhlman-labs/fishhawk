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

// --- advisory source, rules (l)-(t) (#3750) ---
//
// Every row starts from the SHIPPED advisory example (strictly decoded, so its
// json tags are pinned too) and mutates ONE field, so exactly one rule is in
// the path: deleting that rule's branch turns the row's refusal into a nil.
// Finding 0 is a govulncheck `called`/high golang.org/x/net finding citing a
// manifest and a call-site source file; finding 1 a govulncheck
// `imported`/low golang.org/x/text finding; finding 2 a pnpm_audit
// `unanalyzed`/medium finding with fixed_version null.

const upkeepAdvisoryExamplePath = "../../../docs/spec/examples/upkeep-report-v1-advisory-example.json"

func upkeepAdvisoryExample(t *testing.T) *plan.UpkeepReport {
	t.Helper()
	body, err := os.ReadFile(upkeepAdvisoryExamplePath)
	if err != nil {
		t.Fatalf("read advisory example: %v", err)
	}
	var r plan.UpkeepReport
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		t.Fatalf("strict decode advisory example: %v", err)
	}
	return &r
}

// upkeepRekey re-derives finding i's id from its (possibly mutated) subject,
// so rule (a) never masks the rule a row exercises.
func upkeepRekey(r *plan.UpkeepReport, i int) {
	r.Findings[i].ID = plan.UpkeepFindingID(r.Findings[i].Source, r.Findings[i].Subject)
}

func TestCheckUpkeepReportSemantics_AdvisoryRules(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(r *plan.UpkeepReport)
		pointer string // "" = valid
		substr  string
	}{
		{name: "advisory example valid", mutate: func(*plan.UpkeepReport) {}},
		{
			name:    "(l) advisory finding without an advisory object",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].Advisory = nil },
			pointer: "/findings/0/advisory", substr: "must carry an advisory object",
		},
		{
			name: "(m) subject names another package",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[0].Subject = "GO-2024-2687:golang.org/x/text"
				upkeepRekey(r, 0)
			},
			pointer: "/findings/0/subject", substr: `expected "GO-2024-2687:golang.org/x/net"`,
		},
		{
			name: "(m) subject derived from an alias, not the primary id",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[0].Subject = "CVE-2023-45288:golang.org/x/net"
				upkeepRekey(r, 0)
			},
			pointer: "/findings/0/subject", substr: "primary advisory id",
		},
		{
			name:    "(n) govulncheck reporting npm",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].Advisory.Ecosystem = plan.UpkeepEcosystemNPM },
			pointer: "/findings/0/advisory/scanner", substr: `scanner "govulncheck" cannot report ecosystem "npm"`,
		},
		{
			name:    "(n) pnpm_audit reporting go",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[2].Advisory.Ecosystem = plan.UpkeepEcosystemGo },
			pointer: "/findings/2/advisory/scanner", substr: `scanner "pnpm_audit" cannot report ecosystem "go"`,
		},
		{
			name:   "(n) osv reporting npm passes",
			mutate: func(r *plan.UpkeepReport) { r.Findings[2].Advisory.Scanner = plan.UpkeepScannerOSV },
		},
		{
			name: "(n) osv reporting go passes",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[2].Advisory.Scanner = plan.UpkeepScannerOSV
				r.Findings[2].Advisory.Ecosystem = plan.UpkeepEcosystemGo
			},
		},
		{
			// The first frame names a package but no function, so the derived
			// level is imported; severity low so the cap cannot be what refuses.
			name: "(o) called claimed on a package-only call path",
			mutate: func(r *plan.UpkeepReport) {
				a := r.Findings[0].Advisory
				a.CallPath[0].Function, a.CallPath[0].Receiver = "", ""
				a.Severity = plan.UpkeepSeverityLow
			},
			pointer: "/findings/0/advisory/reachability", substr: `derived from call_path[0] ("imported")`,
		},
		{
			name:    "(o) govulncheck with an empty call path",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[0].Advisory.CallPath = nil },
			pointer: "/findings/0/advisory/reachability", substr: "non-empty call_path",
		},
		{
			name:    "(o) pnpm_audit claiming called",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[2].Advisory.Reachability = plan.UpkeepReachabilityCalled },
			pointer: "/findings/2/advisory/reachability", substr: `must be "unanalyzed"`,
		},
		{
			name: "(o) pnpm_audit carrying a call path",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[2].Advisory.CallPath = []plan.UpkeepAdvisoryFrame{{Module: "yaml-front-parser"}}
			},
			pointer: "/findings/2/advisory/reachability", substr: "1 call_path frames",
		},
		{
			name: "(o) package is a package path, not the module",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[0].Advisory.Package = "golang.org/x/net/http2"
				r.Findings[0].Subject = "GO-2024-2687:golang.org/x/net/http2"
				upkeepRekey(r, 0)
			},
			pointer: "/findings/0/advisory/package", substr: `not the vulnerable module "golang.org/x/net"`,
		},
		{
			name:    "(p) imported may not be high",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[1].Advisory.Severity = plan.UpkeepSeverityHigh },
			pointer: "/findings/1/advisory/severity", substr: `severity "high" exceeds the cap for reachability "imported"`,
		},
		{
			name:    "(p) imported may not be medium",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[1].Advisory.Severity = plan.UpkeepSeverityMedium },
			pointer: "/findings/1/advisory/severity", substr: "exceeds the cap",
		},
		{
			name: "(p) required may not be high",
			mutate: func(r *plan.UpkeepReport) {
				a := r.Findings[1].Advisory
				a.CallPath[0].Package = ""
				a.Reachability = plan.UpkeepReachabilityRequired
				a.Severity = plan.UpkeepSeverityHigh
			},
			pointer: "/findings/1/advisory/severity", substr: `for reachability "required"`,
		},
		{
			name: "(p) required at low passes",
			mutate: func(r *plan.UpkeepReport) {
				a := r.Findings[1].Advisory
				a.CallPath[0].Package = ""
				a.Reachability = plan.UpkeepReachabilityRequired
			},
		},
		{
			name:    "(p) unanalyzed may not be high",
			mutate:  func(r *plan.UpkeepReport) { r.Findings[2].Advisory.Severity = plan.UpkeepSeverityHigh },
			pointer: "/findings/2/advisory/severity", substr: `for reachability "unanalyzed"`,
		},
		{
			name:   "(p) unanalyzed at low passes",
			mutate: func(r *plan.UpkeepReport) { r.Findings[2].Advisory.Severity = plan.UpkeepSeverityLow },
		},
		{
			name:   "(p) called at medium passes",
			mutate: func(r *plan.UpkeepReport) { r.Findings[0].Advisory.Severity = plan.UpkeepSeverityMedium },
		},
		{
			name: "(q) advisory citing only a run ref",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[0].Evidence = []plan.UpkeepEvidenceRef{upkeepRunRef(upkeepRunA)}
			},
			pointer: "/findings/0/evidence", substr: "at least one manifest file ref",
		},
		{
			name: "(q) advisory citing only a call-site source file",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[0].Evidence = r.Findings[0].Evidence[1:]
			},
			pointer: "/findings/0/evidence", substr: "go.mod, package.json, pnpm-lock.yaml",
		},
		{
			name: "(q) go.sum is not a manifest",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[1].Evidence = []plan.UpkeepEvidenceRef{upkeepFileRef("runner/go.sum", upkeepIntPtr(40))}
			},
			pointer: "/findings/1/evidence", substr: "manifest file ref",
		},
		{
			name: "(q) a manifest path escaping the repository",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[1].Evidence = []plan.UpkeepEvidenceRef{upkeepFileRef("../go.mod", nil)}
			},
			pointer: "/findings/1/evidence", substr: "repository-relative",
		},
		{
			name: "(q) a root manifest passes",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[2].Evidence = []plan.UpkeepEvidenceRef{upkeepFileRef("package.json", nil)}
			},
		},
		{
			name: "(r) two degrades for one source",
			mutate: func(r *plan.UpkeepReport) {
				r.SourceDegrades = append(r.SourceDegrades, plan.UpkeepSourceDegrade{
					Source: plan.UpkeepSourceDeprecation, Reason: plan.UpkeepDegradeToolFailed,
				})
			},
			pointer: "/source_degrades/1", substr: "already degraded by /source_degrades/0",
		},
		{
			name: "(r) a source that did not run listed as scanned",
			mutate: func(r *plan.UpkeepReport) {
				r.SourceDegrades = []plan.UpkeepSourceDegrade{{Source: plan.UpkeepSourceAdvisory, Reason: plan.UpkeepDegradeNetworkUnavailable}}
			},
			pointer: "/source_degrades/0", substr: "must NOT appear in sources_scanned",
		},
		{
			name: "(r) a partial source listed as scanned passes",
			mutate: func(r *plan.UpkeepReport) {
				r.SourceDegrades = []plan.UpkeepSourceDegrade{{Source: plan.UpkeepSourceAdvisory, Reason: plan.UpkeepDegradePartial, Detail: "site/ only"}}
			},
		},
		// Rule (s): finding 1's fixed_version is the bare "v0.3.8". Each
		// refused row carries exactly one range marker, so the ContainsAny
		// set and the whitespace check are each the only control in the path
		// of their own rows.
		upkeepFixedVersionRow("(s) npm comparator range", ">=0.3.8", false),
		upkeepFixedVersionRow("(s) caret range", "^0.3.8", false),
		upkeepFixedVersionRow("(s) tilde range", "~0.3.8", false),
		upkeepFixedVersionRow("(s) upper-bound comparator", "<0.4.0", false),
		upkeepFixedVersionRow("(s) union", "0.3.8||0.4.1", false),
		upkeepFixedVersionRow("(s) wildcard", "0.3.*", false),
		upkeepFixedVersionRow("(s) comma-separated set", "0.3.8,0.4.1", false),
		upkeepFixedVersionRow("(s) hyphen range (whitespace only)", "0.3.8 - 0.4.0", false),
		upkeepFixedVersionRow("(s) trailing tab (whitespace only)", "0.3.8\t", false),
		upkeepFixedVersionRow("(s) bare version without v passes", "0.3.8", true),
		upkeepFixedVersionRow("(s) prerelease with build metadata passes", "v0.4.0-rc.1+meta", true),
		upkeepFixedVersionRow("(s) Go pseudo-version passes", "v0.0.0-20240312152122-5f9a2e5e7c7d", true),
		{
			// A null is the stated "no fix" (finding 2 already carries one);
			// rule (s) skips it.
			name:   "(s) null fixed_version passes",
			mutate: func(r *plan.UpkeepReport) { r.Findings[1].Advisory.FixedVersion = nil },
		},
		{
			// Rule (t) with the advisory source accounted as not run: the
			// example's findings are dropped so rule (c) cannot fire first.
			name: "(t) advisory degraded as not run passes",
			mutate: func(r *plan.UpkeepReport) {
				r.SourcesScanned = []string{plan.UpkeepSourceFlake}
				r.Findings = []plan.UpkeepFinding{}
				r.SourceDegrades = append(r.SourceDegrades, plan.UpkeepSourceDegrade{
					Source: plan.UpkeepSourceAdvisory, Reason: plan.UpkeepDegradeToolUnavailable,
				})
			},
		},
		{
			// The example keeps its deprecation degrade but no longer names
			// the advisory source anywhere.
			name: "(t) a degrade report silent about the advisory source",
			mutate: func(r *plan.UpkeepReport) {
				r.SourcesScanned = []string{plan.UpkeepSourceFlake}
				r.Findings = []plan.UpkeepFinding{}
			},
			pointer: "/sources_scanned", substr: `must account for the "advisory" source in exactly one place`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := upkeepAdvisoryExample(t)
			tc.mutate(r)
			upkeepAssertSemantic(t, plan.CheckUpkeepReportSemantics(r), tc.pointer, tc.substr)
		})
	}
}

// TestCheckUpkeepReportSemantics_AdvisoryRulesOnLegacyShape exercises (l) and
// (r) on the NON-advisory fixture, where no advisory finding can mask them.
func TestCheckUpkeepReportSemantics_AdvisoryRulesOnLegacyShape(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(r *plan.UpkeepReport)
		pointer string
		substr  string
	}{
		{
			name: "(l) an advisory object on a flake finding",
			mutate: func(r *plan.UpkeepReport) {
				r.Findings[0].Advisory = upkeepAdvisoryExample(t).Findings[1].Advisory
			},
			pointer: "/findings/0/advisory", substr: `this finding's source is "flake"`,
		},
		{
			name: "(r) partial on a source that did not run",
			mutate: func(r *plan.UpkeepReport) {
				r.SourceDegrades = []plan.UpkeepSourceDegrade{{Source: plan.UpkeepSourceAdvisory, Reason: plan.UpkeepDegradePartial}}
			},
			pointer: "/source_degrades/0", substr: "must appear in sources_scanned",
		},
		{
			// Rule (t) on the legacy shape: a degrade entry for another
			// source, and the advisory source neither scanned nor degraded.
			// Only rule (t) is in the path: rule (r) is satisfied (the
			// degraded deprecation source is not scanned) and the report
			// carries no advisory finding.
			name: "(t) a degrade naming another source, advisory unaccounted",
			mutate: func(r *plan.UpkeepReport) {
				r.SourcesScanned = []string{plan.UpkeepSourceFlake}
				r.Findings = r.Findings[:1]
				r.SourceDegrades = []plan.UpkeepSourceDegrade{{Source: plan.UpkeepSourceDeprecation, Reason: plan.UpkeepDegradeNetworkUnavailable}}
			},
			pointer: "/sources_scanned", substr: "an unaccounted advisory source reads as a clean scan that may never have run",
		},
		{
			name: "(t) advisory listed as scanned beside another degrade passes",
			mutate: func(r *plan.UpkeepReport) {
				r.SourcesScanned = []string{plan.UpkeepSourceFlake, plan.UpkeepSourceAdvisory}
				r.Findings = r.Findings[:1]
				r.SourceDegrades = []plan.UpkeepSourceDegrade{{Source: plan.UpkeepSourceDeprecation, Reason: plan.UpkeepDegradeNetworkUnavailable}}
			},
		},
		{
			// A partial degrade alone does not account for a source that is
			// not scanned; rule (r) refuses it before rule (t) runs.
			name: "(t) a partial advisory degrade is not an account of a source that did not run",
			mutate: func(r *plan.UpkeepReport) {
				r.SourcesScanned = []string{plan.UpkeepSourceFlake}
				r.Findings = r.Findings[:1]
				r.SourceDegrades = []plan.UpkeepSourceDegrade{{Source: plan.UpkeepSourceAdvisory, Reason: plan.UpkeepDegradePartial}}
			},
			pointer: "/source_degrades/0", substr: "must appear in sources_scanned",
		},
		{
			// A pre-#3750 report carries neither an advisory finding nor a
			// degrade, so rule (t) does not fire: it still parses.
			name:   "(t) legacy report with no degrade is not required to name advisory",
			mutate: func(*plan.UpkeepReport) {},
		},
		{
			// The named-degradation shape: the advisory source could not reach
			// its database, so it is degraded and absent from sources_scanned.
			name: "(r) network_unavailable advisory with no advisory finding passes",
			mutate: func(r *plan.UpkeepReport) {
				r.SourcesScanned = []string{plan.UpkeepSourceFlake}
				r.Findings = r.Findings[:1]
				r.SourceDegrades = []plan.UpkeepSourceDegrade{{Source: plan.UpkeepSourceAdvisory, Reason: plan.UpkeepDegradeNetworkUnavailable}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := upkeepFixture()
			tc.mutate(r)
			upkeepAssertSemantic(t, plan.CheckUpkeepReportSemantics(r), tc.pointer, tc.substr)
		})
	}
}

// upkeepFixedVersionRow is a rule (s) row on the advisory example's finding
// 1, whose fixed_version is the bare "v0.3.8".
func upkeepFixedVersionRow(name, fixed string, ok bool) struct {
	name    string
	mutate  func(r *plan.UpkeepReport)
	pointer string
	substr  string
} {
	row := struct {
		name    string
		mutate  func(r *plan.UpkeepReport)
		pointer string
		substr  string
	}{
		name:   name,
		mutate: func(r *plan.UpkeepReport) { r.Findings[1].Advisory.FixedVersion = &fixed },
	}
	if !ok {
		row.pointer, row.substr = "/findings/1/advisory/fixed_version", "is a range, not ONE bare version"
	}
	return row
}

func upkeepAssertSemantic(t *testing.T, err error, pointer, substr string) {
	t.Helper()
	if pointer == "" {
		if err != nil {
			t.Fatalf("CheckUpkeepReportSemantics = %v, want nil", err)
		}
		return
	}
	var se *plan.SemanticError
	if !errors.As(err, &se) {
		t.Fatalf("CheckUpkeepReportSemantics = %v (%T), want *SemanticError at %s", err, err, pointer)
	}
	if !strings.HasPrefix(se.Message, pointer+":") {
		t.Errorf("SemanticError = %q, want pointer %q", se.Message, pointer)
	}
	if !strings.Contains(se.Message, substr) {
		t.Errorf("SemanticError = %q, want it to contain %q", se.Message, substr)
	}
}

func TestUpkeepAdvisoryReachability(t *testing.T) {
	cases := []struct {
		name string
		path []plan.UpkeepAdvisoryFrame
		want string
	}{
		{"nil path", nil, ""},
		{"empty path", []plan.UpkeepAdvisoryFrame{}, ""},
		{"frame naming nothing", []plan.UpkeepAdvisoryFrame{{}}, ""},
		{"module only", []plan.UpkeepAdvisoryFrame{{Module: "m"}}, plan.UpkeepReachabilityRequired},
		{"package", []plan.UpkeepAdvisoryFrame{{Module: "m", Package: "m/p"}}, plan.UpkeepReachabilityImported},
		{"function", []plan.UpkeepAdvisoryFrame{{Module: "m", Package: "m/p", Function: "F"}}, plan.UpkeepReachabilityCalled},
		// Only call_path[0] (the vulnerable end) decides: a function in a
		// LATER frame is the caller, not the vulnerable symbol.
		{"function only past index 0", []plan.UpkeepAdvisoryFrame{{Module: "m", Package: "m/p"}, {Module: "app", Package: "app", Function: "main"}}, plan.UpkeepReachabilityImported},
	}
	for _, tc := range cases {
		if got := plan.UpkeepAdvisoryReachability(tc.path); got != tc.want {
			t.Errorf("%s: UpkeepAdvisoryReachability = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestUpkeepAdvisoryManifestDirs: only manifest refs contribute a directory
// (condition 1) — a mixed manifest-plus-source evidence list derives only the
// manifest's directory — and directories are distinct, normalized and
// first-seen ordered.
func TestUpkeepAdvisoryManifestDirs(t *testing.T) {
	cases := []struct {
		name string
		ev   []plan.UpkeepEvidenceRef
		want []string
	}{
		{"manifest plus call-site source", []plan.UpkeepEvidenceRef{
			upkeepFileRef("backend/go.mod", upkeepIntPtr(31)),
			upkeepFileRef("backend/internal/server/serve.go", upkeepIntPtr(88)),
		}, []string{"backend"}},
		{"two modules", []plan.UpkeepEvidenceRef{
			upkeepFileRef("runner/go.mod", nil),
			upkeepFileRef("backend/go.mod", nil),
			upkeepFileRef("./backend/go.mod", nil),
		}, []string{"runner", "backend"}},
		{"root and nested npm manifests", []plan.UpkeepEvidenceRef{
			upkeepFileRef("pnpm-lock.yaml", nil),
			upkeepFileRef("site/package.json", nil),
			upkeepFileRef("site/pnpm-lock.yaml", nil),
		}, []string{".", "site"}},
		{"no manifest", []plan.UpkeepEvidenceRef{
			upkeepFileRef("go.sum", nil),
			upkeepFileRef("/abs/go.mod", nil),
			upkeepFileRef("../go.mod", nil),
			upkeepFileRef("..", nil),
			upkeepFileRef("", nil),
			// A run ref is never a manifest, even carrying a path.
			{Kind: plan.UpkeepEvidenceKindRun, RunID: upkeepRunA, Path: "go.mod"},
		}, []string{}},
	}
	for _, tc := range cases {
		got := plan.UpkeepAdvisoryManifestDirs(&plan.UpkeepFinding{Evidence: tc.ev})
		if got == nil || strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Errorf("%s: UpkeepAdvisoryManifestDirs = %#v, want %#v", tc.name, got, tc.want)
		}
	}
	if got := plan.UpkeepAdvisoryManifestDirs(nil); got == nil || len(got) != 0 {
		t.Errorf("UpkeepAdvisoryManifestDirs(nil) = %#v, want empty non-nil", got)
	}
	if got := strings.Join(plan.UpkeepManifestBasenames(), ","); got != "go.mod,package.json,pnpm-lock.yaml" {
		t.Errorf("UpkeepManifestBasenames = %s, want the closed set go.mod,package.json,pnpm-lock.yaml", got)
	}
}

// TestUpkeepAdvisory_FixedVersionNullRoundTrips: an explicit "no fix" decodes
// to a nil pointer and re-encodes as null — never as an absent key, which the
// schema refuses.
func TestUpkeepAdvisory_FixedVersionNullRoundTrips(t *testing.T) {
	r := upkeepAdvisoryExample(t)
	if r.Findings[2].Advisory.FixedVersion != nil {
		t.Fatalf("finding 2 fixed_version = %q, want nil (explicit null)", *r.Findings[2].Advisory.FixedVersion)
	}
	if fv := r.Findings[0].Advisory.FixedVersion; fv == nil || *fv != "v0.23.0" {
		t.Fatalf("finding 0 fixed_version = %v, want v0.23.0", fv)
	}
	b, err := json.Marshal(r.Findings[2].Advisory)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"fixed_version":null`) {
		t.Errorf("re-encoded advisory = %s, want an explicit fixed_version null", b)
	}
	if strings.Contains(string(b), `"call_path"`) {
		t.Errorf("re-encoded pnpm advisory = %s, want no call_path key", b)
	}
}
