package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// sliceGrantPath is the #4250 shape: a path granted to slice 0 at plan approval
// (add_scope_files_to_slice). It is in no plan scope and no amendment, and no
// in-scope source file has it as a coupled *_test.go sibling, so the slice-grant
// fold is the ONLY channel that can serve it.
const sliceGrantPath = "backend/internal/agenteval/injection_test.go"

// parentFixupScopeFixture serves one implement stage of the #4338 fan-out. The
// review-side fields (#4425) let the SAME server state drive the implement
// review: audit captures appended rows, concerns holds the fix-up-routed
// concern the review's fixupPass keys on, and reviewer records each prompt.
type parentFixupScopeFixture struct {
	s           *Server
	parentRunID uuid.UUID
	runID       uuid.UUID
	stageID     uuid.UUID
	priv        ed25519.PrivateKey
	audit       *feedbackAuditRepo
	concerns    *fakeConcernRepo
	reviewer    *fakePlanReviewer
}

// parentFixupScopeOpts extends the fixture for the review-baseline tests
// (#4425) without moving any byte the #4338 serve tests pin: every field's
// zero value reproduces newParentFixupScopeFixture exactly.
type parentFixupScopeOpts struct {
	remove []string
	// flatAdd is recorded as the parent approval's flat add_scope_files.
	flatAdd []string
	// subPlan1Extra is appended to sub_plan 1's scope.files.
	subPlan1Extra []plan.ScopeFile
	// slice1Grant, when set, is granted to slice 1 at approval.
	slice1Grant string
}

// newParentFixupScopeFixture builds a decomposed parent P whose plan declares
// top-level [prompt.go] and sub_plans 0 [pkg/a/a.go] and 1 [pkg/b/b.go], with
// fan-out children C0 (slice 0) and C1 (slice 1). P's plan-stage approval
// grants sliceGrantPath to slice 0 and removes `remove`. C1 holds an APPROVED
// create amendment for pkg/c/c.go and a PENDING one for pkg/pending/p.go; C0
// holds a DENIED one for pkg/denied/d.go. Shapes:
//
//   - "parent": P's routed fix-up.
//   - "ordinary": the SAME plan and approval row with NO child registered, so
//     isDecomposedParentRun reads false.
//   - "child": C0's own routed fix-up.
//   - "parent_no_fixup": P with no stage_fixup_triggered row.
//   - "parent_merge_candidate": P's fix-up plus a live merge-candidate trigger,
//     so the serve is a verify-only pass with no fix-up block.
//   - "parent_probe_error": P's fix-up whose children probe errors.
//
// Every fix-up shape also seeds an addressed_pending implement concern on the
// served stage (the review's fixupPass trigger), and every run carries the
// gating-reviewer workflow spec with a capturing reviewer, so the implement
// review of the same stage takes the fix-up re-review branch for real.
func newParentFixupScopeFixture(t *testing.T, shape string, remove []string) *parentFixupScopeFixture {
	t.Helper()
	return newParentFixupScopeFixtureWith(t, shape, parentFixupScopeOpts{remove: remove})
}

