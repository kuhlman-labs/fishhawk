package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// fixupIncrementKey is the wire key the runner's upload.FetchedPrompt decodes
// (#4391). Asserted as a RAW key so an absent field is distinguished from a
// false one: the omitempty contract is that every non-parent serve carries no
// key at all.
const fixupIncrementKey = `"fixup_policy_increment"`

// fixupIncrementFixture is one prompt-serve shape for the #4391 flag.
type fixupIncrementFixture struct {
	s       *Server
	runID   uuid.UUID
	stageID uuid.UUID
	priv    ed25519.PrivateKey
}

// newFixupIncrementFixture builds a run whose implement stage is served by the
// prompt handler, in one of five shapes:
//
//   - "parent": a decomposed PARENT (DecomposedFrom nil, a child registered so
//     hasDecomposedChildren probes true) with a routed fix-up.
//   - "ordinary": the same run with NO child registered, so the predicate
//     reads false — an ordinary run's fix-up.
//   - "child": a decomposed CHILD (DecomposedFrom set, a matching slice in the
//     parent plan's decomposition) with a routed fix-up.
//   - "parent_no_fixup": the parent with no stage_fixup_triggered row — a
//     plain implement dispatch.
//   - "parent_merge_candidate": the parent with a routed fix-up AND a newer,
//     LIVE merge-candidate trigger, so the serve is a verify-only pass.
//   - "parent_probe_error": the parent with a routed fix-up whose children
//     probe (ListRuns) ERRORS — the predicate degrades to false.
func newFixupIncrementFixture(t *testing.T, shape string) *fixupIncrementFixture {
	t.Helper()
	rr := newPromptRunRepo()
	sf := newSigningFake()
	art := newFakeArtifactRepo()

	parentRunID := uuid.New()
	childRunID := uuid.New()
	planStageID := uuid.New()
	implStageID := uuid.New()

	runID := parentRunID
	p := &plan.Plan{
		PlanVersion:  "standard_v1",
		Summary:      "scoped plan",
		Verification: plan.Verification{TestStrategy: "ts", RollbackPlan: "rb"},
		Scope: plan.Scope{Files: []plan.ScopeFile{
			{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify},
		}},
	}
	sliceIdx := 1
	if shape == "child" {
		runID = childRunID
		p.Decomposition = &plan.Decomposition{
			Rationale: "scope split",
			SubPlans: []plan.SubPlanSummary{
				{Title: "Part A", ScopeHint: "A", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "pkg/a/a.go", Operation: plan.FileOpModify}}}},
				{Title: "Part B", ScopeHint: "B", Scope: &plan.Scope{Files: []plan.ScopeFile{{Path: "backend/internal/server/prompt.go", Operation: plan.FileOpModify}}}},
			},
		}
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
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{
		runID: {
			{ID: planStageID, RunID: runID, Type: run.StageTypePlan},
			{ID: implStageID, RunID: runID, Type: run.StageTypeImplement},
		},
	}
	rr.getStages[implStageID] = &run.Stage{ID: implStageID, RunID: runID, Type: run.StageTypeImplement}

	switch shape {
	case "child":
		rr.getRuns[childRunID] = &run.Run{ID: childRunID, Repo: "o/r", WorkflowID: "feature_change",
			DecomposedFrom: &parentRunID, SliceIndex: &sliceIdx}
	case "ordinary":
		rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r", WorkflowID: "feature_change"}
	default:
		rr.getRuns[parentRunID] = &run.Run{ID: parentRunID, Repo: "o/r", WorkflowID: "feature_change"}
		rr.getRuns[childRunID] = &run.Run{ID: childRunID, Repo: "o/r", WorkflowID: "feature_change",
			DecomposedFrom: &parentRunID}
	}

	var entries []*audit.Entry
	if shape != "parent_no_fixup" {
		entries = append(entries, makeFixupEntry(runID, implStageID,
			[]planreview.Concern{{Severity: planreview.SeverityHigh, Category: "coverage", Note: "tighten the bound"}}))
	}
	if shape == "parent_merge_candidate" {
		payload, _ := json.Marshal(mergeCandidateVerifyTrigger{
			Branch: orchestrator.ConsolidatedBranch(runID), BaseRef: "main",
			ExpectedHeadSHA: strings.Repeat("a", 40), Cause: mergeCandidateCauseFanIn,
			VerifyCommand: "scripts/test verify", PriorState: string(run.StageStateAwaitingApproval),
		})
		rid, sid := runID, implStageID
		entries = append(entries, &audit.Entry{ID: uuid.New(), Category: CategoryStageMergeCandidateVerifyTriggered,
			RunID: &rid, StageID: &sid, Payload: payload, Sequence: 2})
	}

	if shape == "parent_probe_error" {
		rr.listRunsErr = errors.New("list runs: connection reset")
	}

	priv, _ := sf.issue(t, runID)
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      rr,
		SigningRepo:  sf,
		ArtifactRepo: art,
		AuditRepo:    &feedbackAuditRepo{byRunID: map[uuid.UUID][]*audit.Entry{runID: entries}},
	})
	s.promptIssueGetterOverride = &stubIssueGetter{}
	return &fixupIncrementFixture{s: s, runID: runID, stageID: implStageID, priv: priv}
}

