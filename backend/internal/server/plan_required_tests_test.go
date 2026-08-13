package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// specImplementNoRequiredOutcomes is specImplementPathConstraints with the
// required_outcomes constraint removed: the fail-open leg for a workflow
// that never asks for tests.
var specImplementNoRequiredOutcomes = []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 3
`)

// requiredTestsAuditCategories collects the categories the gate appended.
func requiredTestsAuditCategories(au *approvalAuditFake) []string {
	var out []string
	for _, e := range au.appended {
		out = append(out, e.Category)
	}
	return out
}

func hasCategory(cats []string, want string) bool {
	for _, c := range cats {
		if c == want {
			return true
		}
	}
	return false
}

// assertRefused reads the COMMITTED STATE after the call, not just the error
// body: both this gate and its siblings are pre-Submit, and a gate that fired
// and was rolled back would return a byte-identical error. The load-bearing
// evidence is that no approval row exists and the stage never advanced.
func assertRefused(t *testing.T, w *httptest.ResponseRecorder, app *fakeApprovalRepo, stage *run.Stage, wantCode string) {
	t.Helper()
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"`+wantCode+`"`) {
		t.Fatalf("body missing %s: %s", wantCode, w.Body.String())
	}
	app.mu.Lock()
	rows := len(app.all)
	app.mu.Unlock()
	if rows != 0 {
		t.Fatalf("approval rows = %d, want 0 (a PRE-Submit refusal must insert nothing)", rows)
	}
	if stage.State != run.StageStateAwaitingApproval {
		t.Fatalf("stage.State = %q, want awaiting_approval (a refusal must not advance the stage)", stage.State)
	}
}

// TestPlanRequiredTests_NoTestPath_Refused is issue AC4 and the SHIPPED
// BEHAVIOR assertion for the plan-gate half of #2660: a plan whose effective
// scope declares testable source and no test file is refused at the gate
// rather than after a full implement run. The refusal names the constraint
// and both remedies, appends plan_missing_required_tests, inserts NO approval
// row, and leaves the stage parked.
func TestPlanRequiredTests_NoTestPath_Refused(t *testing.T) {
	s, app, _, au, stage := newScopeCapServer(t, specImplementPathConstraints, []plan.ScopeFile{
		{Path: "backend/foo.go", Operation: plan.FileOpModify},
	})

	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	assertRefused(t, w, app, stage, "plan_missing_required_tests")

	body := w.Body.String()
	for _, want := range []string{"required_outcomes: tests_added_or_updated", "add_scope_files", "--comment-only"} {
		if !strings.Contains(body, want) {
			t.Errorf("message must name %q: %s", want, body)
		}
	}
	if !strings.Contains(body, `"testable_source_files":1`) {
		t.Errorf("details missing testable_source_files: %s", body)
	}
	if !hasCategory(requiredTestsAuditCategories(au), "plan_missing_required_tests") {
		t.Errorf("expected a plan_missing_required_tests audit entry, got %v", requiredTestsAuditCategories(au))
	}
	for _, c := range requiredTestsAuditCategories(au) {
		if c == "approval_submitted" {
			t.Errorf("unexpected approval_submitted audit entry on a refusal")
		}
	}
}

// TestPlanRequiredTests_TestPathInScope_Proceeds: a declared _test.go
// satisfies the gate by construction.
func TestPlanRequiredTests_TestPathInScope_Proceeds(t *testing.T) {
	s, _, _, _, stage := newScopeCapServer(t, specImplementPathConstraints, []plan.ScopeFile{
		{Path: "backend/foo.go", Operation: plan.FileOpModify},
		{Path: "backend/foo_test.go", Operation: plan.FileOpCreate},
	})
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	if stage.State != run.StageStateSucceeded {
		t.Errorf("stage.State = %q, want succeeded", stage.State)
	}
}