func newParentFixupScopeFixtureWith(t *testing.T, shape string, opts parentFixupScopeOpts) *parentFixupScopeFixture {
	t.Helper()
	remove := opts.remove
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()
	sar := newFakeScopeAmendmentRepo()

	parentID, c0ID, c1ID := uuid.New(), uuid.New(), uuid.New()
	planStageID, parentImplID, c0ImplID, c1ImplID := uuid.New(), uuid.New(), uuid.New(), uuid.New()

	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "decomposed plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
		}},
		Decomposition: &plan.Decomposition{
			Rationale: "scope split",
			SubPlans: []plan.SubPlanSummary{
				{Title: "Part A", ScopeHint: "A", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}}},
				{Title: "Part B", ScopeHint: "B", Scope: &plan.Scope{Files: append(
					[]plan.ScopeFile{{Path: "pkg/b/b.go", Operation: plan.FileOpModify}}, opts.subPlan1Extra...)}},
			},
		},
	}
	planBytes, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal plan: %v", err)
	}
	sv := "standard_v1"
	if _, err := art.Create(context.Background(), artifact.CreateParams{
		StageID: planStageID, Kind: artifact.KindPlan, SchemaVersion: &sv, Content: planBytes,
	}); err != nil {
		t.Fatalf("seed plan artifact: %v", err)
	}

	planStage := &run.Stage{ID: planStageID, RunID: parentID, Type: run.StageTypePlan}
	parentImpl := &run.Stage{ID: parentImplID, RunID: parentID, Type: run.StageTypeImplement}
	c0Impl := &run.Stage{ID: c0ImplID, RunID: c0ID, Type: run.StageTypeImplement}
	c1Impl := &run.Stage{ID: c1ImplID, RunID: c1ID, Type: run.StageTypeImplement}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		parentID: {planStage, parentImpl},
		c0ID:     {c0Impl},
		c1ID:     {c1Impl},
	}
	// The plan stage is registered for GetStage so approvalEntryStageIsPlan
	// confirms the approval row (the #2598 wall).
	for _, st := range []*run.Stage{planStage, parentImpl, c0Impl, c1Impl} {
		rr.getStages[st.ID] = st
	}

	idx0, idx1 := 0, 1
	rr.getRuns[parentID] = &run.Run{ID: parentID, Repo: "o/r", WorkflowID: "feature_change",
		WorkflowSpec: specImplementGatingReviewers}
	if shape != "ordinary" {
		rr.getRuns[c0ID] = &run.Run{ID: c0ID, Repo: "o/r", WorkflowID: "feature_change",
			WorkflowSpec: specImplementGatingReviewers,
			ParentRunID:  &parentID, DecomposedFrom: &parentID, SliceIndex: &idx0}
		rr.getRuns[c1ID] = &run.Run{ID: c1ID, Repo: "o/r", WorkflowID: "feature_change",
			WorkflowSpec: specImplementGatingReviewers,
			ParentRunID:  &parentID, DecomposedFrom: &parentID, SliceIndex: &idx1}
	}
	if shape == "parent_probe_error" {
		rr.listRunsErr = errors.New("list runs: connection reset")
	}

	seedAmendmentEntries(sar, c1ID, scopeamendment.StatusApproved,
		scopeamendment.PathEntry{Path: "pkg/c/c.go", Operation: scopeamendment.OperationCreate})
	seedAmendment(sar, c1ID, scopeamendment.StatusPending, "pkg/pending/p.go")
	seedAmendment(sar, c0ID, scopeamendment.StatusDenied, "pkg/denied/d.go")

	sliceGrants := map[string][]string{"0": {sliceGrantPath}}
	if opts.slice1Grant != "" {
		sliceGrants["1"] = []string{opts.slice1Grant}
	}
	approval := map[string]any{
		"decision":                 "approve",
		"stage_id":                 planStageID.String(),
		"add_scope_files_to_slice": sliceGrants,
	}
	if len(remove) > 0 {
		approval["remove_scope_files"] = remove
	}
	if len(opts.flatAdd) > 0 {
		approval["add_scope_files"] = opts.flatAdd
	}
	approvalPayload, _ := json.Marshal(approval)
	pid := parentID
	byRunID := map[uuid.UUID][]*audit.Entry{
		parentID: {{ID: uuid.New(), Category: "approval_submitted", RunID: &pid, Payload: approvalPayload, Sequence: 1}},
	}

	runID, stageID := parentID, parentImplID
	if shape == "child" {
		runID, stageID = c0ID, c0ImplID
	}
	concerns := newFakeConcernRepo()
	if shape != "parent_no_fixup" {
		byRunID[runID] = append(byRunID[runID], makeFixupEntry(runID, stageID,
			[]planreview.Concern{{Severity: planreview.SeverityHigh, Category: "coverage", Note: "tighten the bound"}}))
		// The routed concern the fix-up trigger moved to addressed_pending: what
		// makes the implement review of this stage a fix-up re-review.
		concerns.rows = append(concerns.rows, &concern.Concern{
			ID: uuid.New(), RunID: runID, StageID: stageID, StageKind: concern.StageKindImplement,
			Severity: string(planreview.SeverityHigh), Category: "coverage", Note: "tighten the bound",
			State: concern.StateAddressedPending,
		})
	}
	if shape == "parent_merge_candidate" {
		payload, _ := json.Marshal(mergeCandidateVerifyTrigger{
			Branch: orchestrator.ConsolidatedBranch(runID), BaseRef: "main",
			ExpectedHeadSHA: strings.Repeat("a", 40), Cause: mergeCandidateCauseFanIn,
			VerifyCommand: "scripts/test verify", PriorState: string(run.StageStateAwaitingApproval),
		})
		rid, sid := runID, stageID
		byRunID[runID] = append(byRunID[runID], &audit.Entry{ID: uuid.New(), Category: CategoryStageMergeCandidateVerifyTriggered,
			RunID: &rid, StageID: &sid, Payload: payload, Sequence: 3})
	}

	priv, _ := sf.issue(t, runID)
	auditRepo := &feedbackAuditRepo{byRunID: byRunID}
	reviewer := &fakePlanReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-opus-4-7",
	}
	s := New(Config{
		Addr:               "127.0.0.1:0",
		RunRepo:            rr,
		SigningRepo:        sf,
		ArtifactRepo:       art,
		ScopeAmendmentRepo: sar,
		AuditRepo:          auditRepo,
		ConcernRepo:        concerns,
		PlanReviewers:      singleReviewerSet{reviewer},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}
	return &parentFixupScopeFixture{s: s, parentRunID: parentID, runID: runID, stageID: stageID, priv: priv,
		audit: auditRepo, concerns: concerns, reviewer: reviewer}
}

