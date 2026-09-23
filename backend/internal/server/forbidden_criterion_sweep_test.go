package server

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
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