// serve fetches the stage's prompt through BOTH surfaces: the signed
// runner-facing dispatch (advertising every capability, so a live
// merge-candidate trigger is served as the verify-only pass rather than
// refused) and the unsigned SPA preview.
func (f *fixupIncrementFixture) serve(t *testing.T) map[string]*httptest.ResponseRecorder {
	t.Helper()
	out := map[string]*httptest.ResponseRecorder{
		"dispatch": promptRequestWithCapabilities(t, f.s, f.stageID, f.priv, capableRunnerCapabilities),
		"preview":  promptRenderRequest(t, f.s, f.stageID),
	}
	for surface, w := range out {
		if w.Code != http.StatusOK {
			t.Fatalf("%s status = %d, want 200:\n%s", surface, w.Code, w.Body.String())
		}
	}
	return out
}

// TestGetStagePrompt_FixupPolicyIncrement pins the #4391 serve: ONLY a
// decomposed parent's fix-up carries fixup_policy_increment:true, on BOTH the
// runner-facing dispatch and the SPA preview, beside the consolidated
// fixup_branch derived from the same predicate. Every other shape omits the
// key entirely.
//
// MECHANISM: the parent and ordinary fixtures differ ONLY in whether a child
// row is registered, so isDecomposedParentRun is the sole thing that decides
// the flag between them — mutating its body to return false reddens the
// parent case, to return true reddens the ordinary case (and the child case
// stays absent only because of the DecomposedFrom == nil half). The
// merge-candidate case isolates the fix-up block's mergeCandidate == nil gate.
func TestGetStagePrompt_FixupPolicyIncrement(t *testing.T) {
	t.Run("decomposed parent fix-up carries the flag", func(t *testing.T) {
		f := newFixupIncrementFixture(t, "parent")
		for surface, w := range f.serve(t) {
			var resp promptResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("%s decode: %v", surface, err)
			}
			if !resp.Fixup {
				t.Fatalf("%s: fixup = false, want true", surface)
			}
			if !resp.FixupPolicyIncrement || !strings.Contains(w.Body.String(), fixupIncrementKey+":true") {
				t.Errorf("%s: fixup_policy_increment absent/false on a decomposed parent's fix-up:\n%s", surface, w.Body.String())
			}
			if want := orchestrator.ConsolidatedBranch(f.runID); resp.FixupBranch != want {
				t.Errorf("%s: fixup_branch = %q, want the consolidated branch %q (same predicate as the flag)",
					surface, resp.FixupBranch, want)
			}
		}
	})

	for _, tc := range []struct {
		shape     string
		wantFixup bool
		why       string
	}{
		{"ordinary", true, "an ordinary run's fix-up keeps the cumulative diff"},
		{"child", true, "a decomposed child's fix-up uses its own slice-branch base (#765)"},
		{"parent_no_fixup", false, "a parent's non-fix-up implement dispatch is not a fix-up"},
		{"parent_merge_candidate", false, "a verify-only merge-candidate pass serves no fix-up block"},
		{"parent_probe_error", true, "a children-probe error degrades to today's cumulative diff"},
	} {
		t.Run(tc.shape+" omits the flag", func(t *testing.T) {
			f := newFixupIncrementFixture(t, tc.shape)
			for surface, w := range f.serve(t) {
				if strings.Contains(w.Body.String(), fixupIncrementKey) {
					t.Errorf("%s: %s key present (%s):\n%s", surface, fixupIncrementKey, tc.why, w.Body.String())
				}
				var resp promptResponse
				if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
					t.Fatalf("%s decode: %v", surface, err)
				}
				if resp.Fixup != tc.wantFixup {
					t.Errorf("%s: fixup = %v, want %v (fixture precondition)", surface, resp.Fixup, tc.wantFixup)
				}
				if tc.shape == "parent_merge_candidate" && !resp.MergeCandidateVerify {
					t.Errorf("%s: merge_candidate_verify = false; the fixture must serve the verify-only pass", surface)
				}
			}
		})
	}
}