// serve fetches the stage's prompt through BOTH surfaces — the signed
// runner-facing dispatch (every capability advertised) and the unsigned SPA
// preview — and decodes each response.
func (f *parentFixupScopeFixture) serve(t *testing.T) map[string]promptResponse {
	t.Helper()
	raw := map[string]*httptest.ResponseRecorder{
		"dispatch": promptRequestWithCapabilities(t, f.s, f.stageID, f.priv, capableRunnerCapabilities),
		"preview":  promptRenderRequest(t, f.s, f.stageID),
	}
	out := make(map[string]promptResponse, len(raw))
	for surface, w := range raw {
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200:\n%s", surface, w.Code, w.Body.String())
		}
		var resp promptResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s decode: %v", surface, err)
		}
		out[surface] = resp
	}
	return out
}

// wantScope builds an expected scope_files list; every entry is modify unless named
// in creates.
func wantScope(creates map[string]bool, paths ...string) []scopeFile {
	out := make([]scopeFile, 0, len(paths))
	for _, p := range paths {
		op := "modify"
		if creates[p] {
			op = "create"
		}
		out = append(out, scopeFile{Path: p, Operation: op})
	}
	return out
}

func scopeOp(scope []scopeFile, path string) (string, bool) {
	for _, f := range scope {
		if f.Path == path {
			return f.Operation, true
		}
	}
	return "", false
}

// TestDecomposedParentFixupScope_ServedUnion is #4338's done-means: a decomposed
// parent's fix-up serves, on BOTH surfaces, the union of its sub_plan scope,
// every slice's add_scope_files_to_slice grant and every child's APPROVED
// amendment (operation preserved), with effectiveFixupScope's coupled-test fold
// run over that union.
//
// MECHANISM: each union channel serves a path reachable through it ALONE —
// pkg/a/a.go only via sub_plan 0, sliceGrantPath only via the slice-0 grant,
// pkg/c/c.go only via C1's approved amendment — so deleting a fold makes
// exactly that path absent (and nothing downstream re-adds it). pkg/pending/p.go
// and pkg/denied/d.go are present in the amendment fake, so they are absent
// from the served list only while the approved-only filter holds.
func TestDecomposedParentFixupScope_ServedUnion(t *testing.T) {
	f := newParentFixupScopeFixture(t, "parent", nil)
	want := wantScope(map[string]bool{"pkg/c/c.go": true},
		"backend/internal/server/prompt.go",
		"pkg/a/a.go", "pkg/b/b.go",
		sliceGrantPath,
		"pkg/c/c.go",
		"backend/internal/server/prompt_test.go", "pkg/a/a_test.go", "pkg/b/b_test.go", "pkg/c/c_test.go",
	)
	served := f.serve(t)
	for surface, resp := range served {
		if !resp.Fixup {
			t.Fatalf("%s: fixup = false; the fixture must serve the fix-up block", surface)
		}
		for _, path := range []string{"pkg/a/a.go", "pkg/b/b.go", sliceGrantPath, "pkg/c/c.go", "pkg/a/a_test.go", "pkg/b/b_test.go"} {
			if _, ok := scopeOp(resp.ScopeFiles, path); !ok {
				t.Errorf("%s: %s missing from the parent fix-up scope_files: %+v", surface, path, resp.ScopeFiles)
			}
		}
		if op, ok := scopeOp(resp.ScopeFiles, "pkg/c/c.go"); ok && op != "create" {
			t.Errorf("%s: pkg/c/c.go operation = %q, want create (the child amendment's operation)", surface, op)
		}
		for _, path := range []string{"pkg/pending/p.go", "pkg/denied/d.go"} {
			if _, ok := scopeOp(resp.ScopeFiles, path); ok {
				t.Errorf("%s: %s served; a pending/denied child amendment confers nothing: %+v", surface, path, resp.ScopeFiles)
			}
		}
		if !reflect.DeepEqual(resp.ScopeFiles, want) {
			t.Errorf("%s: scope_files =\n  %+v\nwant\n  %+v", surface, resp.ScopeFiles, want)
		}
		if wantBranch := orchestrator.ConsolidatedBranch(f.parentRunID); resp.FixupBranch != wantBranch {
			t.Errorf("%s: fixup_branch = %q, want the consolidated branch %q", surface, resp.FixupBranch, wantBranch)
		}
	}
	if !reflect.DeepEqual(served["dispatch"].ScopeFiles, served["preview"].ScopeFiles) {
		t.Errorf("dispatch and preview disagree:\n  dispatch %+v\n  preview  %+v",
			served["dispatch"].ScopeFiles, served["preview"].ScopeFiles)
	}
}

