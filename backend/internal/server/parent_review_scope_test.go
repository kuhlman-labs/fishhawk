package server

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/policy"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// The decomposed-parent implement-review baseline (#4425). Every test here
// drives the REAL runImplementReviews prompt builder over the #4338 fan-out
// fixture, so the serve side (decomposedParentFixupScope) and the review side
// (parentFanOutScopeForReview → trig.AmendedScopeFiles + the provenance fold)
// read the SAME server state.

const approvalSectionHeading = "### Scope amended at approval"

// approvalAmendedSectionBody extracts the 'Scope amended at approval' section
// (heading through the next ### heading). Scoped on purpose: every fan-out
// path the tests look for also appears in the diff file list, and the heading
// NAME appears in the standing review instructions, so a document-wide match
// would pass vacuously.
func approvalAmendedSectionBody(p string) string {
	i := strings.Index(p, approvalSectionHeading)
	if i < 0 {
		return ""
	}
	rest := p[i+len(approvalSectionHeading):]
	if j := strings.Index(rest, "\n### "); j >= 0 {
		return approvalSectionHeading + rest[:j]
	}
	return approvalSectionHeading + rest
}

// reviewPrompt runs the implement review of the fixture's stage over diff and
// returns the one prompt the capturing reviewer received.
func (f *parentFixupScopeFixture) reviewPrompt(t *testing.T, diff policy.Diff, ev *prompt.GateEvidence) string {
	t.Helper()
	if f.s.runImplementReviews(context.Background(), f.runID, f.stageID, diff, nil, "", ev) {
		t.Fatal("a gating approve must not gate")
	}
	f.reviewer.mu.Lock()
	defer f.reviewer.mu.Unlock()
	if len(f.reviewer.calls) != 1 {
		t.Fatalf("reviewer invoked %d times, want 1", len(f.reviewer.calls))
	}
	return f.reviewer.calls[0]
}

// servedDeclaredFiles is the runner's ScopeFacts.DeclaredFiles for this stage:
// the length of the scope_files the runner-facing dispatch SERVES from the same
// server state — never a hand-typed literal.
func (f *parentFixupScopeFixture) servedDeclaredFiles(t *testing.T) (int, []scopeFile) {
	t.Helper()
	resp := f.serve(t)["dispatch"]
	return len(resp.ScopeFiles), resp.ScopeFiles
}

func fixupDiff(paths ...string) policy.Diff {
	var d policy.Diff
	for _, p := range paths {
		d.ChangedFiles = append(d.ChangedFiles, policy.ChangedFile{Path: p, Status: policy.StatusModified})
	}
	return d
}

