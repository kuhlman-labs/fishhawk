package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// forbiddenPresetGlobs mirrors the shape every shipped preset's implement
// stage carries, so the evaluator is exercised against realistic globs
// (a `**` directory glob and two bare filenames) rather than a toy pattern.
var forbiddenPresetGlobs = []string{".fishhawk/**", ".github/workflows/**", "LICENSE", "NOTICE"}

// criterionPlan builds a *plan.Plan carrying only the acceptance criteria the
// pure evaluator reads. The evaluator never parses a body, so constructing the
// struct directly keeps each case to the field under test.
func criterionPlan(cs ...plan.AcceptanceCriterion) *plan.Plan {
	return &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: cs}}
}

func TestExtractCriterionPathTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		text string
		want []string
	}{
		{
			name: "backticked path",
			text: "the rendered prompt names `.fishhawk/workflows.yaml` verbatim",
			want: []string{".fishhawk/workflows.yaml"},
		},
		{
			name: "double-quoted path",
			text: `the escalation block in ".fishhawk/workflows.yaml" is raised`,
			want: []string{".fishhawk/workflows.yaml"},
		},
		{
			name: "single-quoted path",
			text: "edit '.gitlab-ci.yml' so the pipeline runs the gate",
			want: []string{".gitlab-ci.yml"},
		},
		{
			name: "curly-quoted path",
			text: "edit “docs/spec/workflow-v2.md” for the reference",
			want: []string{"docs/spec/workflow-v2.md"},
		},
		{
			name: "bare slash-bearing path",
			text: "backend/internal/server/plan.go copies the field",
			want: []string{"backend/internal/server/plan.go"},
		},
		{
			// The false-positive class the quoted-or-slash filter exists to
			// stop: prose naming the WORD would otherwise match the bare
			// `LICENSE` glob.
			name: "bare unquoted word is not captured",
			text: "the change does not alter the LICENSE or NOTICE terms",
			want: nil,
		},
		{
			name: "trailing sentence-final period stripped from a bare token",
			text: "see docs/spec/workflow-v2.md.",
			want: []string{"docs/spec/workflow-v2.md"},
		},
		{
			// A longer identifier yields the WHOLE token, never a matching
			// suffix — this is what the non-word left anchor buys.
			name: "longer identifier yields no suffix token",
			text: "the fixture path foo.fishhawk/bar is unrelated",
			want: []string{"foo.fishhawk/bar"},
		},
		{
			name: "dedup preserves first-seen order",
			text: "`.fishhawk/workflows.yaml` then backend/a.go then .fishhawk/workflows.yaml again",
			want: []string{".fishhawk/workflows.yaml", "backend/a.go"},
		},
		{
			name: "empty text",
			text: "",
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractCriterionPathTokens(tc.text); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("extractCriterionPathTokens(%q) = %#v, want %#v", tc.text, got, tc.want)
			}
		})
	}
}

