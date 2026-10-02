package server

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// rcServerSpec renders a v2 workflow spec declaring conventions (the block
// under review_conventions:, indented two spaces) and selecting sel on BOTH
// the plan and the implement stage.
func rcServerSpec(conventions, sel string) []byte {
	stage := func(id, typ, produces string) string {
		return "      - id: " + id + "\n        type: " + typ + "\n        executor:\n          agent: claude-code\n" +
			"        produces:\n" + produces +
			"        reviewers:\n          agents:\n            - provider: anthropic\n          conventions: " + sel + "\n"
	}
	return []byte("version: \"2\"\nreview_conventions:\n" + conventions +
		"workflows:\n  feature_change:\n    stages:\n" +
		stage("plan", "plan", "          - artifact: plan\n            schema: standard_v1\n") +
		stage("implement", "implement", "          - artifact: pull_request\n"))
}

func rcRun(specBytes []byte, trigger run.TriggerSource, labels ...string) *run.Run {
	r := &run.Run{ID: uuid.New(), Repo: "o/r", WorkflowID: "feature_change", WorkflowSpec: specBytes, TriggerSource: trigger}
	if labels != nil {
		r.IssueContext = &run.IssueContext{Labels: labels}
	}
	return r
}

func selectedNames(sel []spec.SelectedReviewConvention) []string {
	var out []string
	for _, s := range sel {
		out = append(out, s.Name)
	}
	return out
}

// (B parity) A convention whose applies_to names ONLY a label (row 1) or ONLY a
// trigger form (row 2) selects at the plan site AND the implement site, even
// though neither site's paths match anything: both sites build their Change
// from the ONE admission change (runAdmissionChange) and add only paths.
//
// Mechanism: the site paths never match, so only the admission change can
// select the convention. Counterfactual: build the implement Change from the
// diff paths alone (drop runAdmissionChange) -> the implement column goes RED.
func TestReviewConventionSelection_LabelOrTriggerOnlySelectsAtBothSites(t *testing.T) {
	cases := []struct {
		name      string
		appliesTo string
		runRow    *run.Run
	}{
		{"label only", "    applies_to:\n      labels: [security]\n", nil},
		{"trigger only", "    applies_to:\n      trigger: [on_demand]\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			specBytes := rcServerSpec("  sec:\n    path: docs/conventions/sec.md\n"+tc.appliesTo, "[sec]")
			var r *run.Run
			if tc.name == "label only" {
				r = rcRun(specBytes, run.TriggerCLI, "security")
			} else {
				r = rcRun(specBytes, run.TriggerOnDemand)
			}
			planPaths := []string{"frontend/unrelated.ts"}
			diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "cli/unrelated.go", Status: policy.StatusModified}}}
			for _, site := range []struct {
				st    spec.StageType
				paths []string
			}{{spec.StageTypePlan, planPaths}, {spec.StageTypeImplement, implementReviewPaths(diff)}} {
				sel, err := selectReviewConventions(r, site.st, reviewConventionChange(r, site.paths))
				if err != nil {
					t.Fatalf("%s: select: %v", site.st, err)
				}
				if got := selectedNames(sel.Selected); !reflect.DeepEqual(got, []string{"sec"}) {
					t.Errorf("%s site selected %v, want [sec]", site.st, got)
				}
			}
			// Control: the same run WITHOUT the label / trigger selects nothing.
			bare := rcRun(specBytes, run.TriggerCLI)
			sel, err := selectReviewConventions(bare, spec.StageTypeImplement, reviewConventionChange(bare, planPaths))
			if err != nil || len(sel.Selected) != 0 {
				t.Errorf("unmatched run selected %v (err %v), want none", selectedNames(sel.Selected), err)
			}
		})
	}
}