// reviewProvenance recomputes, from the fixture's server state, the scope
// provenance runImplementReviewsForTree builds for this stage — the structured
// form of the block the rendered-prompt assertions read — so a test can
// compare its reconstructed set against the served one.
func reviewProvenance(t *testing.T, f *parentFixupScopeFixture, diff policy.Diff, ev *prompt.GateEvidence) *prompt.GateScopeProvenance {
	t.Helper()
	ctx := context.Background()
	runRow, err := f.s.cfg.RunRepo.GetRun(ctx, f.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	approvedPlan, err := f.s.loadApprovedPlanForRun(ctx, f.runID)
	if err != nil || approvedPlan == nil {
		t.Fatalf("load plan: %v", err)
	}
	fanOut := f.s.parentFanOutScopeForReview(ctx, runRow, approvedPlan)
	trig := prompt.Trigger{
		AmendedScopeFiles:      appendFanOutPaths(f.s.amendedScopeFilesForReview(ctx, runRow, approvedPlan), fanOut),
		ChildAmendedScopeFiles: f.s.childApprovedAmendmentScopePaths(ctx, f.runID),
		PriorConcerns:          f.s.priorConcernsForReview(ctx, f.runID, f.stageID),
	}
	return f.s.scopeProvenanceForReview(ctx, f.runID, f.stageID, approvedPlan, trig, fanOut, diff, ev)
}

// TestParentReviewScope_FixupEditsSliceGrant_NoDrift is #4425's done-means: a
// decomposed parent's fix-up re-review whose diff edits the slice-granted path
// (admitted by the #4338 serve) lists that path — and the sub_plan-only paths —
// in the 'Scope amended at approval' section, folds it under
// parent-slice-add-scope-files in the provenance block, and leaves NO
// 'unexplained by provenance' residual against the DeclaredFiles the serve
// actually handed the runner. The first consolidated round (no routed concern)
// gets the same section and folds.
//
// MECHANISM: DeclaredFiles counts the served union (sub_plan paths, the slice
// grant, their coupled tests). Without the review union the reconstruction
// lacks them, so a positive residual renders and the section is absent (the
// fixture records no flat add_scope_files). The fold assertions name the
// SOURCE and Detail, which only the fan-out fold sets.
func TestParentReviewScope_FixupEditsSliceGrant_NoDrift(t *testing.T) {
	const (
		promptGo     = "backend/internal/server/prompt.go"
		sliceFold    = sliceGrantPath + " (folded: parent-slice-add-scope-files) (granted to slice 0 at plan approval via add_scope_files_to_slice)"
		subPlanAFold = "pkg/a/a.go (folded: parent-sub-plan-scope) (declared in decomposition sub_plan 0 scope.files)"
	)
	assertSection := func(t *testing.T, p string) {
		t.Helper()
		section := approvalAmendedSectionBody(p)
		if section == "" {
			t.Fatalf("'Scope amended at approval' section absent from the parent review prompt:\n%s", p)
		}
		for _, want := range []string{sliceGrantPath, "pkg/a/a.go", "pkg/b/b.go"} {
			if !strings.Contains(section, "- "+want+"\n") {
				t.Errorf("section missing fan-out path %s:\n%s", want, section)
			}
		}
		if strings.Contains(section, promptGo) {
			t.Errorf("section lists the top-level plan path %s (already rendered as plan scope):\n%s", promptGo, section)
		}
	}

	t.Run("fixup_rereview", func(t *testing.T) {
		f := newParentFixupScopeFixture(t, "parent", nil)
		declared, served := f.servedDeclaredFiles(t)
		diff := fixupDiff(promptGo, sliceGrantPath)
		ev := &prompt.GateEvidence{ScopeFacts: &prompt.GateScopeFacts{DeclaredFiles: declared}}

		prov := reviewProvenance(t, f, diff, ev)
		if prov == nil || !prov.FixupPass {
			t.Fatalf("provenance = %+v; the fixture must take the fixupPass branch", prov)
		}
		if prov.UnexplainedCount != 0 {
			t.Errorf("UnexplainedCount = %d against served %+v; folds %+v", prov.UnexplainedCount, served, prov.Folds)
		}

		p := f.reviewPrompt(t, diff, ev)
		assertSection(t, p)
		for _, want := range []string{"  - " + sliceFold + "\n", "  - " + subPlanAFold + " — folded, UNTOUCHED"} {
			if !strings.Contains(p, want) {
				t.Errorf("provenance block missing %q:\n%s", want, p)
			}
		}
		if !strings.Contains(p, "- fix-up pass: the declared scope retains the FULL approved plan scope") {
			t.Errorf("provenance block did not render the fix-up pass line — the fixupPass branch did not run:\n%s", p)
		}
		if strings.Contains(p, "unexplained by provenance") {
			t.Errorf("an 'unexplained by provenance' residual rendered; DeclaredFiles=%d served=%+v:\n%s", declared, served, p)
		}
		if !strings.Contains(p, "Machine classification: the declared-vs-staged divergence is FULLY EXPLAINED") {
			t.Errorf("FULLY EXPLAINED machine classification absent:\n%s", p)
		}
	})

	t.Run("first_consolidated_round", func(t *testing.T) {
		f := newParentFixupScopeFixture(t, "parent_no_fixup", nil)
		diff := fixupDiff(promptGo, sliceGrantPath)
		prov := reviewProvenance(t, f, diff, nil)
		if prov == nil || prov.FixupPass {
			t.Fatalf("provenance = %+v; the first consolidated round is not a fix-up pass", prov)
		}
		p := f.reviewPrompt(t, diff, nil)
		assertSection(t, p)
		if !strings.Contains(p, "  - "+sliceFold+"\n") {
			t.Errorf("provenance block missing %q:\n%s", sliceFold, p)
		}
	})
}

// TestParentReviewScope_ServeReviewParity pins C2: the review provenance
// reconstructs EXACTLY the set the parent fix-up serve hands the runner —
// paths and coupled *_test.go siblings — including a .go path recorded both
// as a flat approval add_scope_files (modify) and in a sub_plan (delete). The
// serve folds the flat add as modify BEFORE the parent union, so it grants
// pkg/x/x_test.go; a review that let the sub_plan delete win would drop it.
func TestParentReviewScope_ServeReviewParity(t *testing.T) {
	f := newParentFixupScopeFixtureWith(t, "parent", parentFixupScopeOpts{
		flatAdd:       []string{"pkg/x/x.go"},
		subPlan1Extra: []plan.ScopeFile{{Path: "pkg/x/x.go", Operation: plan.FileOpDelete}},
	})
	declared, served := f.servedDeclaredFiles(t)
	servedSet := make([]string, 0, len(served))
	for _, sf := range served {
		servedSet = append(servedSet, sf.Path)
	}
	if op, ok := scopeOp(served, "pkg/x/x.go"); !ok || op != "modify" {
		t.Fatalf("fixture precondition: served pkg/x/x.go = %q (present %v), want modify", op, ok)
	}
	if _, ok := scopeOp(served, "pkg/x/x_test.go"); !ok {
		t.Fatalf("fixture precondition: the serve must grant the coupled pkg/x/x_test.go: %+v", served)
	}

	ev := &prompt.GateEvidence{ScopeFacts: &prompt.GateScopeFacts{DeclaredFiles: declared}}
	prov := reviewProvenance(t, f, fixupDiff("backend/internal/server/prompt.go"), ev)
	if prov == nil || !prov.FixupPass {
		t.Fatalf("provenance = %+v; want a fix-up pass", prov)
	}
	reconstructed := []string{"backend/internal/server/prompt.go"} // the plan's one top-level path
	if prov.PlanFiles != len(reconstructed) {
		t.Fatalf("PlanFiles = %d, want %d", prov.PlanFiles, len(reconstructed))
	}
	for _, fold := range prov.Folds {
		reconstructed = append(reconstructed, fold.Path)
		if fold.Path == "pkg/x/x.go" && fold.Source != "approval-add-scope-files" {
			t.Errorf("pkg/x/x.go folded under %q, want approval-add-scope-files (the flat channel folds first)", fold.Source)
		}
	}
	sort.Strings(reconstructed)
	sort.Strings(servedSet)
	if !reflect.DeepEqual(reconstructed, servedSet) {
		t.Errorf("review reconstruction != served set\n  review %v\n  served %v", reconstructed, servedSet)
	}
	if prov.UnexplainedCount != 0 {
		t.Errorf("UnexplainedCount = %d, want 0", prov.UnexplainedCount)
	}
}

// TestParentReviewScope_RemovalWins: a gate-time remove_scope_files path that
// is declared ONLY in sub_plan 1 stays out of the review baseline — absent from
// the section body and from every fold. The serve subtracts it a second time
// after its union; the review has no other subtraction, so the channel removal
// is the only thing keeping it out here.
func TestParentReviewScope_RemovalWins(t *testing.T) {
	f := newParentFixupScopeFixture(t, "parent", []string{"pkg/b/b.go"})
	diff := fixupDiff("backend/internal/server/prompt.go", sliceGrantPath)
	p := f.reviewPrompt(t, diff, nil)
	section := approvalAmendedSectionBody(p)
	if !strings.Contains(section, "- pkg/a/a.go\n") {
		t.Fatalf("section missing pkg/a/a.go — only the removed path may drop:\n%s", section)
	}
	if strings.Contains(section, "pkg/b/b.go") {
		t.Errorf("removed pkg/b/b.go listed in the section:\n%s", section)
	}
	if strings.Contains(p, "pkg/b/b.go (folded:") || strings.Contains(p, "pkg/b/b_test.go (folded:") {
		t.Errorf("removed pkg/b/b.go (or its coupled test) folded into the provenance:\n%s", p)
	}
}

// TestParentReviewScope_OrdinaryRunUnchanged: a run that is not a decomposed
// parent gets no fan-out. "ordinary" carries the SAME decomposed plan and
// slice-grant approval with no child registered, so isDecomposedParentRun alone
// keeps the fan-out off it. "child" (C3) keeps exactly its OWN slice grant —
// the existing per-slice channel — and none of slice 1's grant, the parent's
// other sub_plan paths, or a parent-* fold.
func TestParentReviewScope_OrdinaryRunUnchanged(t *testing.T) {
	const slice1Grant = "pkg/grant1/g1.go"
	opts := parentFixupScopeOpts{slice1Grant: slice1Grant}
	diff := fixupDiff("backend/internal/server/prompt.go", sliceGrantPath)

	t.Run("ordinary", func(t *testing.T) {
		p := newParentFixupScopeFixtureWith(t, "ordinary", opts).reviewPrompt(t, diff, nil)
		section := approvalAmendedSectionBody(p)
		for _, path := range []string{sliceGrantPath, "pkg/a/a.go", "pkg/b/b.go", slice1Grant} {
			if strings.Contains(section, path) {
				t.Errorf("ordinary run's section lists fan-out path %s:\n%s", path, section)
			}
		}
		if strings.Contains(p, "(folded: parent-") {
			t.Errorf("ordinary run carries a parent-* fold:\n%s", p)
		}
	})

	t.Run("child", func(t *testing.T) {
		p := newParentFixupScopeFixtureWith(t, "child", opts).reviewPrompt(t, diff, nil)
		section := approvalAmendedSectionBody(p)
		if !strings.Contains(section, "- "+sliceGrantPath+"\n") {
			t.Errorf("child's section lost its OWN slice grant %s:\n%s", sliceGrantPath, section)
		}
		for _, path := range []string{slice1Grant, "pkg/a/a.go", "pkg/b/b.go"} {
			if strings.Contains(section, path) {
				t.Errorf("child's section lists %s (another slice's grant or a parent sub_plan path):\n%s", path, section)
			}
		}
		if strings.Contains(p, "(folded: parent-") {
			t.Errorf("child carries a parent-* fold:\n%s", p)
		}
	})
}

// TestParentReviewScope_UntouchedFanOutNotOperatorUndelivered pins the #1407
// omission: an untouched fan-out path is a slice's permission, not a
// work-order on the parent, so operator_scope_path_undelivered names only the
// untouched FLAT add_scope_files path. The flat path keeps the signal firing,
// so the test cannot pass by the signal never firing.
func TestParentReviewScope_UntouchedFanOutNotOperatorUndelivered(t *testing.T) {
	const flatPath = "pkg/flat/flat.go"
	f := newParentFixupScopeFixtureWith(t, "parent", parentFixupScopeOpts{flatAdd: []string{flatPath}})
	// Leaves flatPath and the sub_plan-only pkg/a/a.go, pkg/b/b.go untouched.
	_ = f.reviewPrompt(t, fixupDiff("backend/internal/server/prompt.go", sliceGrantPath), nil)

	rows := f.audit.appendedByCategory(operatorScopeUndeliveredCategory)
	if len(rows) != 1 {
		t.Fatalf("operator_scope_path_undelivered rows = %d, want exactly 1", len(rows))
	}
	var payload operatorScopeUndeliveredPayload
	if err := json.Unmarshal(rows[0].Payload, &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if !reflect.DeepEqual(payload.UndeliveredPaths, []string{flatPath}) {
		t.Errorf("undelivered = %v, want only the flat add %s (never a fan-out path)", payload.UndeliveredPaths, flatPath)
	}
}