func TestEvaluateForbiddenCriterionRule(t *testing.T) {
	forbidden := plan.AcceptanceCriterion{
		ID:        "ac-1",
		Statement: "the escalation block in `.fishhawk/workflows.yaml` declares the new path",
		Source:    plan.CriterionSourceExplicit,
	}

	t.Run("criterion naming a forbidden path yields one finding carrying the glob", func(t *testing.T) {
		findings, applied := evaluateForbiddenCriterionRule(criterionPlan(forbidden), forbiddenPresetGlobs, nil)
		if len(findings) != 1 {
			t.Fatalf("findings = %+v, want 1", findings)
		}
		want := SurfaceSweepFinding{
			Pattern:          surfacePatternForbiddenCriterion,
			TriggerPath:      `path ".fishhawk/workflows.yaml" named at acceptance_criteria[ac-1].statement`,
			ForbiddenPath:    ".fishhawk/workflows.yaml",
			ForbiddenPattern: ".fishhawk/**",
			// #5499 J1 (b): "`x` declares …" names the file as a change.
			ForbiddenInScope: true,
		}
		if !reflect.DeepEqual(findings[0], want) {
			t.Errorf("finding =\n %+v\nwant\n %+v", findings[0], want)
		}
		if findings[0].MissingSiblings != nil {
			t.Errorf("MissingSiblings = %+v, want nil (nothing is missing from scope)", findings[0].MissingSiblings)
		}
		if applied != nil {
			t.Errorf("applied = %+v, want nil", applied)
		}
	})

	t.Run("verify_hint is scanned too", func(t *testing.T) {
		c := plan.AcceptanceCriterion{
			ID:         "ac-h",
			Statement:  "the gate fires",
			VerifyHint: "confirm the workflow edit in `.github/workflows/ci.yml`",
			Source:     plan.CriterionSourceExplicit,
		}
		findings, _ := evaluateForbiddenCriterionRule(criterionPlan(c), forbiddenPresetGlobs, nil)
		if len(findings) != 1 || findings[0].ForbiddenPattern != ".github/workflows/**" ||
			findings[0].TriggerPath != `path ".github/workflows/ci.yml" named at acceptance_criteria[ac-h].verify_hint` {
			t.Fatalf("findings = %+v", findings)
		}
	})

	t.Run("criterion naming a non-forbidden path yields none", func(t *testing.T) {
		c := plan.AcceptanceCriterion{
			ID:        "ac-ok",
			Statement: "`GET /v0/runs/{run_id}` returns the field wired in backend/internal/server/reads.go",
			Source:    plan.CriterionSourceExplicit,
		}
		findings, applied := evaluateForbiddenCriterionRule(criterionPlan(c), forbiddenPresetGlobs, nil)
		if findings != nil || applied != nil {
			t.Fatalf("findings = %+v, applied = %+v, want none", findings, applied)
		}
	})

	t.Run("empty forbidden list yields none", func(t *testing.T) {
		findings, applied := evaluateForbiddenCriterionRule(criterionPlan(forbidden), nil, nil)
		if findings != nil || applied != nil {
			t.Fatalf("findings = %+v, applied = %+v, want none", findings, applied)
		}
	})

	t.Run("no acceptance criteria yields none", func(t *testing.T) {
		findings, applied := evaluateForbiddenCriterionRule(criterionPlan(), forbiddenPresetGlobs, nil)
		if findings != nil || applied != nil {
			t.Fatalf("findings = %+v, applied = %+v, want none", findings, applied)
		}
	})

	t.Run("nil plan yields none", func(t *testing.T) {
		findings, applied := evaluateForbiddenCriterionRule(nil, forbiddenPresetGlobs, nil)
		if findings != nil || applied != nil {
			t.Fatalf("findings = %+v, applied = %+v, want none", findings, applied)
		}
	})

	t.Run("bare unquoted LICENSE mention yields none", func(t *testing.T) {
		// The bad state is seeded BY CONSTRUCTION: the sentence carries the
		// bare word and is never routed through the filter under test, so
		// deleting the quoted-or-slash filter reddens THIS assertion.
		c := plan.AcceptanceCriterion{
			ID:        "ac-lic",
			Statement: "the change does not alter the LICENSE or NOTICE terms",
			Source:    plan.CriterionSourceExplicit,
		}
		findings, applied := evaluateForbiddenCriterionRule(criterionPlan(c), forbiddenPresetGlobs, nil)
		if findings != nil || applied != nil {
			t.Fatalf("findings = %+v, applied = %+v, want none (a bare word must not match a bare glob)", findings, applied)
		}
	})

	t.Run("quoted LICENSE mention IS caught", func(t *testing.T) {
		// The control's other side: the filter narrows capture to quoted or
		// slash-bearing tokens, it does not disable bare-glob matching.
		c := plan.AcceptanceCriterion{
			ID:        "ac-lic2",
			Statement: "the new clause is appended to `LICENSE`",
			Source:    plan.CriterionSourceExplicit,
		}
		findings, _ := evaluateForbiddenCriterionRule(criterionPlan(c), forbiddenPresetGlobs, nil)
		if len(findings) != 1 || findings[0].ForbiddenPattern != "LICENSE" || findings[0].ForbiddenPath != "LICENSE" {
			t.Fatalf("findings = %+v", findings)
		}
	})

	t.Run("malformed glob yields none and does not panic", func(t *testing.T) {
		// policy.Evaluate reports an invalid pattern as a Violation carrying
		// NO Files, so the rule (which reads only violations with Files)
		// emits nothing.
		findings, applied := evaluateForbiddenCriterionRule(criterionPlan(forbidden), []string{"[", "{a"}, nil)
		if findings != nil || applied != nil {
			t.Fatalf("findings = %+v, applied = %+v, want none", findings, applied)
		}
	})

	t.Run("matching exemption yields zero findings and one applied exemption", func(t *testing.T) {
		findings, applied := evaluateForbiddenCriterionRule(criterionPlan(forbidden), forbiddenPresetGlobs,
			[]plan.SurfaceSweepExemption{{
				Pattern: surfacePatternForbiddenCriterion,
				Sibling: ".fishhawk/workflows.yaml",
				Reason:  "the criterion asserts the rendered prompt NAMES the file, it does not edit it",
			}})
		if len(findings) != 0 {
			t.Fatalf("findings = %+v, want none", findings)
		}
		want := []AppliedExemption{{
			Pattern: surfacePatternForbiddenCriterion,
			Sibling: ".fishhawk/workflows.yaml",
			Reason:  "the criterion asserts the rendered prompt NAMES the file, it does not edit it",
		}}
		if !reflect.DeepEqual(applied, want) {
			t.Errorf("applied = %+v, want %+v", applied, want)
		}
	})

	t.Run("non-matching exemption is a no-op and the finding still fires", func(t *testing.T) {
		for _, ex := range []plan.SurfaceSweepExemption{
			{Pattern: surfacePatternForbiddenCriterion, Sibling: "LICENSE", Reason: "wrong path"},
			{Pattern: "new audit category requires registry", Sibling: ".fishhawk/workflows.yaml", Reason: "wrong pattern"},
		} {
			findings, applied := evaluateForbiddenCriterionRule(criterionPlan(forbidden), forbiddenPresetGlobs, []plan.SurfaceSweepExemption{ex})
			if len(findings) != 1 {
				t.Errorf("exemption %+v suppressed the finding: %+v", ex, findings)
			}
			if applied != nil {
				t.Errorf("exemption %+v recorded as applied: %+v", ex, applied)
			}
		}
	})

	t.Run("more than ten matching criteria are capped at ten and sorted", func(t *testing.T) {
		var cs []plan.AcceptanceCriterion
		// Declared in DESCENDING id order so a cap applied before the sort
		// would keep the wrong ten.
		for i := 14; i >= 1; i-- {
			cs = append(cs, plan.AcceptanceCriterion{
				ID:        fmt.Sprintf("ac-%02d", i),
				Statement: "the block in `.fishhawk/workflows.yaml` is raised",
				Source:    plan.CriterionSourceExplicit,
			})
		}
		findings, _ := evaluateForbiddenCriterionRule(criterionPlan(cs...), forbiddenPresetGlobs, nil)
		if len(findings) != forbiddenCriterionSweepMaxFindings {
			t.Fatalf("findings = %d, want %d", len(findings), forbiddenCriterionSweepMaxFindings)
		}
		for i, f := range findings {
			want := fmt.Sprintf(`path ".fishhawk/workflows.yaml" named at acceptance_criteria[ac-%02d].statement`, i+1)
			if f.TriggerPath != want {
				t.Errorf("findings[%d].TriggerPath = %q, want %q", i, f.TriggerPath, want)
			}
		}
	})

	t.Run("deterministic order across criteria and tokens", func(t *testing.T) {
		// Declared out of order, two tokens on one criterion: the exact
		// ordered slice is asserted so a map-iteration implementation fails
		// rather than flakes.
		cs := []plan.AcceptanceCriterion{
			{ID: "ac-z", Statement: "raise `.fishhawk/workflows.yaml`", Source: plan.CriterionSourceExplicit},
			{ID: "ac-a", Statement: "edit `.github/workflows/ci.yml` and `.fishhawk/workflows.yaml`", Source: plan.CriterionSourceExplicit},
		}
		findings, _ := evaluateForbiddenCriterionRule(criterionPlan(cs...), forbiddenPresetGlobs, nil)
		got := make([]string, 0, len(findings))
		for _, f := range findings {
			got = append(got, f.TriggerPath)
		}
		want := []string{
			`path ".fishhawk/workflows.yaml" named at acceptance_criteria[ac-a].statement`,
			`path ".github/workflows/ci.yml" named at acceptance_criteria[ac-a].statement`,
			`path ".fishhawk/workflows.yaml" named at acceptance_criteria[ac-z].statement`,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ordered TriggerPaths =\n %#v\nwant\n %#v", got, want)
		}
	})

	t.Run("same path in statement and verify_hint draws one finding", func(t *testing.T) {
		c := plan.AcceptanceCriterion{
			ID:         "ac-dup",
			Statement:  "raise `.fishhawk/workflows.yaml`",
			VerifyHint: "diff `.fishhawk/workflows.yaml`",
			Source:     plan.CriterionSourceExplicit,
		}
		findings, _ := evaluateForbiddenCriterionRule(criterionPlan(c), forbiddenPresetGlobs, nil)
		if len(findings) != 1 || findings[0].TriggerPath != `path ".fishhawk/workflows.yaml" named at acceptance_criteria[ac-dup].statement` {
			t.Fatalf("findings = %+v", findings)
		}
	})
}