// A path-only applies_to selects at the site whose paths match, and only there;
// a rename's OldPath counts as a site path.
func TestReviewConventionSelection_PathOnly(t *testing.T) {
	specBytes := rcServerSpec("  be:\n    path: docs/conventions/be.md\n    severity_cap: low\n    applies_to:\n      paths: [\"backend/**\"]\n", "[be]")
	r := rcRun(specBytes, run.TriggerCLI)
	sel, err := selectReviewConventions(r, spec.StageTypePlan, reviewConventionChange(r, []string{"backend/x.go"}))
	if err != nil || !reflect.DeepEqual(selectedNames(sel.Selected), []string{"be"}) {
		t.Fatalf("plan site matching path: selected %v err %v, want [be]", selectedNames(sel.Selected), err)
	}
	if sel.Selected[0].SeverityCap != "low" || !sel.Selected[0].Required ||
		sel.Selected[0].DeclarationSite != "review_conventions.be in .fishhawk/workflows.yaml" {
		t.Errorf("selection = %+v", sel.Selected[0])
	}
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "frontend/x.ts", Status: policy.StatusModified}}}
	sel, err = selectReviewConventions(r, spec.StageTypeImplement, reviewConventionChange(r, implementReviewPaths(diff)))
	if err != nil || len(sel.Selected) != 0 {
		t.Errorf("implement site non-matching diff: selected %v err %v, want none", selectedNames(sel.Selected), err)
	}
	renamed := policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "frontend/moved.go", OldPath: "backend/moved.go", Status: policy.StatusRenamed}}}
	sel, err = selectReviewConventions(r, spec.StageTypeImplement, reviewConventionChange(r, implementReviewPaths(renamed)))
	if err != nil || !reflect.DeepEqual(selectedNames(sel.Selected), []string{"be"}) {
		t.Errorf("rename out of backend/: selected %v err %v, want [be] via OldPath", selectedNames(sel.Selected), err)
	}
}

// selectReviewConventions: nothing declared is (zero, nil); an unparseable
// snapshot FAILS CLOSED rather than reading as "no convention".
//
// Counterfactual: return (zero, nil) on the parse error -> the unparseable row
// goes RED.
func TestSelectReviewConventions_Degrades(t *testing.T) {
	if sel, err := selectReviewConventions(&run.Run{}, spec.StageTypePlan, spec.Change{}); err != nil || sel.Selected != nil || sel.DeclaredPaths != nil {
		t.Errorf("nil spec: %+v %v, want zero", sel, err)
	}
	if sel, err := selectReviewConventions(nil, spec.StageTypePlan, spec.Change{}); err != nil || sel.Selected != nil {
		t.Errorf("nil run: %+v %v, want zero", sel, err)
	}
	specBytes := rcServerSpec("  be:\n    path: docs/be.md\n", "[be]")
	other := rcRun(specBytes, run.TriggerCLI)
	other.WorkflowID = "absent"
	sel, err := selectReviewConventions(other, spec.StageTypePlan, spec.Change{})
	if err != nil || sel.Selected != nil || !reflect.DeepEqual(sel.DeclaredPaths, []string{"docs/be.md"}) {
		t.Errorf("absent workflow: %+v %v, want no selection, declared [docs/be.md]", sel, err)
	}
	bad := rcRun([]byte("version: [\n"), run.TriggerCLI)
	if _, err := selectReviewConventions(bad, spec.StageTypePlan, spec.Change{}); err == nil ||
		!strings.Contains(err.Error(), "parse the run's workflow spec") {
		t.Errorf("unparseable spec err = %v, want a fail-closed parse error", err)
	}
}

func TestImplementReviewPaths_IncludesOldPathDeduped(t *testing.T) {
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{
		{Path: "b.go", Status: policy.StatusModified},
		{Path: "new/c.go", OldPath: "old/c.go", Status: policy.StatusRenamed},
		{Path: "b.go", Status: policy.StatusModified},
		{Path: "", Status: policy.StatusModified},
	}}
	if got, want := implementReviewPaths(diff), []string{"b.go", "new/c.go", "old/c.go"}; !reflect.DeepEqual(got, want) {
		t.Errorf("implementReviewPaths = %v, want %v", got, want)
	}
}