// TestDecomposedParentFixupScope_Shapes pins that every serve other than a
// decomposed parent's fix-up keeps today's scope_files byte-for-byte.
//
// MECHANISM: every shape carries the decomposed plan, the slice-grant row and
// the child amendments, so a union applied where it must not be is observable
// (sub_plan paths, sliceGrantPath or pkg/c/c.go appear). "ordinary" differs from
// the ServedUnion fixture ONLY in having no child registered, so the
// isDecomposedParentRun gate alone keeps the union off it; "child" is a run
// with DecomposedFrom set, so the gate alone keeps C1's pkg/b/b.go and
// pkg/c/c.go off slice 0.
func TestDecomposedParentFixupScope_Shapes(t *testing.T) {
	for _, tc := range []struct {
		shape     string
		wantFixup bool
		want      []scopeFile
	}{
		{"ordinary", true, wantScope(nil, "backend/internal/server/prompt.go", "backend/internal/server/prompt_test.go")},
		{"child", true, wantScope(nil, "pkg/a/a.go", "pkg/a/a_test.go", sliceGrantPath)},
		{"parent_no_fixup", false, wantScope(nil, "backend/internal/server/prompt.go")},
		{"parent_merge_candidate", false, wantScope(nil, "backend/internal/server/prompt.go")},
		{"parent_probe_error", true, wantScope(nil, "backend/internal/server/prompt.go", "backend/internal/server/prompt_test.go")},
	} {
		t.Run(tc.shape, func(t *testing.T) {
			f := newParentFixupScopeFixture(t, tc.shape, nil)
			for surface, resp := range f.serve(t) {
				if resp.Fixup != tc.wantFixup {
					t.Fatalf("%s: fixup = %v, want %v (fixture precondition)", surface, resp.Fixup, tc.wantFixup)
				}
				if tc.shape == "parent_merge_candidate" && !resp.MergeCandidateVerify {
					t.Fatalf("%s: merge_candidate_verify = false; the fixture must serve the verify-only pass", surface)
				}
				if !reflect.DeepEqual(resp.ScopeFiles, tc.want) {
					t.Errorf("%s: scope_files =\n  %+v\nwant today's\n  %+v", surface, resp.ScopeFiles, tc.want)
				}
			}
		})
	}
}