// namedOnlyCriteria are the two observed evidence_conflict shapes (#5499):
// run 01f0b674 named a forbidden file as an OUTPUT VALUE in a statement, and
// run 39a4e171 named a forbidden runtime-state file as a READ TARGET inside a
// verify_hint shell command. Neither asks the implement agent to change it.
var namedOnlyCriteria = []plan.AcceptanceCriterion{
	{
		ID:        "ac-output-value",
		Statement: "`fishhawk init --dry-run` prints `.fishhawk/workflows.yaml` as the file it would write, and exits 0",
		Source:    plan.CriterionSourceExplicit,
	},
	{
		ID:         "ac-read-target",
		Statement:  "the run records its stage list",
		VerifyHint: "run `jq -r '.stages' .fishhawk/state/run.json` and confirm the stage list is non-empty",
		Source:     plan.CriterionSourceExplicit,
	},
}

// TestEvaluateForbiddenCriterionRule_NamedOnlyIsNotAsserted pins #5499 J1: a
// criterion that only NAMES a forbidden path — as an output value or a
// runtime-state read target — in a plan whose scope and approach never name
// it still draws the finding, but with ForbiddenInScope false, so the review
// prompt renders the advisory rather than the asserted line.
func TestEvaluateForbiddenCriterionRule_NamedOnlyIsNotAsserted(t *testing.T) {
	for _, c := range namedOnlyCriteria {
		t.Run(c.ID, func(t *testing.T) {
			p := criterionPlan(c)
			p.Scope.Files = []plan.ScopeFile{{Path: "cli/internal/initcmd/init.go", Operation: plan.FileOpModify}}
			p.Approach = []plan.ApproachStep{{Step: 1, Description: "Add the dry-run flag to cli/internal/initcmd/init.go."}}
			findings, _ := evaluateForbiddenCriterionRule(p, forbiddenPresetGlobs, nil)
			if len(findings) != 1 {
				t.Fatalf("findings = %+v, want 1 (the gate still measures the named path)", findings)
			}
			if findings[0].ForbiddenInScope {
				t.Errorf("ForbiddenInScope = true, want false: nothing in the plan frames %q as an edit (%+v)", findings[0].ForbiddenPath, findings[0])
			}
		})
	}
}