// Declared paths cover EVERY entry (selected or not); a diff modifying one —
// including moving it away (OldPath) — is detected.
func TestDeclaredAndModifiedConventionFiles(t *testing.T) {
	parsed, err := spec.ParseBytes(rcServerSpec(
		"  a:\n    path: docs/a.md\n    applies_to:\n      paths: [\"nothing/**\"]\n  b:\n    path: docs/b.md\n", "[a, b]"))
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}
	declared := declaredConventionPaths(parsed)
	if want := []string{"docs/a.md", "docs/b.md"}; !reflect.DeepEqual(declared, want) {
		t.Fatalf("declaredConventionPaths = %v, want %v", declared, want)
	}
	if declaredConventionPaths(nil) != nil {
		t.Error("declaredConventionPaths(nil) != nil")
	}
	diff := policy.Diff{ChangedFiles: []policy.ChangedFile{
		{Path: "docs/a.md", Status: policy.StatusModified},
		{Path: "docs/moved.md", OldPath: "docs/b.md", Status: policy.StatusRenamed},
		{Path: "src/x.go", Status: policy.StatusModified},
	}}
	if got, want := modifiedConventionFiles(diff, declared), []string{"docs/a.md", "docs/b.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("modifiedConventionFiles = %v, want %v", got, want)
	}
	if got := modifiedConventionFiles(policy.Diff{ChangedFiles: []policy.ChangedFile{{Path: "src/x.go"}}}, declared); got != nil {
		t.Errorf("unrelated diff modified %v, want none", got)
	}
}

func TestReviewConventionMissingError_NamesEverything(t *testing.T) {
	sel := spec.SelectedReviewConvention{Name: "be", Path: "docs/be.md", Required: true, DeclarationSite: "review_conventions.be in .fishhawk/workflows.yaml"}
	inner := &repodoc.ResolveError{Path: sel.Path, DeclarationSite: sel.DeclarationSite, Err: repodoc.ErrMissingDocument}
	err := reviewConventionMissingError(sel, admCommitA, inner)
	msg := err.Error()
	for _, want := range []string{"review_convention_missing: ", `"be"`, "docs/be.md", sel.DeclarationSite, admCommitA} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
	if !errors.Is(err, repodoc.ErrMissingDocument) {
		t.Error("error does not wrap repodoc.ErrMissingDocument")
	}
	if d := documentInjectionErrorDetails(err); d["path"] != sel.Path || d["declaration_site"] != sel.DeclarationSite {
		t.Errorf("documentInjectionErrorDetails = %v, want path + site", d)
	}
}

func TestConventionDeclarationAndCaps(t *testing.T) {
	sel := spec.SelectedReviewConvention{Name: "be", Path: "docs/be.md", SeverityCap: "low", DeclarationSite: "site"}
	d := conventionDeclaration(sel)
	if d.Path != "docs/be.md" || d.DeclarationSite != "site" || d.Base != repodoc.BaseSourceRunAdmission || d.Framing.Heading != "Review convention be" {
		t.Errorf("conventionDeclaration = %+v", d)
	}
	if conventionCapsFor(nil) != nil {
		t.Error("conventionCapsFor(nil) != nil — the zero round must read as zero conventions rendered")
	}
	caps := conventionCapsFor([]spec.SelectedReviewConvention{sel, {Name: "open"}})
	if want := (planreview.ConventionCaps{"be": planreview.SeverityLow, "open": ""}); !reflect.DeepEqual(caps, want) {
		t.Errorf("conventionCapsFor = %v, want %v", caps, want)
	}
}

// conventionsFileModifiedCarrier: once per round, on the first successful
// verdict, only when no verdict of the round (and no earlier round) raised it.
//
// Counterfactual: ignore alreadyRaised, or stop scanning later verdicts for a
// raised concern -> the matching rows go RED.
func TestConventionsFileModifiedCarrier(t *testing.T) {
	plain := &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove}
	raised := &planreview.ReviewVerdict{Verdict: planreview.VerdictApproveWithConcerns, Concerns: []planreview.Concern{
		{Category: " Conventions_File_Modified ", Severity: planreview.SeverityMedium},
	}}
	cases := []struct {
		name     string
		verdicts []*planreview.ReviewVerdict
		already  bool
		want     int
	}{
		{"none raised -> first verdict", []*planreview.ReviewVerdict{plain, plain}, false, 0},
		{"second reviewer raised -> none", []*planreview.ReviewVerdict{plain, raised}, false, -1},
		{"raised in an earlier round -> none", []*planreview.ReviewVerdict{plain}, true, -1},
		{"first verdict nil -> second", []*planreview.ReviewVerdict{nil, plain}, false, 1},
		{"every reviewer failed -> none", []*planreview.ReviewVerdict{nil, nil}, false, -1},
	}
	for _, tc := range cases {
		if got := conventionsFileModifiedCarrier(tc.verdicts, tc.already); got != tc.want {
			t.Errorf("%s: carrier = %d, want %d", tc.name, got, tc.want)
		}
	}
}