// TestDecomposedParentFixupScope_RemovalWins pins the trailing re-subtraction:
// a gate-time remove_scope_files path declared ONLY in sub_plan 1 stays out of
// the parent fix-up scope even though the sub_plan union would re-introduce it.
//
// MECHANISM: the handler's own removal runs BEFORE the fix-up block, against the
// top-level base [prompt.go], where pkg/b/b.go is absent — so it is a no-op for
// b.go. Only decomposedParentFixupScope's trailing subtractScopePaths removes
// it; with that subtraction deleted pkg/b/b.go (and its coupled pkg/b/b_test.go)
// is served.
func TestDecomposedParentFixupScope_RemovalWins(t *testing.T) {
	f := newParentFixupScopeFixture(t, "parent", []string{"pkg/b/b.go"})
	want := wantScope(map[string]bool{"pkg/c/c.go": true},
		"backend/internal/server/prompt.go",
		"pkg/a/a.go",
		sliceGrantPath,
		"pkg/c/c.go",
		"backend/internal/server/prompt_test.go", "pkg/a/a_test.go", "pkg/c/c_test.go",
	)
	for surface, resp := range f.serve(t) {
		for _, path := range []string{"pkg/b/b.go", "pkg/b/b_test.go"} {
			if _, ok := scopeOp(resp.ScopeFiles, path); ok {
				t.Errorf("%s: %s served; the gate-time removal must win over the sub_plan union: %+v", surface, path, resp.ScopeFiles)
			}
		}
		if _, ok := scopeOp(resp.ScopeFiles, "pkg/a/a.go"); !ok {
			t.Errorf("%s: pkg/a/a.go missing; only the removed path may drop: %+v", surface, resp.ScopeFiles)
		}
		if !reflect.DeepEqual(resp.ScopeFiles, want) {
			t.Errorf("%s: scope_files =\n  %+v\nwant\n  %+v", surface, resp.ScopeFiles, want)
		}
	}
}

// TestDecomposedParentFixupScope_SliceGrantOrderDeterministic pins
// parentSliceGrantPaths: every slice's grant, flattened in numeric slice-index
// order (never Go map order) with non-numeric keys last in lexicographic order,
// deduped first-wins; a row recorded off the plan stage is ignored (#2598); no
// row → nil. Run with -count=20 to expose a map-order dependence.
func TestDecomposedParentFixupScope_SliceGrantOrderDeterministic(t *testing.T) {
	runID := uuid.New()
	planStage := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypePlan}
	implStage := &run.Stage{ID: uuid.New(), RunID: runID, Type: run.StageTypeImplement}
	newServer := func(stageID uuid.UUID, grants map[string][]string) *Server {
		rr := newPromptRunRepo()
		rr.getStages[planStage.ID] = planStage
		rr.getStages[implStage.ID] = implStage
		payload, _ := json.Marshal(map[string]any{
			"decision": "approve", "stage_id": stageID.String(), "add_scope_files_to_slice": grants,
		})
		rid := runID
		return New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{
			runID: {{ID: uuid.New(), Category: "approval_submitted", RunID: &rid, Payload: payload}},
		}}})
	}
	grants := map[string][]string{
		"10":    {"s10.go", "dup.go"},
		"2":     {"s2.go"},
		"extra": {"extra.go"},
		"0":     {"s0.go", "dup.go"},
		"aaa":   {"aaa.go"},
	}
	want := []string{"s0.go", "dup.go", "s2.go", "s10.go", "aaa.go", "extra.go"}
	for i := 0; i < 20; i++ {
		if got := newServer(planStage.ID, grants).parentSliceGrantPaths(context.Background(), runID); !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: got %v, want %v", i, got, want)
		}
	}
	if got := newServer(implStage.ID, grants).parentSliceGrantPaths(context.Background(), runID); got != nil {
		t.Errorf("off-plan-stage row: got %v, want nil (the #2598 wall)", got)
	}
	if got := newServer(planStage.ID, grants).parentSliceGrantPaths(context.Background(), uuid.New()); got != nil {
		t.Errorf("no approval row: got %v, want nil", got)
	}
}

// decomposedParentFixupScope's empty-scope / missing-plan guard: a
// plan_missing_for_implement fix-up keeps the runner's git add -A fallback
// rather than being handed a synthesized scope.
func TestDecomposedParentFixupScope_EmptyScopeOrNilPlanUnchanged(t *testing.T) {
	f := newParentFixupScopeFixture(t, "parent", nil)
	runRow := &run.Run{ID: f.parentRunID}
	if got := f.s.decomposedParentFixupScope(context.Background(), runRow, &plan.Plan{}, nil, nil); got != nil {
		t.Errorf("empty scope: got %+v, want nil", got)
	}
	if got := subPlanScopeEntries(&plan.Plan{}); got != nil {
		t.Errorf("plan with no declared files: subPlanScopeEntries = %+v, want nil", got)
	}
	base := wantScope(nil, "backend/internal/server/prompt.go")
	if got := f.s.decomposedParentFixupScope(context.Background(), runRow, nil, base, nil); !reflect.DeepEqual(got, base) {
		t.Errorf("nil plan: got %+v, want the base scope unchanged", got)
	}
}