// TestEvaluateForbiddenCriterionRule_ScopedPathIsAsserted pins #5499 J1 (a):
// the SAME criteria, with the path scoped only in a decomposition SUB-PLAN —
// or covered by a scoped file under a directory/glob token the criterion
// names — assert. The only difference from the named-only test is scope
// membership, which is what the control reads.
func TestEvaluateForbiddenCriterionRule_ScopedPathIsAsserted(t *testing.T) {
	scopedPlan := func(c plan.AcceptanceCriterion, subPlanFile string) *plan.Plan {
		p := criterionPlan(c)
		p.Scope.Files = []plan.ScopeFile{{Path: "cli/internal/initcmd/init.go", Operation: plan.FileOpModify}}
		p.Decomposition = &plan.Decomposition{Rationale: "x", SubPlans: []plan.SubPlanSummary{
			{Title: "cli", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "cli/internal/initcmd/init.go", Operation: plan.FileOpModify}}}},
			{Title: "governance", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: subPlanFile, Operation: plan.FileOpModify}}}},
		}}
		return p
	}
	for _, tc := range []struct {
		name string
		c    plan.AcceptanceCriterion
		file string
	}{
		{"output-value path scoped in a sub-plan", namedOnlyCriteria[0], ".fishhawk/workflows.yaml"},
		{"read-target path scoped in a sub-plan", namedOnlyCriteria[1], ".fishhawk/state/run.json"},
		{"glob token covers a scoped file under it", plan.AcceptanceCriterion{
			ID:        "ac-glob",
			Statement: "nothing under `.fishhawk/**` is left stale after the migration",
			Source:    plan.CriterionSourceExplicit,
		}, ".fishhawk/workflows.yaml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			findings, _ := evaluateForbiddenCriterionRule(scopedPlan(tc.c, tc.file), forbiddenPresetGlobs, nil)
			if len(findings) != 1 {
				t.Fatalf("findings = %+v, want 1", findings)
			}
			if !findings[0].ForbiddenInScope {
				t.Errorf("ForbiddenInScope = false, want true: a sub-plan scopes %q (%+v)", tc.file, findings[0])
			}
		})
	}
}