// decomposedParentPolicyRepo is the trace handler's policy fake with ONE
// change: ListRuns reports a decomposition child for the run, so the run under
// test IS a decomposed parent by the same probe the prompt serve uses.
type decomposedParentPolicyRepo struct {
	*policyRunRepo
	child *run.Run
}

func (r *decomposedParentPolicyRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	if f.DecomposedFrom != nil && *f.DecomposedFrom == r.runRow.ID {
		return []*run.Run{r.child}, nil
	}
	return nil, nil
}

// fixupIncrementSpec caps the implement stage at 2 files — BELOW the
// cumulative fan-in diff (3 child files + the fix-up's 1) and ABOVE the
// fix-up's own increment.
const fixupIncrementSpec = `
version: "0.3"
roles:
  eng_team:
    members: ["@org/eng"]
workflows:
  feature_change:
    description: Test workflow
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
        constraints:
          - max_files_changed: 2
        gates:
          - type: approval
            approvers:
              any_of: [eng_team]
            sla: 4_hours
`

// fixupBundle packs a runner-shaped bundle whose git_diff events carry the
// given (base_ref, files) payloads in order — the shape computeAndEmitDiff and
// reemitScopedGitDiff write, keyed exactly as the runner's wire.
func fixupBundle(t *testing.T, diffs ...struct {
	baseRef string
	files   []map[string]string
}) []byte {
	t.Helper()
	var raw bytes.Buffer
	manifest, _ := json.Marshal(map[string]any{"bundle_schema": "v1"})
	line, _ := json.Marshal(map[string]any{"seq": 1, "kind": "manifest", "data": json.RawMessage(manifest)})
	raw.Write(line)
	raw.WriteByte('\n')
	for i, d := range diffs {
		payload, _ := json.Marshal(map[string]any{
			"kind": "name_status", "base_ref": d.baseRef, "files": d.files, "num_files": len(d.files),
		})
		line, _ := json.Marshal(map[string]any{"seq": i + 2, "kind": "git_diff", "data": json.RawMessage(payload)})
		raw.Write(line)
		raw.WriteByte('\n')
	}
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	if _, err := w.Write(raw.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// TestShipTrace_DecomposedParentFixup_PolicyBoundedToIncrement is approval
// condition C3: the backend half of parent-fixup-policy-bounded-to-increment.
// It drives a real bundle through the trace-upload path (reEvaluatePolicy →
// bundle.ExtractDiff → the cached spec's constraints → the persisted
// policy_evaluated row) on a decomposed-parent run whose cached spec caps
// max_files_changed at 2, below the cumulative fan-in count of 4.
//
//   - (a) The bundle the runner ships on the increment path — every git_diff
//     measured from the consolidated tip, base_ref the bare tip SHA — whose
//     LAST git_diff lists the fix-up's ONE file records policy_evaluated
//     passed with no max_files_changed violation, and the stage is not failed.
//   - (b) The same run with the cumulative diff (base_ref main, all four
//     files) records the max_files_changed violation and fails the stage B.
//
// Together with the runner's TestRun_Fixup_DecomposedParent_PolicyBoundedToIncrement
// (which asserts the bundle's last git_diff IS the one-file increment) this
// joins the runner-side bundle to the persisted backend policy result: the
// backend needed no policy change, because it re-evaluates whatever the last
// git_diff carries.
func TestShipTrace_DecomposedParentFixup_PolicyBoundedToIncrement(t *testing.T) {
	type diff = struct {
		baseRef string
		files   []map[string]string
	}
	tipSHA := strings.Repeat("c", 40)
	fixupFile := []map[string]string{{"path": "backend/internal/server/fixup.go", "status": "A"}}
	cumulative := []map[string]string{
		{"path": "pkg/a/a.go", "status": "A"},
		{"path": "pkg/b/b.go", "status": "A"},
		{"path": "pkg/c/c.go", "status": "A"},
		{"path": "backend/internal/server/fixup.go", "status": "A"},
	}

	for _, tc := range []struct {
		name          string
		bundle        func(t *testing.T) []byte
		wantViolation bool
	}{
		{"increment", func(t *testing.T) []byte {
			// The in-band emit and the verify-fix re-emit both measured from
			// the tip; the LAST one is what the backend evaluates.
			return fixupBundle(t, diff{tipSHA, fixupFile}, diff{tipSHA, fixupFile})
		}, false},
		{"cumulative", func(t *testing.T) []byte {
			return fixupBundle(t, diff{"main", cumulative}, diff{"main", cumulative})
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, sf, repo, au := newPolicyTraceServer(t, nil)
			repo.runRow.WorkflowSpec = []byte(fixupIncrementSpec)
			parentID := repo.runRow.ID
			s.cfg.RunRepo = &decomposedParentPolicyRepo{policyRunRepo: repo,
				child: &run.Run{ID: uuid.New(), Repo: repo.runRow.Repo, WorkflowID: "feature_change", DecomposedFrom: &parentID}}
			if !s.isDecomposedParentRun(t.Context(), repo.runRow) {
				t.Fatal("fixture precondition: the run must be a decomposed parent")
			}
			priv, _ := sf.issue(t, repo.runRow.ID)

			w := shipRequest(t, s, repo.runRow.ID, repo.stage.ID, "raw", priv, tc.bundle(t), "")
			if w.Code != http.StatusAccepted {
				t.Fatalf("status = %d:\n%s", w.Code, w.Body.String())
			}
			pl := decodePolicyPayload(t, au)
			if pl.Applied.MaxFilesChanged != 2 {
				t.Fatalf("applied max_files_changed = %v, want 2 (the cached spec must resolve)", pl.Applied.MaxFilesChanged)
			}
			var hasCap bool
			for _, v := range pl.Violations {
				if v.Constraint == "max_files_changed" {
					hasCap = true
				}
			}
			if hasCap != tc.wantViolation || pl.Passed == tc.wantViolation {
				t.Fatalf("policy_evaluated passed=%v violations=%+v; want max_files_changed violation=%v",
					pl.Passed, pl.Violations, tc.wantViolation)
			}
			failedB := repo.stage.State == run.StageStateFailed &&
				repo.stage.FailureCategory != nil && *repo.stage.FailureCategory == run.FailureB
			if failedB != tc.wantViolation {
				t.Errorf("stage state = %q (category %v); want failed-B = %v", repo.stage.State, repo.stage.FailureCategory, tc.wantViolation)
			}
		})
	}
}