// TestParentFanOutScopeForReview_Projection pins the #4425 review projection
// of decomposedParentScopeChannels: on a decomposed parent it lists the
// sub_plan-only paths (Detail naming the first declaring sub_plan, operation
// preserved) then the slice grants (Detail naming the slice), never the
// top-level prompt.go; every other shape — no child registered, a child, a
// children-probe error — gets nil (today's baseline).
func TestParentFanOutScopeForReview_Projection(t *testing.T) {
	want := []parentFanOutScopePath{
		{Path: "pkg/a/a.go", Operation: "modify", Source: "parent-sub-plan-scope", Detail: "declared in decomposition sub_plan 0 scope.files"},
		{Path: "pkg/b/b.go", Operation: "modify", Source: "parent-sub-plan-scope", Detail: "declared in decomposition sub_plan 1 scope.files"},
		{Path: "pkg/b/gone.go", Operation: "delete", Source: "parent-sub-plan-scope", Detail: "declared in decomposition sub_plan 1 scope.files"},
		{Path: sliceGrantPath, Operation: "modify", Source: "parent-slice-add-scope-files", Detail: "granted to slice 0 at plan approval via add_scope_files_to_slice"},
		{Path: "pkg/grant1/g1.go", Operation: "modify", Source: "parent-slice-add-scope-files", Detail: "granted to slice 1 at plan approval via add_scope_files_to_slice"},
	}
	opts := parentFixupScopeOpts{
		subPlan1Extra: []plan.ScopeFile{
			{Path: "pkg/b/gone.go", Operation: plan.FileOpDelete},
			// Declared top-level AND in sub_plan 1: never projected.
			{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpDelete},
		},
		slice1Grant: "pkg/grant1/g1.go",
	}
	project := func(f *parentFixupScopeFixture) []parentFanOutScopePath {
		runRow, err := f.s.cfg.RunRepo.GetRun(context.Background(), f.runID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		approvedPlan, err := f.s.loadApprovedPlanForRun(context.Background(), f.runID)
		if err != nil || approvedPlan == nil {
			t.Fatalf("load plan: %v (nil=%v)", err, approvedPlan == nil)
		}
		return f.s.parentFanOutScopeForReview(context.Background(), runRow, approvedPlan)
	}
	if got := project(newParentFixupScopeFixtureWith(t, "parent", opts)); !reflect.DeepEqual(got, want) {
		t.Errorf("parent projection =\n  %+v\nwant\n  %+v", got, want)
	}
	for _, shape := range []string{"ordinary", "child", "parent_probe_error"} {
		if got := project(newParentFixupScopeFixtureWith(t, shape, opts)); got != nil {
			t.Errorf("%s: projection = %+v, want nil (not a decomposed parent)", shape, got)
		}
	}

	f := newParentFixupScopeFixture(t, "parent", nil)
	runRow := &run.Run{ID: f.parentRunID}
	if got := f.s.parentFanOutScopeForReview(context.Background(), runRow, nil); got != nil {
		t.Errorf("nil plan: projection = %+v, want nil", got)
	}
	empty := &plan.Plan{Decomposition: &plan.Decomposition{SubPlans: []plan.SubPlanSummary{
		{Title: "A", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}}},
	}}}
	if got := f.s.parentFanOutScopeForReview(context.Background(), runRow, empty); got != nil {
		t.Errorf("empty top-level scope: projection = %+v, want nil (the serve never widens an empty scope)", got)
	}
}

// TestParentFanOutScopeForReview_FlatAddKeepsItsChannel pins the C2 parity
// rule: a path recorded both as a flat approval add_scope_files AND in a
// sub_plan is NOT projected, so the review folds it under the flat channel
// (modify) ahead of the parent union, exactly as the serve does.
func TestParentFanOutScopeForReview_FlatAddKeepsItsChannel(t *testing.T) {
	f := newParentFixupScopeFixtureWith(t, "parent", parentFixupScopeOpts{
		flatAdd:       []string{"pkg/x/x.go"},
		subPlan1Extra: []plan.ScopeFile{{Path: "pkg/x/x.go", Operation: plan.FileOpDelete}},
	})
	runRow, _ := f.s.cfg.RunRepo.GetRun(context.Background(), f.runID)
	approvedPlan, _ := f.s.loadApprovedPlanForRun(context.Background(), f.runID)
	for _, fp := range f.s.parentFanOutScopeForReview(context.Background(), runRow, approvedPlan) {
		if fp.Path == "pkg/x/x.go" {
			t.Errorf("flat-add path projected as %+v; the flat channel must keep it", fp)
		}
	}
}