// TestPlanRequiredTests_TestPathViaAddScopeFiles_Proceeds: the in-loop remedy
// the refusal message names — declare the test file at the gate, no re-plan.
func TestPlanRequiredTests_TestPathViaAddScopeFiles_Proceeds(t *testing.T) {
	s, _, _, _, stage := newScopeCapServer(t, specImplementPathConstraints, []plan.ScopeFile{
		{Path: "backend/foo.go", Operation: plan.FileOpModify},
	})
	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","add_scope_files":["backend/foo_test.go"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// TestPlanRequiredTests_DocsOnlyScope_Proceeds: a scope with no testable
// source is satisfied at implement by the #610 vacuous branch, so refusing it
// would be wrong.
func TestPlanRequiredTests_DocsOnlyScope_Proceeds(t *testing.T) {
	s, _, _, _, stage := newScopeCapServer(t, specImplementPathConstraints, []plan.ScopeFile{
		{Path: "backend/README.md", Operation: plan.FileOpModify},
	})
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// TestPlanRequiredTests_OutcomeNotRequired_Proceeds: the gate is inert when
// the implement stage never declares tests_added_or_updated.
func TestPlanRequiredTests_OutcomeNotRequired_Proceeds(t *testing.T) {
	s, _, _, _, stage := newScopeCapServer(t, specImplementNoRequiredOutcomes, []plan.ScopeFile{
		{Path: "backend/foo.go", Operation: plan.FileOpModify},
	})
	w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
}

// TestPlanRequiredTests_CommentOnlyOverride_AllGoScope_Honored: the escape
// for the exact case the envelope admits. The honored path writes the
// acknowledgement entry recording that the override covers the PLAN gate only.
func TestPlanRequiredTests_CommentOnlyOverride_AllGoScope_Honored(t *testing.T) {
	s, app, _, au, stage := newScopeCapServer(t, specImplementPathConstraints, []plan.ScopeFile{
		{Path: "backend/foo.go", Operation: plan.FileOpModify},
	})
	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","comment":"doc comment typo --comment-only"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	app.mu.Lock()
	rows := len(app.all)
	app.mu.Unlock()
	if rows != 1 {
		t.Errorf("approval rows = %d, want 1 (the override must reach Submit)", rows)
	}
	var ack []byte
	for _, e := range au.appended {
		if e.Category == "plan_comment_only_override_acknowledged" {
			ack = e.Payload
		}
	}
	if ack == nil {
		t.Fatalf("expected plan_comment_only_override_acknowledged, got %v", requiredTestsAuditCategories(au))
	}
	if !strings.Contains(string(ack), "re-derives the comment-only verdict from the real diff") {
		t.Errorf("ack payload must record that the implement stage re-derives the verdict: %s", ack)
	}
}

// TestPlanRequiredTests_CommentOnlyOverride_NonGoScope_Refused is the
// CATEGORICAL arm: the exemption is .go-only, so no override can authorize a
// scope carrying a .py file. Refusal is committed-state-checked like its
// sibling above.
func TestPlanRequiredTests_CommentOnlyOverride_NonGoScope_Refused(t *testing.T) {
	s, app, _, au, stage := newScopeCapServer(t, specImplementPathConstraints, []plan.ScopeFile{
		{Path: "backend/foo.go", Operation: plan.FileOpModify},
		{Path: "backend/tool.py", Operation: plan.FileOpModify},
	})
	w := submitApproval(t, s, stage.ID,
		`{"decision":"approve","comment":"just comments --comment-only"}`)
	assertRefused(t, w, app, stage, "plan_comment_only_override_unavailable")

	if !strings.Contains(w.Body.String(), "backend/tool.py") {
		t.Errorf("message must name the non-.go path: %s", w.Body.String())
	}
	if !hasCategory(requiredTestsAuditCategories(au), "plan_comment_only_override_refused") {
		t.Errorf("expected plan_comment_only_override_refused, got %v", requiredTestsAuditCategories(au))
	}
}

// TestPlanRequiredTests_FailOpenLegs covers every degraded read the gate must
// survive: without them a backend that cannot resolve a spec or a plan would
// brick the approval path outright.
func TestPlanRequiredTests_FailOpenLegs(t *testing.T) {
	// No workflow spec on the run row → resolveImplementRequiredOutcomes
	// fails open.
	t.Run("no workflow spec", func(t *testing.T) {
		s, _, _, _, stage := newScopeCapServer(t, nil, []plan.ScopeFile{
			{Path: "backend/foo.go", Operation: plan.FileOpModify},
		})
		w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (fail open):\n%s", w.Code, w.Body.String())
		}
	})

	// Unparseable spec → same leg, different cause.
	t.Run("unparseable spec", func(t *testing.T) {
		s, _, _, _, stage := newScopeCapServer(t, []byte("::not yaml::"), []plan.ScopeFile{
			{Path: "backend/foo.go", Operation: plan.FileOpModify},
		})
		w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (fail open):\n%s", w.Code, w.Body.String())
		}
	})

	// A workflow with a plan stage but NO implement stage.
	t.Run("no implement stage", func(t *testing.T) {
		specNoImplement := []byte(`version: "0.3"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`)
		s, _, _, _, stage := newScopeCapServer(t, specNoImplement, []plan.ScopeFile{
			{Path: "backend/foo.go", Operation: plan.FileOpModify},
		})
		w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (fail open):\n%s", w.Code, w.Body.String())
		}
	})

	// No plan artifact → effectiveScopePathSet returns ok=false.
	t.Run("no plan artifact", func(t *testing.T) {
		s, _, _, _, stage := newScopeCapServer(t, specImplementPathConstraints, nil)
		w := submitApproval(t, s, stage.ID, `{"decision":"approve"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (fail open):\n%s", w.Code, w.Body.String())
		}
	})
}