// TestEvaluateForbiddenCriterionRule_ChangePhrasingIsAsserted pins #5499 J1
// (b): with nothing scoped and no approach step naming the path, a criterion
// whose own statement or verify_hint names the path as something to change
// still asserts.
func TestEvaluateForbiddenCriterionRule_ChangePhrasingIsAsserted(t *testing.T) {
	for _, c := range []plan.AcceptanceCriterion{
		{ID: "ac-wired", Statement: "the workflow gate is wired in `.github/workflows/ci.yml`", Source: plan.CriterionSourceExplicit},
		{ID: "ac-subject", Statement: "deploy/pipelines/verify.yml runs make lint-templates as a required step on every push", Source: plan.CriterionSourceExplicit},
		{ID: "ac-hint", Statement: "the gate fires", VerifyHint: "confirm a step was added to `.github/workflows/ci.yml`", Source: plan.CriterionSourceExplicit},
		{ID: "ac-passive", Statement: "after the change `.fishhawk/workflows.yaml` is updated with the new stage", Source: plan.CriterionSourceExplicit},
	} {
		t.Run(c.ID, func(t *testing.T) {
			globs := append([]string{"deploy/pipelines/**"}, forbiddenPresetGlobs...)
			findings, _ := evaluateForbiddenCriterionRule(criterionPlan(c), globs, nil)
			if len(findings) != 1 {
				t.Fatalf("findings = %+v, want 1", findings)
			}
			if !findings[0].ForbiddenInScope {
				t.Errorf("ForbiddenInScope = false, want true: %q names the path as a change", c.Statement+" / "+c.VerifyHint)
			}
		})
	}
}

// TestEvaluateForbiddenCriterionRule_ApproachNamedIsAsserted pins #5499 J1
// (c): a named-only criterion asserts when an approach step names the path —
// exactly, or through a directory token covering it.
func TestEvaluateForbiddenCriterionRule_ApproachNamedIsAsserted(t *testing.T) {
	for _, step := range []string{
		"Regenerate the golden for `.fishhawk/workflows.yaml`.",
		"Touch nothing outside cli/ and `.fishhawk/`.",
	} {
		t.Run(step, func(t *testing.T) {
			p := criterionPlan(namedOnlyCriteria[0])
			p.Scope.Files = []plan.ScopeFile{{Path: "cli/internal/initcmd/init.go", Operation: plan.FileOpModify}}
			p.Approach = []plan.ApproachStep{{Step: 1, Description: step}}
			findings, _ := evaluateForbiddenCriterionRule(p, forbiddenPresetGlobs, nil)
			if len(findings) != 1 || !findings[0].ForbiddenInScope {
				t.Fatalf("findings = %+v, want 1 with ForbiddenInScope true (an approach step names the path)", findings)
			}
		})
	}
}