// TestSubPlanScopeChannel_MatchesFullPlanWalk pins the indexed walk to
// scopeFilesFromApprovedPlanFull (the same first-seen rule, so serve's fold
// entries cannot drift) and pins each entry's index to the FIRST declaring
// sub_plan (-1 for top-level) — the index the review Detail renders (C4).
func TestSubPlanScopeChannel_MatchesFullPlanWalk(t *testing.T) {
	p := &plan.Plan{
		Scope: plan.Scope{Files: []plan.ScopeFile{{Path: "t.go", Operation: plan.FileOpModify}}},
		Decomposition: &plan.Decomposition{SubPlans: []plan.SubPlanSummary{
			{Title: "zero", Scope: &plan.Scope{Files: []plan.ScopeFile{
				{Path: "a.go", Operation: plan.FileOpCreate}, {Path: "t.go", Operation: plan.FileOpDelete},
			}}},
			{Title: "one", Scope: &plan.Scope{Files: []plan.ScopeFile{
				{Path: "a.go", Operation: plan.FileOpDelete}, {Path: "b.go", Operation: plan.FileOpModify},
			}}},
			{Title: "two (no scope)"},
		}},
	}
	got := subPlanScopeChannel(p)
	full := scopeFilesFromApprovedPlanFull(p)
	if len(got) != len(full) {
		t.Fatalf("channel = %+v, full walk = %+v", got, full)
	}
	wantIdx := []int{-1, 0, 1}
	for i, e := range got {
		if e.entry.Path != full[i].Path || e.entry.Operation != full[i].Operation {
			t.Errorf("entry %d = %+v, full walk = %+v", i, e.entry, full[i])
		}
		if e.subPlanIndex != wantIdx[i] {
			t.Errorf("entry %d (%s) sub_plan index = %d, want %d", i, e.entry.Path, e.subPlanIndex, wantIdx[i])
		}
	}
	if subPlanScopeChannel(nil) != nil {
		t.Error("nil plan: want nil channel")
	}
}

// TestParentScopeChannels_WithoutAndAppend pins the channel removal and the
// fan-out append: removal drops a path from both channels, and appendFanOutPaths
// keeps base order, dedups against it and stays nil for an ordinary run.
func TestParentScopeChannels_WithoutAndAppend(t *testing.T) {
	ch := parentScopeChannels{
		subPlan:     []parentSubPlanEntry{{entry: scopeamendment.PathEntry{Path: "a.go"}}, {entry: scopeamendment.PathEntry{Path: "b.go"}}},
		sliceGrants: []parentSliceGrant{{path: "b.go", sliceKey: "0"}, {path: "g.go", sliceKey: "1"}},
	}
	got := ch.without([]string{"b.go"})
	if !reflect.DeepEqual(got.subPlanEntries(), []scopeamendment.PathEntry{{Path: "a.go"}}) ||
		!reflect.DeepEqual(got.sliceGrantPaths(), []string{"g.go"}) {
		t.Errorf("without(b.go) = %+v", got)
	}
	if !reflect.DeepEqual(ch.without(nil), ch) {
		t.Error("without(nil) must be the identity")
	}
	if got := appendFanOutPaths(nil, nil); got != nil {
		t.Errorf("appendFanOutPaths(nil, nil) = %v, want nil", got)
	}
	base := []string{"flat.go", "a.go"}
	fan := []parentFanOutScopePath{{Path: "a.go"}, {Path: "g.go"}, {Path: "g.go"}}
	if got := appendFanOutPaths(base, fan); !reflect.DeepEqual(got, []string{"flat.go", "a.go", "g.go"}) {
		t.Errorf("appendFanOutPaths = %v", got)
	}
	if !reflect.DeepEqual(base, []string{"flat.go", "a.go"}) {
		t.Errorf("appendFanOutPaths mutated its base: %v", base)
	}
}