// TestNamesPathAsChange covers the J1 (b) keyword classifier's branches.
func TestNamesPathAsChange(t *testing.T) {
	for _, tc := range []struct {
		text, tok string
		want      bool
	}{
		{"`x/y.yml` runs the lint", "x/y.yml", true},                    // subject position
		{"The file `x/y.yml` holds the step", "x/y.yml", true},          // subject behind fillers
		{"the gate is wired in `x/y.yml`", "x/y.yml", true},             // change word before
		{"the block in `x/y.yml` declares a step", "x/y.yml", true},     // change word after
		{"then `x/y.yml` is updated", "x/y.yml", true},                  // passive change
		{"the loader says `x/y.yml` is read by init", "x/y.yml", false}, // passive read
		{"the command prints `x/y.yml`", "x/y.yml", false},              // read word before
		{"run `jq '.s' x/y.yml` and check it", "x/y.yml", false},        // command argument
		{"a long sentence that mentions nothing in particular about x/y.yml", "x/y.yml", false},
		{"added a guard. The output lists `x/y.yml`", "x/y.yml", false}, // clause boundary stops the scan
		{"", "x/y.yml", false},
	} {
		if got := namesPathAsChange(tc.text, tc.tok); got != tc.want {
			t.Errorf("namesPathAsChange(%q, %q) = %v, want %v", tc.text, tc.tok, got, tc.want)
		}
	}
}

// TestPathCoveredBy covers the J1 (a)/(c) path matcher: exact, glob and
// directory tokens on either side.
func TestPathCoveredBy(t *testing.T) {
	for _, tc := range []struct {
		tok   string
		paths []string
		want  bool
	}{
		{".fishhawk/workflows.yaml", []string{".fishhawk/workflows.yaml"}, true},
		{".fishhawk/**", []string{".fishhawk/workflows.yaml"}, true},
		{".fishhawk/", []string{".fishhawk/workflows.yaml"}, true},
		{".fishhawk/workflows.yaml", []string{".fishhawk"}, true},
		{".fishhawk/workflows.yaml", []string{".fishhawk/*.yaml"}, true},
		{"./.fishhawk/workflows.yaml", []string{".fishhawk/workflows.yaml"}, true},
		{".fishhawk/workflows.yaml", []string{".fishhawkx/workflows.yaml", "cli/x.go"}, false},
		{".fishhawk/**", []string{"cli/x.go"}, false},
		{"", []string{"cli/x.go"}, false},
	} {
		if got := pathCoveredBy(tc.tok, tc.paths); got != tc.want {
			t.Errorf("pathCoveredBy(%q, %v) = %v, want %v", tc.tok, tc.paths, got, tc.want)
		}
	}
}

// TestForbiddenCriterion_TruePositivesStillAsserted pins #5499 J1's
// keep-the-true-positive condition end to end: the #3620 seam fixture's
// criterion and the miss-corpus case
// seed-synthetic-realmiss-criterion-needs-forbidden-path (read from its
// committed review_input.json, against the glob its risk entry names) both
// map through planGateEvidence and prompt.Build to the asserted
// UNENFORCEABLE CRITERION line carrying the two-merge-request remedy.
func TestForbiddenCriterion_TruePositivesStillAsserted(t *testing.T) {
	const remedy = "Operator escape: land the in-repository half in this merge request and edit the forbidden governance file by hand in a second one"
	assertRendered := func(t *testing.T, p *plan.Plan, findings []SurfaceSweepFinding, wantPath string) {
		t.Helper()
		if len(findings) != 1 || !findings[0].ForbiddenInScope || findings[0].ForbiddenPath != wantPath {
			t.Fatalf("findings = %+v, want one asserted finding for %q", findings, wantPath)
		}
		ev := planGateEvidence(nil, &SurfaceSweepPayload{ScannedFiles: len(p.Scope.Files), Findings: findings}, nil, nil, nil)
		rendered, err := prompt.Build("plan_review", prompt.Trigger{Repo: "x/y", ApprovedPlan: p, PlanGateEvidence: ev})
		if err != nil {
			t.Fatalf("prompt.Build: %v", err)
		}
		for _, want := range []string{"UNENFORCEABLE CRITERION (acceptance criterion requires a forbidden path)", remedy} {
			if !strings.Contains(rendered, want) {
				t.Errorf("plan-review prompt missing %q:\n%s", want, rendered)
			}
		}
		if strings.Contains(rendered, "FORBIDDEN PATH NAMED:") {
			t.Errorf("a true positive must not render the advisory:\n%s", rendered)
		}
	}

	t.Run("#3620 seam fixture", func(t *testing.T) {
		s, _, runRow := newScopePrecheckServer(t, specImplementPathConstraints)
		body := forbiddenCriterionPlanBody(t,
			[]plan.ScopeFile{{Path: "backend/internal/server/foo.go", Operation: plan.FileOpModify}},
			[]string{"the workflow gate is wired in `.github/workflows/ci.yml`"}, nil)
		sweep := s.runSurfaceSweep(context.Background(), runRow.ID, runRow.ID, body)
		if sweep == nil {
			t.Fatal("runSurfaceSweep = nil")
		}
		p, err := plan.Parse(body)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		assertRendered(t, p, sweep.Findings, ".github/workflows/ci.yml")
	})

	t.Run("miss-corpus seed-synthetic-realmiss-criterion-needs-forbidden-path", func(t *testing.T) {
		raw, err := os.ReadFile("../agenteval/testdata/planreview-miss-corpus/seed-synthetic-realmiss-criterion-needs-forbidden-path/review_input.json")
		if err != nil {
			t.Fatalf("read miss-corpus case: %v", err)
		}
		var in struct {
			Plan plan.Plan `json:"plan"`
		}
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatalf("decode miss-corpus case: %v", err)
		}
		// The case's own risk entry puts deploy/pipelines/** outside what the
		// implementing workflow may write.
		findings, _ := evaluateForbiddenCriterionRule(&in.Plan, []string{"deploy/pipelines/**"}, nil)
		assertRendered(t, &in.Plan, findings, "deploy/pipelines/verify.yml")
	})

	// P5 (b) of #5499: one true positive per phrasing class namesPathAsChange
	// must assert, each isolated so ONLY its own branch of
	// occurrenceNamesChange decides: nothing scoped, no approach step, and no
	// other deciding word in the clause. Narrowing a class turns its subtest
	// RED instead of silently demoting a real edit to the advisory.
	for _, tc := range []struct {
		class, statement, path string
	}{
		{"change verb right after the path", "the block in `.github/workflows/ci.yml` declares the lint step", ".github/workflows/ci.yml"},
		{"passive change", "on merge `.fishhawk/workflows.yaml` is updated with the new stage", ".fishhawk/workflows.yaml"},
		{"subject position", "deploy/pipelines/verify.yml runs make lint-templates as a required step on every push", "deploy/pipelines/verify.yml"},
		{"change word before the path", "the workflow gate is wired in `.github/workflows/ci.yml`", ".github/workflows/ci.yml"},
	} {
		t.Run("phrasing class: "+tc.class, func(t *testing.T) {
			p := criterionPlan(plan.AcceptanceCriterion{ID: "ac-1", Statement: tc.statement, Source: plan.CriterionSourceExplicit})
			findings, _ := evaluateForbiddenCriterionRule(p, append([]string{"deploy/pipelines/**"}, forbiddenPresetGlobs...), nil)
			assertRendered(t, p, findings, tc.path)
		})
	}
}
